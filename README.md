# Audit Pass-Through APIServer Prototype

This directory now contains the first implementation slice for the pass-through aggregated API
server described in [PLAN.md](./PLAN.md).

## What It Does

- stands in front of a real aggregated backend
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
- webhook delivery remains best-effort and non-blocking relative to the proxied API call

## Current Limitations

- this is not yet a full `k8s.io/apiserver` bootstrap
- inbound TLS and caller authentication are not implemented yet
- webhook delivery is best-effort and asynchronous, with logging only
- request and response body capture assumes JSON payloads
- truncation is coarse and size-based rather than semantic
- proxy behavior is still intentionally narrow; it is not trying to perfectly reproduce every edge
  case of kube-aggregator / `ReverseProxy`

## First Usable Container Args

For the first actually usable container image, this is the argument surface to plan around:

Implemented now:

- `--listen-address`
- `--backend-url`
- `--webhook-kubeconfig`
- `--webhook-timeout`
- `--max-audit-body-bytes`
- `--capture-temp-dir`

Very likely next before real cluster use:

- `--tls-cert-file`
- `--tls-private-key-file`
- `--client-ca-file`
- `--backend-server-name` or equivalent backend TLS override

## Run Locally

```bash
go run ./cmd/server \
  --listen-address=:9445 \
  --backend-url=https://sample-apiserver.wardle.svc:443 \
  --webhook-kubeconfig=/path/to/audit-webhook.kubeconfig \
  --max-audit-body-bytes=1048576
```

## Build Image

```bash
docker build -t audit-pass-through-apiserver:dev .
```

## Tests

The small prototype testset lives in:

- `pkg/identity`: delegated header extraction
- `pkg/audit`: event construction and truncation behavior
- `pkg/proxy`: end-to-end proxying, best-effort webhook behavior, and hop-by-hop header stripping
