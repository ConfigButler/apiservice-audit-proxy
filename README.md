# APIService proxy prototype that allows you to get more complete ResponseComplete audit events

This directory contains a small pass-through aggregated API server.

## What It Does

- stands in front [of a real aggregated backend](https://kubernetes.io/docs/concepts/extend-kubernetes/api-extension/apiserver-aggregation/)
- proxies mutating resource requests to that backend
- captures delegated `X-Remote-*` identity
- builds one synthetic `audit.k8s.io/v1` `Event` at `stage: ResponseComplete`
- wraps that event in an `EventList`
- POSTs that `EventList` to a kubeconfig-configured audit webhook endpoint

See [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md) for the current design and
[WHY.md](./WHY.md) for the upstream rationale.

## Explicit Non-Goals

Two things should be stated plainly because this prototype cannot solve them correctly on its own:

- Duplicate suppression is out of scope. If kube-apiserver emits its own sparse aggregated audit
  event and this proxy emits a richer synthetic one, this prototype does not try to reconcile or
  deduplicate them.
- This does not attempt to fully reimplement kube-aggregator requestheader policy. The proxy can
  now verify the front-proxy client certificate with `--client-ca-file`, but it still does not
  reproduce every upstream requestheader configuration surface such as allowed client names.
- Does not replace https://github.com/kubernetes/kube-aggregator: the overall proxy work is kept as it, this is just one extra proxy to get completer audit events.

## Current Scope

- standalone Go module, intentionally isolated from the main operator binary
- one best-effort `audit.k8s.io/v1` `EventList` per supported mutating request
- early tests for identity extraction, event construction, and end-to-end handler flow
- Docker image build for quick cluster experiments

## Current Hardening

- audited request and response bodies are spooled to temp files instead of being kept fully in
  memory
- only the first `--max-audit-body-bytes` are captured into audit payload construction
- the audited proxy path strips common hop-by-hop headers before forwarding
- inbound HTTPS serving is available via `--tls-cert-file` and `--tls-private-key-file`
- backend HTTPS trust is explicit via `--backend-insecure-skip-verify` or `--backend-ca-file`
- backend mTLS is available via `--backend-client-cert-file` and `--backend-client-key-file`
- webhook delivery remains best-effort and non-blocking relative to the proxied API call

## Current Limitations

- this is not yet a full `k8s.io/apiserver` bootstrap
- the proxy can now require a verified front-proxy client certificate with `--client-ca-file`, but
  it still keeps the requestheader trust model intentionally small
- the prototype should currently be read as preserving the **effective delegated user** for
  downstream attribution; it should not yet promise perfect field-by-field preservation of the
  original auth-vs-impersonation split
- webhook delivery is best-effort and asynchronous, with logging only
- request and response body capture assumes JSON payloads
- truncation is coarse and size-based rather than semantic
- proxy behavior is still intentionally narrow; it is not trying to perfectly reproduce every edge
  case of kube-aggregator / `ReverseProxy`

## Identity Semantics

The most important identity property for this prototype is:

- can a downstream consumer attribute the change to the correct effective user?

At the moment, the answer is "yes" for the tested path:

- delegated requestheader identity is preserved well enough that an aggregated API request made with
  `kubectl --as=...` can still result in a Git commit attributed to that effective user

What the prototype does **not** guarantee yet is stronger than that:

- it does not promise that every synthetic audit event will contain a distinct
  `audit.Event.impersonatedUser` field matching upstream kube semantics
- in practice, the effective delegated identity may already appear as the resolved `user`
  attribution surface exposed to the proxy

So for now, the contract should be read as:

- correct effective user attribution: intended and tested
- exact upstream impersonation-field fidelity: still a follow-up question

## First Usable Container Args

For the first actually usable container image, this is the argument surface to plan around:

Implemented now:

- `--listen-address`
- `--backend-url`
- `--backend-insecure-skip-verify`
- `--backend-ca-file`
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

## Run Locally

```bash
go run ./cmd/server \
  --listen-address=:9445 \
  --backend-url=https://sample-apiserver.wardle.svc:443 \
  --backend-insecure-skip-verify \
  --backend-client-cert-file=/path/to/backend-client.crt \
  --backend-client-key-file=/path/to/backend-client.key \
  --webhook-kubeconfig=/path/to/audit-webhook.kubeconfig \
  --tls-cert-file=/path/to/tls.crt \
  --tls-private-key-file=/path/to/tls.key \
  --max-audit-body-bytes=1048576
```

## Build Images

```bash
docker build -t apiservice-audit-proxy:dev .
docker build --build-arg BINARY=mock-audit-webhook -t audit-pass-through-mock-audit-webhook:dev .
```

## Local Workflow

This folder now has its own project-local task entrypoints:

```bash
task fmt
task fmt:check
task lint
task test
task build
task helm:lint
task dist
task e2e:cluster-up
task e2e:flux-bootstrap
task e2e:prepare
task e2e:test-smoke
task e2e:prepare-backend-ca
task e2e:test-smoke-backend-ca
```

The release bundle generated by `task dist` lands in `dist/` and includes:

- a packaged Helm chart `.tgz`
- `checksums.txt`

## Helm Chart

A simple chart now lives at `charts/apiservice-audit-proxy` and now owns:

- the proxy `Deployment`, `Service`, and `ServiceAccount`
- optional `APIService` registration
- three serving certificate modes:
  - `existing-secret`
  - `dev-self-signed`
  - `cert-manager`

Quick example:

```bash
kubectl create namespace audit-pass-through-system

kubectl -n audit-pass-through-system create secret generic audit-pass-through-webhook-kubeconfig \
  --from-file=kubeconfig=/path/to/audit-webhook.kubeconfig

helm upgrade --install apiservice-audit-proxy ./charts/apiservice-audit-proxy \
  --namespace audit-pass-through-system \
  --set backend.url=https://sample-apiserver.wardle.svc:443
```

Important chart assumptions:

- `webhook.kubeconfigSecretName` must point at a Secret containing the audit webhook kubeconfig
- `backend.url` must point at the real aggregated backend
- `certificates.mode=existing-secret` requires `certificates.existingSecretName`
- `certificates.mode=dev-self-signed` is for local/dev use and pairs naturally with
  `apiService.insecureSkipTLSVerify=true`
- `certificates.mode=cert-manager` creates the serving `Certificate` and injects CA into the
  `APIService`
- backend CA and backend client certs are optional and are mounted from Secrets when configured
- `requestHeader.clientCASecretName` is optional and mounts the CA bundle used by
  `--client-ca-file` for delegated `X-Remote-*` trust
- if `apiService.enabled=true` and `certificates.mode=existing-secret`, set either
  `apiService.caBundle` or `apiService.insecureSkipTLSVerify=true`

## Standalone k3d Smoke Flow

This folder now carries its own small-cluster development loop.

What gets bootstrapped:

- a dedicated `k3d` cluster
- Flux
- cert-manager
- Traefik
- Reflector
- the upstream Wardle sample-apiserver backend
- a small in-repo mock audit webhook receiver
- the proxy chart installed with cert-manager-managed serving TLS

Run the full standalone smoke setup:

```bash
task e2e:test-smoke
```

Run the second smoke lane that validates the backend serving certificate with an explicit CA bundle:

```bash
task e2e:test-smoke-backend-ca
```

That flow verifies the path this project cares about most:

1. `APIService` becomes `Available`
2. a `Flunder` is created through the aggregated API path
3. the backend accepts the proxied request
4. the proxy emits a synthetic `EventList`
5. the stored event includes `objectRef.name`, `requestObject`, and `responseObject`
6. delegated `X-Remote-*` identity is accepted only when the front-proxy client certificate chains
   to the configured requestheader CA bundle

If you want the environment without rerunning the smoke assertions:

```bash
task e2e:prepare
```

To tear the local cluster down:

```bash
task e2e:cluster-down
```

## Tilt

A thin `Tiltfile` now lives at the project root.

Use it when you want the cluster and Flux stack once, then quick proxy rebuild/reload loops:

```bash
tilt up
```

The Tilt resources are intentionally small:

- `e2e-prepare`
- `proxy-update`
- `mock-webhook-update`
- `smoke-test`

## CI And Devcontainer

This folder now also carries its own:

- `.github/workflows/ci.yml`
- `.devcontainer/`

The workflow now has three layers:

- devcontainer/tool validation
- unit/lint/build/chart validation
- a standalone `k3d` smoke job

That keeps the prototype closer to what a real extracted repository needs.

## Tests

The small prototype testset lives in:

- `pkg/identity`: delegated header extraction
- `pkg/audit`: event construction and truncation behavior
- `pkg/proxy`: end-to-end proxying, best-effort webhook behavior, and hop-by-hop header stripping
- `cmd/mock-audit-webhook`: in-memory receiver behavior used by the local smoke flow

## Docs

Current living docs:

- [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md)
- [docs/TODO.md](./docs/TODO.md)
- [WHY.md](./WHY.md)
