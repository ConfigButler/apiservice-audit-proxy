# TODO

This file tracks the remaining work that still matters for the current project
state. Historical packaging and implementation plans have been intentionally
collapsed into this shorter list.

## Highest Priority

### 1. Stabilize Local E2E Bootstrap

The repo now has Go-based smoke coverage, but the local k3d/bootstrap path
still needs hardening in fresh environments.

Current focus:

- make `task e2e:test-smoke` reliable from a clean devcontainer
- reduce k3d/bootstrap flakiness around kubeconfig and cluster startup
- document known recovery steps when local Docker or k3d state is messy

Done when:

- a contributor can clone the repo, open the devcontainer, and run the smoke
  lane without manual cluster repair

### 2. Extract To A Standalone Repository

The codebase is now structurally ready for extraction, but the standalone-repo
cutover still needs to happen.

Remaining work:

- decide final image names and release names
- move the project into its own repository
- update README badges, GHCR paths, and chart metadata
- keep `.devcontainer/`, `Taskfile.yml`, `Taskfile.e2e.yml`, `Tiltfile`,
  `hack/e2e/`, and `test/e2e/` together

Done when:

- the project lives in its own repo and the local workflow still works

### 3. Decide E2E CI Posture

The repo already has smoke lanes locally. The next decision is how strongly
they gate changes.

Options still to settle:

- run at least one k3d smoke scenario on every PR
- run it on protected branches only
- keep manual dispatch as a fallback if runtime is too expensive

Done when:

- the intended CI gating model is documented and implemented

## Near-Term Improvements

### Fast Dev Lane

- add `task e2e:test-smoke-dev-cert` for the fast `dev-self-signed` path

### Cluster Reset

- add `task e2e:cluster-reset` to remove `.stamps/` and recreate the cluster

### Operator Examples

- add `test/e2e/values/proxy-existing-secret.yaml`
- keep chart examples for the supported certificate modes easy to discover

### Better Failure Inspection

- persist mock webhook payloads to a file or PVC so failed runs are easier to
  inspect

### Chart Assertions

- add Helm tests or chart template assertions for the supported certificate
  modes

## Trust And TLS Documentation

The project needs a shorter operator-facing description of certificate
ownership and rotation boundaries.

Document clearly:

- proxy serving TLS
- backend server trust
- backend client certificates
- webhook transport credentials
- delegated requestheader trust boundaries

Also clarify whether the project ever needs more upstream-like requestheader
policy, such as allowed client names.

## Certificate UX Follow-Ups

- support `ClusterIssuer`-driven serving certificates without requiring a
  namespaced self-signed issuer
- keep the supported paths clear:
  - local/dev
  - cert-manager managed
  - existing secret

## Release Follow-Ups

- publish the chart to OCI
- align release tags with image tags
- add SBOM generation
- add image signing

## Current Non-Goals

These are intentionally not on the near-term roadmap:

- duplicate suppression
- generalized audit-policy behavior inside the proxy
- production-grade retry or backpressure systems
- full kube-aggregator behavior parity
