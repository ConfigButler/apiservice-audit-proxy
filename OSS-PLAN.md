# Audit Pass-Through APIServer OSS Packaging Plan

## Goal

Take the existing `audit-pass-through-apiserver` prototype in this folder and make it ready to be
published as a small standalone open source project with the right operational basics:

- local developer workflow
- linting and tests in CI
- container image build and release
- simple Helm chart packaging
- deployable release artifacts
- a repo-local devcontainer

This plan borrows the strongest packaging and contributor-experience ideas from the main
`gitops-reverser` repository without importing its full operational complexity.

## Fit With This Folder

This plan fits the current contents of this folder well.

What is already here and should be preserved:

- standalone Go module in `go.mod`
- one binary entrypoint in `cmd/server`
- focused runtime packages in `pkg/audit`, `pkg/identity`, `pkg/proxy`, and `pkg/webhook`
- unit and handler-flow tests already present
- a minimal `Dockerfile`
- architecture and scope docs in `PLAN.md`, `WHY.md`, `TODO.md`, and `README.md`

What is intentionally missing today, and is the focus of this plan:

- `Taskfile.yml`
- project-local `.golangci.yml`
- `.github/workflows/`
- Helm chart
- release artifact generation
- `.devcontainer/`

## Relationship To Existing Docs

Keep the existing docs with their current responsibilities:

- `PLAN.md`: runtime architecture and behavior scope
- `WHY.md`: technical rationale and upstream behavior analysis
- `TODO.md`: remaining prototype/runtime implementation follow-ups
- `README.md`: user-facing overview

Add this plan as the packaging and publishing companion:

- `OSS-PLAN.md`: repo/release/tooling work needed to publish and maintain the project cleanly

## Desired End State

The project in this folder should support the following workflow:

1. open the repo in a devcontainer
2. run `task lint`, `task test`, `task build`, and `task helm:*`
3. push a branch and get CI feedback
4. tag a release and automatically publish:
   - a GHCR image
   - a packaged Helm chart
   - a rendered install manifest

## Workstreams

### 1. Quality baseline

Add a repo-local `.golangci.yml` based on the main project’s chosen quality bar, but scaled to this
smaller codebase.

Source to borrow from:

- [`external-sources/kubernetes/hack/golangci.yaml`](/home/simon/git/gitops-reverser/external-sources/kubernetes/hack/golangci.yaml:1)

Approach:

- derive a smaller config instead of copying the Kubernetes file verbatim
- keep checks that are useful for a fresh standalone Go repo
- remove Kubernetes-tree-specific exclusions and path rules

Recommended checks:

- `govet`
- `staticcheck`
- `revive`
- `errcheck`
- `ineffassign`
- `unused`
- import/format enforcement

Recommended local quality commands:

- `go test ./... -coverprofile=coverage.out`
- `golangci-lint run`

### 2. Task runner

Add a small `Taskfile.yml` modeled after the main project’s style, but with a much simpler task
graph.

Recommended tasks:

- `task help`
- `task fmt`
- `task lint`
- `task lint-fix`
- `task test`
- `task build`
- `task docker-build`
- `task helm:lint`
- `task helm:template`
- `task helm:package`
- `task dist`
- `task ci`

Recommended behavior:

- `task ci` should match the PR CI checks closely
- `task dist` should assemble a release bundle under `dist/`
- task names should stay explicit and unsurprising

Recommended `dist/` outputs:

- packaged Helm chart
- rendered install manifest
- optional checksums if cheap to add

### 3. Helm chart

Add a simple chart that mirrors the main project’s chart conventions where helpful, without pulling
in HA or certificate-management complexity that this prototype does not need yet.

Recommended chart path:

- `charts/audit-pass-through-apiserver`

Recommended chart contents:

- `Chart.yaml`
- `values.yaml`
- `templates/_helpers.tpl`
- `templates/serviceaccount.yaml`
- `templates/deployment.yaml`
- `templates/service.yaml`
- `templates/NOTES.txt`

Recommended value surface:

- image repository/tag/pull policy
- service account settings
- replica count
- service port
- flags for:
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
- secret mounts for serving TLS and webhook kubeconfig

Deliberate v1 simplifications:

- no cert-manager integration
- no ServiceMonitor unless metrics become real and stable
- default `replicaCount: 1`
- no quickstart/demo manifests in the chart itself

### 4. CI pipeline

Add a simplified GitHub Actions setup inspired by the main project CI.

Recommended workflows:

- `.github/workflows/ci.yml`
- `.github/workflows/release.yml`

Recommended `ci.yml` jobs:

- lint
- unit test
- docker build validation
- helm lint/template

Recommended `release.yml` behavior:

- trigger on tags like `v*`
- build and push image to GHCR
- package the Helm chart
- render `dist/install.yaml`
- attach release artifacts to GitHub Releases

Recommended release artifacts:

- `ghcr.io/<org>/audit-pass-through-apiserver:<tag>`
- `audit-pass-through-apiserver-<version>.tgz`
- `install.yaml`

Keep the first version boring:

- GitHub Releases + GHCR is enough
- OCI chart publishing can be added later if it becomes worthwhile

### 5. Devcontainer

Add a trimmed `.devcontainer/` derived from the main project’s setup.

Keep:

- Go toolchain
- `golangci-lint`
- `helm`
- `task`
- Docker CLI
- Go module and build caches
- VS Code Go tooling

Drop initially:

- `k3d`
- `flux`
- `kubebuilder`
- extra forwarded ports not used by this project

Recommended files:

- `.devcontainer/devcontainer.json`
- `.devcontainer/Dockerfile`
- `.devcontainer/post-create.sh`

Success criteria:

- a contributor can open the folder and immediately run lint/test/build/chart tasks

### 6. Documentation refresh

Update the user-facing documentation so the folder reads like a small standalone project, not only
an internal spike.

README additions:

- installation and local run
- image build/run
- Helm install example
- release artifact overview
- current trust model and limitations
- explicit non-goals

Important:

- do not collapse `PLAN.md`, `WHY.md`, and `TODO.md` into the README
- let the README stay short and onboarding-focused

## Suggested Implementation Order

1. Add `.golangci.yml`.
2. Add `Taskfile.yml`.
3. Make `task lint`, `task test`, and `task build` pass locally.
4. Add the Helm chart and `task helm:*`.
5. Add `task dist`.
6. Add the devcontainer.
7. Add `ci.yml`.
8. Add `release.yml`.
9. Refresh the README to reference the new workflow.

## Definition Of Done

This packaging pass is complete when:

- `task lint` passes
- `task test` passes
- `task build` passes
- `task helm:lint` passes
- `task helm:template` passes
- CI runs those checks automatically
- tag-based release automation can publish image and deployment artifacts
- the folder can be opened and used directly via a repo-local devcontainer

## Explicit Non-Goals For This Pass

Do not turn this packaging pass into runtime feature expansion.

Out of scope here:

- full `k8s.io/apiserver` bootstrap migration
- production-grade retry/backpressure
- solving delegated header trust beyond current documented assumptions
- cert-manager integration
- large e2e cluster automation
- broadening supported API behavior beyond current prototype scope

## First Follow-Up After This Plan

The right first implementation step is plumbing, not new runtime behavior:

- add the quality baseline
- add the task runner
- add the Helm chart
- add CI and release packaging
- add the devcontainer

Once that foundation exists, the project will be much easier to publish, iterate on, and accept
outside contributions for.
