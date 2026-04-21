# CI Pipeline

## Overview

The pipeline is defined in [workflows/ci.yml](workflows/ci.yml). It runs on every push to `main`, every pull request targeting `main`, and on version tags (`v*`).

## Job dependency graph

```
build-ci-container ──┬── validate-devcontainer
                     ├── lint ────────────────┐
                     ├── test ────────────────┤
                     ├── build ───────────────┤── docker-build ── e2e-smoke ── release (tag only)
                     └── lint-helm ───────────┘
```

## Design: build once, run everywhere

The CI container (`ghcr.io/configbutler/apiservice-audit-proxy-ci:<sha>`) is built from `.devcontainer/Dockerfile` (target `ci`) **once** in `build-ci-container`, then **pushed to GHCR** and referenced by all downstream jobs via the `container:` directive. This means:

- Each job pulls the pre-built image from the registry rather than rebuilding it.
- Steps in `lint`, `test`, `build`, and `lint-helm` run directly inside the container — no `docker run` wrapper needed.
- The SHA-tagged image name is passed between jobs as a job output (`needs.build-ci-container.outputs.image`).

GHA layer caching (`type=gha`) is also maintained alongside the registry push to speed up rebuilds when the Dockerfile changes.

## Jobs

| Job | Runs in CI container | What it does |
|-----|----------------------|--------------|
| `build-ci-container` | — | Builds and pushes the CI container to GHCR; validates all tools are present |
| `validate-devcontainer` | No (builds its own) | Builds the `dev` target and validates the full developer toolset including Flux, k3d, Tilt, and tab completion |
| `lint` | Yes | Runs `gofmt` check via `task fmt:check`, then `golangci-lint` via the official action (`install-mode: none` uses the pre-installed binary) |
| `test` | Yes | Runs unit tests via `task test` |
| `build` | Yes | Compiles the server binary via `task build` |
| `lint-helm` | Yes | Lints the Helm chart and builds the release bundle (`dist/`) via `task helm:lint` + `task dist`; uploads `dist/` as an artifact |
| `docker-build` | No | Builds the application container image (cache-only, not pushed) to validate the `Dockerfile` |
| `e2e-smoke` | No (host runner) | Installs kubectl, Helm, Task, Flux, and k3d on the runner, then runs a full k3d-based smoke test via `task e2e:test-smoke` |
| `release` | No | On `v*` tags only: pushes the application image to GHCR and publishes a GitHub release with the Helm chart and install manifest |

## Permissions

The workflow defaults to `contents: read`. Jobs that need broader access declare it explicitly at job level:

- `build-ci-container` — `packages: write` (push CI container to GHCR)
- `release` — `contents: write` + `packages: write` (create GitHub release, push application image)

## Caching

Four independent GHA cache scopes are used to avoid cross-contamination between image layers:

| Scope env var | Used by |
|---|---|
| `CI_CACHE_SCOPE` | CI container build layers |
| `DEV_CACHE_SCOPE` | Dev container build layers |
| `IMAGE_CACHE_SCOPE` | Application image build layers |
| `RELEASE_CACHE_SCOPE` | Release image build layers |

## Releasing

Push a `v`-prefixed tag to trigger the `release` job:

```bash
git tag v1.2.3
git push origin v1.2.3
```

The release job publishes:
- `ghcr.io/configbutler/apiservice-audit-proxy:<tag>` and `:latest`
- A GitHub release containing `dist/install.yaml`, the Helm chart `.tgz`, and `dist/checksums.txt`
