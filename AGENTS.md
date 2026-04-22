# Agent Instructions

This file tells AI coding agents how to work correctly in this repository.
Read it before making any changes.

## Project Overview

`apiservice-audit-proxy` is a Go pass-through aggregated API server that sits
in front of a real Kubernetes aggregated backend and emits synthetic
`audit.k8s.io/v1` events for mutating requests. See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)
for a full description and component diagram.

Module: `github.com/ConfigButler/apiservice-audit-proxy`

## Task Runner

All development tasks use [Task](https://taskfile.dev). Never invoke `go`,
`golangci-lint`, `helm`, or `docker` directly when an equivalent `task` target
exists — the Taskfile sets the correct flags and environment.

```bash
task --list          # see all available targets
```

## Essential Commands

### Code quality (run before every commit)

```bash
task fmt             # auto-format Go code with gofmt
task fmt:check       # verify formatting without modifying (used in CI)
task lint            # run golangci-lint (config: .golangci.yml)
task lint-fix        # run golangci-lint with --fix
task test            # unit tests with coverage (produces coverage.out)
```

### Build

```bash
task build           # compile the server binary → bin/apiservice-audit-proxy
task docker-build    # build the container image (local tag)
```

### Helm chart

```bash
task helm:lint       # lint charts/apiservice-audit-proxy
task helm:template   # render chart to stdout (useful for review)
task helm:package    # package chart into dist/
task dist            # build all release artefacts in dist/
```

### Full CI equivalent (runs all of the above)

```bash
task ci              # fmt:check → lint → test → build → helm:lint → dist
```

## Local E2E Tests

The e2e suite requires a running k3d cluster. Tasks are composable — each
higher-level task calls its dependencies automatically.

### Prerequisites

```bash
task e2e:doctor      # verify docker, k3d, flux, helm, kubectl are available
```

### Common flows

```bash
# Full smoke test from scratch (creates cluster if needed, deploys everything,
# runs the Go test suite)
task e2e:test-smoke

# Same but with explicit backend CA validation enabled
task e2e:test-smoke-backend-ca

# Bring the cluster up without running tests
task e2e:cluster-up

# Tear the cluster down
task e2e:cluster-down

# Rebuild and reload images without rerunning tests
task e2e:load-images

# Re-deploy the proxy only (faster than a full smoke run when iterating on
# chart or proxy code changes)
task e2e:deploy-proxy
```

### How the e2e stack is layered

```
e2e:test-smoke
 └─ e2e:prepare
     ├─ e2e:load-images
     │   ├─ e2e:build-image          (builds apiservice-audit-proxy:e2e-local)
     │   └─ e2e:build-mock-webhook-image
     └─ e2e:deploy-proxy
         ├─ e2e:prepare-requestheader-client-ca
         │   └─ e2e:deploy-backend
         │       └─ e2e:flux-bootstrap
         │           └─ e2e:cluster-up
         └─ e2e:prepare-webhook-kubeconfig
             └─ e2e:deploy-mock-webhook
```

### What `e2e:flux-bootstrap` installs

The Flux bootstrap step installs the following into the cluster, then waits for
all resources to become Ready before continuing:

| Component | Purpose |
|---|---|
| cert-manager | Issues TLS certificates for the proxy serving endpoint |
| traefik | Ingress controller (also provides ServiceMonitor targets for Prometheus) |
| reflector | Mirrors Secrets and ConfigMaps across namespaces |
| prometheus-operator | Installs `monitoring.coreos.com/v1` CRDs and the operator |

After the Flux resources are ready, the bootstrap step waits for the Prometheus
CRDs to be established and then applies `test/e2e/setup/manifests/`, which
creates the Prometheus instance, RBAC, and the traefik ServiceMonitor.

### Cluster context

The default cluster is `k3d-audit-pass-through-e2e` (kubectl context name).
All `kubectl` and `helm` calls in tasks use `--context {{.CTX}}` so they
target only this cluster even when other clusters are present.

### Running two clusters concurrently

If `gitops-reverser` is open in the same devcontainer, both clusters share the
same Docker daemon and host kernel. The `start-cluster.sh` script automatically
bumps `fs.inotify.max_user_instances` to 512 before creating the cluster.
See [test/e2e/cluster/README.md](test/e2e/cluster/README.md) for details.

## Code Conventions

- **Go version**: see `go.mod` (`go 1.26.2`)
- **No generated files** in version control; `controller-gen` outputs are
  committed but regenerated with `controller-gen` when CRD types change
- **Imports**: grouped as stdlib / external / internal, formatted with
  `goimports` — run `task fmt` to apply
- **Error handling**: always wrap errors with context (`fmt.Errorf("... : %w",
  err)`); never discard errors silently
- **Comments**: only add a comment when the *why* is non-obvious; do not
  describe what the code does

## Linting

Config is in `.golangci.yml`. Key linters enabled:

- `errcheck` — no unchecked errors
- `govet` — standard vet checks
- `staticcheck` — SA* and S* checks
- `gocritic` — code quality
- `gosec` — security anti-patterns
- `revive` — style
- `goimports` / `gofmt` — formatting

Run `task lint` to check, `task lint-fix` to auto-fix where possible.

## Helm Chart

The chart lives in `charts/apiservice-audit-proxy/`. Key notes:

- Integer values passed as CLI arguments must use `| int` in templates to
  prevent Helm rendering them as scientific notation (e.g. `1.048576e+06`).
- TLS is mandatory; the chart supports three modes: `cert-manager`,
  `dev-self-signed`, and `existing-secret`.
- The `APIService` resource is only created when `apiService.enabled: true`.

## Commit and PR Hygiene

- Run `task ci` and confirm it passes before opening a PR
- For e2e changes, run `task e2e:test-smoke` and confirm `--- PASS: TestSmoke`
- Keep commits focused; reference the issue or context in the commit body
- Do not commit `.stamps/` directories (they are gitignored build artefacts)

## Useful Reference

| File | Purpose |
|---|---|
| [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) | Component diagram, request flow, trust model |
| [docs/E2E_SETUP_LESSONS.md](docs/E2E_SETUP_LESSONS.md) | Detailed write-up of every e2e issue ever encountered and why |
| [test/e2e/cluster/README.md](test/e2e/cluster/README.md) | inotify limits and two-cluster DooD behaviour |
| [Taskfile.yml](Taskfile.yml) | All non-e2e tasks |
| [Taskfile.e2e.yml](Taskfile.e2e.yml) | All e2e tasks and variables |
| [.golangci.yml](.golangci.yml) | Linter configuration |
| [charts/apiservice-audit-proxy/values.yaml](charts/apiservice-audit-proxy/values.yaml) | Helm chart defaults |
| [test/e2e/setup/flux/](test/e2e/setup/flux/) | Flux resources applied during bootstrap |
| [test/e2e/setup/manifests/](test/e2e/setup/manifests/) | Prometheus instance and ServiceMonitors |
