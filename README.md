# apiservice-audit-proxy

`apiservice-audit-proxy` is a Go pass-through aggregated API server for
Kubernetes. It sits in front of a real aggregated backend and emits synthetic
`audit.k8s.io/v1` events for mutating requests.

The goal is simple: recover the audit fields that aggregated API requests often
lose, especially `objectRef`, `requestObject`, and `responseObject`.

## Highlights

- proxies mutating aggregated API requests to a real backend
- preserves delegated `X-Remote-*` identity for downstream attribution
- emits one best-effort `ResponseComplete` audit event per request to a
  configured webhook
- supports serving TLS, backend TLS validation, backend mTLS, and optional
  front-proxy client CA verification
- ships with a Helm chart and local k3d-based e2e smoke tests

## Development

This repo uses [`task`](https://taskfile.dev) for all common workflows:

```bash
task --list
task fmt
task lint
task test
task build
task helm:lint
task e2e:test-smoke
```

## Docs

- [Architecture](docs/ARCHITECTURE.md)
- [Why this exists](WHY.md)
- [E2E setup notes](docs/E2E_SETUP_LESSONS.md)
- [Helm chart values](charts/apiservice-audit-proxy/values.yaml)
