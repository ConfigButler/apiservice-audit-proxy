# Plan: image_refresh_test.go

Validates that `task e2e:load-image` correctly imports the built image into the k3d
cluster and that a source change results in the right bits actually running in the pod.
Mirrors the `image_refresh_test.go` pattern from gitops-reverser.

## Scope

These tests run **inside the smoke suite** — they require a running cluster and a
deployed proxy. They are heavier than the taskfile tests and are designed to run last
(see "Execution order" below).

| Test file | Task | Cluster? | What it proves |
|---|---|---|---|
| `taskfile_test.go` (existing) | `e2e:test-taskfile` | No | `e2e:build-image` rebuilds on source change, skips for external image |
| `image_refresh_test.go` (this plan) | `e2e:test-image-refresh` | Yes | `e2e:load-image` imports the right image; after source change + reload, the running pod's imageID matches the stamp |

## Dockerfile layer order

Both gitops-reverser and this project follow the correct caching pattern:

```dockerfile
COPY go.mod go.sum ./        # slow-changing: cached until deps change
RUN go mod download
COPY cmd/ pkg/               # fast-changing: only production sources
RUN go build ...
```

Critically, neither project copies `test/` into the build context. This makes S3 below
fully meaningful: a change to a file under `test/e2e/` will not bust the Docker build
cache and therefore must not trigger a pod restart.

gitops-reverser also copies `api/` and `internal/` because its source layout uses those
paths. We have `cmd/` and `pkg/` — the principle is identical.

## Tests

### S1 — No-op run does not restart the pod

1. Record the current proxy pod name (label selector `app.kubernetes.io/name=apiservice-audit-proxy`).
2. Run `task e2e:load-image` with no source change.
3. Assert the pod name is unchanged (no rollout restart occurred).

**Why**: confirms the stamp-based cache in `hack/e2e/load-image.sh` works and that
idempotent runs are cheap.

### S2 — Go source change triggers rebuild, new pod runs the new digest

1. Record the current proxy pod name.
2. Append a harmless comment to `cmd/server/main.go` (restore in `t.Cleanup`).
3. Run `task e2e:load-image`.
4. Run `kubectl rollout restart deployment/apiservice-audit-proxy -n wardle` and wait
   for rollout to complete (180 s timeout).
5. Assert the new pod name differs from step 1.
6. Query `kubectl get pod <new-pod> -o jsonpath='{.status.containerStatuses[0].imageID}'`.
7. Read the image digest from the stamp file written by `load-image.sh`
   (`.stamps/e2e/<cluster>/proxy-image.stamp`).
8. Assert the pod's imageID contains the sha256 digest from the stamp.

**Why**: end-to-end proof that the right bits are running — not just that Docker built a
new layer, but that k3d imported it and the pod is actually using it.

### S3 — Test file change does not trigger rebuild

1. Record the current pod name.
2. Append a comment to a file under `test/e2e/` (restore in `t.Cleanup`).
3. Run `task e2e:load-image`.
4. Assert the pod name is unchanged.

**Why**: `test/` is not copied into the Docker build context (`COPY cmd/ pkg/` in our
Dockerfile), so test file changes must not bust the image cache or cause a pod restart.
This guards against accidental widening of the `COPY` instruction in the future.

## Execution order and separate invocation

These tests must run **after** the environment is fully deployed (cluster up, proxy
running). They also mutate source files and trigger image rebuilds, so they should not
interleave with smoke tests.

**File naming**: Go runs test files within a package in alphabetical order.
`image_refresh_test.go` sorts before `smoke_test.go` (i < s), which is wrong.
Rename to `z_image_refresh_test.go` so it sorts last naturally, or — preferred — drive
execution through a dedicated task that uses `-run`:

```yaml
# Taskfile.e2e.yml additions

e2e:test-image-refresh:
  desc: Validate image load → pod digest chain (cluster required; runs after smoke)
  cmds:
    - >
      CTX={{.CTX}}
      go test -tags=e2e -run 'TestImageRefresh' -count=1 -v ./test/e2e

e2e:test-smoke-full:
  desc: Run the full e2e suite in order: smoke first, then image-refresh
  cmds:
    - task: e2e:test-smoke
    - task: e2e:test-image-refresh
```

Naming rationale: gitops-reverser uses `test-e2e` / `test-e2e-full` / `test-image-refresh`
(in its own `test/e2e/Taskfile.yml`). We use an explicit `e2e:` namespace prefix on all
tasks, so the equivalents are `e2e:test-smoke` / `e2e:test-smoke-full` /
`e2e:test-image-refresh`. gitops-reverser filters by Ginkgo label
(`-ginkgo.label-filter=image-refresh`); we use standard Go testing (`-run 'TestImageRefresh'`)
— both achieve the same separation.

`e2e:test-smoke` stays unchanged and does not run `TestImageRefresh` (the `-run` regex
in the current `go test` invocation already selects by name — just confirm it doesn't
accidentally match). If needed, tighten the smoke invocation to `-run 'TestSmoke'`.

## Implementation notes

### Helpers needed

```go
// proxyPodName returns the name of the single running proxy pod.
func proxyPodName(t *testing.T, ctx, namespace string) string

// waitForNewPod polls until a pod matching the selector exists whose name differs
// from oldName, then returns the new name.
func waitForNewPod(t *testing.T, ctx, namespace, selector, oldName string, timeout time.Duration) string

// podImageID returns containerStatuses[0].imageID for the named pod.
func podImageID(t *testing.T, ctx, namespace, podName string) string

// readStamp reads the proxy-image stamp file and returns its digest (the sha256:... part).
func readProxyImageDigest(t *testing.T, projectDir, clusterName string) string
```

### Rollout restart

Drive the restart directly from the test via `exec.Command`:

```go
exec.Command("kubectl", "--context", ctx, "-n", namespace,
    "rollout", "restart", "deployment/apiservice-audit-proxy").Run()
```

A thin `e2e:restart-proxy` task could be added to `Taskfile.e2e.yml` if the restart
step is also useful interactively, but it is not required for the test.

### Stamp file path

`load-image.sh` writes `.stamps/e2e/<CLUSTER_NAME>/proxy-image.stamp`.
`CLUSTER_NAME` is derived from the kubeconfig context by stripping the `k3d-` prefix
(e.g. context `k3d-audit-pass-through-e2e` → cluster `audit-pass-through-e2e`).

The stamp format is `IMAGE_REPO:TAG@sha256:DIGEST` — extract the digest with
`strings.SplitN(stamp, "@sha256:", 2)[1]`.

### Build tag

File must carry `//go:build e2e` (same as the rest of the suite).

### CI impact

Add an `e2e-image-refresh` job to `.github/workflows/ci.yml` that runs after
`e2e-smoke` (same DooD pattern). Wire it into `release-please` needs alongside
`e2e-smoke`. No other CI changes needed.
