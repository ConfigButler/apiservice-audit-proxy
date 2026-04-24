# Taskfile Improvement Plan

This is a concrete, project-specific plan to bring
[Taskfile.yml](../Taskfile.yml) and [Taskfile.e2e.yml](../Taskfile.e2e.yml)
closer to the principles laid out in
[docs/TASKFILE_PRINCIPLES.md](TASKFILE_PRINCIPLES.md).

The project is small and young. The goal is not to reproduce every pattern
from gitops-reverser wholesale — it is to lay a clean base that we can grow
into without rewrites later.

---

## 1. Where we are today

### Root [Taskfile.yml](../Taskfile.yml) (112 lines)

Every non-e2e task is a plain `cmds:` block. Nothing declares
`sources:`/`generates:`/`deps:`/`status:`. That means:

- `task build` rebuilds the binary every time, even when nothing changed.
- `task test` reruns the whole test suite unconditionally.
- `task dist` re-packages the chart every time.
- `task ci` runs its six steps strictly in series — no parallelism across
  `fmt:check` / `lint` / `test` / `helm:lint` that could easily run together.

### E2E [Taskfile.e2e.yml](../Taskfile.e2e.yml) (262 lines)

Ordering is done correctly with sequential `cmds: - task:` chains, so the
dependency graph is roughly right. But:

- Only one task (`e2e:build-image`) has any `status:` guard, and it's an
  environment check, not a caching check.
- `hack/e2e/load-image.sh` already writes content-rich stamps to
  `.stamps/e2e/<cluster>/proxy-image.stamp` and `mock-webhook-image.stamp`
  (image ref + digest). **The stamps exist on disk — we just don't read them
  from the Taskfile.** So every `task e2e:test-smoke` rebuilds, reloads, and
  re-imports both images, even when the stamp says nothing changed.
- There are no stamps for any other readiness step: cluster-up, flux-bootstrap,
  requestheader-ca-copied, proxy-helm-installed. So every smoke run re-executes
  all of them.
- The "build images concurrently with cluster-up" opportunity is missed —
  these have no dependency on each other.

### Summary

The shape is fine. The declarations are missing. This is a good state to be
in — we can add `sources:`/`generates:`/`status:` incrementally without
rewriting anything structural.

---

## 2. Goals for this plan

In priority order:

1. **Re-running a no-op should be fast.** `task ci` on a clean tree after a
   successful run should skip most work.
2. **E2E smoke should cache readiness.** `task e2e:test-smoke` after a prior
   successful run should skip cluster-up, image loads, and installs, and just
   run the Go test.
3. **Trivial parallelism in CI.** Independent checks should not run serially.
4. **Keep the file layout simple for now.** Don't split into subfiles until
   we actually feel the pain.
5. **Every new rule should match the principles doc** so we learn one pattern.

---

## 3. Phased plan

Each phase is independently shippable. Prefer merging phase-by-phase with its
own PR so that behavior changes are easy to bisect.

### Phase 1 — incremental caching for build-side tasks

Target file: [Taskfile.yml](../Taskfile.yml).

Tasks to update:

- **`test`** — add `sources: cmd/**/*.go, pkg/**/*.go, go.mod, go.sum` and
  `generates: coverage.out`. Default `checksum` method. A re-run after a
  successful test with no code changes skips instantly.
- **`build`** — add the same `sources:` plus `Dockerfile` is *not* needed
  here (that's for `docker-build`), and `generates: bin/apiservice-audit-proxy`.
- **`docker-build`** — add `sources: Dockerfile, cmd/**/*.go, pkg/**/*.go,
  go.mod, go.sum`, plus a new `generates: .stamps/build/docker-image.id`
  stamp whose content is the `docker inspect --format='{{.Id}}'` of the
  resulting image. Mirrors gitops-reverser's `_controller-image-id`. Adds a
  `status:` that asserts both the stamp exists and `docker image inspect`
  still succeeds. This is the template for all future image-build tasks.
- **`helm:package`** — add `sources: charts/apiservice-audit-proxy/**` and
  `generates:` for the versioned tgz. The filename is version-dependent, so
  either glob it (`dist/apiservice-audit-proxy-*.tgz`) or derive the version
  into a var.
- **`dist`** — add `sources: charts/apiservice-audit-proxy/**` and
  `generates: dist/checksums.txt, dist/apiservice-audit-proxy-*.tgz`.

Leave alone (§9 of the principles doc):

- `fmt`, `fmt:check`, `lint`, `lint-fix`, `helm:lint`, `helm:template`,
  `help`, `default` — always-run.

Create a `.gitignore` entry for `.stamps/` if not already present *(already
covered in the current root `.gitignore`)*.

### Phase 2 — parallelism in `ci`

Target: the single `ci:` task.

Replace the serial `cmds:` chain with a two-stage form:

```yaml
ci:
  desc: Run the local CI-equivalent checks
  deps:
    - fmt:check
    - lint
    - test
    - helm:lint
  cmds:
    - task: build
    - task: dist
```

Rationale:

- `fmt:check`, `lint`, `test`, `helm:lint` are all read-only checks that
  touch disjoint state; running them in parallel via `deps:` is safe and
  noticeably faster on a cold cache.
- `build` and `dist` stay sequential inside `cmds:` because `dist` reads the
  chart dir and doesn't depend on `build`, but they do share the filesystem
  under `dist/` — keeping them ordered avoids churn. (If profiling later
  shows they'd benefit from running in parallel, they can both move into
  `deps:` as well.)

### Phase 3 — wire the existing image stamps into e2e

Target file: [Taskfile.e2e.yml](../Taskfile.e2e.yml).

The stamps already exist — we just need the tasks to respect them.

Add to `e2e:load-image` and `e2e:load-mock-webhook-image`:

```yaml
e2e:load-image:
  method: timestamp
  sources:
    - Dockerfile
    - cmd/**/*.go
    - pkg/**/*.go
    - go.mod
    - go.sum
    - exclude: cmd/**/*_test.go
    - exclude: pkg/**/*_test.go
  generates:
    - '{{.STAMPS_DIR}}/proxy-image.stamp'
  status:
    - test -f "{{.STAMPS_DIR}}/proxy-image.stamp"
    - '{{.DOCKER}} image inspect "{{.E2E_PROXY_IMAGE}}" >/dev/null 2>&1'
  cmds:
    - task: e2e:cluster-up
    - task: e2e:build-image
    - mkdir -p {{.STAMPS_DIR}}
    - |
      CLUSTER_NAME="{{.CLUSTER_NAME}}" IMAGE="{{.E2E_PROXY_IMAGE}}" \
        K3D="{{.K3D}}" DOCKER="{{.DOCKER}}" \
        STAMP_FILE="{{.STAMPS_DIR}}/proxy-image.stamp" \
        bash hack/e2e/load-image.sh
```

Impact: after the first successful smoke run, re-running `task
e2e:load-image` with no code changes is instant. The existing
`load-image.sh` script already does the heavy lifting — it writes the stamp
with `IMAGE@ID` content and short-circuits when the cluster manifest digest
matches.

Do the same for `e2e:load-mock-webhook-image` (using its own stamp and
sources — pkg-wise, the mock webhook also lives under `cmd/` /`pkg/`, so
source overlap is intentional; the stamp filename distinguishes them).

### Phase 4 — introduce internal `_`-prefixed readiness tasks

This is where we lean on the full gitops-reverser pattern. Add one internal
task per readiness boundary, each with its own stamp under
`.stamps/e2e/<cluster>/`:

| Internal task | Stamp | Sources (what invalidates it) |
|---|---|---|
| `_cluster-ready` | `ready` | `test/e2e/cluster/start-cluster.sh` |
| `_flux-installed` | `flux.installed` | `ready` |
| `_flux-setup-ready` | `flux-setup.ready` | `flux.installed`, `test/e2e/setup/flux/**` |
| `_services-ready` | `services.ready` | `flux-setup.ready`, `test/e2e/setup/manifests/**/*.yaml` |
| `_proxy-image-loaded` | `proxy-image.stamp` (already exists) | image source tree + Dockerfile |
| `_requestheader-ca-copied` | `requestheader-ca.applied` | `services.ready`, `hack/e2e/write-requestheader-client-ca.sh` |
| `_proxy-with-webhook-tester-installed` | `proxy-webhook-tester.installed` (content = image digest + values file) | `proxy-image.stamp`, `requestheader-ca.applied`, `charts/apiservice-audit-proxy/**`, `{{.E2E_PROXY_VALUES_FILE}}` |

Then rewrite the public tasks as thin chains:

```yaml
e2e:cluster-up:
  desc: Create or reuse the local k3d cluster
  cmds:
    - task: _cluster-ready

e2e:flux-bootstrap:
  desc: Install Flux and the shared add-ons
  cmds:
    - task: _services-ready   # transitively brings up cluster + flux + flux-setup

e2e:deploy-proxy:
  cmds:
    - task: _proxy-installed

e2e:test-smoke:
  cmds:
    - task: _proxy-installed
    - CTX={{.CTX}} ... go test -tags=e2e -run 'TestSmoke' -count=1 -v ./test/e2e
```

Running `task e2e:test-smoke` after a green run becomes: check ~10 stamps,
skip everything, run the Go test. That is the payoff the principles promise.

### Phase 5 — add a `clean-stamps` target and document recovery

Small but important operator ergonomics:

```yaml
clean-stamps:
  desc: Forget cached readiness; forces the next run to re-do setup
  cmds:
    - rm -rf .stamps/

clean:
  desc: Remove build artifacts, dist, and cached stamps
  cmds:
    - rm -rf bin/ coverage.out dist/ .stamps/
```

And one note in [AGENTS.md](../AGENTS.md): "if something looks stale and you
don't know why, `task clean-stamps` is the escape hatch." This is a standard
consequence of caching — give humans an obvious lever.

### Phase 6 (deferred) — split `Taskfile.yml`

**Not yet.** At 112 lines the root Taskfile is still readable and mixes two
concerns (build + e2e) only through `includes:`. Split only when:

- the file passes ~200 lines of non-comment content, or
- two people are editing it for unrelated reasons in the same PR, or
- we grow a third concern (e.g., release tooling) that deserves its own file.

When the time comes, mirror gitops-reverser: `Taskfile.yml` stays a tiny root
that only includes, with `Taskfile.build.yml` holding the build/test/lint
surface and `Taskfile.e2e.yml` unchanged.

---

## 4. Risks and how we mitigate them

- **Wrong `sources:` globs produce stale artifacts.** Mitigated by starting
  with the minimum viable list and widening it when we see a "why did that
  not rebuild?" moment. Phase 1 explicitly keeps lint/fmt un-cached — those
  are the checks that would catch most staleness before it matters.
- **Stamps claiming state that external systems have lost.** Example: docker
  prunes the image; k3d deletes a node. Mitigated by putting an external
  reality check in every runtime task's `status:` (`docker image inspect`,
  `kubectl get ns`, `k3d cluster list`). This is why `status:` exists —
  don't skip it on runtime tasks.
- **Caching hides real regressions.** Anyone running `task clean-stamps`
  before a release check, or running CI with `--force`, will see the
  uncached answer. CI in GitHub Actions starts with an empty filesystem, so
  the remote `ci` run is always a full build. Local caching is a
  productivity tool, not a correctness contract.

---

## 5. Order of operations if a human wants to do this today

1. Phase 1 + Phase 2 together — self-contained, small PR, immediate
   dev-loop speedup.
2. Phase 5 — cheap, makes the rest safer to roll out.
3. Phase 3 — one commit per image stamp; each is testable via
   `task e2e:load-image && task e2e:load-image` and checking the second
   invocation is a no-op.
4. Phase 4 — larger but mechanical. Doable in one sitting using
   gitops-reverser's e2e Taskfile as the shape template.
5. Phase 6 — only when we feel the pain.

---

## 6. Out of scope for this plan

- Adopting gitops-reverser's "provided vs local" image handling
  (`_project-image-ready-provided` / `-local` split). Useful once we start
  pulling pre-built images in CI, not needed now.
- Parallel e2e deploys beyond `build-images` — the setup chain has real
  ordering (cluster → flux → crds → services → install), and forcing
  parallelism inside the chain is not a win.
- Generator tasks (we don't use `controller-gen` here — CRDs aren't owned
  by this repo). If that changes, revisit.

---

## References

- Principles: [docs/TASKFILE_PRINCIPLES.md](TASKFILE_PRINCIPLES.md)
- Current files: [Taskfile.yml](../Taskfile.yml),
  [Taskfile.e2e.yml](../Taskfile.e2e.yml)
- Template to copy from:
  [gitops-reverser/Taskfile-build.yml](../external-resources/gitops-reverser/Taskfile-build.yml),
  [gitops-reverser/test/e2e/Taskfile.yml](../external-resources/gitops-reverser/test/e2e/Taskfile.yml)
