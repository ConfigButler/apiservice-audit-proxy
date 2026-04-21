package proxy

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	auditv1 "k8s.io/apiserver/pkg/apis/audit/v1"
)

func TestHandler_MutatingRequest_ProxiesAndEmitsEvent(t *testing.T) {
	t.Parallel()

	requestBody := `{"apiVersion":"wardle.example.com/v1alpha1","kind":"Flunder","metadata":{"name":"audit-probe","namespace":"default"},"spec":{"reference":"alpha"}}`
	responseBody := `{"apiVersion":"wardle.example.com/v1alpha1","kind":"Flunder","metadata":{"name":"audit-probe","namespace":"default","uid":"uid-123"},"spec":{"reference":"alpha"}}`

	backendRequests := make(chan *http.Request, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		assert.JSONEq(t, requestBody, string(body))

		backendRequests <- r.Clone(context.Background())

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(responseBody))
	}))
	defer backend.Close()

	webhookClient := &fakeWebhookClient{delivered: make(chan auditv1.EventList, 1)}
	backendURL, err := url.Parse(backend.URL)
	require.NoError(t, err)

	handler, err := NewHandler(HandlerConfig{
		BackendURL:        backendURL,
		WebhookClient:     webhookClient,
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaxAuditBodyBytes: 4096,
	})
	require.NoError(t, err)

	req := httptest.NewRequest(
		http.MethodPost,
		"http://proxy.local/apis/wardle.example.com/v1alpha1/namespaces/default/flunders",
		strings.NewReader(requestBody),
	)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Audit-ID", "audit-123")
	req.Header.Set("X-Remote-User", "alice")
	req.Header.Set("X-Remote-Group", "devs")
	req.RemoteAddr = "10.0.0.5:12345"

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)

	resp := recorder.Result()
	defer func() {
		require.NoError(t, resp.Body.Close())
	}()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, http.StatusCreated, resp.StatusCode)
	assert.JSONEq(t, responseBody, string(body))

	select {
	case backendRequest := <-backendRequests:
		assert.Equal(t, "/apis/wardle.example.com/v1alpha1/namespaces/default/flunders", backendRequest.URL.Path)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for backend request")
	}

	select {
	case delivered := <-webhookClient.delivered:
		require.Len(t, delivered.Items, 1)
		assert.Equal(t, "create", delivered.Items[0].Verb)
		assert.Equal(t, "alice", delivered.Items[0].User.Username)
		require.NotNil(t, delivered.Items[0].ObjectRef)
		assert.Equal(t, "audit-probe", delivered.Items[0].ObjectRef.Name)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for webhook delivery")
	}
}

func TestHandler_GetRequest_PassesThroughWithoutAuditDelivery(t *testing.T) {
	t.Parallel()

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/apis/wardle.example.com/v1alpha1/namespaces/default/flunders", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"items":[]}`))
	}))
	defer backend.Close()

	webhookClient := &fakeWebhookClient{delivered: make(chan auditv1.EventList, 1)}
	backendURL, err := url.Parse(backend.URL)
	require.NoError(t, err)

	handler, err := NewHandler(HandlerConfig{
		BackendURL:        backendURL,
		WebhookClient:     webhookClient,
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaxAuditBodyBytes: 4096,
	})
	require.NoError(t, err)

	req := httptest.NewRequest(
		http.MethodGet,
		"http://proxy.local/apis/wardle.example.com/v1alpha1/namespaces/default/flunders",
		nil,
	)

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)

	resp := recorder.Result()
	defer func() {
		require.NoError(t, resp.Body.Close())
	}()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	select {
	case <-webhookClient.delivered:
		t.Fatal("did not expect audit delivery for GET request")
	default:
	}
}

func TestHandler_WebhookFailure_DoesNotFailProxiedResponse(t *testing.T) {
	t.Parallel()

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer backend.Close()

	backendURL, err := url.Parse(backend.URL)
	require.NoError(t, err)

	handler, err := NewHandler(HandlerConfig{
		BackendURL:        backendURL,
		WebhookClient:     &fakeWebhookClient{sendErr: errors.New("webhook down")},
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaxAuditBodyBytes: 4096,
	})
	require.NoError(t, err)

	req := httptest.NewRequest(
		http.MethodPost,
		"http://proxy.local/apis/wardle.example.com/v1alpha1/namespaces/default/flunders",
		strings.NewReader(`{"metadata":{"name":"audit-probe","namespace":"default"}}`),
	)
	req.Header.Set("X-Remote-User", "alice")

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)

	resp := recorder.Result()
	defer func() {
		require.NoError(t, resp.Body.Close())
	}()
	assert.Equal(t, http.StatusCreated, resp.StatusCode)
}

func TestHandler_AuditedPath_StripsHopByHopHeaders(t *testing.T) {
	t.Parallel()

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Empty(t, r.Header.Get("Connection"))
		assert.Empty(t, r.Header.Get("Proxy-Connection"))
		assert.Empty(t, r.Header.Get("X-Remove-Me"))
		assert.Equal(t, "10.0.0.5", r.Header.Get("X-Forwarded-For"))

		w.Header().Set("Connection", "close")
		w.Header().Set("Proxy-Connection", "keep-alive")
		w.Header().Set("Upgrade", "websocket")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer backend.Close()

	webhookClient := &fakeWebhookClient{delivered: make(chan auditv1.EventList, 1)}
	backendURL, err := url.Parse(backend.URL)
	require.NoError(t, err)

	handler, err := NewHandler(HandlerConfig{
		BackendURL:        backendURL,
		WebhookClient:     webhookClient,
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaxAuditBodyBytes: 4096,
	})
	require.NoError(t, err)

	req := httptest.NewRequest(
		http.MethodPatch,
		"http://proxy.local/apis/wardle.example.com/v1alpha1/namespaces/default/flunders/audit-probe",
		strings.NewReader(`{"metadata":{"name":"audit-probe","namespace":"default"}}`),
	)
	req.Header.Set("Connection", "X-Remove-Me")
	req.Header.Set("Proxy-Connection", "keep-alive")
	req.Header.Set("X-Remove-Me", "please-strip-me")
	req.Header.Set("X-Remote-User", "alice")
	req.RemoteAddr = "10.0.0.5:12345"

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)

	resp := recorder.Result()
	defer func() {
		require.NoError(t, resp.Body.Close())
	}()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Empty(t, resp.Header.Get("Connection"))
	assert.Empty(t, resp.Header.Get("Proxy-Connection"))
	assert.Empty(t, resp.Header.Get("Upgrade"))
}

type fakeWebhookClient struct {
	delivered chan auditv1.EventList
	sendErr   error
}

func (f *fakeWebhookClient) Send(_ context.Context, eventList auditv1.EventList) error {
	if f.delivered != nil {
		f.delivered <- eventList
	}

	return f.sendErr
}
