# TODO

Remaining work that matters for the current project state. Historical packaging
and implementation plans have been intentionally collapsed into this shorter
list.

## Highest Priority

### 1. Release Hardening

The image and chart need to ship with the supply-chain artifacts operators
expect before this can be recommended for production use.

- generate an SBOM for the proxy image and publish it alongside the image
- sign the image (and ideally the chart) with cosign and document the
  verification command
- decide the GHCR/cosign flow once, then wire it into the release workflow so
  signing cannot be skipped

Done when:

- every published image has an attached SBOM and signature, and a one-line
  `cosign verify` example lives in the README

### 2. Chart Quality

The Helm chart has grown to cover several certificate modes and an optional
demo stack. It needs assertions so a refactor cannot silently break a mode.

- add Helm template tests for each supported certificate mode (self-signed,
  cert-manager, existing secret)
- assert the APIService CA bundle is wired correctly in each mode
- cover the `audit.testWebhookReceiver` and `backend.testApiserver` toggles so
  the demo path stays green

Done when:

- `helm template` and `helm test` catch a regression in any supported mode
  without needing a full e2e run

### 3. One-Command E2E

`Taskfile.e2e.yml` currently exposes several lanes. Collapse the entrypoint so
contributors and CI run the same thing, and CI cannot drift from local by
forgetting a lane.

- one canonical `task e2e` that runs the full suite end-to-end
- keep per-scenario targets as internal helpers, not the documented entrypoint
- update CI to call exactly that one target
- upload cluster, pod, webhook, and APIService diagnostics from failed runs as
  CI artifacts

Done when:

- the same `task e2e` command runs locally and in CI, and a failed PR run
  leaves enough context to debug without rerunning anything

### 4. Startup Observability

First-boot behavior is currently hard to read. Operators need to see what trust
config loaded, what the proxy is listening on, and what the first few requests
did — without attaching a debugger.

- structured startup logs covering loaded `extension-apiserver-authentication`
  values, listening addresses, and the first inbound request
- Prometheus metrics for request counts, status codes, and webhook delivery
  outcomes
- a minimal example dashboard or PromQL set for the first-run case

Done when:

- a fresh install can be diagnosed from logs and `/metrics` alone

### 5. Multi-Pod Deployment Coverage

The proxy is deployed as a single replica in every existing test. Validate it
behaves correctly when replicated — no shared local state assumptions, no
audit duplication or loss.

- run an e2e lane with `replicas: 2+`
- verify audit events are neither duplicated nor dropped across pods
- document any constraint a multi-pod operator must know (sticky sessions,
  webhook receiver expectations, etc.)

## Near-Term Improvements

### Operator Examples

- add `test/e2e/values/proxy-existing-secret.yaml`
- add a cert-manager `ClusterIssuer` example values file
- add a production webhook mTLS example once a receiver-side test fixture
  exists

### Local E2E Recovery

- add `task e2e:cluster-reset` to stop port-forwards, delete `.stamps/`, and
  recreate the cluster
- document the shortest recovery path for stale Docker, k3d, Flux, and
  kubeconfig state

### Demo Preset

- decide whether a `demo.enabled` convenience preset is worth adding, or keep
  the explicit `values-demo.yaml` path as the only demo entrypoint

## Current Non-Goals

These are intentionally not on the near-term roadmap:

- duplicate suppression
- generalized audit-policy behavior inside the proxy
- production-grade retry or backpressure systems
- full kube-aggregator behavior parity
