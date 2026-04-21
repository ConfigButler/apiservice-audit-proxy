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

## Status

The prototype now has the first runtime wiring needed for the e2e hookup:

- inbound HTTPS serving via `--tls-cert-file` and `--tls-private-key-file`
- explicit backend HTTPS behavior via:
  - `--backend-insecure-skip-verify`
  - `--backend-ca-file`
  - `--backend-client-cert-file`
  - `--backend-client-key-file`
  - `--backend-server-name`

That means the remaining work is narrower now: document the trust model clearly, then wire the
prototype into the main project's e2e environment.

## Load-Bearing Remaining Work

### 1. Inbound TLS has landed for the prototype

The prototype binary can now serve HTTPS directly when mounted cert/key files are provided.

This keeps the preferred e2e path intact:

- create a hand-rolled self-signed Kubernetes Secret
- mount that Secret into the proxy pod
- point the proxy at the mounted `tls.crt` and `tls.key`
- rely on `APIService.spec.insecureSkipTLSVerify: true` for the spike, so the aggregator does not
  need to trust that serving cert chain yet

Done when:

- the prototype binary can serve HTTPS using mounted cert/key files
- the proxy can be placed behind `APIService` without failing immediately due to plain HTTP on an
  HTTPS path

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

Decision for the first spike:

- remain network-trust-only and document that clearly
- defer `--client-ca-file` and aggregator client cert verification until after the first e2e path
  is proven

Done when:

- the README and e2e hookup plan both state that delegated header trust is currently derived from
  deployment topology / network path rather than verified aggregator client identity
- no one reading the spike docs would assume `X-Remote-*` is cryptographically authenticated by the
  prototype itself

### 3. Backend TLS is now explicit

The prototype now exposes an explicit backend HTTPS story instead of leaving it implicit:

- `--backend-insecure-skip-verify`
- `--backend-ca-file`
- `--backend-client-cert-file`
- `--backend-client-key-file`
- `--backend-server-name`

For the first cluster spike, `--backend-insecure-skip-verify` is still the expected shortest path
because the sample-apiserver generates self-signed serving certs at startup.

For the sample-apiserver e2e hookup specifically, backend client authentication also matters: the
proxy is the immediate TLS caller to the real backend, so it may need its own client certificate in
order for the backend to accept the forwarded request path.

Done when:

- the proxy can connect successfully to the sample-apiserver backend over HTTPS in e2e
- the backend trust mode is visible in code and flags rather than being an unstated assumption

### 4. Final flag surface checklist

This is not a separate work item. It is the expected flag surface once items 1-3 above are landed.

Minimum useful next argument set:

- `--listen-address`
- `--backend-url`
- `--backend-insecure-skip-verify` or `--backend-ca-file`
- `--backend-client-cert-file`
- `--backend-client-key-file`
- `--backend-server-name`
- `--webhook-kubeconfig`
- `--webhook-timeout`
- `--max-audit-body-bytes`
- `--capture-temp-dir`
- `--tls-cert-file`
- `--tls-private-key-file`

Optional for the first spike:

- `--client-ca-file`

Test expectation for this work:

- do not overbuild dedicated TLS unit tests for the spike
- keep unit tests where they are already valuable at the package level
- treat TLS wiring as primarily smoke-verified through the e2e hookup path once the flags exist

This can be revisited later if the TLS/configuration surface becomes more complex.

### 5. Effective user attribution is proven, but impersonation fidelity still needs a decision

The main project e2e now proves an important part of the end-to-end story:

- an aggregated API request made with `kubectl --as=jane@acme.com` can still produce a Git commit
  attributed to `jane@acme.com`

That is the behavior GitOps Reverser needs most. But it does **not** automatically mean the
prototype can promise a distinct upstream-style `impersonatedUser` field in every synthetic audit
event.

Current understanding:

- the delegated requestheader path reliably gives the proxy the **effective** caller identity
- that effective identity is sufficient for downstream author attribution
- the original split between authenticated caller and impersonated caller may not always survive in
  a way the prototype can reconstruct confidently

For the prototype, that means we should document the current semantics clearly before treating
impersonation fidelity as solved:

- user attribution is based on the delegated effective identity
- preserving a separate `impersonatedUser` field is still a follow-up decision, not a guaranteed
  property of the spike

Done when:

- the README states that effective delegated identity is the current contract
- the README does not imply that the prototype always preserves a separate
  `audit.Event.impersonatedUser`
- a future work session can tell, just by reading the prototype docs, whether impersonation support
  means "correct final user attribution" or "full upstream field-by-field impersonation fidelity"

## Current Boundaries To Preserve

These are still intentionally out of scope for the prototype:

- duplicate suppression
- generalized audit-policy behavior
- production-grade retry/backpressure
- solving delegated header trust beyond documented deployment assumptions

## Suggested Next Work Order

1. Use the new serving TLS flags in the proxy pod deployment and Secret mount wiring.
2. Use `--backend-insecure-skip-verify` for the first sample-apiserver HTTPS spike unless backend
   CA wiring is worth proving immediately.
3. Keep delegated header trust explicitly network-based until `--client-ca-file` becomes necessary.
4. Execute the e2e hookup plan in the main project docs.
