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
- delegated requestheader trust verification via `--client-ca-file`
- a Go e2e smoke package under `test/e2e/`
- a second backend-CA e2e lane

That means the remaining work is narrower now: keep the docs aligned with the implemented trust
model, stabilize the local cluster story, and carry the prototype cleanly into standalone release
and CI flows.

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

### 2. Caller authentication is now verified, but the policy surface stays intentionally small

The prototype currently extracts delegated identity from:

- `X-Remote-User`
- `X-Remote-Group`
- `X-Remote-Extra-*`

and it can now verify the upstream front-proxy client certificate before trusting those headers
when `--client-ca-file` is configured.

What remains intentionally out of scope:

- reproducing every upstream kube-aggregator requestheader policy toggle
- adding allowed client-name filtering before the project proves it needs that complexity

Done when:

- the README and e2e hookup plan both state that delegated header trust can be anchored in a
  verified front-proxy CA bundle
- the docs also state plainly that the requestheader trust model is still intentionally narrower
  than a full kube-aggregator bootstrap

### 3. Backend TLS is now explicit

The prototype now exposes an explicit backend HTTPS story instead of leaving it implicit:

- `--backend-insecure-skip-verify`
- `--backend-ca-file`
- `--backend-client-cert-file`
- `--backend-client-key-file`
- `--backend-server-name`

The repo now supports both:

- the shorter `--backend-insecure-skip-verify` smoke lane
- an explicit backend CA validation lane for the sample-apiserver

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
- fully reproducing kube-aggregator requestheader policy beyond the documented CA-backed trust path

## Suggested Next Work Order

1. Stabilize the live k3d/bootstrap path so the Go smoke lanes are easy to run in fresh
   environments.
2. Add the optional fast `dev-self-signed` smoke lane if the local feedback loop needs it.
3. Decide whether standalone extraction happens before or after e2e becomes a CI gate.
4. Execute the remaining e2e hookup and extraction work in the main project docs.
