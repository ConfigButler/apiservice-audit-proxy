# Follow-Up Plan

This file is meant to be used from inside the project devcontainer after the current standalone
bootstrap is in place.

## Primary Next Steps

1. Replace the shell smoke flow with a Go e2e package under `test/e2e/`.
   Run from the devcontainer with:
   ```bash
   task e2e:prepare
   ```

2. Add a second certificate lane that verifies backend server CA validation instead of relying on
   `backend.insecureSkipVerify=true`.
   Start from:
   - `test/e2e/setup/manifests/sample-apiserver/`
   - `test/e2e/values/proxy-cert-manager.yaml`

3. Add delegated requestheader trust validation with a real `--client-ca-file` implementation in
   the proxy.
   This is the biggest remaining security gap in the request identity story.

4. Extract the project into its own repository once image names, release names, and CI secrets are
   decided.
   Before cutting over, make sure these still work from the devcontainer:
   ```bash
   task ci
   task e2e:test-smoke
   ```

## Useful Repo-Local Improvements

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
