# Why An Audit Pass-Through APIServer Is Needed

## Problem Statement

GitOps Reverser reconstructs cluster state into Git from the Kubernetes audit webhook stream. For built-in resources (`ConfigMap`, `Deployment`, etc.) this works well: each mutating request produces an audit event with `objectRef.name`, `requestObject`, and `responseObject`, which is everything needed to identify the object and attribute the change to a specific actor.

For resources served by **aggregated API servers** (the `APIService` extension mechanism — e.g. `metrics.k8s.io`, `wardle.example.com`, `custom.metrics.k8s.io`, and anything built with `sample-apiserver`, `apiserver-builder`, or the raw `k8s.io/apiserver` library), this breaks. See [../../investigate.md](../../investigate.md) for the empirical capture.

The aggregated `create` audit event contains:

- `objectRef.resource`, `objectRef.namespace`, `objectRef.apiGroup`, `objectRef.apiVersion`
- collection-style `requestURI` (e.g. `/apis/wardle.example.com/v1alpha1/namespaces/default/flunders`)
- `verb`, `user`, timestamps, HTTP status code

It **does not** contain:

- `objectRef.name`
- `objectRef.uid`
- `requestObject`
- `responseObject`

That means from audit alone:

- we do not know *which object* was created
- we cannot recover the intended spec
- we cannot recover the server-assigned UID or server-applied defaults
- time-window correlation against the watch stream is unsafe (any other concurrent create on the same resource would be indistinguishable)

For a reverse-GitOps system, this is a structural gap: we cannot write the correct file to Git.

## Why This Is Not A Policy Mistake

A natural first reaction is "the audit policy is set too low." That is not the case here. GitOps Reverser's e2e setup uses [test/e2e/cluster/audit/policy.yaml](../../test/e2e/cluster/audit/policy.yaml) which already sets `level: RequestResponse` with `omitManagedFields: true` for every mutating verb across every resource. The fields are empty because kube-apiserver never fills them for aggregated requests, regardless of policy level.

## Why This Is Not A Backing-Server Bug

A second reaction is "the aggregated backend isn't returning the object." That is also not the case. The investigation captured direct `GET`, `LIST`, and `WATCH` responses from the same aggregated `Flunder` API and all of them contain the full object with `metadata` and `spec` populated. The backing aggregated server is serving correctly. The object data exists on the wire. It is the audit layer that is not recording it.

## Why This Is Not A k3s Quirk

A third reaction is "maybe this is k3s-specific and upstream kube fixes it." We wanted to validate that empirically against a kind cluster; that experiment was blocked by a devcontainer `inotify` limit ([see the attempt log](../../hack/audit-kind-check/ATTEMPT.md)). We instead validated it by reading the upstream kube source, which is more definitive because it shows the behavior is structural rather than version- or distro-specific.

The rest of this document is that source-level analysis.

## Where The Behavior Comes From In Upstream kube

Kubernetes' audit subsystem is split across two layers that do not interact for aggregated requests:

1. An **HTTP middleware filter** that records request metadata, response status, and latency.
2. A **REST handler layer** that records the deserialized request and response objects.

For built-in resources, every request passes through both layers, so every field gets populated. For aggregated resources, the request passes through the middleware filter but is then reverse-proxied at the HTTP byte level directly to the backing apiserver. The REST handler layer is bypassed, and so is the code that fills in `requestObject`, `responseObject`, and the body-derived `objectRef.name` / `objectRef.uid`.

### Layer 1: The audit middleware filter

File: [`k8s.io/apiserver/pkg/endpoints/filters/audit.go`](../../external-sources/kubernetes/staging/src/k8s.io/apiserver/pkg/endpoints/filters/audit.go)

- Entry point: `WithAudit(handler, sink, policy, longRunningCheck)` — the HTTP middleware that every request goes through.
- Inside `WithAudit`, the response is wrapped with `auditResponseWriter`, whose only job is to intercept `WriteHeader` / `Write` and record the status code via `AuditContext.SetEventResponseStatusCode`.
- Request metadata is populated by `audit.LogRequestMetadata` in [`k8s.io/apiserver/pkg/audit/request.go`](../../external-sources/kubernetes/staging/src/k8s.io/apiserver/pkg/audit/request.go#L43), using `authorizer.Attributes`:
  - `ev.Verb`, `ev.RequestURI`, `ev.UserAgent`, `ev.SourceIPs`
  - `ev.User` from the authenticated request
  - `ev.ObjectRef` populated from `attribs.GetName()`, `attribs.GetNamespace()`, `attribs.GetResource()`, `attribs.GetSubresource()`, `attribs.GetAPIGroup()`, `attribs.GetAPIVersion()`

**Key detail:** For a collection-style create (`POST /apis/<group>/<version>/namespaces/<ns>/<resource>`), the request URL contains no name segment, so `RequestInfoFactory.NewRequestInfo` produces `Attributes.Name == ""`. The middleware filter has nothing to populate `objectRef.name` with at this stage. The name has to come from the request body — and reading the body is not the filter's job.

**What `WithAudit` never does:** it never reads the request body and never reads the response body. The middleware is a metadata-only layer.

### Layer 2: The REST handler layer (built-in resources only)

The functions that populate the body-derived fields are:

- `audit.LogRequestObject(ctx, obj, objGV, gvr, subresource, serializer)` — fills `requestObject`, and as a side effect fills `ObjectRef.Name` / `Namespace` / `UID` / `ResourceVersion` from the request body's `ObjectMeta` (lines 124-143 of [`request.go`](../../external-sources/kubernetes/staging/src/k8s.io/apiserver/pkg/audit/request.go#L124-L143)).
- `audit.LogResponseObject(ctx, obj, gv, serializer)` — fills `responseObject`.

Their call sites are confined to the native REST machinery:

| Verb | Call site |
| --- | --- |
| `create` | [`endpoints/handlers/create.go:161`](../../external-sources/kubernetes/staging/src/k8s.io/apiserver/pkg/endpoints/handlers/create.go#L161) — inside `createHandler` / `CreateResource`, called after the body has been decoded and before admission runs |
| `update` | [`endpoints/handlers/update.go:139`](../../external-sources/kubernetes/staging/src/k8s.io/apiserver/pkg/endpoints/handlers/update.go#L139) |
| `delete` | [`endpoints/handlers/delete.go:118`](../../external-sources/kubernetes/staging/src/k8s.io/apiserver/pkg/endpoints/handlers/delete.go#L118), [`delete.go:294`](../../external-sources/kubernetes/staging/src/k8s.io/apiserver/pkg/endpoints/handlers/delete.go#L294) — for single delete and `deletecollection` respectively |
| Response side | [`endpoints/handlers/responsewriters/writers.go:346`](../../external-sources/kubernetes/staging/src/k8s.io/apiserver/pkg/endpoints/handlers/responsewriters/writers.go#L346) — inside `WriteObjectNegotiated`, called for every successful response produced by a REST handler |

These call sites all require access to a `runtime.Object` — the decoded in-memory representation of the resource. That decoding only happens inside the REST handler layer, which knows the resource scheme.

### Layer 3: The aggregated-API proxy (skips layer 2)

File: [`k8s.io/kube-aggregator/pkg/apiserver/handler_proxy.go`](../../external-sources/kubernetes/staging/src/k8s.io/kube-aggregator/pkg/apiserver/handler_proxy.go)

- Type: `proxyHandler.ServeHTTP` (line 107).
- When a request matches an `APIService` pointing at a remote backend, kube-apiserver builds a new `*http.Request` pointing at the backing service and dispatches it through `proxy.NewUpgradeAwareHandler` (line 182).

File: [`k8s.io/apimachinery/pkg/util/proxy/upgradeaware.go`](../../external-sources/kubernetes/staging/src/k8s.io/apimachinery/pkg/util/proxy/upgradeaware.go)

- Constructor: `NewUpgradeAwareHandler` (line 181).
- Handler: `UpgradeAwareHandler.ServeHTTP` (line 213).
- Implementation strategy: an `httputil.ReverseProxy` with a streaming transport, augmented to handle protocol upgrades (`WATCH`, `exec`, `attach`, `port-forward`). For non-upgrade requests it copies the request body byte-for-byte into the upstream connection and copies the response body byte-for-byte back to the client.

**Key detail:** `UpgradeAwareHandler` never deserializes the request or the response. It does not know the resource's `runtime.Scheme`, because by design it proxies requests for API groups whose schemas are owned by the aggregated backend, not by kube-apiserver itself. That is the whole point of API aggregation — the aggregator host does not have to link in the types.

Because deserialization never happens in the aggregator process, there is no `runtime.Object` to hand to `audit.LogRequestObject` or `audit.LogResponseObject`. Those functions are never called on the aggregated path. No call to them, no `requestObject`, no `responseObject`, no body-derived `objectRef.name` or `objectRef.uid`.

### Why `WithAudit` alone cannot fix this

One could ask: why doesn't `WithAudit` just read the request body itself? Three reasons, visible in the code:

1. **Body semantics are unknown at the middleware layer.** The middleware does not know whether the request body is a JSON `Flunder`, a YAML patch, a `DeleteOptions`, a streaming exec frame, or something else. Parsing requires the resource's scheme, and the aggregator process does not have schemes for aggregated resources.
2. **Body consumption is destructive.** Reading the body at middleware time would require buffering and re-injecting it — possible, but kube avoids this on the proxy path because it would break streaming verbs (`WATCH`, `exec`, `attach`, `port-forward`) that share the same handler chain.
3. **Even with a buffered body, response-side capture is not available.** `auditResponseWriter` intercepts `WriteHeader`/`Write` but does not buffer bytes. Adding buffering would change latency, memory behavior, and correctness for long-running and streaming endpoints.

Fixing this in upstream kube-apiserver would be a non-trivial architectural change touching a layer that is deliberately schema-agnostic. There is no sign of it being proposed, and attempts to propose it would have to address the three points above.

## Why The Pass-Through APIServer Is The Right Recovery Point

Given the structural constraint above, the question becomes: where in the request flow are **both** the deserialized request body **and** the deserialized response body simultaneously available?

- Not at the client.
- Not at the kube-apiserver middleware layer (deliberately schema-agnostic for aggregated requests).
- Not inside `UpgradeAwareHandler` (pure byte-level proxy).
- **Inside a process that knows the resource's scheme and sits in the request path** — i.e. the aggregated API backend itself, or a proxy placed immediately in front of it.

That is exactly the position this prototype occupies. By registering as the `APIService` backend and proxying to the real backing server, the pass-through APIServer:

- sees the raw HTTP request with body intact (before forwarding)
- sees the raw HTTP response with body intact (after the backend returns)
- can decode both bodies against the known aggregated resource scheme
- can emit a synthetic `audit.k8s.io/v1` `Event` that includes `requestObject`, `responseObject`, and `objectRef.name`

This recovers the information that kube-apiserver structurally cannot record for aggregated resources.

## Why CRDs Don't Have This Problem

A common counter-question is "why not just use CRDs instead of aggregated APIs?" The reason CRDs do not have the sparse-audit problem is instructive and worth spelling out:

- `CustomResourceDefinition` objects register a resource with kube-apiserver itself.
- Requests for CRD-defined resources are served by kube-apiserver's **native REST handler chain**, not by an aggregated backend. The relevant code lives in [`k8s.io/apiextensions-apiserver/pkg/apiserver/customresource_handler.go`](../../external-sources/kubernetes/staging/src/k8s.io/apiextensions-apiserver/pkg/apiserver/customresource_handler.go), which wires CRD resources through the same `createHandler`, `updateHandler`, etc. that built-in resources use.
- Because CRD requests go through the REST handler layer, `audit.LogRequestObject` and `audit.LogResponseObject` both fire, and the audit event is populated identically to a built-in resource.

Current Kubernetes guidance does recommend CRDs as the default extension mechanism and reserves aggregation for cases that cannot be expressed as CRDs. But that guidance does not retire aggregated APIs — they are still the foundation of `metrics.k8s.io`, `custom.metrics.k8s.io`, `external.metrics.k8s.io`, `apiregistration.k8s.io` consumers in the wild, and any extension that needs handler-level logic beyond what CRDs offer. GitOps Reverser needs to handle what exists in real clusters, not only what is newly recommended.

## What This Prototype Is (And Is Not) Claiming

**Claim:** For aggregated API resources, there is no position in the kube-apiserver request path where request and response bodies are simultaneously available to the audit subsystem. Recovering that visibility requires code running at the aggregated backend position, where the resource scheme is known. This prototype fills that position with a minimal, auditable proxy.

**Not claimed:**

- That upstream kube-apiserver audit is broken. It is behaving as its architecture dictates.
- That upstream kube-apiserver should change. The middleware's schema-agnostic stance is a reasonable design choice.
- That this prototype replaces kube audit. It runs in parallel; duplicate-event suppression and stream ownership are higher-level deployment concerns.
- That this prototype is a general audit pipeline. It is intentionally scoped to the mutating verbs and events needed by GitOps Reverser.

## Source References Summary

For any reviewer who wants to re-derive this conclusion, the load-bearing files are:

**Middleware and metadata:**

- [`k8s.io/apiserver/pkg/endpoints/filters/audit.go`](../../external-sources/kubernetes/staging/src/k8s.io/apiserver/pkg/endpoints/filters/audit.go) — `WithAudit`, `auditResponseWriter`
- [`k8s.io/apiserver/pkg/audit/request.go`](../../external-sources/kubernetes/staging/src/k8s.io/apiserver/pkg/audit/request.go) — `LogRequestMetadata`, `LogRequestObject`, `LogResponseObject`
- [`k8s.io/apiserver/pkg/audit/context.go`](../../external-sources/kubernetes/staging/src/k8s.io/apiserver/pkg/audit/context.go) — `AuditContext`, `LogResponseObject` (context method)

**REST handler call sites (fire only for built-in and CRD resources):**

- [`k8s.io/apiserver/pkg/endpoints/handlers/create.go`](../../external-sources/kubernetes/staging/src/k8s.io/apiserver/pkg/endpoints/handlers/create.go)
- [`k8s.io/apiserver/pkg/endpoints/handlers/update.go`](../../external-sources/kubernetes/staging/src/k8s.io/apiserver/pkg/endpoints/handlers/update.go)
- [`k8s.io/apiserver/pkg/endpoints/handlers/delete.go`](../../external-sources/kubernetes/staging/src/k8s.io/apiserver/pkg/endpoints/handlers/delete.go)
- [`k8s.io/apiserver/pkg/endpoints/handlers/responsewriters/writers.go`](../../external-sources/kubernetes/staging/src/k8s.io/apiserver/pkg/endpoints/handlers/responsewriters/writers.go) — `WriteObjectNegotiated`

**Aggregated proxy path (body-level bypass):**

- [`k8s.io/kube-aggregator/pkg/apiserver/handler_proxy.go`](../../external-sources/kubernetes/staging/src/k8s.io/kube-aggregator/pkg/apiserver/handler_proxy.go) — `proxyHandler.ServeHTTP`
- [`k8s.io/apimachinery/pkg/util/proxy/upgradeaware.go`](../../external-sources/kubernetes/staging/src/k8s.io/apimachinery/pkg/util/proxy/upgradeaware.go) — `NewUpgradeAwareHandler`, `UpgradeAwareHandler.ServeHTTP`

**CRD handler chain (comparison — audit works here):**

- [`k8s.io/apiextensions-apiserver/pkg/apiserver/customresource_handler.go`](../../external-sources/kubernetes/staging/src/k8s.io/apiextensions-apiserver/pkg/apiserver/customresource_handler.go) — dispatches CRD requests through the same REST handlers as built-ins

**Empirical evidence:**

- [`investigate.md`](../../investigate.md) — captured k3s audit events, `GET/LIST/WATCH` traces
- [`.stamps/debug/flunder-create-audit.json`](../../.stamps/debug/flunder-create-audit.json) — sparse aggregated-create event
- [`.stamps/debug/configmap-create-audit.json`](../../.stamps/debug/configmap-create-audit.json) — rich built-in-create event for comparison
- [`hack/audit-kind-check/ATTEMPT.md`](../../hack/audit-kind-check/ATTEMPT.md) — record of the attempted kind validation and why it was replaced by source-level analysis
