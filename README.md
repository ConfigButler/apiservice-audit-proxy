# apiservice-audit-proxy

`apiservice-audit-proxy` is a Go pass-through aggregated API server for
Kubernetes. It sits in front of a real aggregated backend and emits synthetic
`audit.k8s.io/v1` events for mutating requests.

The goal is simple: recover the audit fields that aggregated API requests often
lose, especially `objectRef`, `requestObject`, and `responseObject`.

It is intentionally a simple implementation, scoped to the gap needed for
`gitops-reverser` to work with `APIService`-backed aggregated APIs.

## Highlights

- proxies mutating aggregated API requests to a real backend
- preserves delegated `X-Remote-*` identity for downstream attribution
- emits one best-effort `ResponseComplete` audit event per request to a
  configured webhook
- supports serving TLS, backend TLS validation, backend mTLS, and optional
  front-proxy client CA verification
- ships with a Helm chart, optional demo components, and local k3d-based e2e
  smoke tests

## Limitations

- intentionally narrow in scope; this is not a full `k8s.io/apiserver` or
  `kube-aggregator` replacement
- focused on mutating aggregated API requests and the audit data needed by
  `gitops-reverser`
- webhook delivery is best-effort and does not fail the proxied API request

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

To open the local webhook-tester UI used by the e2e demo path:

```bash
task e2e:portforward-webhook-tester
```

It reuses an existing healthy forward when possible. Then open:

- proxy audit events: <http://localhost:18090/s/aabbccdd-0000-4000-0000-000000000002>
- kube-apiserver audit events: <http://localhost:18090/s/aabbccdd-0000-4000-0000-000000000001>

Stop it with `task e2e:portforward-stop`.

## Demo Chart Path

The chart keeps production defaults in `values.yaml` and puts the coordinated
demo choices in `values-demo.yaml`:

```bash
helm template apiservice-audit-proxy charts/apiservice-audit-proxy \
  --namespace wardle \
  --values charts/apiservice-audit-proxy/values-demo.yaml
```

`values-demo.yaml` explicitly enables the Wardle sample-apiserver backend, the
matching APIService, and webhook-tester. The templates do not silently rewrite
top-level proxy settings when demo components are enabled.

## Docs

- [Architecture](docs/ARCHITECTURE.md)
- [Why this exists](docs/WHY.md)
- [E2E setup notes](docs/E2E_SETUP_LESSONS.md)
- [Helm chart values](charts/apiservice-audit-proxy/values.yaml)
