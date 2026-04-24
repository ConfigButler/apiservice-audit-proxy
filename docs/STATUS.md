# Status: Demo Stack & Webhook-Tester Integration

Snapshot of where the two WORK plans stand on 2026-04-24, what still needs
doing, and how to do it. Source plans:

- [docs/WORK-webhook-tester-unified-receiver.md](WORK-webhook-tester-unified-receiver.md)
- [docs/WORK-demo-stack.md](WORK-demo-stack.md)

---

## TL;DR

The webhook-tester / "audit gap" path **is built and validated locally** —
Helm chart, audit policy, side-by-side `TestAggregatedAPIAuditGap`, the
auto-generated kubeconfig Secret, and Helm `NOTES.txt` are all in place. The
older "WORK-demo-stack" plan that proposed a custom SSE viewer in
`mock-audit-webhook` plus a `testApiserver` / `mockAuditWebhook` Helm sub-chart
is **largely superseded** by the webhook-tester approach and is not done.

CI was failing for an unrelated reason (an unused Go import added by commit
[`18462dc`](https://github.com/ConfigButler/apiservice-audit-proxy/commit/18462dc)).
That has been fixed — see "CI status" below.

---

## What is done

### From WORK-webhook-tester-unified-receiver.md

| Area | State | Evidence |
|---|---|---|
| webhook-tester Helm templates (Deployment / Service / Ingress) | ✅ | [charts/.../webhook-tester-deployment.yaml](../charts/apiservice-audit-proxy/templates/webhook-tester-deployment.yaml), [-service.yaml](../charts/apiservice-audit-proxy/templates/webhook-tester-service.yaml), [-ingress.yaml](../charts/apiservice-audit-proxy/templates/webhook-tester-ingress.yaml) |
| Auto-generated webhook kubeconfig Secret (Lane B) | ✅ | [charts/.../webhook-tester-kubeconfig-secret.yaml](../charts/apiservice-audit-proxy/templates/webhook-tester-kubeconfig-secret.yaml) — `webhookTester.enabled=true` writes the Secret named by `webhook.kubeconfigSecretName` |
| `webhookTester` values block + helpers | ✅ | [values.yaml](../charts/apiservice-audit-proxy/values.yaml), [_helpers.tpl](../charts/apiservice-audit-proxy/templates/_helpers.tpl) |
| Helm `NOTES.txt` shows Lane A and Lane B URLs side-by-side | ✅ | [NOTES.txt](../charts/apiservice-audit-proxy/templates/NOTES.txt) |
| kube-apiserver audit policy + webhook config | ✅ | [test/e2e/cluster/audit/policy.yaml](../test/e2e/cluster/audit/policy.yaml), [webhook-config.yaml](../test/e2e/cluster/audit/webhook-config.yaml) |
| `start-cluster.sh` baked-in audit webhook (conditional, with Docker-outside-of-Docker support) | ✅ | [test/e2e/cluster/start-cluster.sh](../test/e2e/cluster/start-cluster.sh) — `audit_files_present` + `resolve_host_project_path` + `ensure_k3d_stat_compat_path` |
| Traefik websecure NodePort 30444 | ✅ | [test/e2e/setup/flux/releases/ingress.yaml](../test/e2e/setup/flux/releases/ingress.yaml) |
| `HOST_PROJECT_PATH` in devcontainer | ✅ | [.devcontainer/devcontainer.json](../.devcontainer/devcontainer.json) `containerEnv` |
| `TestAggregatedAPIAuditGap` e2e test | ✅ | [test/e2e/audit_gap_test.go](../test/e2e/audit_gap_test.go) — asserts Lane A is hollow, Lane B is complete |
| `e2e:deploy-with-webhook-tester` + `e2e:test-audit-gap` Taskfile targets | ✅ | [Taskfile.e2e.yml](../Taskfile.e2e.yml) |

### Devcontainer / tooling (added in this pass)

| Area | State | Evidence |
|---|---|---|
| `gh` CLI installed in CI/dev container | ✅ | [.devcontainer/Dockerfile](../.devcontainer/Dockerfile) — installed alongside `docker-ce-cli` |
| `.env` auto-loaded into login shells (e.g. `GH_TOKEN`) | ✅ | `/etc/profile.d/workspace-dotenv.sh` baked into image; warns interactively when missing |
| `post-start.sh` sources `.env` and warns if absent | ✅ | [.devcontainer/post-start.sh](../.devcontainer/post-start.sh) |

> Devcontainer changes take effect on rebuild. The `.env` file at the repo
> root is `.gitignore`'d (`*.env` rule).

---

## What is *not* done

### From WORK-webhook-tester-unified-receiver.md

| Phase | Item | State |
|---|---|---|
| 3 | Retire `mock-audit-webhook` once webhook-tester proves sufficient | ❌ Not started |

`cmd/mock-audit-webhook/` still exists (208 lines, used by `e2e:test-smoke`
via [test/e2e/setup/manifests/mock-audit-webhook/](../test/e2e/setup/manifests/mock-audit-webhook/)).
The conditions in WORK-webhook-tester-unified-receiver.md "Phase 3" are met
(Phases 1 & 2 work and the demo has run), so this is the next clean cut to
make if you want it.

### From WORK-demo-stack.md

| Phase | Item | State | Notes |
|---|---|---|---|
| 1a | `testApiserver` (sample-apiserver) as optional Helm sub-deployment | ❌ Not started | Still kustomize-only at [test/e2e/setup/manifests/sample-apiserver/](../test/e2e/setup/manifests/sample-apiserver/) |
| 1b | `mockAuditWebhook` as optional Helm sub-deployment | ❌ Not started | Still kustomize-only at [test/e2e/setup/manifests/mock-audit-webhook/](../test/e2e/setup/manifests/mock-audit-webhook/) |
| 1 | `demo.enabled` convenience preset | ❌ Not started | |
| 2 | Add SSE stream + embedded HTML viewer to `mock-audit-webhook` | ❌ Superseded | webhook-tester now provides the live UI. Reconsider only if you want a Kubernetes-audit-aware view |
| 3 | Decision: extract `audit-webhook-receiver` to its own repo | ❌ Open | Touches Phase 2 outcome; defer until retirement decision is made |
| CI | Publish second container image (`mock-audit-webhook`) | ❌ Not started | [.github/workflows/ci.yml](../.github/workflows/ci.yml) still only publishes the proxy |

The "side-by-side demo" section of WORK-demo-stack.md is **superseded and
done** via the webhook-tester unified receiver work.

---

## Wishlist (with concrete how-to)

Ordered roughly by smallest-impact-first.

### W1 — Retire `mock-audit-webhook` (closes WORK-webhook-tester Phase 3)

webhook-tester has proven sufficient as a receiver. The remaining users of
`mock-audit-webhook` are:

- `cmd/mock-audit-webhook/main.go` (208 LOC binary)
- `test/e2e/setup/manifests/mock-audit-webhook/` (kustomize)
- `e2e:build-mock-webhook-image`, `e2e:load-mock-webhook-image`, `e2e:deploy-mock-webhook`, `e2e:prepare-webhook-kubeconfig` Taskfile targets
- `e2e:test-smoke` and `e2e:test-smoke-full` (which assert on the events the mock receiver captured)
- `Dockerfile` `BINARY` build-arg multiplexer
- `Tiltfile` `mock-webhook-update` resource

**How**:

1. Migrate `TestSmoke` assertions from the mock receiver's HTTP API to
   webhook-tester's `GET /api/session/<uuid>/requests` (same shape as
   `TestAggregatedAPIAuditGap` already uses — see [test/e2e/webhook_tester_test.go](../test/e2e/webhook_tester_test.go)).
2. Switch `e2e:prepare` to deploy via `webhookTester.enabled=true` instead of
   `e2e:prepare-webhook-kubeconfig`.
3. Delete `cmd/mock-audit-webhook/`, `test/e2e/setup/manifests/mock-audit-webhook/`,
   the four `e2e:*-mock-webhook*` tasks, the `BINARY` arg from the Dockerfile,
   and the `mock-webhook-update` Tiltfile resource.
4. Remove `e2e:build-mock-webhook-image` / `e2e:load-mock-webhook-image` /
   `e2e:deploy-mock-webhook` chains; `e2e:test-smoke` becomes
   `e2e:test-smoke-with-tester`.

Risk: low. The `mock-audit-webhook`'s job (capture, store, expose via HTTP) is
a strict subset of webhook-tester's.

### W2 — Optional `testApiserver` Helm sub-deployment (WORK-demo-stack 1a)

Today the only way to get sample-apiserver into the cluster is via the e2e
kustomize. Promote it to an optional chart deployment for the "operator
deploys the chart and tries it" flow.

**How**:

1. Translate [test/e2e/setup/manifests/sample-apiserver/](../test/e2e/setup/manifests/sample-apiserver/)
   (Deployment + Service + RBAC + APIService) into chart templates gated on
   `testApiserver.enabled`.
2. Auto-wire `backend.url` and `apiService.{group,version}` when
   `testApiserver.enabled=true` (the upstream chart values would otherwise
   need three coordinated overrides).
3. Re-run `helm template` with both `testApiserver.enabled=true` and
   `webhookTester.enabled=true` for the full demo stack.

The sample-apiserver image (`registry.k8s.io/e2e-test-images/sample-apiserver:1.33.8`)
is public — no build pipeline change needed.

### W3 — `demo.enabled` preset (WORK-demo-stack 1)

Only worth doing after W2. A single `--set demo.enabled=true` should set both
`testApiserver.enabled=true` and `webhookTester.enabled=true` and leave the
chart in a fully-working demo state with no other overrides.

### W4 — Publish a second image *or* drop the build-arg (WORK-demo-stack CI)

If W1 happens, the `BINARY` build arg can go away — that closes this item.
If W1 *doesn't* happen, then duplicate `docker-build` → `docker-build-mock-webhook`
in [ci.yml](../.github/workflows/ci.yml) and call the `multi-arch-publish.yml`
reusable workflow a second time with `image-name: configbutler/mock-audit-webhook`.
Verify first that `configbutler/.github/actions/docker-build@main` exposes a
`build-args` input (the existing call doesn't use it).

### W5 — Native CRD support in webhook-tester UI (Phase-2-equivalent)

webhook-tester is generic — it shows raw JSON. Live audit events would be
nicer with `verb`, `user.username`, `objectRef`, `responseStatus.code` called
out. Three possible directions:

- **(a)** Contribute a "K8s audit" view to upstream webhook-tester. Highest
  leverage, slowest path.
- **(b)** Resurrect the WORK-demo-stack Phase 2 plan (SSE + embedded HTML in
  a dedicated receiver). Highest control, highest maintenance.
- **(c)** Do nothing. The raw JSON view is sufficient for the gap demo.
  Recommended unless someone asks.

### W6 — Repo extraction (`audit-webhook-receiver`)

Open until W1 lands or external demand materializes. WORK-demo-stack Phase 3
already lays out the trade-offs.

---

## CI status

The most recent main runs were failing on **E2E Taskfile Validation** and
**E2E Smoke**. Both jobs failed at compile time with:

```
test/e2e/smoke_test.go:13:2: "path/filepath" imported and not used
```

Introduced by [`18462dc`](https://github.com/ConfigButler/apiservice-audit-proxy/commit/18462dc)
("feat: add e2e test to show the projects why"). That commit also rewrote
`TestSmoke` to query webhook-tester instead of `mock-audit-webhook`, but did
**not** update `e2e:test-smoke` to deploy the webhook-tester path — so even
after the import fix, the test would still time out trying to port-forward to
a service that wasn't deployed. And the chart itself had no checksum
annotation on the kubeconfig Secret, so when the Helm-managed Secret content
changed, the proxy pod was not restarted and kept the old (mock-webhook) URL
in memory.

Three fixes applied in this pass:

1. Remove the unused `path/filepath` import in [test/e2e/smoke_test.go](../test/e2e/smoke_test.go).
2. Switch `e2e:test-smoke` and `e2e:test-smoke-backend-ca` to deploy via
   `e2e:deploy-with-webhook-tester` (matches what the rewritten test asserts on)
   — see [Taskfile.e2e.yml](../Taskfile.e2e.yml).
3. Add `checksum/webhook-kubeconfig` annotation to the Deployment template so
   a Secret content change triggers a rolling restart — see
   [charts/.../deployment.yaml](../charts/apiservice-audit-proxy/templates/deployment.yaml).

### Local validation (this pass)

| Task | Result |
|---|---|
| `task fmt:check` | ✅ clean |
| `task lint` | ✅ 0 issues |
| `task test` | ✅ all unit packages pass (proxy 71.7%, audit 80.5%, identity 81.1%, server 57.9%, mock-audit-webhook 33.8%) |
| `task helm:lint` | ✅ 1 chart, 0 failures |
| `task helm:template` (default + `webhookTester.enabled=true`) | ✅ renders cleanly |
| `task e2e:test-taskfile` | ✅ both `TestBuildImageTask_*` PASS (~1.8s) |
| `task e2e:test-smoke` | ✅ `TestSmoke` PASS (2.4s) — full event in proxy session |
| `task e2e:test-image-refresh` | ✅ all 3 `TestImageRefresh*` PASS (55s) |
| `task e2e:test-audit-gap` | ✅ `TestAggregatedAPIAuditGap` PASS (5.4s) — Lane B complete, Lane A hollow |

---

## Local commands referenced in the WORK plans

```bash
task fmt:check          # gofmt
task lint               # golangci-lint
task test               # unit tests + coverage
task helm:lint          # chart lint
task helm:template      # render chart to stdout

task e2e:test-taskfile  # validates the e2e Taskfile (no cluster, needs Docker)
task e2e:test-smoke     # full smoke against current mock-audit-webhook path
task e2e:test-smoke-full       # smoke + image-refresh
task e2e:test-audit-gap        # webhook-tester side-by-side audit gap
task e2e:cluster-down          # tear down local k3d cluster
```
