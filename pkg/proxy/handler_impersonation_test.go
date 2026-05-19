package proxy

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	auditv1 "k8s.io/apiserver/pkg/apis/audit/v1"
	"k8s.io/client-go/transport"
)

const impersonationBackendToken = "proxy-serviceaccount-token"

func writeTokenFile(t *testing.T, token string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(path, []byte(token), 0o600))
	return path
}

func newImpersonationHandler(t *testing.T, backendURL *url.URL, webhookClient *fakeWebhookClient) *Handler {
	t.Helper()

	tokenFile := writeTokenFile(t, impersonationBackendToken)
	bearerTransport, err := transport.NewBearerAuthWithRefreshRoundTripper("", tokenFile, http.DefaultTransport)
	require.NoError(t, err)

	handler, err := NewHandler(HandlerConfig{
		BackendURL:    backendURL,
		WebhookClient: webhookClient,
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Transport:     bearerTransport,
		BackendIdentity: NewImpersonator(ImpersonatorConfig{
			ExtraKeys:  []string{"scopes"},
			ForwardUID: true,
		}),
		MaxAuditBodyBytes: 4096,
	})
	require.NoError(t, err)
	return handler
}

func setDelegatedIdentityHeaders(req *http.Request) {
	req.Header.Set("X-Remote-User", "alice")
	req.Header.Add("X-Remote-Group", "devs")
	req.Header.Add("X-Remote-Group", "admins")
	req.Header.Set("X-Remote-Uid", "uid-alice")
	req.Header.Set("X-Remote-Extra-Scopes", "read")
	// Caller-supplied headers that must never reach the backend.
	req.Header.Set("Authorization", "Bearer user-supplied-token")
	req.Header.Set("Impersonate-User", "cluster-admin")
	req.RemoteAddr = "10.0.0.5:12345"
}

func assertBackendImpersonation(t *testing.T, backendRequest *http.Request) {
	t.Helper()

	assert.Equal(t, "Bearer "+impersonationBackendToken, backendRequest.Header.Get("Authorization"))
	assert.Equal(t, "alice", backendRequest.Header.Get("Impersonate-User"))
	assert.Equal(t, []string{"devs", "admins"}, backendRequest.Header.Values("Impersonate-Group"))
	assert.Equal(t, "uid-alice", backendRequest.Header.Get("Impersonate-Uid"))
	assert.Equal(t, "10.0.0.5", backendRequest.Header.Get("X-Forwarded-For"))
	// Inbound identity surface must be stripped.
	assert.Empty(t, backendRequest.Header.Get("X-Remote-User"))
	assert.Empty(t, backendRequest.Header.Values("X-Remote-Extra-Scopes"))
}

func TestHandler_ImpersonationMode_AuditedPost(t *testing.T) {
	t.Parallel()

	backendRequests := make(chan *http.Request, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backendRequests <- r.Clone(context.Background())
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"metadata":{"name":"imp-probe"}}`))
	}))
	defer backend.Close()

	backendURL, err := url.Parse(backend.URL)
	require.NoError(t, err)

	webhookClient := &fakeWebhookClient{delivered: make(chan auditv1.EventList, 1)}
	handler := newImpersonationHandler(t, backendURL, webhookClient)

	req := httptest.NewRequest(
		http.MethodPost,
		"http://proxy.local/apis/wardle.example.com/v1alpha1/namespaces/default/flunders",
		strings.NewReader(`{"metadata":{"name":"imp-probe","namespace":"default"}}`),
	)
	req.Header.Set("Content-Type", "application/json")
	setDelegatedIdentityHeaders(req)

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	assert.Equal(t, http.StatusCreated, recorder.Code)

	select {
	case backendRequest := <-backendRequests:
		assertBackendImpersonation(t, backendRequest)
		assert.Equal(t, []string{"read"}, backendRequest.Header.Values("Impersonate-Extra-Scopes"))
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for backend request")
	}

	select {
	case delivered := <-webhookClient.delivered:
		require.Len(t, delivered.Items, 1)
		// The audit event keeps the real delegated user, not the proxy ServiceAccount.
		assert.Equal(t, "alice", delivered.Items[0].User.Username)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for webhook delivery")
	}
}

func TestHandler_ImpersonationMode_GetPassthrough(t *testing.T) {
	t.Parallel()

	backendRequests := make(chan *http.Request, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backendRequests <- r.Clone(context.Background())
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"items":[]}`))
	}))
	defer backend.Close()

	backendURL, err := url.Parse(backend.URL)
	require.NoError(t, err)

	webhookClient := &fakeWebhookClient{delivered: make(chan auditv1.EventList, 1)}
	handler := newImpersonationHandler(t, backendURL, webhookClient)

	req := httptest.NewRequest(
		http.MethodGet,
		"http://proxy.local/apis/wardle.example.com/v1alpha1/namespaces/default/flunders",
		nil,
	)
	setDelegatedIdentityHeaders(req)

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	assert.Equal(t, http.StatusOK, recorder.Code)

	select {
	case backendRequest := <-backendRequests:
		// A GET is not audited, so this exercises the ReverseProxy Rewrite hook.
		// The same impersonation decoration must be applied.
		assertBackendImpersonation(t, backendRequest)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for backend request")
	}

	select {
	case <-webhookClient.delivered:
		t.Fatal("did not expect audit delivery for GET request")
	default:
	}
}
