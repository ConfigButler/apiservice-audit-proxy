# Watches, Streams, and Metrics Through `apiservice-audit-proxy`

This document records the current state of the watch streaming fix, the tests
that prove the important invariants, and the metrics surface used to operate
the proxy.

The short version: the proxy now treats Kubernetes watches as long-lived
streams, not normal request/response RPCs. It does not set an inbound write
deadline, it flushes passthrough responses immediately, it keeps watches out of
the audited body-spooling path, and it exposes stream, byte, latency, audit, and
connection metrics on a dedicated plain HTTP metrics server.

## 1. Why This Matters

The proxy fronts Kubernetes aggregated APIs through an `APIService`. That means
kube-aggregator sends every verb for that API group through us, including
`get`, `list`, and `watch`. Those verbs remain out of scope for synthetic audit
event emission, but they are not out of scope for proxying.

A Kubernetes watch is a long-lived HTTP response:

| Property | Value |
|---|---|
| Method | `GET` |
| URL shape | `/apis/<group>/<version>/<resource>?watch=true&allowWatchBookmarks=true&timeoutSeconds=...` |
| Transport | HTTP/2 over TLS in normal aggregated API paths; HTTP/1.1 chunked in tests/fallbacks |
| Body | newline-framed Kubernetes `WatchEvent` objects |
| End condition | client cancellation, backend timeout such as `timeoutSeconds`, or backend close |

The production symptom that motivated this work was a downstream
`gitops-reverser` reflector repeatedly reporting missing bookmarks and HTTP/2
`INTERNAL_ERROR` stream resets. That pattern is exactly what a Go server can
produce when a finite `http.Server.WriteTimeout` expires during a long-running
response.

## 2. Current Streaming Contract

The proxy must preserve these invariants:

1. The API listener has no finite `WriteTimeout`.
2. Slowloris protection uses `ReadHeaderTimeout`, not a whole-request
   `ReadTimeout`.
3. The API listener explicitly configures HTTP/2.
4. Passthrough responses flush immediately.
5. Watches and other read/long-running verbs bypass audit body spooling.
6. The proxy does not impose extra per-request deadlines on watches. The
   backend and the client-owned context decide when a watch ends.

The implementation now matches this contract:

| Invariant | Current implementation |
|---|---|
| No write deadline | `newHTTPServer` leaves `WriteTimeout` and `ReadTimeout` at zero in [cmd/server/main.go](../cmd/server/main.go#L624). |
| Header-only read protection | `ReadHeaderTimeout` is set to `15s` in [cmd/server/main.go](../cmd/server/main.go#L628). |
| HTTP/2 serving | `http2.ConfigureServer` is called explicitly in [cmd/server/main.go](../cmd/server/main.go#L633). |
| Immediate passthrough flush | `ReverseProxy.FlushInterval = -1` in [pkg/proxy/handler.go](../pkg/proxy/handler.go#L157). |
| Watch bypasses audit spool | `shouldAudit` excludes `get`, `list`, `watch`, `connect`, and `proxy` in [pkg/proxy/handler.go](../pkg/proxy/handler.go#L466). |
| Streaming telemetry wraps passthrough body | stream start/end and backend byte reads are observed in [pkg/proxy/handler.go](../pkg/proxy/handler.go#L400). |

## 3. Tests That Prove The Fix

The first useful tests were intentionally small and red against the old code.
They made the failure mode concrete before the implementation changed.

| Test | What it proves |
|---|---|
| `TestNewHTTPServer_UsesStreamingSafeTimeouts` in [cmd/server/main_test.go](../cmd/server/main_test.go#L391) | The API server cannot regress to finite `WriteTimeout` or whole-request `ReadTimeout`, and keeps `ReadHeaderTimeout`. |
| `TestNewHandler_PassthroughFlushesImmediately` in [pkg/proxy/handler_test.go](../pkg/proxy/handler_test.go#L155) | The passthrough `ReverseProxy` advertises immediate flush intent. |
| `TestHandler_WatchRequest_UsesPassthroughWithoutAudit` in [pkg/proxy/handler_test.go](../pkg/proxy/handler_test.go#L172) | A watch streams through a real `httptest.Server`, reaches the backend with query/header context, and does not emit audit events. |
| `TestShouldAudit_ExcludesReadAndLongRunningVerbs` in [pkg/proxy/handler_test.go](../pkg/proxy/handler_test.go#L270) | Future edits cannot accidentally route `watch` or other read/long-running verbs into audited spooling. |
| `TestWatchStaysOpenThroughProxy` in [test/e2e/watch_stream_test.go](../test/e2e/watch_stream_test.go) | A real aggregated API watch stays usable past the old 30s write-deadline failure point. |

The focused e2e lane is:

```bash
task e2e:test-watch-streams
```

This is the most important behavioral guard because it exercises the same
APIService path as client-go reflectors.

## 4. Metrics Server

Metrics are served from a dedicated plain HTTP server, separate from the TLS
APIService listener.

Runtime flag:

```text
--metrics-listen-address=:8080
```

Behavior:

- default: `:8080`
- disable: `--metrics-listen-address=0` or an empty value
- path: `/metrics`
- protocol: plain HTTP
- connection metrics are recorded for both the API server and the metrics
  server via `http.Server.ConnState`

The server is created in [cmd/server/main.go](../cmd/server/main.go#L642) and
the flag is wired in [cmd/server/main.go](../cmd/server/main.go#L314).

Helm values:

```yaml
server:
  metrics:
    listenAddress: ":8080"
    containerPort: 8080
    service:
      port: 8080
```

When `server.metrics.listenAddress` is not `0`, the chart renders a `metrics`
container port and a `metrics` Service port. The scraper resources are still
off by default.

## 5. Scraper Options

### Prometheus Operator

`ServiceMonitor` support is available and disabled by default:

```yaml
monitoring:
  serviceMonitor:
    enabled: false
    namespace: ""
    labels: {}
    interval: 30s
    scrapeTimeout: 10s
    path: /metrics
    port: metrics
    scheme: http
```

The template is [charts/apiservice-audit-proxy/templates/servicemonitor.yaml](../charts/apiservice-audit-proxy/templates/servicemonitor.yaml).

### VictoriaMetrics Operator

`VMServiceScrape` support is also available and disabled by default:

```yaml
monitoring:
  vmServiceScrape:
    enabled: false
    namespace: ""
    labels: {}
    interval: 30s
    path: /metrics
    port: metrics
    scheme: http
```

Rendered shape:

```yaml
apiVersion: operator.victoriametrics.com/v1beta1
kind: VMServiceScrape
metadata:
  name: apiservice-audit-proxy
  namespace: audit-pass-through-system
spec:
  selector:
    matchLabels:
      app.kubernetes.io/name: apiservice-audit-proxy
  endpoints:
    - port: metrics
      path: /metrics
      scheme: http
      interval: 30s
```

The template is [charts/apiservice-audit-proxy/templates/vmservicescrape.yaml](../charts/apiservice-audit-proxy/templates/vmservicescrape.yaml).

## 6. Metrics Surface

The telemetry package follows the GitOps Reverser pattern: package-level
OpenTelemetry instruments, a real Prometheus exporter, and a manual-reader test
exporter with helpers for unit assertions.

Keep labels bounded. Do not label by full path, namespace, object name, user,
audit ID, or raw error string.

| Metric | Type | Labels | Purpose |
|---|---|---|---|
| `apiservice_audit_proxy_requests_total` | Counter | `verb`, `resource_group`, `resource`, `subresource`, `audited`, `streaming`, `status_class`, `outcome`, `inbound_proto`, `backend_proto` | Request volume and success/failure split. |
| `apiservice_audit_proxy_request_duration_seconds` | Histogram | request labels | End-to-end request latency. For watches, this behaves like stream lifetime at request level. |
| `apiservice_audit_proxy_backend_roundtrip_seconds` | Histogram | `verb`, `streaming`, `outcome`, `backend_proto` | Time until backend response headers arrive. |
| `apiservice_audit_proxy_streams_active` | UpDownCounter | `kind`, `inbound_proto`, `backend_proto` | Current open watch/stream count. |
| `apiservice_audit_proxy_stream_duration_seconds` | Histogram | `kind`, `outcome`, `inbound_proto`, `backend_proto` | Stream lifetime and terminal outcome. |
| `apiservice_audit_proxy_transport_bytes_total` | Counter | `leg`, `streaming`, `direction` | Transported bytes on client/backend legs without decoding bodies. |
| `apiservice_audit_proxy_connections_active` | UpDownCounter | `state` | Inbound TCP connection counts from `http.Server.ConnState`. HTTP/2 can multiplex many streams on one socket. |
| `apiservice_audit_proxy_audit_events_total` | Counter | `outcome` | Synthetic audit build/send health. |
| `apiservice_audit_proxy_audit_delivery_duration_seconds` | Histogram | `outcome` | Webhook delivery latency. |

Notable label decisions from the review:

- `streams_active` does not carry a constant `outcome="active"` label. The
  active state is already encoded by the metric itself.
- `connections_active` does not carry a protocol label. `ConnState` does not
  know the negotiated HTTP protocol, so a `proto="unknown"` label would be
  decorative rather than useful.
- stream duration outcomes distinguish `backend_close`, `client_cancel`, and
  `read_error`. Non-EOF backend read errors are recorded as `read_error`, not
  silently folded into `client_cancel`.

## 7. Metrics Tests

The unit coverage now exercises the instrumentation at the levels where it is
most deterministic:

| Test | What it proves |
|---|---|
| `pkg/telemetry` exporter tests | Instruments initialize and manual-reader helpers can assert counters, gauges/up-down counters, and histograms by attributes. |
| `TestHandler_RecordsRequestMetrics` in [pkg/proxy/handler_test.go](../pkg/proxy/handler_test.go#L301) | A proxied request records request, request-duration, and backend-roundtrip samples with bounded labels. |
| `TestHandler_RecordsStreamingMetrics` in [pkg/proxy/handler_test.go](../pkg/proxy/handler_test.go#L358) | A fake watch moves `streams_active` from `0` to `1` and back to `0`, records stream duration, and counts transported bytes on both legs. |
| `TestObservedReadCloser_ClassifiesBackendReadError` in [pkg/proxy/handler_test.go](../pkg/proxy/handler_test.go#L494) | Backend non-EOF read errors become `read_error` outcomes. |
| `TestNewMetricsServer_UsesPlainHTTPMetricsPort` in [cmd/server/main_test.go](../cmd/server/main_test.go#L403) | The metrics listener is separate, plain HTTP, configurable, and disableable. |
| `TestConnStateTracker_RecordsConnectionStateGauge` in [cmd/server/main_test.go](../cmd/server/main_test.go#L417) | ConnState transitions update `connections_active` by state. |

Chart rendering is verified with `task helm:template` and focused Helm template
commands for the optional scraper resources. A Go-level Helm template test is
still optional; command-level verification is enough for the current chart
surface.

## 8. Current Gaps

These items are intentionally not claimed as complete yet:

1. The watch e2e test proves the stream stays useful past the old 30s failure
   point, but it does not yet query Prometheus/VictoriaMetrics for scraped
   samples.
2. HTTP/2 protocol labels are verified indirectly by unit/e2e behavior. A
   future e2e can query `inbound_proto="http2"` and `backend_proto="http2"`
   from the scraper after a live watch.
3. The e2e watch test fails on early EOF/decode errors while waiting for the
   post-45s event. It does not currently inspect `kubectl` stderr for the exact
   string `INTERNAL_ERROR`.
4. `ServiceMonitor` and `VMServiceScrape` use plain HTTP because metrics are on
   the dedicated metrics port. Hardening that endpoint with TLS is a future
   operational choice, not part of the simple first implementation.

## 9. Implementation Plan From Here

1. Keep the current red-test-first guards as permanent regression tests.
2. Add scraper-backed e2e assertions:
   - enable `monitoring.serviceMonitor.enabled` in the e2e chart values;
   - wait until the proxy target is scraped;
   - run `TestWatchStaysOpenThroughProxy`;
   - query for `streams_active`, `stream_duration_seconds`,
     `transport_bytes_total`, request latency, and protocol labels.
3. Add the VictoriaMetrics scraper path to a chart-render check if CI starts
   installing VictoriaMetrics CRDs.
4. Optionally tighten the e2e watch process assertion by capturing stderr and
   explicitly rejecting `INTERNAL_ERROR`, while still treating early EOF/decode
   errors as failures.
5. Keep timeout values hard-coded unless an operator use case appears. Making
   stream-safe values configurable would add a footgun that can reintroduce the
   original bug.

## 10. Verification Commands

For code and chart changes in this area, run:

```bash
task fmt
task test
task lint
task helm:lint
task helm:template
task e2e:test-watch-streams
```

Before merge, the broader proxy/audit lanes should also stay green:

```bash
task e2e:test-smoke
task e2e:test-audit-gap
task e2e:test-impersonation
task e2e:test-impersonation-no-rbac
```

Run `task e2e:test-smoke-backend-ca` when backend CA validation or Taskfile
wiring changes.

## 11. References

- GitOps Reverser telemetry pattern:
  - [external-resources/gitops-reverser/internal/telemetry/exporter.go](../external-resources/gitops-reverser/internal/telemetry/exporter.go)
  - [external-resources/gitops-reverser/internal/telemetry/metricread.go](../external-resources/gitops-reverser/internal/telemetry/metricread.go)
  - [external-resources/gitops-reverser/internal/webhook/audit_metrics_test.go](../external-resources/gitops-reverser/internal/webhook/audit_metrics_test.go)
  - [external-resources/gitops-reverser/charts/gitops-reverser/templates/servicemonitor.yaml](../external-resources/gitops-reverser/charts/gitops-reverser/templates/servicemonitor.yaml)
- Go behavior:
  - `net/http.Server.WriteTimeout`
  - `net/http.Server.ReadHeaderTimeout`
  - `net/http/httputil.ReverseProxy.FlushInterval`
  - `golang.org/x/net/http2.ConfigureServer`
