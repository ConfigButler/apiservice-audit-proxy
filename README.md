# APIService proxy prototype that allows you to get more complete ResponseComplete audit events

This directory now contains the first implementation slice for the pass-through aggregated API
server described in [PLAN.md](./PLAN.md).

## What It Does

- stands in front [of a real aggregated backend](https://kubernetes.io/docs/concepts/extend-kubernetes/api-extension/apiserver-aggregation/)
- proxies mutating resource requests to that backend
- captures delegated `X-Remote-*` identity
- builds one synthetic `audit.k8s.io/v1` `Event` at `stage: ResponseComplete`
- wraps that event in an `EventList`
- POSTs that `EventList` to a kubeconfig-configured audit webhook endpoint

This matches the event shape and webhook body shape described in [PLAN.md](./PLAN.md) and
motivated in [WHY.md](./WHY.md).

## Explicit Non-Goals

Two things should be stated plainly because this prototype cannot solve them correctly on its own:

- Duplicate suppression is out of scope. If kube-apiserver emits its own sparse aggregated audit
  event and this proxy emits a richer synthetic one, this prototype does not try to reconcile or
  deduplicate them.
- Delegated header trust is not solved here. The proxy can read `X-Remote-User`,
  `X-Remote-Group`, and `X-Remote-Extra-*`, but it cannot make those headers trustworthy by
  itself. That must come from deployment topology and TLS/authentication in front of the service.
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
- delegated `X-Remote-*` identity is still trusted from deployment topology / network path; the
  prototype does not yet verify an aggregator client certificate with `--client-ca-file`
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

Still deferred:

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

## Build Image

```bash
docker build -t audit-pass-through-apiserver:dev .
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
```

The release bundle generated by `task dist` lands in `dist/` and includes:

- `install.yaml`
- a packaged Helm chart
- `checksums.txt`

## Helm Chart

A simple chart now lives at `charts/audit-pass-through-apiserver`.

Quick example:

```bash
kubectl create namespace audit-pass-through-system

kubectl -n audit-pass-through-system create secret generic audit-pass-through-webhook-kubeconfig \
  --from-file=kubeconfig=/path/to/audit-webhook.kubeconfig

helm upgrade --install audit-pass-through-apiserver ./charts/audit-pass-through-apiserver \
  --namespace audit-pass-through-system \
  --set backend.url=https://sample-apiserver.wardle.svc:443
```

Important chart assumptions:

- `webhook.kubeconfigSecretName` must point at a Secret containing the audit webhook kubeconfig
- `backend.url` must point at the real aggregated backend
- enabling `tls.enabled` requires a Secret with `tls.crt` and `tls.key`
- backend CA and backend client certs are optional and are mounted from Secrets when configured

## CI And Devcontainer

This folder now also carries its own:

- `.github/workflows/ci.yml`
- `.github/workflows/release.yml`
- `.devcontainer/`

That keeps the prototype usable as a small standalone project before extraction into its own
repository.

## Tests

The small prototype testset lives in:

- `pkg/identity`: delegated header extraction
- `pkg/audit`: event construction and truncation behavior
- `pkg/proxy`: end-to-end proxying, best-effort webhook behavior, and hop-by-hop header stripping
