package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	authnv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	auditv1 "k8s.io/apiserver/pkg/apis/audit/v1"
	requestinfo "k8s.io/apiserver/pkg/endpoints/request"

	auditevents "github.com/ConfigButler/apiservice-audit-proxy/pkg/audit"
	"github.com/ConfigButler/apiservice-audit-proxy/pkg/identity"
	"github.com/ConfigButler/apiservice-audit-proxy/pkg/telemetry"
	"github.com/ConfigButler/apiservice-audit-proxy/pkg/webhook"
)

const asyncSendTimeout = 5 * time.Second

const (
	transportLegBackend = "backend"
	transportLegClient  = "client"
	directionRead       = "read"
	directionWrite      = "write"
	watchVerb           = "watch"
	outcomeBackendClose = "backend_close"
	outcomeClientCancel = "client_cancel"
	outcomeError        = "error"
	outcomeOK           = "ok"
	labelUnknown        = "unknown"

	statusServerErrorMin = 500
)

// HandlerConfig configures the proxy handler.
type HandlerConfig struct {
	BackendURL        *url.URL
	WebhookClient     webhook.Sender
	IdentityExtractor *identity.Extractor
	Logger            *slog.Logger
	Transport         http.RoundTripper
	BackendIdentity   BackendIdentity
	MaxAuditBodyBytes int64
	TempDir           string
}

// Handler proxies requests to the real aggregated backend and emits
// ResponseComplete audit events for supported mutating requests.
type Handler struct {
	backendURL      *url.URL
	webhook         webhook.Sender
	identity        *identity.Extractor
	logger          *slog.Logger
	transport       http.RoundTripper
	backendIdentity BackendIdentity
	builder         *auditevents.Builder
	resolver        requestinfo.RequestInfoResolver
	passthrough     *httputil.ReverseProxy
	tempDir         string
	captureMax      int64

	// First-event milestones. Each fires exactly once per process lifetime so
	// an operator can confirm — without enabling per-request logging — that:
	//   inbound traffic is arriving, the requestheader trust is actually
	//   verifying identities, the backend is reachable, and the audit webhook
	//   is accepting deliveries. After the first occurrence these are silent.
	firstRequest         sync.Once
	firstVerifiedRequest sync.Once
	firstBackendOK       sync.Once
	firstWebhookOK       sync.Once
}

// userInfoContextKey is the typed context key carrying the verified delegated
// identity into the passthrough ReverseProxy Rewrite hook.
type userInfoContextKey struct{}
type requestMetricContextKey struct{}

func contextWithUserInfo(parent context.Context, user authnv1.UserInfo) context.Context {
	return context.WithValue(parent, userInfoContextKey{}, user)
}

func userInfoFromContext(ctx context.Context) (authnv1.UserInfo, bool) {
	user, ok := ctx.Value(userInfoContextKey{}).(authnv1.UserInfo)
	return user, ok
}

func contextWithRequestMetrics(parent context.Context, state *requestMetricState) context.Context {
	return context.WithValue(parent, requestMetricContextKey{}, state)
}

func requestMetricsFromContext(ctx context.Context) (*requestMetricState, bool) {
	state, ok := ctx.Value(requestMetricContextKey{}).(*requestMetricState)
	return state, ok
}

// NewHandler creates a new proxy handler.
func NewHandler(cfg HandlerConfig) (*Handler, error) {
	if cfg.BackendURL == nil {
		return nil, errors.New("backend URL is required")
	}
	if cfg.WebhookClient == nil {
		return nil, errors.New("webhook client is required")
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
		identityExtractor, err = identity.NewExtractor("", nil)
		if err != nil {
			return nil, fmt.Errorf("build identity extractor: %w", err)
		}
	}

	backendIdentity := cfg.BackendIdentity
	if backendIdentity == nil {
		backendIdentity = RequestHeaderForwarder{}
	}

	handler := &Handler{
		backendURL:      cfg.BackendURL,
		webhook:         cfg.WebhookClient,
		identity:        identityExtractor,
		logger:          logger,
		transport:       transport,
		backendIdentity: backendIdentity,
		builder:         auditevents.NewBuilder(cfg.MaxAuditBodyBytes),
		resolver: &requestinfo.RequestInfoFactory{
			APIPrefixes:          sets.NewString("api", "apis"),
			GrouplessAPIPrefixes: sets.NewString("api"),
		},
		tempDir:    cfg.TempDir,
		captureMax: cfg.MaxAuditBodyBytes,
	}

	handler.passthrough = &httputil.ReverseProxy{
		Transport:     transport,
		FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(cfg.BackendURL)
			// SetURL does not touch X-Forwarded-*. Set X-Forwarded-For
			// explicitly via the shared helper so the passthrough path matches
			// the audited path exactly; deliberately not SetXForwarded, which
			// would also inject X-Forwarded-Host and X-Forwarded-Proto.
			appendForwardedFor(pr.Out.Header, pr.In.RemoteAddr)
			if user, ok := userInfoFromContext(pr.In.Context()); ok {
				handler.backendIdentity.Apply(pr.Out, user)
			}
		},
		ModifyResponse: func(resp *http.Response) error {
			handler.notifyFirstBackendOK(resp)
			handler.observePassthroughResponse(resp)
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if state, ok := requestMetricsFromContext(r.Context()); ok {
				state.recordBackendRoundTrip(r.Context(), outcomeError)
			}
			logger.Error("passthrough proxy request failed", "error", err, "path", r.URL.Path)
			http.Error(w, http.StatusText(http.StatusBadGateway), http.StatusBadGateway)
		},
	}

	return handler, nil
}

// ServeHTTP proxies the request and, for supported mutating resource verbs,
// emits one best-effort ResponseComplete audit event after the proxied response
// has been captured.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	metricState := newRequestMetricState(r)
	r = r.WithContext(contextWithRequestMetrics(r.Context(), metricState))
	r.Body = observeReadCloser(r.Body, func(n int64) {
		telemetry.AddTransportBytes(r.Context(), telemetry.TransportByteLabels{
			Leg:       transportLegClient,
			Streaming: metricState.Streaming(),
			Direction: directionRead,
		}, n)
	})
	metricWriter := newMetricResponseWriter(r.Context(), w, metricState)
	defer func() {
		telemetry.RecordRequest(
			r.Context(),
			metricState.RequestLabels(metricWriter.StatusCode()),
			time.Since(metricState.start),
		)
	}()
	w = metricWriter

	h.firstRequest.Do(func() {
		h.logger.Info("first inbound request received — proxy is reachable from the cluster",
			"method", r.Method,
			"path", r.URL.Path,
			"remote_addr", r.RemoteAddr,
		)
	})

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

	h.firstVerifiedRequest.Do(func() {
		h.logger.Info("first request with verified delegated identity — requestheader trust is wired correctly",
			"user", userInfo.Username,
			"groups", userInfo.Groups,
			"path", r.URL.Path,
		)
	})

	r = r.WithContext(contextWithUserInfo(r.Context(), userInfo))

	info, err := h.resolver.NewRequestInfo(r)
	if err != nil {
		h.logger.Error("unable to resolve request info; using passthrough path", "error", err, "path", r.URL.Path)
		h.passthrough.ServeHTTP(w, r)
		return
	}
	audited := shouldAudit(info)
	metricState.SetRequestInfo(info, audited)
	if !audited {
		h.passthrough.ServeHTTP(w, r)
		return
	}

	h.serveAudited(w, r, info, userInfo)
}

func (h *Handler) serveAudited(
	w http.ResponseWriter,
	r *http.Request,
	info *requestinfo.RequestInfo,
	userInfo authnv1.UserInfo,
) {
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

	upstreamFile, err := requestBody.Open()
	if err != nil {
		h.logger.Error("unable to reopen request body", "error", err, "path", r.URL.Path)
		http.Error(w, http.StatusText(http.StatusBadGateway), http.StatusBadGateway)
		return
	}
	var upstreamBody io.ReadCloser = upstreamFile
	upstreamBody = observeTransportBytes(r.Context(), upstreamBody, transportLegBackend, false, directionWrite)

	upstreamRequest := h.buildUpstreamRequest(r, upstreamBody, requestBody.size, userInfo)

	response, err := h.transport.RoundTrip(upstreamRequest)
	if err != nil {
		if state, ok := requestMetricsFromContext(r.Context()); ok {
			state.recordBackendRoundTrip(r.Context(), outcomeError)
		}
		h.logger.Error("upstream request failed", "error", err, "path", r.URL.Path)
		http.Error(w, http.StatusText(http.StatusBadGateway), http.StatusBadGateway)
		return
	}
	h.notifyFirstBackendOK(response)
	observeAuditedBackendResponse(r.Context(), response)
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

	//nolint:gosec // Audit delivery is best-effort work deliberately detached from the request lifetime.
	go h.buildAndSendAuditEvent(r, info, userInfo, requestBody, responseBody, response.StatusCode, requestReceivedAt)
}

func (h *Handler) buildAndSendAuditEvent(
	r *http.Request,
	info *requestinfo.RequestInfo,
	userInfo authnv1.UserInfo,
	requestBody, responseBody *spooledBody,
	statusCode int,
	requestReceivedAt time.Time,
) {
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
		ResponseStatusCode:    statusCode,
		RequestReceivedAt:     requestReceivedAt,
		ResponseCompletedAt:   time.Now().UTC(),
	})
	if err != nil {
		telemetry.AddAuditEvent(context.Background(), "build_error")
		h.logger.Error("unable to build audit event", "error", err, "path", r.URL.Path)
		return
	}

	telemetry.AddAuditEvent(context.Background(), "built")
	h.sendBestEffort(*event, r.URL.Path)
}

func (h *Handler) sendBestEffort(event auditv1.Event, path string) {
	ctx, cancel := context.WithTimeout(context.Background(), asyncSendTimeout)
	defer cancel()

	start := time.Now()
	if err := h.webhook.Send(ctx, auditevents.Wrap(event)); err != nil {
		telemetry.AddAuditEvent(context.Background(), "send_error")
		telemetry.RecordAuditDelivery(context.Background(), "send_error", time.Since(start))
		h.logger.Error("best-effort webhook delivery failed", "error", err, "path", path)
		return
	}
	telemetry.AddAuditEvent(context.Background(), "sent")
	telemetry.RecordAuditDelivery(context.Background(), "sent", time.Since(start))
	h.firstWebhookOK.Do(func() {
		h.logger.Info("first audit event delivered to webhook receiver — audit pipeline is healthy",
			"path", path,
		)
	})
}

// notifyFirstBackendOK fires the first-backend-OK milestone exactly once. It
// is shared between the audited path (direct RoundTrip) and the passthrough
// path (httputil.ReverseProxy.ModifyResponse) so either is sufficient evidence
// that the proxy actually reached the aggregated backend.
func (h *Handler) notifyFirstBackendOK(resp *http.Response) {
	if resp == nil {
		return
	}
	h.firstBackendOK.Do(func() {
		h.logger.Info("first successful backend response — connection to aggregated backend is established",
			"status", resp.StatusCode,
		)
	})
}

func (h *Handler) observePassthroughResponse(resp *http.Response) {
	if resp == nil || resp.Request == nil {
		return
	}
	state, ok := requestMetricsFromContext(resp.Request.Context())
	if !ok {
		return
	}

	streaming := isStreamingResponse(resp) || state.Streaming()
	state.SetBackend(resp.Proto, streaming)
	state.recordBackendRoundTrip(resp.Request.Context(), outcomeOK)

	resp.Body = observeReadCloser(resp.Body, func(n int64) {
		telemetry.AddTransportBytes(resp.Request.Context(), telemetry.TransportByteLabels{
			Leg:       transportLegBackend,
			Streaming: state.Streaming(),
			Direction: directionRead,
		}, n)
	})

	if !streaming {
		return
	}

	done := telemetry.StreamStarted(resp.Request.Context(), telemetry.StreamLabels{
		Kind:         state.StreamKind(),
		InboundProto: state.inboundProto,
		BackendProto: resp.Proto,
	})
	resp.Body = observeClose(resp.Body, done)
}

func observeAuditedBackendResponse(ctx context.Context, response *http.Response) {
	if state, ok := requestMetricsFromContext(ctx); ok {
		state.SetBackend(response.Proto, false)
		state.recordBackendRoundTrip(ctx, outcomeOK)
	}
	response.Body = observeTransportBytes(ctx, response.Body, transportLegBackend, false, directionRead)
}

func (h *Handler) buildUpstreamRequest(
	r *http.Request,
	body io.ReadCloser,
	contentLength int64,
	userInfo authnv1.UserInfo,
) *http.Request {
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
	h.backendIdentity.Apply(upstreamRequest, userInfo)

	return upstreamRequest
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

type requestMetricState struct {
	mu              sync.Mutex
	start           time.Time
	verb            string
	resourceGroup   string
	resource        string
	subresource     string
	audited         bool
	streaming       bool
	inboundProto    string
	backendProto    string
	backendObserved bool
}

func newRequestMetricState(r *http.Request) *requestMetricState {
	state := &requestMetricState{
		start:        time.Now(),
		verb:         strings.ToLower(r.Method),
		streaming:    isWatchRequest(r),
		inboundProto: r.Proto,
		backendProto: labelUnknown,
	}
	if state.streaming {
		state.verb = watchVerb
	}
	return state
}

func (s *requestMetricState) SetRequestInfo(info *requestinfo.RequestInfo, audited bool) {
	if info == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	s.verb = info.Verb
	s.resourceGroup = info.APIGroup
	s.resource = info.Resource
	s.subresource = info.Subresource
	s.audited = audited
	if info.Verb == watchVerb {
		s.streaming = true
	}
}

func (s *requestMetricState) SetBackend(proto string, streaming bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.backendProto = proto
	if streaming {
		s.streaming = true
	}
}

func (s *requestMetricState) Streaming() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.streaming
}

func (s *requestMetricState) StreamKind() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.verb == watchVerb || s.streaming {
		return watchVerb
	}
	return "response"
}

func (s *requestMetricState) RequestLabels(statusCode int) telemetry.RequestLabels {
	s.mu.Lock()
	defer s.mu.Unlock()

	return telemetry.RequestLabels{
		Verb:          s.verb,
		ResourceGroup: s.resourceGroup,
		Resource:      s.resource,
		Subresource:   s.subresource,
		Audited:       s.audited,
		Streaming:     s.streaming,
		StatusClass:   telemetry.StatusClass(statusCode),
		Outcome:       requestOutcome(statusCode),
		InboundProto:  s.inboundProto,
		BackendProto:  s.backendProto,
	}
}

func (s *requestMetricState) recordBackendRoundTrip(ctx context.Context, outcome string) {
	s.mu.Lock()
	if s.backendObserved {
		s.mu.Unlock()
		return
	}
	s.backendObserved = true
	labels := telemetry.BackendLabels{
		Verb:         s.verb,
		Streaming:    s.streaming,
		Outcome:      outcome,
		BackendProto: s.backendProto,
	}
	duration := time.Since(s.start)
	s.mu.Unlock()

	telemetry.RecordBackendRoundTrip(ctx, labels, duration)
}

type metricResponseWriter struct {
	http.ResponseWriter

	ctx        context.Context
	state      *requestMetricState
	statusCode int
}

func newMetricResponseWriter(
	ctx context.Context,
	w http.ResponseWriter,
	state *requestMetricState,
) *metricResponseWriter {
	return &metricResponseWriter{
		ResponseWriter: w,
		ctx:            ctx,
		state:          state,
	}
}

func (w *metricResponseWriter) WriteHeader(statusCode int) {
	if w.statusCode == 0 {
		w.statusCode = statusCode
	}
	w.ResponseWriter.WriteHeader(statusCode)
}

func (w *metricResponseWriter) Write(p []byte) (int, error) {
	if w.statusCode == 0 {
		w.statusCode = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(p)
	telemetry.AddTransportBytes(w.ctx, telemetry.TransportByteLabels{
		Leg:       transportLegClient,
		Streaming: w.state.Streaming(),
		Direction: directionWrite,
	}, int64(n))
	return n, err
}

func (w *metricResponseWriter) Flush() {
	flusher, ok := w.ResponseWriter.(http.Flusher)
	if ok {
		flusher.Flush()
	}
}

func (w *metricResponseWriter) StatusCode() int {
	return w.statusCode
}

type observedReadCloser struct {
	io.ReadCloser

	observe func(int64)
	onClose func(string)
}

func observeReadCloser(body io.ReadCloser, observe func(int64)) io.ReadCloser {
	if body == nil {
		return nil
	}
	return &observedReadCloser{ReadCloser: body, observe: observe}
}

func observeClose(body io.ReadCloser, onClose func(string)) io.ReadCloser {
	if body == nil {
		return nil
	}
	return &observedReadCloser{ReadCloser: body, onClose: onClose}
}

func observeTransportBytes(
	ctx context.Context,
	body io.ReadCloser,
	leg string,
	streaming bool,
	direction string,
) io.ReadCloser {
	return observeReadCloser(body, func(n int64) {
		telemetry.AddTransportBytes(ctx, telemetry.TransportByteLabels{
			Leg:       leg,
			Streaming: streaming,
			Direction: direction,
		}, n)
	})
}

func (r *observedReadCloser) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if n > 0 && r.observe != nil {
		r.observe(int64(n))
	}
	if err == io.EOF && r.onClose != nil {
		r.onClose(outcomeBackendClose)
		r.onClose = nil
	}
	return n, err
}

func (r *observedReadCloser) Close() error {
	if r.onClose != nil {
		r.onClose(outcomeClientCancel)
		r.onClose = nil
	}
	return r.ReadCloser.Close()
}

func requestOutcome(statusCode int) string {
	if statusCode == 0 {
		return labelUnknown
	}
	if statusCode >= statusServerErrorMin {
		return outcomeError
	}
	return outcomeOK
}

func isWatchRequest(r *http.Request) bool {
	return r != nil && strings.EqualFold(r.URL.Query().Get("watch"), "true")
}

func isStreamingResponse(resp *http.Response) bool {
	if resp == nil {
		return false
	}
	return resp.ContentLength == -1
}

func copyHeaders(dst, src http.Header) {
	for key, values := range src {
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func hopByHop() []string {
	return []string{
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
}

func stripHopByHopHeaders(header http.Header) http.Header {
	connectionValues := append([]string(nil), header.Values("Connection")...)
	for _, key := range hopByHop() {
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
