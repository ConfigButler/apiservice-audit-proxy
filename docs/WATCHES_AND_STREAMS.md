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

1. `bookmark expired` — the Kubernetes informer asked for periodic
   `Bookmark` events. The apiserver promises one roughly every 10s, but the
   reflector has not seen one for ~20s. Something between the apiserver and
   the reflector is **buffering** or **dropping** the stream.
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
| `Content-Type` | `application/json` (or `application/vnd.kubernetes.protobuf`) |
| `Content-Length` | absent — chunked / indeterminate length (`-1` to Go's transport) |
| Body | a sequence of newline-framed `WatchEvent` JSON objects, written as the apiserver receives them |

The body has **no fixed end**. The server writes events as resources change,
and writes `Bookmark` events at the cadence advertised by `timeoutSeconds`
(client-supplied; client-go uses a random value in `[minWatchTimeout, 2·min)`,
default min 5m). Bookmarks exist precisely so that long-idle watches keep
flowing bytes through every intermediary — if intermediate buffers eat them,
the reflector gives up.

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

## 7. Open Questions / Things To Verify In e2e

Before we ship the fix we should add e2e coverage. Two things to prove:

1. **Long watch stays open through the proxy**: open a `watch` against
   `flunders.wardle.example.com` (the sample-apiserver) via the proxy, hold
   it for 90s, write a resource mid-watch, and assert the client receives:
   - an `ADDED` or `MODIFIED` event for the write, and
   - at least one `BOOKMARK` event between connect and the write.
   This guards against a regression to the current 30s kill.

2. **HTTP/2 inbound and outbound**: log the protocol on both sides for one
   request and assert `HTTP/2.0`. This guards against an accidental
   `TLSNextProto` change or a transport clone that drops HTTP/2.

The smoke and audit-gap suites already cover the mutating side; the watch
e2e is new ground.

## 8. References

- Symptom log lines: see the top of this document.
- Go behavior cited from stdlib `net/http/httputil/reverseproxy.go`
  (`flushInterval`, `copyResponse`) and `net/http/server.go`
  (`WriteTimeout`, `ListenAndServeTLS` ALPN).
- Kubernetes serving side: `k8s.io/apiserver/pkg/server/secure_serving.go`
  (lines ~184–205 configure HTTP/2 explicitly).
- Proxy code touched by this design:
  - [cmd/server/main.go:34-37](../cmd/server/main.go#L34-L37) (timeouts)
  - [cmd/server/main.go:170-176](../cmd/server/main.go#L170-L176) (server build)
  - [cmd/server/main.go:606-615](../cmd/server/main.go#L606-L615) (TLS config)
  - [cmd/server/main.go:627-680](../cmd/server/main.go#L627-L680) (backend transport)
  - [pkg/proxy/handler.go:128-149](../pkg/proxy/handler.go#L128-L149) (ReverseProxy)
  - [pkg/proxy/handler.go:194-199](../pkg/proxy/handler.go#L194-L199) (audit vs passthrough split)
