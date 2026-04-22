# CI Pipeline

## Overview

The pipeline is defined in [workflows/ci.yml](workflows/ci.yml). It runs on every push to `main` and every pull request targeting `main`.

## Job dependency graph

```
build-ci-container ──┬── validate-devcontainer
                     ├── lint ─────────────────────────────────┐
                     ├── test ─────────────────────────────────┤
                     ├── build ────────────────────────────────┤── e2e-smoke ──┐
                     ├── lint-helm ───────────────────────────┤               │
                     └── docker-build ───────────────────────┘               │
                                                                   release-please (main only)
                                                                        ├── publish (amd64) ──┐
                                                                        ├── publish (arm64) ──┤── publish-manifest
                                                                        └── publish-helm
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
| `lint-helm` | Yes | Lints the Helm chart and packages it (`dist/`) via `task helm:lint` + `task dist`; uploads `dist/` as an artifact |
| `docker-build` | No | Builds the application container image in parallel with lint/test (cache-only, not pushed) to validate the `Dockerfile` independently |
| `e2e-smoke` | No (host runner) | Installs kubectl, Helm, Task, Flux, and k3d on the runner, then runs a full k3d-based smoke test via `task e2e:test-smoke`; gates on all quality jobs |
| `release-please` | No | On `main` push only: runs `googleapis/release-please-action` to maintain a release PR; merging that PR creates a Git tag and GitHub release |
| `publish` (matrix) | No | When a release is created: builds each platform image (`linux/amd64` on ubuntu-latest, `linux/arm64` on ubuntu-24.04-arm) and uploads its digest |
| `publish-manifest` | No | Merges the per-platform digests into a multi-arch manifest list; appends installation instructions and platform list to the GitHub release body |
| `publish-helm` | Yes (CI container) | When a release is created: pushes the Helm chart as an OCI artifact to `ghcr.io/configbutler/charts`; uploads the chart `.tgz` and `checksums.txt` as GitHub release assets |

## Permissions

The workflow sets `contents: write`, `pull-requests: write`, and `packages: write` at the top level. These are all required by release-please (PR creation, tag pushing) and the publish jobs (GHCR push).

## Caching

Four independent GHA cache scopes are used to avoid cross-contamination between image layers:

| Scope env var / value | Used by |
|---|---|
| `CI_CACHE_SCOPE` | CI container build layers |
| `DEV_CACHE_SCOPE` | Dev container build layers |
| `IMAGE_CACHE_SCOPE` | Application image build layers (docker-build pre-flight) |
| `build-linux/amd64` | Release amd64 image layers (publish matrix) |
| `build-linux/arm64` | Release arm64 image layers (publish matrix) |

## Releasing

Releases are fully automated via [release-please](https://github.com/googleapis/release-please). The flow is:

1. Merge conventional commits (`feat:`, `fix:`, etc.) to `main`.
2. `release-please` opens or updates a "chore: release X.Y.Z" PR with a generated `CHANGELOG.md` and bumped versions in `Chart.yaml` and `values.yaml`.
3. Merge the release PR.
4. `release-please` creates the Git tag and GitHub release; `publish` and `publish-helm` fire automatically.

The release publishes:
- `ghcr.io/configbutler/apiservice-audit-proxy:<version>` and `:latest` (multi-arch Docker image: linux/amd64, linux/arm64)
- `oci://ghcr.io/configbutler/charts/apiservice-audit-proxy:<version>` (Helm chart OCI artifact)
- A GitHub release with the chart `.tgz` and `checksums.txt` attached
