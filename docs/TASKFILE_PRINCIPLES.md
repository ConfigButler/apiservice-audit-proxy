# Taskfile Principles: Make-Style Incremental Tasks

This document captures the rules we want to follow for any [Task](https://taskfile.dev)
file in this repository. It is written up-front as a standalone reference so it
can become a skill later (`.claude/skills/taskfile-principles/SKILL.md`).

The rules are derived from reading the
[ConfigButler/gitops-reverser](https://github.com/ConfigButler/gitops-reverser)
Taskfiles and its own post-mortem
[task-migration-plan.md](../external-resources/gitops-reverser/docs/task-migration-plan.md).
That project started as a `Makefile`, switched to Task, and kept the parts of
the Make model that actually earn their keep: **explicit inputs, explicit
outputs, explicit state, and a real dependency graph**. This document is a
short, opinionated condensation of what made that work.

---

## 1. The mental model: a Taskfile is a declared dependency graph

A well-written task reads like a declaration, not a procedure:

- What files, if they change, should make this task re-run? → `sources:`
- What files does this task produce? → `generates:`
- What other tasks must be done first? → `deps:` (parallel) or `cmds: - task: X` (sequential)
- What cheap non-file reality checks must still pass before we skip? → `status:`
- What do we actually execute? → `cmds:`

If you are writing a task and you can't answer all five questions in your head,
you are writing a shell script with a `desc:` on top. That is fine for some
tasks (see §10), but it should be a conscious choice, not the default.

The payoff is simple: running `task <anything>` a second time on an unchanged
tree should be near-instant, because everything in the graph already knows it
is up to date. That is the whole point.

---

## 2. The five building blocks

### 2.1 `sources:` — inputs that invalidate the task

Globs of files whose content (or mtime, see §7) determines whether the task
needs to run.

```yaml
sources:
  - cmd/**/*.go
  - pkg/**/*.go
  - go.mod
  - go.sum
  - exclude: pkg/**/*_test.go
  - exclude: pkg/**/zz_generated.deepcopy.go
```

Rules of thumb:

- **List every file that legitimately affects the output.** Miss one and you
  get stale artifacts.
- **Exclude everything that doesn't.** Leaving tests or generated files in the
  source set means every unrelated edit invalidates the cache. Gitops-reverser
  is strict about this — see `manifests`, `generate`, `_controller-image-id` —
  and that strictness is why its incremental builds actually feel incremental.
- **Include tool inputs too.** `go.mod`, `go.sum`, `Dockerfile`, build args —
  these affect the output and belong in `sources`.

### 2.2 `generates:` — outputs the task produces

The target files. If any are missing, the task runs. If all are present and
newer than sources (under `method: timestamp`) or sources are unchanged (under
`method: checksum`), the task is skipped.

```yaml
generates:
  - bin/apiservice-audit-proxy
  - coverage.out
```

Rules of thumb:

- **One task, one coherent set of outputs.** If a task produces "some stuff in
  `dist/` and also a stamp file somewhere else," it probably wants to be two
  tasks.
- **If the task doesn't naturally produce a file, produce a stamp.** See §3.
- **Do not repeat simple output-existence checks in `status:`.** Task already
  treats a missing `generates:` path as "not up to date." Use `status:` for
  facts Task cannot infer from `sources:`/`generates:`.

### 2.3 `deps:` vs `cmds: - task: X` — parallel vs sequential

- `deps:` — list of tasks that must finish first, run **in parallel**. Use when
  the prerequisites are independent.
- `cmds: - task: X` — runs the referenced task **in sequence**, in the order
  listed. Use when order matters or when state from one step feeds the next.

Both compose; pick based on dependency semantics, not stylistic preference.

Example from gitops-reverser (`test` wants four independent things ready, so
they run in parallel):

```yaml
test:
  deps:
    - manifests
    - fmt
    - vet
    - setup-envtest
  cmds:
    - go test ...
```

Example (`prepare-e2e` is a strict ordered chain — the cluster must exist
before the image can be loaded, etc.):

```yaml
prepare-e2e:
  cmds:
    - task: install
    - task: _project-image-ready
    - task: _image-loaded
    - task: _controller-deployed
    - task: _age-key
    - task: _sops-secret-yaml
    - task: _sops-secret-applied
    - task: _webhook-tls-ready
    - task: _prepare-e2e-ready
    - task: portforward-ensure
```

When in doubt: if two tasks could genuinely run at the same time without
stepping on each other, make them `deps:`. Getting this right is a noticeable
speed win in CI.

### 2.4 `status:` — cheap runtime reality checks

`sources:` and `generates:` handle file-based invalidation. `status:` handles
everything else Task cannot know from the local filesystem — "is the cluster
still there?", "does this image still exist in Docker?", "does the stamp say
what we expect?"

Each line is a shell command. If **all** of them exit 0, the task is considered
up to date and skipped. If any exit non-zero, the task runs. When combined with
`sources:`/`generates:`, Task runs the task if either the fingerprinted files
are stale/missing or the `status:` check fails.

```yaml
_project-image-ready-provided:
  generates:
    - "{{.IS}}/project-image.ready"
  status:
    - test "$(cat "{{.IS}}/project-image.ready" 2>/dev/null)" = "{{.PROJECT_IMAGE}}"
    - '{{.CONTAINER_TOOL}} image inspect "{{.PROJECT_IMAGE}}" >/dev/null 2>&1'
  cmds:
    - ...
```

This is the key to making "runtime state" behave like a cache entry. A stamp
file alone says "we did the thing once." `generates:` checks that the stamp
exists; `status:` asserts the external world still agrees.

The official Task docs show `status:` with plain `test -f` checks for a task
that has no `sources:`/`generates:`. That is valid, but it is not the pattern
we should use when the output is already declared in `generates:`.

`status:` is especially useful for remote artifacts such as Docker images,
cluster deployments, and CD releases. For those, checksums and timestamps need
either direct access to the artifact or an out-of-band local fingerprint; a
small, fast `status:` command can verify the important remote fact without
rebuilding the world.

### 2.5 `cmds:` — what actually runs

Keep it short. Push non-trivial shell into `hack/*.sh` scripts that take
environment variables as input. Gitops-reverser does this consistently
(`hack/e2e/load-image.sh`, `start-cluster.sh`, `ensure-valkey-auth.sh`) and it
pays off: the Taskfile stays readable, the scripts stay testable, and the
split between orchestration (what to run and when) and action (how to run it)
is obvious.

---

## 3. Stamp files: making runtime state cacheable

Most build tasks produce a natural file (a binary, a YAML, a coverage report).
Runtime tasks don't — "the k3d cluster is up," "this image is loaded into
containerd," "the controller rollout finished" — there's no file in your
working tree that represents that fact.

Instead of giving up on caching, **write a stamp file**: a small file under
`.stamps/` whose presence (and sometimes content) encodes the fact you care
about.

Use stamps only when there is no natural repo-local output to list in
`generates:`. Do not add a `.stamps/...` file just to mirror something Task can
already track directly, such as a binary, package, rendered manifest, coverage
file, or generated source file.

### The pattern

```yaml
_cluster-ready:
  method: timestamp
  sources:
    - test/e2e/cluster/start-cluster.sh
    - '{{.AUDIT_POLICY_SOURCE}}'
  generates:
    - '{{.CS}}/ready'
  status:
    - '{{.K3D}} cluster list -o json 2>/dev/null | grep -q "\"name\":\"{{.CLUSTER_NAME}}\""'
    - '{{.KUBECTL}} --context {{.CTX}} --request-timeout=10s get ns >/dev/null 2>&1'
  cmds:
    - mkdir -p "{{.CS}}"
    - bash test/e2e/cluster/start-cluster.sh
    - touch "{{.CS}}/ready"
```

Gitops-reverser has an entire ladder of these: `ready` → `flux.installed` →
`flux-setup.ready` → `services.ready` → `image.loaded` → `controller.deployed`
→ `webhook-tls.ready` → `sops-secret.applied` → `prepare-e2e.ready`. Each stamp
is also the `sources:` entry of the next stamp, so the graph rebuilds exactly
the right subtree when any underlying input changes.

### Stamp content carries identity when "existence" isn't enough

When "the thing was done once" isn't a strong enough signal — e.g., a docker
image could be rebuilt or evicted externally — write the identity into the
stamp and check it in `status:`:

```
.stamps/e2e/<cluster>/proxy-image.stamp     ← contents: "apiservice-audit-proxy:e2e-local@sha256:abc123..."
```

Then `status:` can read the stamp and compare it against the current image ID.
If they drift, the task re-runs. This is exactly what
[hack/e2e/load-image.sh](../hack/e2e/load-image.sh) does already in this repo
— we just don't consume the stamp from the Taskfile side yet (see the
improvement plan).

### Redundant status checks

Avoid this pattern:

```yaml
e2e:_requestheader-ca-copied:
  method: checksum
  sources:
    - '{{.STAMPS_DIR}}/backend.deployed'
    - hack/e2e/write-requestheader-client-ca.sh
  generates:
    - '{{.STAMPS_DIR}}/requestheader-ca.applied'
  status:
    - test -f "{{.STAMPS_DIR}}/requestheader-ca.applied"
```

The `test -f` line is redundant because `requestheader-ca.applied` is already
declared in `generates:`. Keep a `status:` check only for the remote state that
the stamp represents, for example whether the Kubernetes Secret still exists.

### Stamp file layout conventions

- Live under `.stamps/` (gitignored).
- Namespace by concern: `.stamps/e2e/<cluster>/...`, `.stamps/build/...`,
  `.stamps/envtest-1.30.ready`.
- **Never commit stamps.** They are machine-local facts.
- Clean them in a `clean` task (`rm -rf .stamps/`) so operators have an easy
  way to force a full rebuild.

---

## 4. Internal (`_`-prefixed) tasks

Gitops-reverser's e2e Taskfile has two layers:

- **Public tasks** (`prepare-e2e`, `test-e2e`, `install-helm`) — the names
  humans type. Small, ordered, descriptive.
- **Internal tasks** (`_cluster-ready`, `_flux-installed`, `_image-loaded`) —
  individually cacheable units of work, each with their own
  sources/generates/status.

The split earns its keep three ways:

1. `task --list` stays clean (internal tasks don't need a `desc:`).
2. Each internal task gets its own independent cache entry.
3. Public tasks become readable as flow documents — you skim the `cmds: -
   task:` list and you know the whole pipeline in ten seconds.

Rule: if a step has non-trivial sources/generates, it wants to be an internal
task. Don't inline it into the public one.

---

## 5. `method: timestamp` vs the default (`checksum`)

- **`checksum`** (default). Task hashes the content of `sources:`. The task
  re-runs when the hash changes. Best for code-generates-artifact tasks
  (`build`, `generate`, `manifests`, `test`).
- **`method: timestamp`**. Task compares source mtimes against generate-target
  mtimes. The task re-runs when sources are newer than the target. Best for
  **stamp-based** tasks where the generate target *is* the readiness marker
  and content equality doesn't matter — you already touched the stamp, the
  stamp's mtime is the truth.

Gitops-reverser uses `timestamp` for every `.stamps/...` task and the default
for every Go/Helm build task. Follow that split, unless the stamp content is
itself the identity being compared; in that case, `checksum` plus a focused
`status:` check can be the clearer choice.

Task is responsible for the state that matters to this repository, not for
proving the entire outside world has not silently changed. Prefer fast,
repo-scoped fingerprints and assume stable external state when that is an
acceptable development tradeoff. Add runtime `status:` checks for important
remote facts, but keep them cheap: local `docker image inspect`, short
`kubectl get`, or a small file comparison are good; slow cluster-wide probes
belong in explicit validation or e2e tasks.

---

## 6. The "write if changed" pattern

When a task generates a file that downstream tasks watch, regenerating
byte-identical content should **not** cascade invalidations. Gitops-reverser
solves this by writing to a tmp file and only replacing the real target when
content differs:

```bash
tmp="{{.CS}}/{{.NAMESPACE}}/sops-secret.yaml.tmp"
go run ... --secret-file "${tmp}" ...
if [ -f "{{.CS}}/{{.NAMESPACE}}/sops-secret.yaml" ] \
   && cmp -s "${tmp}" "{{.CS}}/{{.NAMESPACE}}/sops-secret.yaml"; then
  rm -f "${tmp}"
else
  mv "${tmp}" "{{.CS}}/{{.NAMESPACE}}/sops-secret.yaml"
fi
```

Use this whenever a task's output is consumed as the `sources:` of another
task and the generator is non-deterministic in mtime but deterministic in
content.

---

## 7. Source-list hygiene

Sources are a contract. Break the contract and you get stale artifacts or
phantom rebuilds. Two sub-rules:

1. **List it if it affects the output.** Go files, `go.mod`, `go.sum`, the
   `Dockerfile`, Helm chart templates, `values.yaml`, generator input
   boilerplate.
2. **Exclude it if it doesn't.** `_test.go`, `zz_generated.deepcopy.go`,
   docs, CI config. The gitops-reverser pattern:

   ```yaml
   sources:
     - api/**/*.go
     - internal/**/*.go
     - cmd/**/*.go
     - exclude: api/**/*_test.go
     - exclude: internal/**/*_test.go
     - exclude: cmd/**/*_test.go
     - exclude: api/**/zz_generated.deepcopy.go
     - exclude: internal/**/zz_generated.deepcopy.go
     - exclude: cmd/**/zz_generated.deepcopy.go
   ```

   is repetitive on purpose — each exclude documents *why it is not a real
   input*. Be explicit rather than clever.

---

## 8. Thin root Taskfile + `includes:`

The public surface lives at the root. Keep the root file tiny:

```yaml
version: '3'
includes:
  build: { taskfile: ./Taskfile-build.yml, flatten: true }
  e2e:   { taskfile: ./test/e2e/Taskfile.yml, flatten: true }
tasks:
  default: { cmds: [task --list], silent: true }
```

Reasons:

- Concerns separate cleanly. Build changes don't touch e2e files.
- `flatten: true` exposes child tasks at the root namespace when you want
  `task build`; omit it when you want namespaced `task e2e:cluster-up`.
- Readers can find anything by grep on a small number of files instead of
  scrolling one huge file.

Don't pre-split. Split when a file starts being edited by two unrelated
concerns at once, or when it passes ~200 lines and is still growing.

---

## 9. Don't cache what doesn't need caching

Not every task is a build step. Some are always-run actions and trying to
cache them just makes them fragile:

- `fmt` / `fmt:check` / `lint` — fast, idempotent, always want the live answer.
- `docker-push` — you *want* it to run every time.
- Interactive or inspection tasks (`helm:template`, `help`, `doctor`).
- `clean` — obviously.

Leave these as plain `cmds:` tasks. Adding `sources:`/`generates:` here adds
noise, not speed.

A good rule: a task earns caching only if (a) it takes noticeably long and
(b) its output is a well-defined set of files.

---

## 10. Worked examples

### 10.1 A canonical "build with incremental cache"

From [Taskfile-build.yml](../external-resources/gitops-reverser/Taskfile-build.yml):

```yaml
generate:
  desc: Generate DeepCopy implementations
  sources:
    - api/**/*.go
    - hack/boilerplate.go.txt
    - exclude: api/**/*_test.go
    - exclude: api/**/zz_generated.deepcopy.go
  generates:
    - api/v1alpha1/zz_generated.deepcopy.go
  cmds:
    - '{{.CONTROLLER_GEN}} object:headerFile="hack/boilerplate.go.txt" paths="./..."'
```

Every essential piece is declared. `task generate` twice in a row runs
`controller-gen` once.

### 10.2 A canonical "stamp-based runtime readiness"

Adapted from [test/e2e/Taskfile.yml](../external-resources/gitops-reverser/test/e2e/Taskfile.yml):

```yaml
_cluster-ready:
  method: timestamp
  sources:
    - test/e2e/cluster/start-cluster.sh
    - '{{.AUDIT_POLICY_SOURCE}}'
    - '{{.AUDIT_WEBHOOK_BOOTSTRAP_SOURCE}}'
  generates:
    - '{{.CS}}/ready'
    - '{{.AUDIT_POLICY_PATH}}'
    - '{{.AUDIT_WEBHOOK_CONFIG_PATH}}'
  status:
    - kubectl --context "{{.CTX}}" get ns >/dev/null
  cmds:
    - mkdir -p "{{.CS}}" "{{.AUDIT_ASSET_DIR}}"
    - cp "{{.AUDIT_POLICY_SOURCE}}" "{{.AUDIT_POLICY_PATH}}"
    - cp "{{.AUDIT_WEBHOOK_BOOTSTRAP_SOURCE}}" "{{.AUDIT_WEBHOOK_CONFIG_PATH}}"
    - |
      bash test/e2e/cluster/start-cluster.sh
      kubectl --context "{{.CTX}}" get ns >/dev/null
      touch "{{.CS}}/ready"
```

Notice the layering: the cluster start script is in `sources`, the stamp is in
`generates`, additional runtime facts are asserted in `status`, and the `cmds`
both produce the artifacts and finalize the stamp. A `status:` line for the
stamp itself would be redundant.

### 10.3 A canonical "identity in the stamp"

```yaml
_project-image-ready-provided:
  generates:
    - "{{.IS}}/project-image.ready"
  status:
    - test "$(cat "{{.IS}}/project-image.ready" 2>/dev/null)" = "{{.PROJECT_IMAGE}}"
    - '{{.CONTAINER_TOOL}} image inspect "{{.PROJECT_IMAGE}}" >/dev/null 2>&1'
  cmds:
    - ...
```

Two status checks: the stamp records the expected image ref, and the image
still exists in the local container store. `generates:` already handles stamp
existence. Any drift re-runs the task. That is what "automatic invalidation"
means in practice.

---

## Checklist when adding a new task

- [ ] Does it have a one-sentence `desc:`?
- [ ] Is it a build-like task (produces files) or a always-run action?
  - If build-like: `sources:`, `generates:`, correct `method:`, relevant `exclude:`s.
  - If always-run: skip sources/generates; keep it simple.
- [ ] Is it a runtime-readiness task? If yes:
  - [ ] Stamp file under `.stamps/...`
  - [ ] `method: timestamp`
  - [ ] `status:` that asserts only cheap external reality the stamp stands for
  - [ ] No `status: test -f <stamp>` if `<stamp>` is already in `generates:`
- [ ] Does it depend on other tasks?
  - Independent prerequisites → `deps:` (parallel)
  - Ordered pipeline → `cmds: - task:` (sequential)
- [ ] If the task is only a sub-step of a public flow, name it with a leading `_`.
- [ ] Is the `cmds:` block short? If it grew a bash heredoc, move it to `hack/*.sh`.

---

## References

- [Task guide: task dependencies and fingerprinting](https://taskfile.dev/docs/guide#task-dependencies)
- [gitops-reverser Taskfile-build.yml](../external-resources/gitops-reverser/Taskfile-build.yml)
- [gitops-reverser test/e2e/Taskfile.yml](../external-resources/gitops-reverser/test/e2e/Taskfile.yml)
- [gitops-reverser task-migration-plan.md](../external-resources/gitops-reverser/docs/task-migration-plan.md)
  — post-mortem on why the `Makefile` → Task move worked
