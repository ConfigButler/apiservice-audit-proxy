# Follow-Up Plan

This file is meant to be used from inside the project devcontainer after the current standalone
bootstrap is in place.

## Primary Next Steps

1. Keep the Go e2e package under `test/e2e/` as the source of truth for smoke coverage.
   The repo now runs smoke assertions from:
   ```bash
   task e2e:test-smoke
   ```

2. Keep the second certificate lane that verifies backend server CA validation instead of relying
   on `backend.insecureSkipVerify=true`.
   The repo now carries that path in:
   - `test/e2e/setup/manifests/sample-apiserver-backend-ca/`
   - `test/e2e/values/proxy-cert-manager-backend-ca.yaml`
   and runs it with:
   ```bash
   task e2e:test-smoke-backend-ca
   ```

3. Keep delegated requestheader trust validation wired through the proxy with
   `--client-ca-file`.
   The remaining work here is narrower now: document the trust model clearly and decide whether the
   project ever needs additional upstream-style requestheader policy such as allowed client names.

4. Extract the project into its own repository once image names, release names, and CI secrets are
   decided.
   Before cutting over, make sure these still work from the devcontainer:
   ```bash
   task ci
   task e2e:test-smoke
   ```

## Useful Repo-Local Improvements

- Add a small doc note for local e2e environments where `k3d` bootstrap or kubeconfig merge is
  flaky, so the recovery path is explicit for contributors.
- Add `task e2e:test-smoke-dev-cert` for the fast `dev-self-signed` lane.
- Add `task e2e:cluster-reset` to remove `.stamps/` and recreate the cluster from scratch.
- Add a `test/e2e/values/proxy-existing-secret.yaml` example for advanced operators.
- Persist mock webhook payloads to a file or PVC so failed CI runs are easier to inspect.
- Add Helm tests or chart template assertions for the three certificate modes.

## Production-Oriented Follow-Ups

- Support `ClusterIssuer`-driven serving certs without requiring a namespaced self-signed issuer.
- Document CA ownership and rotation boundaries for:
  - proxy serving TLS
  - backend client certs
  - webhook transport credentials
- Publish the chart to OCI and align release tags with the container image tags.
- Add SBOM and image signing to the release workflow.

## Extraction Checklist

- Rename workflow badges and GHCR paths in `README.md` and `charts/.../Chart.yaml`.
- Replace any remaining references to the parent repo layout.
- Keep `.devcontainer/`, `Taskfile.yml`, `Taskfile.e2e.yml`, `Tiltfile`, `hack/e2e/`, and
  `test/e2e/` together so the standalone dev loop stays intact.
