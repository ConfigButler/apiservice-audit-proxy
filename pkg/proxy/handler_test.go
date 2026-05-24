package proxy

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	auditv1 "k8s.io/apiserver/pkg/apis/audit/v1"
	requestinfo "k8s.io/apiserver/pkg/endpoints/request"

	"github.com/ConfigButler/apiservice-audit-proxy/pkg/identity"
	"github.com/ConfigButler/apiservice-audit-proxy/pkg/telemetry"
)

func TestHandler_MutatingRequest_ProxiesAndEmitsEvent(t *testing.T) {
	t.Parallel()

	requestBody := `{"apiVersion":"wardle.example.com/v1alpha1","kind":"Flunder","metadata":{"name":"audit-probe","namespace":"default"},"spec":{"reference":"alpha"}}`
	responseBody := `{"apiVersion":"wardle.example.com/v1alpha1","kind":"Flunder","metadata":{"name":"audit-probe","namespace":"default","uid":"uid-123"},"spec":{"reference":"alpha"}}`

	backendRequests := make(chan *http.Request, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		assert.NoError(t, err)
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
	req.Header.Set("Audit-Id", "audit-123")
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

func TestNewHandler_PassthroughFlushesImmediately(t *testing.T) {
	t.Parallel()

	backendURL, err := url.Parse("http://backend.local")
	require.NoError(t, err)

	handler, err := NewHandler(HandlerConfig{
		BackendURL:        backendURL,
		WebhookClient:     &fakeWebhookClient{},
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaxAuditBodyBytes: 4096,
	})
	require.NoError(t, err)

	assert.Equal(t, -1*time.Nanosecond, handler.passthrough.FlushInterval)
}

func TestHandler_WatchRequest_UsesPassthroughWithoutAudit(t *testing.T) {
	t.Parallel()

	firstEventWritten := make(chan struct{})
	writeSecondEvent := make(chan struct{})
	backendRequests := make(chan *http.Request, 1)

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backendRequests <- r.Clone(context.Background())

		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Errorf("test backend must support flushing")
			return
		}

		w.Header().Set("Content-Type", "application/json;stream=watch")
		w.WriteHeader(http.StatusOK)
		firstEvent := `{"type":"BOOKMARK","object":{"kind":"Status","apiVersion":"v1","metadata":{"resourceVersion":"1"}}}` + "\n"
		_, err := w.Write([]byte(firstEvent))
		assert.NoError(t, err)
		flusher.Flush()
		close(firstEventWritten)

		<-writeSecondEvent
		secondEvent := `{"type":"ADDED","object":{"apiVersion":"wardle.example.com/v1alpha1","kind":"Flunder","metadata":{"name":"streamed","namespace":"default","resourceVersion":"2"}}}` + "\n"
		_, err = w.Write([]byte(secondEvent))
		assert.NoError(t, err)
		flusher.Flush()
	}))
	defer backend.Close()

	backendURL, err := url.Parse(backend.URL)
	require.NoError(t, err)

	webhookClient := &fakeWebhookClient{delivered: make(chan auditv1.EventList, 1)}
	handler, err := NewHandler(HandlerConfig{
		BackendURL:        backendURL,
		WebhookClient:     webhookClient,
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaxAuditBodyBytes: 4096,
	})
	require.NoError(t, err)

	proxy := httptest.NewServer(handler)
	defer proxy.Close()

	req, err := http.NewRequest(
		http.MethodGet,
		proxy.URL+"/apis/wardle.example.com/v1alpha1/namespaces/default/flunders"+
			"?watch=true&allowWatchBookmarks=true&resourceVersion=1",
		nil,
	)
	require.NoError(t, err)
	req.Header.Set("X-Remote-User", "alice")

	resp, err := proxy.Client().Do(req)
	require.NoError(t, err)
	defer func() {
		require.NoError(t, resp.Body.Close())
	}()

	require.Equal(t, http.StatusOK, resp.StatusCode)

	reader := bufio.NewReader(resp.Body)
	select {
	case <-firstEventWritten:
	case <-time.After(2 * time.Second):
		t.Fatal("backend did not write first watch event")
	}

	firstLine, err := reader.ReadString('\n')
	require.NoError(t, err)
	assert.Contains(t, firstLine, `"BOOKMARK"`)

	close(writeSecondEvent)
	secondLine, err := reader.ReadString('\n')
	require.NoError(t, err)
	assert.Contains(t, secondLine, `"ADDED"`)

	select {
	case backendRequest := <-backendRequests:
		assert.Equal(t, "/apis/wardle.example.com/v1alpha1/namespaces/default/flunders", backendRequest.URL.Path)
		assert.Equal(t, "true", backendRequest.URL.Query().Get("watch"))
		assert.Equal(t, "true", backendRequest.URL.Query().Get("allowWatchBookmarks"))
		assert.Equal(t, "alice", backendRequest.Header.Get("X-Remote-User"))
		assert.NotEmpty(t, backendRequest.Header.Get("X-Forwarded-For"))
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for backend request")
	}

	select {
	case <-webhookClient.delivered:
		t.Fatal("did not expect audit delivery for watch request")
	default:
	}
}

func TestShouldAudit_ExcludesReadAndLongRunningVerbs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		verb string
		want bool
	}{
		{verb: "get"},
		{verb: "list"},
		{verb: "watch"},
		{verb: "proxy"},
		{verb: "connect"},
		{verb: "create", want: true},
		{verb: "update", want: true},
		{verb: "patch", want: true},
		{verb: "delete", want: true},
	}

	for _, tt := range tests {
		t.Run(tt.verb, func(t *testing.T) {
			t.Parallel()

			got := shouldAudit(&requestinfo.RequestInfo{
				IsResourceRequest: true,
				Verb:              tt.verb,
			})
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestHandler_RecordsRequestMetrics(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[]}`))
	}))
	defer backend.Close()

	backendURL, err := url.Parse(backend.URL)
	require.NoError(t, err)

	handler, err := NewHandler(HandlerConfig{
		BackendURL:        backendURL,
		WebhookClient:     &fakeWebhookClient{delivered: make(chan auditv1.EventList, 1)},
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaxAuditBodyBytes: 4096,
	})
	require.NoError(t, err)

	req := httptest.NewRequest(
		http.MethodGet,
		"http://proxy.local/apis/wardle.example.com/v1alpha1/namespaces/default/flunders",
		nil,
	)
	req.Header.Set("X-Remote-User", "alice")

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	assert.Equal(t, http.StatusOK, recorder.Code)

	requests, ok := telemetry.CollectInt64Sum(reader, "apiservice_audit_proxy_requests_total", map[string]string{
		"verb":           "list",
		"resource_group": "wardle.example.com",
		"resource":       "flunders",
		"audited":        "false",
		"streaming":      "false",
		"status_class":   "2xx",
		"outcome":        "ok",
	})
	require.True(t, ok)
	assert.Equal(t, int64(1), requests)

	durationCount, ok := telemetry.CollectHistogramCount(reader,
		"apiservice_audit_proxy_request_duration_seconds",
		map[string]string{"verb": "list", "resource": "flunders"})
	require.True(t, ok)
	assert.Equal(t, uint64(1), durationCount)

	backendDurationCount, ok := telemetry.CollectHistogramCount(reader,
		"apiservice_audit_proxy_backend_roundtrip_seconds",
		map[string]string{"verb": "list", "outcome": "ok"})
	require.True(t, ok)
	assert.Equal(t, uint64(1), backendDurationCount)
}

func TestHandler_RecordsStreamingMetrics(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	firstEventWritten := make(chan struct{})
	releaseBackend := make(chan struct{})
	releaseBackendOnce := sync.Once{}
	releaseBackendForNextEvent := func() {
		releaseBackendOnce.Do(func() {
			close(releaseBackend)
		})
	}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Errorf("test backend must support flushing")
			return
		}

		w.Header().Set("Content-Type", "application/json;stream=watch")
		w.WriteHeader(http.StatusOK)
		firstEvent := `{"type":"BOOKMARK","object":{"kind":"Status","apiVersion":"v1","metadata":{"resourceVersion":"1"}}}` + "\n"
		_, writeErr := w.Write([]byte(firstEvent))
		assert.NoError(t, writeErr)
		flusher.Flush()
		close(firstEventWritten)

		select {
		case <-releaseBackend:
			nextEvent := `{"type":"BOOKMARK","object":{"kind":"Status","apiVersion":"v1","metadata":{"resourceVersion":"2"}}}` + "\n"
			_, writeErr := w.Write([]byte(nextEvent))
			assert.NoError(t, writeErr)
			flusher.Flush()
			<-r.Context().Done()
		case <-r.Context().Done():
		}
	}))
	defer backend.Close()
	defer releaseBackendForNextEvent()

	backendURL, err := url.Parse(backend.URL)
	require.NoError(t, err)

	handler, err := NewHandler(HandlerConfig{
		BackendURL:        backendURL,
		WebhookClient:     &fakeWebhookClient{delivered: make(chan auditv1.EventList, 1)},
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaxAuditBodyBytes: 4096,
	})
	require.NoError(t, err)

	proxy := httptest.NewServer(handler)
	defer proxy.Close()

	req, err := http.NewRequest(
		http.MethodGet,
		proxy.URL+"/apis/wardle.example.com/v1alpha1/namespaces/default/flunders?watch=true",
		nil,
	)
	require.NoError(t, err)
	req.Header.Set("X-Remote-User", "alice")

	resp, err := proxy.Client().Do(req)
	require.NoError(t, err)

	select {
	case <-firstEventWritten:
	case <-time.After(2 * time.Second):
		t.Fatal("backend did not write first watch event")
	}

	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	require.NoError(t, err)
	assert.Contains(t, line, `"BOOKMARK"`)

	active, ok := telemetry.CollectInt64Sum(reader, "apiservice_audit_proxy_streams_active", map[string]string{
		"kind":          "watch",
		"outcome":       "active",
		"inbound_proto": "http1",
	})
	assert.False(t, ok, "active stream gauge should not carry an outcome label")
	_ = active

	active, ok = telemetry.CollectInt64Sum(reader, "apiservice_audit_proxy_streams_active", map[string]string{
		"kind":          "watch",
		"inbound_proto": "http1",
		"backend_proto": "http1",
	})
	require.True(t, ok)
	assert.Equal(t, int64(1), active)

	require.NoError(t, resp.Body.Close())
	releaseBackendForNextEvent()

	require.EventuallyWithT(t, func(c *assert.CollectT) {
		active, ok := telemetry.CollectInt64Sum(reader, "apiservice_audit_proxy_streams_active", map[string]string{
			"kind":          "watch",
			"inbound_proto": "http1",
			"backend_proto": "http1",
		})
		require.True(c, ok)
		assert.Equal(c, int64(0), active)

		var durationCount uint64
		for _, outcome := range []string{outcomeClientCancel, outcomeBackendClose, outcomeReadError} {
			count, found := telemetry.CollectHistogramCount(reader,
				"apiservice_audit_proxy_stream_duration_seconds",
				map[string]string{"kind": "watch", "outcome": outcome})
			if found {
				durationCount += count
			}
		}
		assert.Equal(c, uint64(1), durationCount)
	}, 2*time.Second, 25*time.Millisecond)

	backendBytes, ok := telemetry.CollectInt64Sum(reader,
		"apiservice_audit_proxy_transport_bytes_total",
		map[string]string{
			"leg":       "backend",
			"streaming": "true",
			"direction": "read",
		})
	require.True(t, ok)
	assert.Positive(t, backendBytes)

	clientBytes, ok := telemetry.CollectInt64Sum(reader,
		"apiservice_audit_proxy_transport_bytes_total",
		map[string]string{
			"leg":       "client",
			"streaming": "true",
			"direction": "write",
		})
	require.True(t, ok)
	assert.Positive(t, clientBytes)
}

func TestObservedReadCloser_ClassifiesBackendReadError(t *testing.T) {
	t.Parallel()

	expectedErr := errors.New("backend reset")
	var gotOutcome string
	body := observeClose(readCloserFunc{
		read: func(_ []byte) (int, error) {
			return 0, expectedErr
		},
		close: func() error {
			return nil
		},
	}, func(outcome string) {
		gotOutcome = outcome
	})

	_, err := body.Read(make([]byte, 1))
	require.ErrorIs(t, err, expectedErr)
	assert.Equal(t, outcomeReadError, gotOutcome)

	require.NoError(t, body.Close())
	assert.Equal(t, outcomeReadError, gotOutcome)
}

type readCloserFunc struct {
	read  func([]byte) (int, error)
	close func() error
}

func (f readCloserFunc) Read(p []byte) (int, error) {
	return f.read(p)
}

func (f readCloserFunc) Close() error {
	return f.close()
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

func TestHandler_RequiresVerifiedDelegatedIdentity_WhenClientCAConfigured(t *testing.T) {
	t.Parallel()

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(backend.Close)

	backendURL, err := url.Parse(backend.URL)
	require.NoError(t, err)

	caFile, clientCertificate := writeRequestHeaderClientCAFixture(t)
	identityExtractor, err := identity.NewExtractor(caFile, nil)
	require.NoError(t, err)

	handler, err := NewHandler(HandlerConfig{
		BackendURL:        backendURL,
		WebhookClient:     &fakeWebhookClient{delivered: make(chan auditv1.EventList, 1)},
		IdentityExtractor: identityExtractor,
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaxAuditBodyBytes: 4096,
	})
	require.NoError(t, err)

	t.Run("missing client certificate", func(t *testing.T) {
		t.Parallel()
		req := httptest.NewRequest(
			http.MethodGet,
			"http://proxy.local/apis/wardle.example.com/v1alpha1/namespaces/default/flunders",
			nil,
		)
		req.Header.Set("X-Remote-User", "alice")

		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, req)
		assert.Equal(t, http.StatusUnauthorized, recorder.Code)
	})

	t.Run("valid client certificate", func(t *testing.T) {
		t.Parallel()
		req := httptest.NewRequest(
			http.MethodGet,
			"http://proxy.local/apis/wardle.example.com/v1alpha1/namespaces/default/flunders",
			nil,
		)
		req.Header.Set("X-Remote-User", "alice")
		req.TLS = &tls.ConnectionState{
			PeerCertificates: []*x509.Certificate{clientCertificate},
		}

		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, req)
		assert.Equal(t, http.StatusOK, recorder.Code)
	})
}

func TestHandler_AuditedPath_PreservesBackendErrorStatus(t *testing.T) {
	t.Parallel()

	// Checklist D: a backend denial must be returned faithfully to the client
	// and never converted into a proxy-level success. The emitted audit event
	// must also record the backend's real status code.
	const forbiddenBody = `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Forbidden","code":403}`

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(forbiddenBody))
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
		strings.NewReader(`{"metadata":{"name":"audit-probe","namespace":"default"}}`),
	)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Remote-User", "alice")

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)

	resp := recorder.Result()
	defer func() {
		require.NoError(t, resp.Body.Close())
	}()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	assert.JSONEq(t, forbiddenBody, string(body))

	select {
	case delivered := <-webhookClient.delivered:
		require.Len(t, delivered.Items, 1)
		require.NotNil(t, delivered.Items[0].ResponseStatus)
		assert.EqualValues(t, http.StatusForbidden, delivered.Items[0].ResponseStatus.Code)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for webhook delivery")
	}
}

func TestHandler_AuditedPath_ForwardsRequestFaithfully(t *testing.T) {
	t.Parallel()

	// Checklist D: method, path, query string, body, and content type must reach
	// the backend unchanged — the proxy must not introduce hidden semantic edits.
	const requestBody = `{"metadata":{"name":"audit-probe","namespace":"default"}}`

	backendRequests := make(chan *http.Request, 1)
	backendBodies := make(chan string, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		assert.NoError(t, err)
		backendBodies <- string(body)
		backendRequests <- r.Clone(context.Background())

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
		"http://proxy.local/apis/wardle.example.com/v1alpha1/namespaces/default/flunders/audit-probe"+
			"?fieldManager=kubectl&dryRun=All&fieldValidation=Strict",
		strings.NewReader(requestBody),
	)
	req.Header.Set("Content-Type", "application/merge-patch+json")
	req.Header.Set("X-Remote-User", "alice")

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	assert.Equal(t, http.StatusOK, recorder.Code)

	select {
	case backendRequest := <-backendRequests:
		assert.Equal(t, http.MethodPatch, backendRequest.Method)
		assert.Equal(t,
			"/apis/wardle.example.com/v1alpha1/namespaces/default/flunders/audit-probe",
			backendRequest.URL.Path,
		)
		assert.Equal(t, "fieldManager=kubectl&dryRun=All&fieldValidation=Strict", backendRequest.URL.RawQuery)
		assert.Equal(t, "application/merge-patch+json", backendRequest.Header.Get("Content-Type"))
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for backend request")
	}

	select {
	case body := <-backendBodies:
		assert.JSONEq(t, requestBody, body)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for backend body")
	}
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

func writeRequestHeaderClientCAFixture(t *testing.T) (string, *x509.Certificate) {
	t.Helper()

	caPrivateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "front-proxy-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}

	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caPrivateKey.PublicKey, caPrivateKey)
	require.NoError(t, err)

	clientPrivateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	clientTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(2),
		Subject:               pkix.Name{CommonName: "kube-aggregator"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}

	caCert, err := x509.ParseCertificate(caDER)
	require.NoError(t, err)

	clientDER, err := x509.CreateCertificate(
		rand.Reader,
		clientTemplate,
		caCert,
		&clientPrivateKey.PublicKey,
		caPrivateKey,
	)
	require.NoError(t, err)

	caFile := filepath.Join(t.TempDir(), "client-ca.pem")
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	require.NotEmpty(t, caPEM)
	require.NoError(t, os.WriteFile(caFile, caPEM, 0o600))

	clientCert, err := x509.ParseCertificate(clientDER)
	require.NoError(t, err)
	return caFile, clientCert
}
