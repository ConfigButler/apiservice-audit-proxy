# Prototype TODO

This file records the prototype-local work that should happen before the audit pass-through
APIServer is treated as ready for the main project's e2e integration.

The goal here is to keep the implementation prerequisites close to the prototype itself, so a new
work session can pick up the remaining engineering tasks without needing the full surrounding
conversation.

## Why This Exists

The e2e hookup plan now lives in the main project docs because it belongs to the larger system:

- [docs/design/e2e-aggregated-apiserver-proxy-hookup-plan.md](../../docs/design/e2e-aggregated-apiserver-proxy-hookup-plan.md)

This file is narrower. It covers only the **prototype implementation gaps** that matter before that
e2e plan can be executed cleanly.

## Load-Bearing Remaining Work

### 1. Inbound TLS is a prerequisite, not a future polish item

The current prototype serves plain HTTP only.

That is not enough for the `APIService` spike, because kube-apiserver will dial the aggregated
backend over HTTPS. Even with:

- `spec.insecureSkipTLSVerify: true`

on the `APIService`, the backend still needs to speak TLS on the wire.

Required next step:

- add inbound TLS support to `cmd/server`
- expose flags such as:
  - `--tls-cert-file`
  - `--tls-private-key-file`

Possible but less-preferred spike fallback:

- run a sidecar TLS terminator in front of the prototype and keep the Go server itself on HTTP

The direct in-process TLS option is the cleaner next step.

### 2. Caller authentication remains unresolved by design

The prototype currently extracts delegated identity from:

- `X-Remote-User`
- `X-Remote-Group`
- `X-Remote-Extra-*`

but it does not verify the upstream aggregator client certificate before trusting those headers.

This is aligned with the explicit non-goal that delegated header trust is not solved here. Still,
for clarity:

- without a `--client-ca-file` style check, the spike effectively trusts the cluster network path
  and service reachability

Required follow-up:

- decide whether the first spike will:
  - remain network-trust-only and document that clearly, or
  - add optional client certificate verification via something like `--client-ca-file`

For the very first e2e spike, documented network trust is acceptable if called out plainly.

### 3. Backend TLS needs an explicit story

The real sample-apiserver currently serves HTTPS with its own certificate behavior. The prototype
runtime plan already assumes:

- `--backend-url=https://...:443`

But the backend trust behavior is not implemented yet.

Required next step:

- add one explicit backend TLS mode for the spike

Most likely first option:

- `--backend-insecure-skip-verify`

Better but slightly heavier option:

- `--backend-ca-file`
- optional `--backend-server-name`

The main point is to make this explicit in code and flags rather than implicit in docs.

### 4. First usable container args should become real, not aspirational

The current README lists likely-next flags. Those need to become actual implemented flags if the
prototype is going to sit behind `APIService`.

Minimum useful next argument set:

- `--listen-address`
- `--backend-url`
- `--backend-insecure-skip-verify` or `--backend-ca-file`
- `--backend-server-name`
- `--webhook-kubeconfig`
- `--webhook-timeout`
- `--max-audit-body-bytes`
- `--capture-temp-dir`
- `--tls-cert-file`
- `--tls-private-key-file`

Optional for the first spike:

- `--client-ca-file`

### 5. Keep the e2e scope narrow

Once the TLS pieces above are in place, the first useful e2e target should still stay small:

- aggregated `create`
- one successful request path
- one proof that the downstream audit receiver gets:
  - `objectRef.name`
  - `requestObject`
  - `responseObject`

Do not expand to broader semantics before that path works.

## Current Boundaries To Preserve

These are still intentionally out of scope for the prototype:

- duplicate suppression
- generalized audit-policy behavior
- production-grade retry/backpressure
- solving delegated header trust beyond documented deployment assumptions

## Suggested Next Work Order

1. Implement inbound TLS flags and server wiring.
2. Implement backend TLS behavior for the real sample-apiserver path.
3. Decide whether to add optional client CA verification now or document network trust for the
   first spike.
4. Then execute the e2e hookup plan in the main project docs.
