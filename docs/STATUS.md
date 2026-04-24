# Status: Demo Stack & Webhook-Tester Integration

Snapshot of where the demo/audit-gap work stands on 2026-04-24, what still
needs doing, and how to continue. Source plans:

- [docs/WORK-webhook-tester-unified-receiver.md](WORK-webhook-tester-unified-receiver.md)
- [docs/WORK-demo-stack.md](WORK-demo-stack.md)

---

## TL;DR

The webhook-tester / audit-gap path is the active path now. The Helm chart can
deploy webhook-tester, auto-generate the proxy webhook kubeconfig Secret, and
show the kube-apiserver-native lane beside the proxy lane. The e2e smoke and
audit-gap tests both deploy through `e2e:deploy-with-webhook-tester`.

The older demo-stack plan that proposed a custom SSE viewer in
`mock-audit-webhook`, plus `testApiserver` / `mockAuditWebhook` Helm
sub-deployments, is mostly superseded. Optional `testApiserver` chart support
is implemented, and the complete demo install now lives in
`charts/apiservice-audit-proxy/values-demo.yaml`. The in-repo
`mock-audit-webhook` binary and old kustomize path have been retired.

---

## What Is Done

### Webhook-Tester Unified Receiver

| Area | State | Evidence |
|---|---|---|
| webhook-tester Helm templates (Deployment / Service / Ingress) | Done | [webhook-tester-deployment.yaml](../charts/apiservice-audit-proxy/templates/webhook-tester-deployment.yaml), [webhook-tester-service.yaml](../charts/apiservice-audit-proxy/templates/webhook-tester-service.yaml), [webhook-tester-ingress.yaml](../charts/apiservice-audit-proxy/templates/webhook-tester-ingress.yaml) |
| Auto-generated webhook kubeconfig Secret (Lane B) | Done | [webhook-tester-kubeconfig-secret.yaml](../charts/apiservice-audit-proxy/templates/webhook-tester-kubeconfig-secret.yaml) writes the Secret named by `webhook.kubeconfigSecretName` when `webhookTester.enabled=true` |
| `webhookTester` values block + helpers | Done | [values.yaml](../charts/apiservice-audit-proxy/values.yaml), [_helpers.tpl](../charts/apiservice-audit-proxy/templates/_helpers.tpl) |
| Helm `NOTES.txt` shows Lane A and Lane B URLs side by side | Done | [NOTES.txt](../charts/apiservice-audit-proxy/templates/NOTES.txt) |
| kube-apiserver audit policy + webhook config | Done | [policy.yaml](../test/e2e/cluster/audit/policy.yaml), [webhook-config.yaml](../test/e2e/cluster/audit/webhook-config.yaml) |
| k3d startup can bake in the native audit webhook | Done | [start-cluster.sh](../test/e2e/cluster/start-cluster.sh) |
| Traefik websecure NodePort 30444 for Lane A | Done | [ingress.yaml](../test/e2e/setup/flux/releases/ingress.yaml) |
| `HOST_PROJECT_PATH` in devcontainer for Docker-outside-of-Docker path resolution | Done | [.devcontainer/devcontainer.json](../.devcontainer/devcontainer.json) |
| `TestAggregatedAPIAuditGap` e2e test | Done | [audit_gap_test.go](../test/e2e/audit_gap_test.go) asserts Lane A is hollow and Lane B is complete |
| `TestSmoke` reads events from webhook-tester | Done | [smoke_test.go](../test/e2e/smoke_test.go), [webhook_tester_test.go](../test/e2e/webhook_tester_test.go) |
| `e2e:deploy-with-webhook-tester`, `e2e:test-smoke`, and `e2e:test-audit-gap` Taskfile paths | Done | [Taskfile.e2e.yml](../Taskfile.e2e.yml) |
| Proxy pod restarts when the Helm-managed webhook kubeconfig changes | Done | [deployment.yaml](../charts/apiservice-audit-proxy/templates/deployment.yaml) includes `checksum/webhook-kubeconfig` when webhook-tester is enabled |
| `mock-audit-webhook` binary, manifests, script, Docker build arg, and Tilt resource removed | Done | [Dockerfile](../Dockerfile), [Taskfile.e2e.yml](../Taskfile.e2e.yml), [Tiltfile](../Tiltfile) |
| Optional `testApiserver` Helm deployment | Done | [test-apiserver-deployment.yaml](../charts/apiservice-audit-proxy/templates/test-apiserver-deployment.yaml), [test-apiserver-service.yaml](../charts/apiservice-audit-proxy/templates/test-apiserver-service.yaml), [test-apiserver-rbac.yaml](../charts/apiservice-audit-proxy/templates/test-apiserver-rbac.yaml), [test-apiserver-client-certs.yaml](../charts/apiservice-audit-proxy/templates/test-apiserver-client-certs.yaml) |
| Explicit full-demo values file | Done | [values-demo.yaml](../charts/apiservice-audit-proxy/values-demo.yaml) enables `testApiserver`, `webhookTester`, APIService registration, backend URL, and backend mTLS together |

### Devcontainer / Tooling

| Area | State | Evidence |
|---|---|---|
| `gh` CLI installed in the dev container image | Done | [.devcontainer/Dockerfile](../.devcontainer/Dockerfile) |
| `.env` auto-loaded into login shells (for example `GH_TOKEN`) | Done | `/etc/profile.d/workspace-dotenv.sh` baked into the image; warns interactively when missing |
| `post-start.sh` sources `.env` and warns if absent | Done | [.devcontainer/post-start.sh](../.devcontainer/post-start.sh) |

Devcontainer changes take effect on rebuild. The repo-root `.env` file is
ignored by the existing `*.env` rule.

---

## Plan Status

### From WORK-webhook-tester-unified-receiver.md

| Phase | Item | State |
|---|---|---|
| 3 | Retire `mock-audit-webhook` now that webhook-tester is the active receiver | Done |

The old binary, kustomize manifests, kubeconfig writer script, Docker `BINARY`
branch, and Tilt resource have been removed. Local e2e now uses the
chart-managed webhook-tester receiver.

### From WORK-demo-stack.md

| Phase | Item | State | Notes |
|---|---|---|---|
| 1a | `testApiserver` (sample-apiserver) as optional Helm sub-deployment | Done | Gated on `testApiserver.enabled`; complete demo config is in `values-demo.yaml` |
| 1b | `mockAuditWebhook` as optional Helm sub-deployment | Superseded | webhook-tester is the chosen receiver |
| 1 | `demo.enabled` convenience preset | Not started | Depends on deciding what a Helm-only demo should include |
| 2 | Add SSE stream + embedded HTML viewer to `mock-audit-webhook` | Superseded | webhook-tester now provides the live UI |
| 3 | Decision: extract `audit-webhook-receiver` to its own repo | Deferred | Revisit only if a schema-aware audit viewer becomes real work |

The side-by-side audit-gap demo from `WORK-demo-stack.md` is done through the
webhook-tester unified receiver work.

---

## Wishlist

Ordered roughly by smallest-impact-first.

### W1 - Optional `demo.enabled` Preset

`values-demo.yaml` is the preferred clear path for now. A future
`demo.enabled=true` preset is optional, but should not hide rewrites of
top-level values unless the chart has a very explicit preset mechanism.

---

## Local Commands

```bash
task fmt:check             # gofmt check
task lint                  # golangci-lint
task test                  # unit tests + coverage
task helm:lint             # chart lint
task helm:template         # render chart to stdout

task e2e:test-taskfile     # validates e2e Taskfile behavior; no cluster
task e2e:test-smoke        # smoke test through webhook-tester
task e2e:test-smoke-full   # smoke + image-refresh
task e2e:test-audit-gap    # side-by-side audit-gap assertion
task e2e:cluster-down      # tear down local k3d cluster
```
