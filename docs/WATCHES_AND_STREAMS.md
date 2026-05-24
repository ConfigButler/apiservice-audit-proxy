# Watches and Streams Through `apiservice-audit-proxy`

> Scoping note: today the proxy declares `get`, `list`, and `watch` as **out of
> scope** in [ARCHITECTURE.md](ARCHITECTURE.md#scope). That phrasing was about
> *audit emission* — we do not synthesize audit events for reads. It was never
> meant to say "drop those requests". The proxy is registered through an
> `APIService`, so kube-aggregator routes *every* verb for the fronted
> `APIService` group to us, including `watch`. We must therefore proxy those
> requests faithfully — streaming end-to-end, with no buffering and no
> deadline-based termination.
>
> This document is the design write-up that motivates the streaming-correctness
> fixes. It does not change any code.

## 1. The Symptom

A downstream `gitops-reverser` running against a cluster where this proxy
fronts the `cozystack` aggregated APIs produces a steady stream of:

```
reflector.go:1261 "Warning: event bookmark expired" err="awaiting required
    bookmark event for initial events stream, no events received for 20.008s"

reflector.go:664  "Warning: watch ended with error"
    type="apps.cozystack.io/v1alpha1, Resource=natses"
    err="unable to decode an event from the watch stream:
         stream error: stream ID …; INTERNAL_ERROR; received from peer"
```

Two distinct signals, but the same root cause:

1. `bookmark expired` — the Kubernetes informer opened a watch-list stream
   and is waiting for the required initial-events `Bookmark`. Separately,
   apiserver storage can send periodic bookmarks when asked, but the generic
   Kubernetes API contract says clients must not rely on a fixed bookmark
   cadence. Either way, if a backend under our control writes watch bytes and
   the reflector sees none before its deadline, something between the backend
   and the reflector is **buffering** or **dropping** the stream.
2. `stream error … INTERNAL_ERROR; received from peer` — this is the wire
   form of an HTTP/2 `RST_STREAM` frame with code `INTERNAL_ERROR (0x02)`.
   It is sent by a peer that wants to abort a stream that it can no longer
   service — typically a server that has hit a write deadline, or a proxy
   whose ResponseWriter was closed under it.

Both are consistent with the proxy serving the watch with the *wrong* HTTP
plumbing: either it buffers, or it tears down the stream on a fixed
deadline, or both.

## 2. How a Kubernetes Watch Looks On The Wire

A Kubernetes `watch` is **not** server-sent events and **not** a websocket. It
is a long-lived HTTP response with:

| Property | Value |
|---|---|
| Method | `GET` |
| URL | `…/apis/<group>/<version>/<resource>?watch=true&allowWatchBookmarks=true&resourceVersion=…&timeoutSeconds=…` |
| Transport | HTTP/2 over TLS in modern clusters; HTTP/1.1 chunked as a fallback |
| `Content-Type` | `application/json`, often negotiated as `application/json;stream=watch` (or the protobuf equivalents) |
| `Content-Length` | absent — chunked / indeterminate length (`-1` to Go's transport) |
| Body | a sequence of newline-framed `WatchEvent` JSON objects, written as the apiserver receives them |

The body has **no fixed end**. The server writes events as resources change.
When `allowWatchBookmarks=true`, the server may also write `Bookmark` events;
when `sendInitialEvents=true`, Kubernetes uses a synthetic bookmark to mark
the end of the initial replay. Those bookmarks are not a strict heartbeat
contract, but they are an excellent diagnostic: if the backend wrote one and
the client never observed it, the stream was not proxied faithfully.

The `sample-apiserver` we vendor in [external-resources/sample-apiserver/](../external-resources/sample-apiserver/)
gets all of this for free from the `k8s.io/apiserver` framework — it does not
write any watch code itself. See `pkg/registry/wardle/flunder/etcd.go`: it
returns a standard `genericregistry.Store`, whose `Watch` is wired through the
shared apiserver runtime. The framework:

- enables HTTP/2 explicitly on the serving side via `http2.ConfigureServer`
  (cf. `k8s.io/apiserver/pkg/server/secure_serving.go` lines ~184–205)
- does **not** set `WriteTimeout` on the `http.Server` for long-running verbs;
  it relies on per-request deadlines that the apiserver framework manages
  itself (`timeoutSeconds`, longrunning request filter, etc.)
- uses an `io.Writer` chain that flushes on every `WatchEvent` write

The contract a proxy has to honor is therefore: **be a faithful HTTP/2
streaming pipe with no write deadline and no buffering.**

## 3. What Go's `httputil.ReverseProxy` Does Out Of The Box

The proxy uses [`net/http/httputil.ReverseProxy`](https://pkg.go.dev/net/http/httputil#ReverseProxy)
for the passthrough path (see [pkg/proxy/handler.go:128-149](../pkg/proxy/handler.go#L128-L149)).

Two pieces of `ReverseProxy` behavior are relevant:

### 3a. Streaming detection and flushing

`ReverseProxy.copyResponse` consults `ReverseProxy.flushInterval(res)` before
copying. The stdlib implementation (Go 1.16+, unchanged through 1.26):

```go
func (p *ReverseProxy) flushInterval(res *http.Response) time.Duration {
    resCT := res.Header.Get("Content-Type")
    if baseCT, _, _ := mime.ParseMediaType(resCT); baseCT == "text/event-stream" {
        return -1                  // immediate flush after every write
    }
    if res.ContentLength == -1 {
        return -1                  // immediate flush after every write
    }
    return p.FlushInterval
}
```

A Kubernetes watch response has `Content-Length: -1` (chunked / streaming),
so `ReverseProxy` **does** flush immediately on every write **even when we
have not set `FlushInterval` explicitly**. The flushing layer wraps the
`http.ResponseWriter` in a `maxLatencyWriter` whose `Write` calls
`Flush()` after every successful inner write.

So flushing is, by default, correct. Setting `FlushInterval = -1` explicitly
is a belt-and-braces good idea — it makes intent obvious and removes any
dependency on the heuristic — but it is not strictly *required* for watches
to flow.

### 3b. What `ReverseProxy` does NOT do

`ReverseProxy` does *not*:

- override `http.Server.WriteTimeout` on the inbound side;
- override `http.Server.ReadTimeout`;
- detach the response from the server's per-connection write deadline;
- coordinate HTTP/2 stream lifetimes between inbound and outbound;
- detect that the verb is `watch` and treat it specially.

Everything in this list is the embedder's job — meaning **our job, in
`cmd/server/main.go`**.

## 4. Why The Current Proxy Mishandles Watches

[cmd/server/main.go:34-37](../cmd/server/main.go#L34-L37) sets:

```go
defaultReadTimeout      = 15 * time.Second
defaultWriteTimeout     = 30 * time.Second
defaultIdleTimeout      = 60 * time.Second
```

and applies them at [cmd/server/main.go:170-176](../cmd/server/main.go#L170-L176):

```go
server := &http.Server{
    Addr:         cfg.listenAddress,
    Handler:      mux,
    ReadTimeout:  defaultReadTimeout,
    WriteTimeout: defaultWriteTimeout,
    IdleTimeout:  defaultIdleTimeout,
}
```

These are sensible defaults for short request/response RPC-style traffic.
They are catastrophic for watches:

### 4a. `WriteTimeout = 30s` is an absolute kill switch

`http.Server.WriteTimeout` is the "maximum duration before timing out writes
of the response" — it is wired through to `net.Conn.SetWriteDeadline` once,
when the request starts, and is **not extended** by writes during the
response. Every byte of the response shares the same deadline.

What this looks like for a watch:

```
T+0s    client opens GET …?watch=true  → proxy → backend
        backend starts streaming.
T+0s    proxy sets connection write deadline to T+30s.
T+10s   backend sends Bookmark, proxy flushes — deadline unchanged.
T+20s   backend sends Bookmark, proxy flushes — deadline unchanged.
T+30s   deadline fires. Next write fails with "i/o timeout".
        For HTTP/2 inbound, the runtime sends RST_STREAM(INTERNAL_ERROR).
        Client sees: "stream error … INTERNAL_ERROR; received from peer".
```

This matches the reflector logs to the second: the 20s no-events
warning fires inside the reflector's own watcher (10s expected bookmark
cadence × 2). The "INTERNAL_ERROR" warning is the inbound stream getting
RST'd. The reflector reconnects, gets a few more seconds of stream, and the
cycle repeats — which is exactly the log pattern in the symptom.

### 4b. `ReadTimeout = 15s` is mostly fine but should be `ReadHeaderTimeout`

For watch *requests* the body is empty, so `ReadTimeout` doesn't matter for
them. It does matter for mutating requests with large bodies, and for
slowloris-style attacks the right knob is `ReadHeaderTimeout`. We can keep
a tight `ReadHeaderTimeout` and lift `ReadTimeout` (or leave it 0).

### 4c. `IdleTimeout = 60s` is fine

`IdleTimeout` only applies between requests on a keep-alive connection. A
watch counts as an active request, so this is not what kills it.

### 4d. The audited path's body spool would also break watches — but it does not fire for them

[pkg/proxy/handler.go:194-199](../pkg/proxy/handler.go#L194-L199) takes the
passthrough path for any verb that is not `create`, `update`, `patch`, or
`delete`. `watch`, `list`, `get`, `connect`, etc. all skip the spool. That is
correct; the spool would block forever on a watch (it reads the response
body to EOF before writing anything to the client).

We should keep that invariant. If we ever extend audit emission to `connect`
or long-running verbs, the spool needs an explicit bypass for streaming
responses.

### 4e. HTTP/2 on the outbound transport: probably fine, worth confirming

[cmd/server/main.go:627-680](../cmd/server/main.go#L627-L680) builds the
backend transport from `http.DefaultTransport.Clone()`. The default sets
`ForceAttemptHTTP2: true`, and Go's transport calls
`http2.ConfigureTransports` automatically when ALPN negotiates `h2`. With
the cozystack backend speaking HTTP/2 (it is a `kube-apiserver`-shaped
aggregated server), the outbound leg is HTTP/2 by default.

Two things to watch:

- `transport.TLSClientConfig` is set in [cmd/server/main.go:657-679](../cmd/server/main.go#L657-L679).
  Since Go 1.13, setting `TLSClientConfig` *does not* disable HTTP/2 as long
  as `ForceAttemptHTTP2` is true (which it is, inherited from the clone of
  `DefaultTransport`). We are OK here.
- Whenever we want to be sure, we can log the response's `Proto` in
  `ModifyResponse` and confirm `HTTP/2.0`.

### 4f. HTTP/2 on the inbound serving side: relies on Go's auto-enable

[cmd/server/main.go:606-615](../cmd/server/main.go#L606-L615) configures only
`MinVersion` and `ClientAuth`. `server.ListenAndServeTLS` then enables HTTP/2
automatically because:

- `Server.TLSConfig.NextProtos` is unset, so Go appends `"h2"` and `"http/1.1"`,
- and `server.TLSNextProto` is nil at start, so Go installs the HTTP/2 handler.

In practice this means the proxy advertises h2 over ALPN, and clients
(`kube-apiserver` proxying for an APIService) speak HTTP/2 with us. If we
ever set `TLSNextProto` or `NextProtos` we must make sure we do not
inadvertently disable h2.

Recommended explicit configuration (so intent is visible, and so the
`http2.Server` knobs become tunable):

```go
import (
    "golang.org/x/net/http2"
    "golang.org/x/net/http2/h2c" // only if we ever want h2c, which we don't
)

http2Server := &http2.Server{
    IdleTimeout: server.IdleTimeout,
    // MaxConcurrentStreams: leave default, kube-apiserver uses 100
}
if err := http2.ConfigureServer(server, http2Server); err != nil {
    return fmt.Errorf("configure HTTP/2 server: %w", err)
}
```

This mirrors what `k8s.io/apiserver` does itself.

## 5. The Streaming Contract The Proxy Must Implement

To pass watches faithfully, the proxy must:

1. **Listen on HTTP/2** with explicit configuration (or rely on
   `ListenAndServeTLS` defaults but document the dependency).
2. **Not set a finite `WriteTimeout` on the `http.Server`** for the listener
   that handles proxy traffic. Use `ReadHeaderTimeout` to protect against
   slowloris instead.
3. **Speak HTTP/2 outbound** when the backend offers it (already the case).
4. **Pass `Content-Length: -1` responses straight through** with immediate
   flushing — `httputil.ReverseProxy` does this automatically, but we
   should set `FlushInterval = -1` for clarity.
5. **Not run watches through `serveAudited`** — they must take the
   passthrough path. They already do, by virtue of `shouldAudit` excluding
   `watch`. Keep that invariant.
6. **Not impose extra per-request deadlines** in the handler. The client
   passes `timeoutSeconds`; the apiserver framework on the backend honors it
   and closes the watch from its end at the right time.

## 6. What This Means For The Implementation

The minimum viable fix is two lines in [cmd/server/main.go](../cmd/server/main.go):

- Drop `WriteTimeout` from the `http.Server` (or set it to 0).
- Replace `ReadTimeout` with `ReadHeaderTimeout` (small, e.g. 15s).

The belt-and-braces fix additionally:

- Sets `httputil.ReverseProxy.FlushInterval = -1` in
  [pkg/proxy/handler.go:128](../pkg/proxy/handler.go#L128).
- Calls `http2.ConfigureServer` explicitly so HTTP/2 enablement is not
  implicit in the stdlib defaults.

Mutating-request safety is preserved: those requests take the audited path,
which has its own implicit per-request shape (limited by
`max-audit-body-bytes` and the upstream `RoundTrip` call's context). They
were never the requests that benefited from `WriteTimeout` anyway —
`WriteTimeout` is a connection-level guard, and the only thing it protected
was "client keeps a slow consumer open forever after the body is sent",
which `IdleTimeout` already covers.

## 7. Red Tests First

The tests should prove the failure mode before we touch the fix. There are two
kinds of coverage we want:

- **Fast red tests**: fail in unit/in-process suites and point directly at the
  bad proxy defaults.
- **Behavioral red tests**: reproduce the production symptom with an actual
  long-lived watch through the proxy.

### 7a. Fast red tests

1. `cmd/server`: `TestServerTimeoutDefaults_AreStreamingSafe`

   Add a test that asserts the listener used for APIService traffic has:

   - `WriteTimeout == 0`
   - `ReadTimeout == 0`
   - `ReadHeaderTimeout == 15s`
   - `IdleTimeout == 60s`

   This fails today because `defaultWriteTimeout` is `30s` and the server uses
   `ReadTimeout` instead of `ReadHeaderTimeout`. It is intentionally boring:
   it turns the core design contract into a small, unmistakable regression
   guard.

   If we do not want the test to reach into constants forever, first extract a
   tiny `newHTTPServer(addr string, handler http.Handler, tls *tls.Config)`
   helper that preserves current behavior, then write the test against the
   returned `*http.Server`. The first commit still goes red because the helper
   will expose the current finite write timeout.

2. `pkg/proxy`: `TestNewHandler_PassthroughFlushesImmediately`

   Assert `handler.passthrough.FlushInterval == -1`.

   This fails today because the field is left at `0`. It is not the root cause
   for Kubernetes watches — stdlib `ReverseProxy` already flushes when
   `ContentLength == -1` — but the explicit setting documents intent and
   protects us if a future response shape no longer trips the heuristic.

3. `pkg/proxy`: `TestHandler_WatchRequest_UsesPassthroughWithoutAudit`

   Build a fake backend that records the incoming request and then streams two
   newline-delimited watch events through an `http.Flusher`. Serve the proxy
   through a real `httptest.Server`; do not use `httptest.ResponseRecorder`,
   because it does not model streaming pressure.

   Assertions:

   - backend sees `watch=true`, `allowWatchBookmarks=true`, and the original
     query string;
   - the client can decode the first event before the backend writes the
     second event;
   - no webhook delivery occurs;
   - `X-Forwarded-For` and backend identity headers are still applied.

   This test will probably pass today. That is useful: it proves the handler
   split is already correct and narrows the bug to server-level plumbing.

4. `pkg/proxy`: `TestHandler_AuditedPath_DoesNotClaimWatch`

   Table-test `shouldAudit` for `get`, `list`, `watch`, `proxy`, `connect`,
   `create`, `update`, `patch`, and `delete`. The red value to guard against
   is any future expansion that accidentally routes `watch` into
   `serveAudited`, where `spoolBody(response.Body, ...)` would wait for EOF
   and turn the watch into a black hole.

### 7b. Behavioral red test

Add a new e2e test, `TestWatchStaysOpenThroughProxy`, in
`test/e2e/watch_stream_test.go`, and run it via `task e2e:test-watch-streams`.
The existing impersonation test creates the object after a few seconds; it
proves basic passthrough streaming and identity rewriting, but it does not stay
open long enough to hit the current 30s write deadline.

The test should:

1. Run after `e2e:prepare` so the Wardle sample-apiserver, APIService, proxy,
   and webhook receiver are installed.
2. Open a watch through the Kubernetes API, not directly to the backend:
   `GET /apis/wardle.example.com/v1alpha1/namespaces/default/flunders?watch=true&allowWatchBookmarks=true&timeoutSeconds=90`.
3. Decode the response incrementally with `watch.NewStreamWatcher` or a JSON
   decoder over `rest.RESTClient().Verb("GET").AbsPath(...).Param(...).Stream(ctx)`.
4. Keep the watch open past `30s`. At about `45s`, create or patch a Flunder
   through the same aggregated API.
5. Assert the existing watch receives the `ADDED` or `MODIFIED` event after
   the 45s write without reconnecting.
6. Assert the stream closes only because the test context cancels it or because
   `timeoutSeconds=90` expires, not because the proxy returned an early EOF,
   HTTP/2 `INTERNAL_ERROR`, or decode error.

This should fail against the current binary around the 30s mark. After the
fix, it becomes the most important regression test because it exercises the
same path used by client-go reflectors.

Optional stronger version: use `sendInitialEvents=true`,
`resourceVersionMatch=NotOlderThan`, and `allowWatchBookmarks=true`, then
assert the initial-events bookmark arrives. That variant maps more directly to
the observed reflector log line, but it depends on the cluster/backend feature
set. The 45s mutation test is less Kubernetes-version-sensitive and still
proves the proxy no longer kills long streams.

### 7c. HTTP/2 verification tests

Unit-level protocol assertions are awkward because `httptest.Server` hides
some of the APIService path, so prefer e2e plus metrics:

- Add a streaming request metric labelled with `inbound_proto` and
  `backend_proto`, with bounded values such as `http1`, `http2`, and
  `unknown`.
- In e2e, run one watch and query Prometheus for samples where
  `inbound_proto="http2"` and `backend_proto="http2"`.

This avoids adding debug headers to production responses and gives operators
the same signal we use in tests.

## 8. Metrics Plan

Use the GitOps Reverser pattern as the template:

- create an internal `pkg/telemetry` package;
- register package-level OpenTelemetry instruments in one place;
- expose an `InitPrometheusExporter` for the real binary;
- expose `InitTestExporter` plus small collection helpers for unit tests,
  mirroring `external-resources/gitops-reverser/internal/telemetry`;
- serve `/metrics` from the proxy process, then add Helm ServiceMonitor
  templates. The first implementation exposes it on the existing HTTPS API
  listener; a dedicated listener can be added later if we want stricter
  separation.

The proxy is not a controller-runtime manager, so we can either:

- use the OpenTelemetry Prometheus exporter directly with the default
  Prometheus registry, or
- add controller-runtime only for its metrics server/registry.

Prefer the direct exporter unless another controller-runtime dependency appears
for a stronger reason. The important part to copy from GitOps Reverser is not
controller-runtime itself; it is the testable instrument registration pattern.

### 8a. Proposed instruments

Keep labels bounded. Do not label by full path, namespace, name, user, or
audit ID.

| Metric | Type | Labels | Why |
|---|---|---|---|
| `apiservice_audit_proxy_requests_total` | Counter | `verb`, `resource_group`, `resource`, `subresource`, `audited`, `streaming`, `status_class`, `outcome`, `inbound_proto`, `backend_proto` | Request volume and success/failure split without high-cardinality paths. |
| `apiservice_audit_proxy_request_duration_seconds` | Histogram | same bounded request labels minus protocol if too noisy | End-to-end request latency; for watches this is stream lifetime. |
| `apiservice_audit_proxy_backend_roundtrip_seconds` | Histogram | `verb`, `streaming`, `outcome`, `backend_proto` | Time from proxy receiving the request to backend response headers. |
| `apiservice_audit_proxy_streams_active` | UpDownCounter | `kind`, `inbound_proto`, `backend_proto` | Current open watch/stream count. This is the operator-facing "how many open streams do we have?" metric. |
| `apiservice_audit_proxy_stream_duration_seconds` | Histogram | `kind`, `outcome`, `inbound_proto`, `backend_proto` | How long streams live and whether they end normally, by client cancel, by backend close, or by proxy error. |
| `apiservice_audit_proxy_transport_bytes_total` | Counter | `leg`, `streaming`, `direction` | Transported bytes without decoding bodies. `leg` is `client` or `backend`; `direction` is `read` or `write`. |
| `apiservice_audit_proxy_connections_active` | UpDownCounter | `state`, `proto` | Current inbound TCP connection counts from `http.Server.ConnState`. Useful but remember HTTP/2 multiplexes many watches on one socket. |
| `apiservice_audit_proxy_audit_events_total` | Counter | `outcome` | Synthetic audit delivery health: `built`, `sent`, `build_error`, `send_error`. |
| `apiservice_audit_proxy_audit_delivery_duration_seconds` | Histogram | `outcome` | Webhook delivery latency. |

Latency buckets should cover both RPCs and long streams:

- request/backend histograms: `0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120`
- stream duration histogram: `1, 5, 10, 30, 60, 120, 300, 600, 1800, 3600`

### 8b. Instrumentation points

1. Wrap `Handler.ServeHTTP` with request timing and label extraction from
   `requestinfo.RequestInfo`. For failed request-info parsing, use
   `resource_group="unknown"` and `resource="unknown"`.
2. In the passthrough `ReverseProxy`:
   - record backend header latency in `ModifyResponse`;
   - capture `resp.Proto` as the backend protocol;
   - classify streaming responses by `resp.ContentLength == -1`, watch query
     params, or both;
   - increment/decrement `streams_active` around the response body copy.
3. For byte counts, wrap:
   - inbound request bodies before audited spooling;
   - outbound request bodies before `RoundTrip`;
   - backend response bodies;
   - the `ResponseWriter` used to write back to the client.
4. For open sockets, set `http.Server.ConnState` and maintain deltas for
   `new`, `active`, `idle`, `hijacked`, and `closed`. Treat this as connection
   telemetry, not stream telemetry.
5. For audits, record around `Build` and `webhook.Send`.

### 8c. Metrics tests

Mirror GitOps Reverser's `InitTestExporter` tests:

1. `pkg/telemetry`: initialization test proves every instrument is non-nil and
   can be used.
2. `pkg/telemetry`: collection helper test proves counters, gauges/up-down
   counters, and histograms can be asserted by attributes.
3. `pkg/proxy`: request metrics test sends a normal GET, a mutating POST, and
   a backend failure, then asserts `requests_total` and duration histogram
   counts by bounded labels.
4. `pkg/proxy`: streaming metrics test opens a fake watch, waits until
   `streams_active == 1`, sends two events, cancels the client, then asserts
   active streams returns to `0`, bytes increased on both legs, and one stream
   duration sample exists.
5. `cmd/server`: connection metrics test drives `ConnState` transitions with a
   tiny TLS server and asserts active/idle connection samples move as expected.
6. Helm tests: `task helm:template` should render a metrics Service port and a
   ServiceMonitor only when `monitoring.serviceMonitor.enabled=true`.

## 9. Implementation Plan

1. Add the red tests:
   - `cmd/server` timeout defaults test;
   - `pkg/proxy` explicit passthrough flush interval test;
   - `pkg/proxy` watch passthrough/no-audit streaming guard;
   - e2e `TestWatchStaysOpenThroughProxy`.
2. Run the smallest relevant suite and confirm the intended failures:
   - `task test` should fail on the timeout and flush tests;
   - `task e2e:test-smoke` or a new focused e2e task should fail around 30s
     once the watch test is wired in.
3. Apply the streaming fix:
   - set the API server listener `WriteTimeout` to `0`;
   - replace `ReadTimeout` with `ReadHeaderTimeout`;
   - set `ReverseProxy.FlushInterval = -1`;
   - explicitly configure HTTP/2 serving with `http2.ConfigureServer`.
4. Re-run the red tests and confirm they turn green.
5. Add telemetry:
   - `pkg/telemetry` exporter and test reader helpers;
   - request, stream, byte, connection, and audit instruments;
   - `/metrics` serving;
   - Helm values, Service port, ServiceMonitor, and e2e Prometheus scrape
     resources.
6. Add metrics assertions:
   - unit tests with the manual reader;
   - e2e query against Prometheus after the watch test to confirm open stream,
     byte, latency, and protocol samples.
7. Run the required verification:
   - `task fmt`
   - `task test`
   - `task helm:lint`
   - `task helm:template`
   - streaming/audit-related e2e lanes:
     `task e2e:test-watch-streams`, `task e2e:test-smoke`,
     `task e2e:test-audit-gap`,
     `task e2e:test-impersonation`, and
     `task e2e:test-impersonation-no-rbac`.

## 10. Open Questions

1. Should metrics eventually move from the existing TLS API listener to a
   dedicated plain HTTP metrics listener? GitOps Reverser uses a dedicated
   metrics service; that is operationally cleaner and avoids mixing Prometheus
   scrapes with APIService traffic.
2. Do we want a production config flag for server timeouts, or should the
   streaming-safe values be hard-coded? For an APIService proxy, hard-coded
   streaming-safe defaults are simpler and less footgun-prone.
3. Should e2e assert bookmarks specifically, or only prove that a watch remains
   usable beyond 30s? Start with the version-insensitive long-watch mutation
   test; add the bookmark-specific test once the sample backend behavior is
   stable in the e2e cluster.

## 11. References

- Symptom log lines: see the top of this document.
- Go behavior cited from stdlib `net/http/httputil/reverseproxy.go`
  (`flushInterval`, `copyResponse`) and `net/http/server.go`
  (`WriteTimeout`, `ListenAndServeTLS` ALPN).
- Kubernetes serving side: `k8s.io/apiserver/pkg/server/secure_serving.go`
  (lines ~184–205 configure HTTP/2 explicitly).
- Metrics pattern:
  - [external-resources/gitops-reverser/internal/telemetry/exporter.go](../external-resources/gitops-reverser/internal/telemetry/exporter.go)
  - [external-resources/gitops-reverser/internal/telemetry/metricread.go](../external-resources/gitops-reverser/internal/telemetry/metricread.go)
  - [external-resources/gitops-reverser/internal/webhook/audit_metrics_test.go](../external-resources/gitops-reverser/internal/webhook/audit_metrics_test.go)
  - [external-resources/gitops-reverser/charts/gitops-reverser/templates/servicemonitor.yaml](../external-resources/gitops-reverser/charts/gitops-reverser/templates/servicemonitor.yaml)
- Proxy code touched by this design:
  - [cmd/server/main.go:34-37](../cmd/server/main.go#L34-L37) (timeouts)
  - [cmd/server/main.go:170-176](../cmd/server/main.go#L170-L176) (server build)
  - [cmd/server/main.go:606-615](../cmd/server/main.go#L606-L615) (TLS config)
  - [cmd/server/main.go:627-680](../cmd/server/main.go#L627-L680) (backend transport)
  - [pkg/proxy/handler.go:128-149](../pkg/proxy/handler.go#L128-L149) (ReverseProxy)
  - [pkg/proxy/handler.go:194-199](../pkg/proxy/handler.go#L194-L199) (audit vs passthrough split)
