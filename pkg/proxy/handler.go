package proxy

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	auditevents "github.com/ConfigButler/audit-pass-through-apiserver/pkg/audit"
	"github.com/ConfigButler/audit-pass-through-apiserver/pkg/identity"
	"github.com/ConfigButler/audit-pass-through-apiserver/pkg/webhook"
	authnv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	auditv1 "k8s.io/apiserver/pkg/apis/audit/v1"
	requestinfo "k8s.io/apiserver/pkg/endpoints/request"
)

const asyncSendTimeout = 5 * time.Second

// HandlerConfig configures the proxy handler.
type HandlerConfig struct {
	BackendURL        *url.URL
	WebhookClient     webhook.Sender
	IdentityExtractor *identity.Extractor
	Logger            *slog.Logger
	Transport         http.RoundTripper
	MaxAuditBodyBytes int64
	TempDir           string
}

// Handler proxies requests to the real aggregated backend and emits
// ResponseComplete audit events for supported mutating requests.
type Handler struct {
	backendURL  *url.URL
	webhook     webhook.Sender
	identity    *identity.Extractor
	logger      *slog.Logger
	transport   http.RoundTripper
	builder     *auditevents.Builder
	resolver    requestinfo.RequestInfoResolver
	passthrough *httputil.ReverseProxy
	tempDir     string
	captureMax  int64
}

// NewHandler creates a new proxy handler.
func NewHandler(cfg HandlerConfig) (*Handler, error) {
	if cfg.BackendURL == nil {
		return nil, fmt.Errorf("backend URL is required")
	}
	if cfg.WebhookClient == nil {
		return nil, fmt.Errorf("webhook client is required")
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	transport := cfg.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	identityExtractor := cfg.IdentityExtractor
	if identityExtractor == nil {
		var err error
		identityExtractor, err = identity.NewExtractor("")
		if err != nil {
			return nil, fmt.Errorf("build identity extractor: %w", err)
		}
	}

	reverseProxy := httputil.NewSingleHostReverseProxy(cfg.BackendURL)
	reverseProxy.Transport = transport
	reverseProxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		logger.Error("passthrough proxy request failed", "error", err, "path", r.URL.Path)
		http.Error(w, http.StatusText(http.StatusBadGateway), http.StatusBadGateway)
	}

	return &Handler{
		backendURL: cfg.BackendURL,
		webhook:    cfg.WebhookClient,
		identity:   identityExtractor,
		logger:     logger,
		transport:  transport,
		builder:    auditevents.NewBuilder(cfg.MaxAuditBodyBytes),
		resolver: &requestinfo.RequestInfoFactory{
			APIPrefixes:          sets.NewString("api", "apis"),
			GrouplessAPIPrefixes: sets.NewString("api"),
		},
		passthrough: reverseProxy,
		tempDir:     cfg.TempDir,
		captureMax:  cfg.MaxAuditBodyBytes,
	}, nil
}

// ServeHTTP proxies the request and, for supported mutating resource verbs,
// emits one best-effort ResponseComplete audit event after the proxied response
// has been captured.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	userInfo, trustedIdentity, err := h.identity.FromRequest(r)
	if err != nil {
		h.logger.Warn("rejecting request with untrusted delegated identity", "error", err, "path", r.URL.Path)
		http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
		return
	}
	if h.identity.RequiresVerifiedHeaders() && !trustedIdentity {
		h.logger.Warn("rejecting request without verified delegated identity", "path", r.URL.Path)
		http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
		return
	}

	info, err := h.resolver.NewRequestInfo(r)
	if err != nil {
		h.logger.Error("unable to resolve request info; using passthrough path", "error", err, "path", r.URL.Path)
		h.passthrough.ServeHTTP(w, r)
		return
	}
	if !shouldAudit(info) {
		h.passthrough.ServeHTTP(w, r)
		return
	}

	h.serveAudited(w, r, info, userInfo)
}

func (h *Handler) serveAudited(w http.ResponseWriter, r *http.Request, info *requestinfo.RequestInfo, userInfo authnv1.UserInfo) {
	requestReceivedAt := time.Now().UTC()

	requestBody, err := spoolBody(r.Body, h.tempDir, h.captureMax)
	if err != nil {
		h.logger.Error("unable to read request body", "error", err, "path", r.URL.Path)
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	defer func() {
		if err := requestBody.Cleanup(); err != nil {
			h.logger.Error("unable to remove request temp file", "error", err, "path", r.URL.Path)
		}
	}()

	upstreamBody, err := requestBody.Open()
	if err != nil {
		h.logger.Error("unable to reopen request body", "error", err, "path", r.URL.Path)
		http.Error(w, http.StatusText(http.StatusBadGateway), http.StatusBadGateway)
		return
	}

	upstreamRequest, err := h.buildUpstreamRequest(r, upstreamBody, requestBody.size)
	if err != nil {
		h.logger.Error("unable to build upstream request", "error", err, "path", r.URL.Path)
		_ = upstreamBody.Close()
		http.Error(w, http.StatusText(http.StatusBadGateway), http.StatusBadGateway)
		return
	}

	response, err := h.transport.RoundTrip(upstreamRequest)
	if err != nil {
		h.logger.Error("upstream request failed", "error", err, "path", r.URL.Path)
		http.Error(w, http.StatusText(http.StatusBadGateway), http.StatusBadGateway)
		return
	}
	defer func() {
		_ = response.Body.Close()
	}()

	responseBody, err := spoolBody(response.Body, h.tempDir, h.captureMax)
	if err != nil {
		h.logger.Error("unable to read upstream response body", "error", err, "path", r.URL.Path)
		http.Error(w, http.StatusText(http.StatusBadGateway), http.StatusBadGateway)
		return
	}
	defer func() {
		if err := responseBody.Cleanup(); err != nil {
			h.logger.Error("unable to remove response temp file", "error", err, "path", r.URL.Path)
		}
	}()

	responseReader, err := responseBody.Open()
	if err != nil {
		h.logger.Error("unable to reopen response body", "error", err, "path", r.URL.Path)
		http.Error(w, http.StatusText(http.StatusBadGateway), http.StatusBadGateway)
		return
	}
	defer func() {
		_ = responseReader.Close()
	}()

	copyHeaders(w.Header(), stripHopByHopHeaders(response.Header.Clone()))
	w.WriteHeader(response.StatusCode)
	if _, err := io.Copy(w, responseReader); err != nil {
		h.logger.Error("unable to write proxied response", "error", err, "path", r.URL.Path)
	}

	event, err := h.builder.Build(auditevents.Input{
		Request:               r,
		RequestInfo:           info,
		User:                  userInfo,
		RequestBody:           requestBody.captured,
		RequestBodyBytes:      requestBody.size,
		RequestBodyTruncated:  requestBody.truncated,
		ResponseBody:          responseBody.captured,
		ResponseBodyBytes:     responseBody.size,
		ResponseBodyTruncated: responseBody.truncated,
		ResponseStatusCode:    response.StatusCode,
		RequestReceivedAt:     requestReceivedAt,
		ResponseCompletedAt:   time.Now().UTC(),
	})
	if err != nil {
		h.logger.Error("unable to build audit event", "error", err, "path", r.URL.Path)
		return
	}

	go h.sendBestEffort(*event, r.URL.Path)
}

func (h *Handler) sendBestEffort(event auditv1.Event, path string) {
	ctx, cancel := context.WithTimeout(context.Background(), asyncSendTimeout)
	defer cancel()

	if err := h.webhook.Send(ctx, auditevents.Wrap(event)); err != nil {
		h.logger.Error("best-effort webhook delivery failed", "error", err, "path", path)
	}
}

func (h *Handler) buildUpstreamRequest(r *http.Request, body io.ReadCloser, contentLength int64) (*http.Request, error) {
	upstreamURL := h.backendURL.ResolveReference(&url.URL{
		Path:     r.URL.Path,
		RawPath:  r.URL.RawPath,
		RawQuery: r.URL.RawQuery,
	})

	upstreamRequest := r.Clone(r.Context())
	upstreamRequest.URL = upstreamURL
	upstreamRequest.RequestURI = ""
	upstreamRequest.Host = h.backendURL.Host
	upstreamRequest.Body = body
	upstreamRequest.ContentLength = contentLength
	upstreamRequest.Header = stripHopByHopHeaders(upstreamRequest.Header.Clone())
	appendForwardedFor(upstreamRequest.Header, r.RemoteAddr)

	return upstreamRequest, nil
}

func shouldAudit(info *requestinfo.RequestInfo) bool {
	if info == nil || !info.IsResourceRequest {
		return false
	}

	switch info.Verb {
	case "create", "update", "patch", "delete":
		return true
	default:
		return false
	}
}

func copyHeaders(dst, src http.Header) {
	for key, values := range src {
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

var hopByHopHeaders = []string{
	"Connection",
	"Proxy-Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Te",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

func stripHopByHopHeaders(header http.Header) http.Header {
	connectionValues := append([]string(nil), header.Values("Connection")...)
	for _, key := range hopByHopHeaders {
		header.Del(key)
	}

	for _, connectionValue := range connectionValues {
		for _, token := range strings.Split(connectionValue, ",") {
			header.Del(strings.TrimSpace(token))
		}
	}

	return header
}

func appendForwardedFor(header http.Header, remoteAddr string) {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	host = strings.TrimSpace(host)
	if host == "" {
		return
	}

	existing := header.Get("X-Forwarded-For")
	if existing == "" {
		header.Set("X-Forwarded-For", host)
		return
	}

	header.Set("X-Forwarded-For", existing+", "+host)
}
