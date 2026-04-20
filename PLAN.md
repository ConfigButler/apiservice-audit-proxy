# Audit Pass-Through APIServer Prototype Plan

## Goal

Build a small aggregated APIServer that stands in front of an existing aggregated backend, proxies mutating requests to that backend, and emits Kubernetes audit webhook payloads that are good enough for GitOps Reverser.

For the first prototype, "good enough" means:

- use upstream `audit.k8s.io/v1` types
- emit only `ResponseComplete`
- support only `create`, `update`, `patch`, and `delete`
- capture enough identity and object data for GitOps Reverser to attribute and write Git state safely

## Why This Exists

The investigation in [investigate.md](../../investigate.md) showed that aggregated API `create` audit events are too sparse in the current setup:

- no `objectRef.name`
- no `requestObject`
- no `responseObject`
- no stable object identity for safe reverse-GitOps writes

At the same time, the underlying aggregated API serving path looks healthy. `GET`, `LIST`, and `WATCH` already return full objects. That strongly suggests the problem is in audit capture, not object serving.

This is not a policy misconfiguration or a k3s quirk — it is structural upstream kube behavior. The aggregated-API proxy path (`kube-aggregator/pkg/apiserver/handler_proxy.go` → `apimachinery/pkg/util/proxy.UpgradeAwareHandler`) bypasses the native REST handler layer where `audit.LogRequestObject` and `audit.LogResponseObject` are invoked, so the body-derived audit fields have nothing to populate them. [WHY.md](WHY.md) walks through the kube source to prove this; [../../hack/audit-kind-check/ATTEMPT.md](../../hack/audit-kind-check/ATTEMPT.md) records the kind-based empirical check that was replaced by the source-level analysis.

## Current Scope

The prototype is intentionally narrow.

In scope:

- aggregated API stand-in replacement via `APIService`
- proxying to an existing aggregated backend we do not want to modify
- mutating verbs only: `create`, `update`, `patch`, `delete`
- `ResponseComplete` only
- immediate webhook POST after proxied request completion
- `audit.k8s.io/v1` `EventList` payloads
- actor identity capture from aggregated request headers

Out of scope:

- `get`
- `list`
- `watch`
- duplicate suppression
- audit policy filtering inside the proxy
- generalized configuration surface
- exact parity with every optional kube audit field
- reimplementing the underlying aggregated API business logic

## Core Architectural Decision

This server is a proxy in front of an existing aggregated API server.

It is not:

- an HTTP middleware injected into kube-apiserver
- a rewrite of the original aggregated server
- a general-purpose API framework

It is a stand-in backend replacement for the existing `APIService`.

That means:

- kube-apiserver talks to this proxy as the aggregated backend
- this proxy then talks to the real aggregated backend
- there is an extra hop, extra TLS, and separate service identity

This is more complex than embedding audit emission inside the real backend. It is still the preferred prototype shape for one reason:

- it works even when we do not own the backing aggregated server code

That assumption matters. If we owned the backend and were happy to modify it, emitting audit-compatible events inside its handler chain would be simpler.

## Recommended Starting Point

For this prototype, the recommended implementation path is:

- use `k8s.io/apiserver` directly
- start from a minimal bootstrap, not a full repo clone
- use `sample-apiserver` as reference only
- use `kubernetes/kubernetes` to confirm exact behavior when needed
- treat `apiserver-builder-alpha` as historical reference only

Local reference checkouts already exist in `external-sources/` for:

- `kubernetes/kubernetes`
- `sample-apiserver`
- `apiserver-builder-alpha`

`apiserver-builder-alpha` does not appear actively maintained, so it should not be the foundation for the prototype.

## Identity Capture

Identity capture is central to the value of this prototype.

For aggregated API backends, the canonical identity source is the delegated requestheader auth path from kube-apiserver:

- `X-Remote-User`
- `X-Remote-Group`
- `X-Remote-Extra-*`

These headers are the authoritative actor identity surface for the proxy.

Important implication:

- the proxy should not treat incoming `Impersonate-*` headers from the original client as the canonical identity source

By the time the request reaches the aggregated backend, kube-apiserver has already authenticated the caller and forwarded identity through the requestheader mechanism. The proxy should therefore build `audit.Event.user` from:

- `X-Remote-User`
- `X-Remote-Group`
- `X-Remote-Extra-*`

Fidelity note:

- this should preserve the effective caller identity that kube-apiserver delegated to the backend
- original impersonation intent may not be reconstructible as a separate `impersonatedUser` field

For the prototype, if true impersonation fidelity is not available from the delegated headers with confidence, leave `impersonatedUser` unset and document that limitation explicitly.

## Event Model

The prototype should emit upstream Kubernetes audit types:

- `audit.k8s.io/v1`
- `EventList` on every webhook POST

Even when there is only one event, the webhook body should still be an `EventList`. Downstream consumers should not need a separate single-event code path.

For the first prototype, each proxied mutating request emits exactly one audit event:

- `stage: ResponseComplete`

No `RequestReceived`, no `ResponseStarted`, no `Panic` export path for the initial version.

Fields to populate when easy and reliable:

- `apiVersion`
- `kind`
- `level`
- `auditID`
- `stage`
- `requestURI`
- `verb`
- `user`
- `sourceIPs` if available
- `objectRef`
- `requestObject`
- `responseObject`
- `responseStatus`
- `requestReceivedTimestamp`
- `stageTimestamp`
- selected annotations if useful

Fields that may be left unset in the prototype:

- fields that require kube internal context the proxy does not have
- fields whose semantics would be guessed rather than reproduced faithfully
- fields not needed by GitOps Reverser

The project should document every intentionally omitted field and why it is omitted.

## Supported Request Flow

1. Client sends a mutating request for an aggregated resource to kube-apiserver.
2. kube-apiserver authenticates the caller and forwards the request to the proxy backend via API aggregation.
3. The proxy reads:
   - request body
   - delegated identity headers
   - verb, URI, query, namespace, resource info
4. The proxy forwards the request to the real aggregated backend.
5. The real backend completes the request and returns its response.
6. The proxy captures:
   - response body
   - HTTP status
   - timing
7. The proxy builds one `audit.k8s.io/v1` `Event`.
8. The proxy wraps that event in an `EventList`.
9. The proxy POSTs the `EventList` to the configured webhook endpoint.
10. The proxy returns the backend response unchanged to kube-apiserver.

## Delivery Decision

The prototype should be explicitly best-effort and non-blocking with respect to audit delivery outcome.

That means:

- the proxy attempts webhook delivery after request completion
- webhook failure does not fail the proxied API request
- failures are logged clearly
- failure metrics are exposed
- no durable retry queue in the first version

Reason:

- this is a prototype
- we want to validate the premise, not build a production-grade backpressure system first
- committing to non-blocking behavior simplifies the design substantially

## Configuration Philosophy

This prototype should not become a configurable audit product.

Constraints:

- no audit-policy-like filtering inside the proxy
- no per-verb toggle matrix beyond the hard-coded supported verbs
- no mode switching between multiple delivery strategies
- no alternate event schema

Instead:

- the proxy forwards all supported events it handles
- unsupported verbs are simply out of scope

## Kubeconfig And TLS Approach

The outbound webhook client should follow the same model already used by the parent project:

- static kubeconfig-style client config
- server certificate validation via CA data
- client certificate authentication for the sender
- mutual validation between sender and receiver

Relevant local references:

- [`docs/design/audit-webhook-api-server-connectivity.md`](../../docs/design/audit-webhook-api-server-connectivity.md)
- [`docs/architecture.md`](../../docs/architecture.md)
- [`hack/generate-audit-webhook-kubeconfig.sh`](../../hack/generate-audit-webhook-kubeconfig.sh)

Open point:

- the exact kubeconfig generation flow for the future standalone repo is still undecided

But the prototype should assume that insecure transport is not acceptable as the normal path.

## Minimal Components

- `cmd/server`: process bootstrap
- `pkg/proxy`: reverse proxy and response capture
- `pkg/audit`: `Event` builder and `EventList` wrapper
- `pkg/identity`: requestheader identity extraction
- `pkg/webhook`: HTTP POST client for audit delivery
- `test/`: golden payload tests and small integration tests

## Redaction And Payload Limits

For the prototype, redaction and truncation can stay simple.

Allowed first version behavior:

- size-based truncation for very large request or response bodies
- minimal coarse redaction if obviously needed

Explicit limitation:

- this does not imply robust semantic redaction of sensitive paths such as every Secret-like field pattern

That limitation should be called out clearly in the project notes.

## Milestones

### Milestone 1: Minimal Stand-In Proxy

- build a minimal `k8s.io/apiserver` bootstrap
- register one aggregated resource path as stand-in replacement
- proxy to an upstream test server
- preserve response body and status code

### Milestone 2: Identity And ResponseComplete Capture

- capture delegated identity from `X-Remote-*`
- capture request and response bodies
- capture resource metadata needed for `objectRef`
- build one `ResponseComplete` audit event per supported request

### Milestone 3: Real Cluster Integration Spike

- deploy as actual `APIService` backend replacement
- connect it to an existing aggregated server
- confirm delegated auth headers arrive as expected
- confirm response pass-through remains transparent
- confirm emitted `EventList` payloads are accepted by downstream receiver

This is the load-bearing experiment and should happen early.

### Milestone 4: Event Polish

- tighten field mapping to `audit.k8s.io/v1`
- add golden tests for emitted payloads
- document intentionally omitted fields
- add failure logs and basic metrics

### Milestone 5: GitOps Reverser Validation

- point the existing consumer at the prototype stream
- verify deterministic mapping from event to Git path
- verify attribution works for aggregated mutating requests
- verify the reduced event surface is enough for GitOps Reverser

## Pros

- works even against aggregated backends we do not own
- captures request and response at the one point where both are visible together
- keeps the first version small by using `ResponseComplete` only
- uses standard Kubernetes audit types
- stays tightly aligned with GitOps Reverser instead of becoming a generic platform

## Cons

- adds a second hop between kube-apiserver and the real backend
- requires separate TLS and service identity for the stand-in proxy
- is more complex than embedding logic inside a backend we own
- will not perfectly match kube-apiserver audit semantics in the first version

## Main Risks

- delegated headers may not give every identity nuance we want
- stand-in replacement behavior must stay transparent enough that clients do not notice the proxy
- exact `audit.k8s.io` fidelity may be harder than expected
- audit delivery failure visibility must be good enough even in best-effort mode
- payload truncation without semantic redaction is not safe enough for all object types

## Recommendation

This is worth prototyping.

The clearest first version is:

"build a stand-in aggregated API proxy for an existing backend that emits one best-effort `audit.k8s.io/v1` `ResponseComplete` `EventList` per supported mutating request, using delegated `X-Remote-*` identity and only the fields GitOps Reverser actually needs."

That is narrow, testable, and strong enough to validate the core premise before investing in more fidelity.
