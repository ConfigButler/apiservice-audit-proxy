# Watches, Streams, and Metrics Through `apiservice-audit-proxy`

This document is the design contract for how the proxy handles long-lived
Kubernetes watches and how it exposes operational metrics. It pairs that
contract with the tests that pin it, and a short status of what is shipped
versus what is still to land.

The short version: the proxy treats Kubernetes watches as long-lived streams,
not normal request/response RPCs. It does not set an inbound write deadline,
it flushes passthrough responses immediately, it keeps watches out of the
audited body-spooling path, and it exposes stream, byte, latency, audit, and
connection metrics on a dedicated metrics listener (plain HTTP by default,
optional HTTPS).

§4 and §5 describe the target shape of the metrics listener and its chart
surface. §8 calls out gaps where the implementation has not yet caught up;
§9 is the plan to close them.

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
| Immediate passthrough flush | `ReverseProxy.FlushInterval = -1` in [pkg/proxy/handler.go](../pkg/proxy/handler.go#L156). |
| Watch bypasses audit spool | `shouldAudit` excludes `get`, `list`, `watch`, `connect`, and `proxy` in [pkg/proxy/handler.go](../pkg/proxy/handler.go#L465). |
| Streaming telemetry wraps passthrough body | stream start/end and backend byte reads are observed in [pkg/proxy/handler.go](../pkg/proxy/handler.go#L399). |

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

Metrics are served from a dedicated listener, separate from the APIService
TLS listener. The endpoint is plain HTTP by default and can be served over
HTTPS when a key pair is supplied. OpenTelemetry instruments are registered
once during process start; there is no package `init()` that lazily binds
them to a no-op meter.

The listener is intentionally minimal: a single goroutine that runs
`http.ListenAndServe` (or `ListenAndServeTLS`) with a small `*http.ServeMux`
serving `/metrics`. It has no `ConnState` tracker (Prometheus scrape sockets
must not inflate `connections_active`), no extra timeouts beyond Go's
defaults, and no graceful-shutdown plumbing: Prometheus retries scrapes that
race a process exit. This mirrors the simplicity of gitops-reverser, where
controller-runtime owns the metrics server with no per-process ceremony.

Runtime flags:

```text
--metrics-listen-address=:8080
--metrics-tls-cert-file=""
--metrics-tls-private-key-file=""
```

Behavior:

- default address: `:8080`
- disable the listener: `--metrics-listen-address=0` or empty
- path: `/metrics`
- transport: plain HTTP when both TLS flags are empty; HTTPS when both are
  set. Setting only one of the TLS flags is a startup error, mirroring the
  APIService TLS flag pair.

Helm values:

```yaml
metrics:
  # Metrics listener. Set listenAddress to "0" to disable the listener,
  # the container port, and the Service port together.
  listenAddress: ":8080"
  containerPort: 8080
  service:
    port: 8080

  # Optional TLS for the metrics endpoint. Plain HTTP by default; on a
  # dedicated metrics port that is usually fine. Enable this when cluster
  # policy requires authenticated or encrypted scrapes.
  tls:
    enabled: false
    # Existing Secret in the release namespace containing tls.crt and tls.key.
    # Required when tls.enabled is true.
    secretName: ""
```

When `metrics.listenAddress` is not `0`, the chart renders a `metrics`
container port and a `metrics` Service port. Setting `metrics.tls.enabled=true`
mounts the named Secret read-only and passes `--metrics-tls-cert-file` and
`--metrics-tls-private-key-file` to the Deployment. The scraper resources stay
off by default; see §5.

## 5. Scraper Options

Scraper resources live alongside the metrics listener under `metrics.*` and are
disabled by default.

### Prometheus Operator

```yaml
metrics:
  serviceMonitor:
    enabled: false
    namespace: ""
    labels: {}
    interval: 30s
    scrapeTimeout: 10s
    path: /metrics
    port: metrics
    # When metrics.tls.enabled is true, set scheme: https and supply a
    # tlsConfig (for example {insecureSkipVerify: true} or a caFile/caSecret).
    scheme: http
    tlsConfig: {}
```

The template is [charts/apiservice-audit-proxy/templates/servicemonitor.yaml](../charts/apiservice-audit-proxy/templates/servicemonitor.yaml).

### VictoriaMetrics Operator

```yaml
metrics:
  vmServiceScrape:
    enabled: false
    namespace: ""
    labels: {}
    interval: 30s
    path: /metrics
    port: metrics
    scheme: http
    tlsConfig: {}
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
| `apiservice_audit_proxy_requests_total` | Counter | `verb`, `resource_group`, `api_version`, `resource`, `subresource`, `audited`, `streaming`, `status_class`, `outcome`, `inbound_proto`, `backend_proto` | Request volume and success/failure split. |
| `apiservice_audit_proxy_request_duration_seconds` | Histogram | `verb`, `resource_group`, `api_version`, `resource`, `subresource`, `audited`, `streaming`, `status_class`, `outcome`, `inbound_proto`, `backend_proto` | End-to-end request latency. For watches, this behaves like stream lifetime at request level. |
| `apiservice_audit_proxy_backend_roundtrip_seconds` | Histogram | `verb`, `streaming`, `outcome`, `backend_proto` | Time until backend response headers arrive. |
| `apiservice_audit_proxy_streams_active` | UpDownCounter | `kind`, `inbound_proto`, `backend_proto` | Current open watch/stream count. |
| `apiservice_audit_proxy_stream_duration_seconds` | Histogram | `kind`, `outcome`, `inbound_proto`, `backend_proto` | Stream lifetime and terminal outcome. |
| `apiservice_audit_proxy_transport_bytes_total` | Counter | `leg`, `streaming`, `direction` | Transported bytes on client/backend legs without decoding bodies. |
| `apiservice_audit_proxy_connections_active` | UpDownCounter | `state` | Inbound TCP connection counts on the APIService listener from `http.Server.ConnState`. The metrics listener is intentionally not tracked here, so Prometheus scrape sockets do not inflate the gauge. HTTP/2 can multiplex many streams on one socket. |
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

Operators can answer "what resources pass through this proxy, and how often?"
with a single query against `apiservice_audit_proxy_requests_total`:

```promql
topk(20, sum by (resource_group, api_version, resource, verb) (
  rate(apiservice_audit_proxy_requests_total[5m])
))
```

This mirrors gitops-reverser's use of group/version/resource/verb labels on
its webhook metrics. Cardinality stays bounded: no namespace, object name,
user, request path, audit ID, or raw error string is ever labelled. Empty
group/version/resource values normalize to `unknown` rather than expanding the
label space.

## 7. Metrics Tests

The unit coverage now exercises the instrumentation at the levels where it is
most deterministic:

| Test | What it proves |
|---|---|
| `pkg/telemetry` exporter tests | Instruments initialize and manual-reader helpers can assert counters, gauges/up-down counters, and histograms by attributes. |
| `TestHandler_RecordsRequestMetrics` in [pkg/proxy/handler_test.go](../pkg/proxy/handler_test.go#L301) | A proxied request records request, request-duration, and backend-roundtrip samples with bounded labels. |
| `TestHandler_RecordsStreamingMetrics` in [pkg/proxy/handler_test.go](../pkg/proxy/handler_test.go#L358) | A fake watch moves `streams_active` from `0` to `1` and back to `0`, records stream duration, and counts transported bytes on both legs. |
| `TestObservedReadCloser_ClassifiesBackendReadError` in [pkg/proxy/handler_test.go](../pkg/proxy/handler_test.go#L494) | Backend non-EOF read errors become `read_error` outcomes. |
| `TestConnStateTracker_RecordsConnectionStateGauge` in [cmd/server/main_test.go](../cmd/server/main_test.go#L417) | ConnState transitions on the APIService listener update `connections_active` by state. |

The metrics listener itself is an inline goroutine around `http.ListenAndServe`
/ `http.ListenAndServeTLS` and does not warrant its own factory test. Coverage
that `/metrics` actually serves the expected payload comes from the scraper-
backed e2e in §9.

Chart rendering is verified with `task helm:template` and focused Helm template
commands for the optional scraper resources. A Go-level Helm template test is
still optional; command-level verification is enough for the current chart
surface.

## 8. Current Gaps

These items describe deltas between the §4–§7 contract and the code today.

1. The watch e2e test proves the stream stays useful past the old 30s failure
   point but does not yet query Prometheus / VictoriaMetrics for scraped
   samples, and does not inspect `kubectl` stderr for the literal string
   `INTERNAL_ERROR`. HTTP/2 protocol labels are therefore verified only
   indirectly. §9 keeps this as a planned follow-up.

## 9. Implementation Plan From Here

Land in one PR; the chart change is breaking but pre-release, so no compat
shims or fail-guards are required.

1. **Slim the telemetry package** ([pkg/telemetry/exporter.go](../pkg/telemetry/exporter.go)).
   - Delete the package `init()`. Instruments stay nil until `InitPrometheusExporter`
     or `InitTestExporter` runs. Missing Init now panics loudly on first use.
   - Change `InitPrometheusExporter` to `func() error`. Drop the unused `ctx`
     and `registerer` parameters and the returned `Shutdown` function.

2. **Inline the metrics listener** ([cmd/server/main.go](../cmd/server/main.go)).
   - Delete `newMetricsServer` and the metrics branch of the shutdown
     goroutine.
   - Replace with a small inline goroutine: when `metricsListenAddress` is
     non-empty and not `"0"`, build a `*http.ServeMux` with `/metrics`, then
     `go func() { http.ListenAndServe(addr, mux) }()` (or `ListenAndServeTLS`
     when both `--metrics-tls-cert-file` and `--metrics-tls-private-key-file`
     are set). No `ConnState`, no extra timeouts, no `Shutdown` call.
   - Add the two TLS flags and a `validateMetricsTLSFlags` check that mirrors
     the existing `validateServingTLSFlags`: both flags or neither.
   - Drop the captured `metricsShutdown` variable and its shutdown-goroutine
     branch.

3. **Reshape the chart**.
   - Move `server.metrics.*` → top-level `metrics.*` in
     [values.yaml](../charts/apiservice-audit-proxy/values.yaml).
   - Move `monitoring.serviceMonitor` → `metrics.serviceMonitor` and
     `monitoring.vmServiceScrape` → `metrics.vmServiceScrape`. Remove the
     top-level `monitoring:` key.
   - Add `metrics.tls.{enabled, secretName}` and wire it through
     [deployment.yaml](../charts/apiservice-audit-proxy/templates/deployment.yaml):
     when `tls.enabled` is true, mount the named Secret read-only at a known
     path and pass `--metrics-tls-cert-file` / `--metrics-tls-private-key-file`.
   - Update [service.yaml](../charts/apiservice-audit-proxy/templates/service.yaml),
     [servicemonitor.yaml](../charts/apiservice-audit-proxy/templates/servicemonitor.yaml),
     and [vmservicescrape.yaml](../charts/apiservice-audit-proxy/templates/vmservicescrape.yaml)
     to read from the new paths.

4. **Adjust tests**.
   - Remove `TestNewMetricsServer_UsesPlainHTTPMetricsPort` along with the
     factory.
   - Keep `TestConnStateTracker_RecordsConnectionStateGauge` — the tracker
     still exists, just only on the APIService listener.
   - Add `TestParseFlags_MetricsTLSFlagsValidation` (both-or-neither) and a
     small `TestMetricsTLSStartupArgs` if you want to assert the deployment
     args. The metrics goroutine itself does not need its own unit test.

5. **Planned follow-ups (separate PRs)**:
   - Add a scraper-backed e2e: enable `metrics.serviceMonitor.enabled` in the
     e2e values, wait for the proxy target to be scraped, run
     `TestWatchStaysOpenThroughProxy`, then query Prometheus for
     `streams_active`, `stream_duration_seconds`, `transport_bytes_total`,
     request-duration, and protocol-label samples.
   - Tighten the watch e2e assertion to fail explicitly on `INTERNAL_ERROR`
     in `kubectl` stderr.
   - Optionally extend `metrics.tls.*` with chart-managed cert-manager wiring
     (mirroring `server.tls.mode: cert-manager`) once a real operator asks
     for it.

Keep timeout values for the APIService listener hard-coded. Making
stream-safe values configurable would reintroduce a footgun.

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
