# E2E Setup — What Broke and Why

This document describes every issue encountered while getting `task e2e:test-smoke`
to pass from a cold devcontainer. Each entry explains the root cause in enough
depth to help you recognise and prevent the same class of problem in the future.

---

## 1. k3d cluster creation hung — serverlb port mappings

### What happened

`k3d cluster create` timed out after ~12 minutes. The k3s server node started
but then entered `status=removing`. The error was:

```
Failed to start server k3d-audit-pass-through-e2e-server-0:
Node failed to get ready: error waiting for log line `k3s is up and running`
```

### Root cause

The cluster creation call included:

```bash
--port "8081:80@loadbalancer"
--port "8443:443@loadbalancer"
```

These flags tell k3d to spin up a separate **serverlb** container (an nginx
reverse proxy) and expose those host ports through it. k3d must write an nginx
config file into that container before k3s starts. In a
**Docker-outside-of-Docker (DooD)** devcontainer environment, that file write
hangs indefinitely — the container filesystem write goes through the host
Docker daemon, and under certain kernel/cgroup configurations this blocks.

Because k3s waits for the serverlb to be ready before marking itself up, the
whole cluster creation eventually times out and rolls back.

### Fix

Removed the `--port @loadbalancer` flags. The e2e tests do not need to reach
the cluster over host ports — all access goes through `kubectl` using the
kubeconfig, which talks to the k3d API port directly.

### Lesson

Port mappings via `@loadbalancer` add a dependency on a separate nginx
container. In DooD environments, prefer `--api-port` (a direct port on the
server container itself) for API access, and skip the serverlb entirely unless
you genuinely need host-port HTTP/S ingress.

---

## 2. k3s crashed — inotify instance limit exhausted

### What happened

After fixing the serverlb issue, the k3s server still failed to start. The
containerd logs showed:

```
failed to create image import watcher: too many open files
```

### Root cause

Linux exposes a kernel parameter `fs.inotify.max_user_instances` that caps how
many inotify instances a single user can hold open simultaneously. The default
value is **128**. Inotify is the Linux file-system event notification API;
containerd uses it internally to watch for image imports.

The devcontainer uses Docker-outside-of-Docker: all containers share the same
host Docker daemon and therefore the **same host kernel**. The `gitops-reverser`
cluster (1 server + 3 agents) was already running and had consumed most of those
128 slots. Starting a second cluster pushed the total over the limit.

### Fix

Added `ensure_inotify_limits()` to `test/e2e/cluster/start-cluster.sh`:

```bash
ensure_inotify_limits() {
  local current
  current="$(cat /proc/sys/fs/inotify/max_user_instances 2>/dev/null || echo 0)"
  if [[ "${current}" -lt 512 ]]; then
    echo "bumping fs.inotify.max_user_instances from ${current} to 512"
    docker run --rm --privileged alpine \
      sysctl -w fs.inotify.max_user_instances=512 >/dev/null
  fi
}
```

A container started with `--privileged` can write host kernel parameters via
`sysctl`. The write is not sandboxed — it propagates to the real kernel and
affects all containers immediately. 512 comfortably covers two four-node
clusters with headroom to spare.

### Lesson

In DooD environments, inotify limits are shared across every container on the
host. Any project that spins up multiple clusters simultaneously needs to budget
for this. You can check the current state with:

```bash
sysctl fs.inotify
```

The fix reverts to the system default on reboot, so it needs to be re-applied
each time (the script handles this automatically).

---

## 3. Traefik HelmRelease failed — Prometheus Operator CRDs missing

### What happened

The `flux-bootstrap` step timed out with:

```
Helm install failed for release traefik-system/traefik-system-traefik:
execution error at (traefik/templates/servicemonitor.yaml):
ERROR: You have to deploy monitoring.coreos.com/v1 first
```

### Root cause

The traefik Helm chart at v39 attempts to create a `ServiceMonitor` resource
(a Prometheus Operator CRD) when `metrics.prometheus.serviceMonitor.enabled: true`.
`ServiceMonitor` belongs to the API group `monitoring.coreos.com/v1`, which is
provided by the **Prometheus Operator**. Since the Prometheus Operator was not
installed, the CRD did not exist, and the Helm install failed hard.

### Fix

Two-part:

1. **Install the Prometheus Operator** via a Flux `Kustomization` pointing to
   the official `prometheus-operator/prometheus-operator` GitHub repo at a
   pinned tag. This installs the CRDs and the operator deployment.

2. **Decouple the ServiceMonitor from the HelmRelease.** Set
   `serviceMonitor.enabled: false` on the traefik HelmRelease so Helm never
   tries to create the CRD resource. Instead, the ServiceMonitor is created
   separately in `test/e2e/setup/manifests/` — a kustomization that is only
   applied after the Taskfile has confirmed the CRDs are established:

   ```bash
   kubectl wait --for=condition=Established \
     crd/prometheuses.monitoring.coreos.com \
     crd/servicemonitors.monitoring.coreos.com \
     --timeout=180s
   kubectl apply -k test/e2e/setup/manifests
   ```

### Lesson

When a Helm chart optionally creates CRD-backed resources, the CRDs must exist
**before** `helm install` runs. Helm's rendering step tries to look up the CRD
schema at install time — it doesn't gracefully skip unknown resources. The two
reliable patterns are:

- Install the CRDs first (separate Helm chart or Kustomization) and make the
  consumer wait for them.
- Disable the CRD-backed feature in the chart and create the resource separately
  after the CRDs are confirmed present.

---

## 4. Flux `HelmRelease.spec.dependsOn` cannot reference a Kustomization

### What happened

To fix the ordering problem above, a `dependsOn` block was added to the traefik
HelmRelease:

```yaml
spec:
  dependsOn:
    - name: prometheus-operator
      namespace: flux-system
```

This produced:

```
unable to get 'flux-system/prometheus-operator' dependency:
helmreleases.helm.toolkit.fluxcd.io "prometheus-operator" not found
```

### Root cause

In Flux v2, `HelmRelease.spec.dependsOn` can only reference other `HelmRelease`
objects. The prometheus-operator install is a `Kustomization` object.
`Kustomization.spec.dependsOn` can only reference other `Kustomization` objects.
Cross-type dependencies are not supported — you cannot make a `HelmRelease` wait
for a `Kustomization` directly.

### Fix

Removed `dependsOn` from the traefik HelmRelease. The ordering is handled at the
Taskfile level (explicit `kubectl wait` for CRDs, then `kubectl apply` for
manifests) rather than inside Flux's own dependency graph.

### Lesson

Flux dependency chains are type-scoped. If you need to express "install Helm
chart B after Kustomization A finishes", your options are:

- Wrap B's HelmRelease in a Kustomization and add `dependsOn` to that wrapper
  Kustomization.
- Use the `fluxcd.io/reconcile: enabled` annotation and handle ordering in
  your bootstrap script.
- Decouple the dependency from Flux entirely and enforce it in the deploy
  script (the approach taken here).

---

## 5. `spec.retryInterval` is not a valid field in HelmRelease v2

### What happened

An attempt was made to set `spec.retryInterval: 1m` on the traefik HelmRelease
so that a failed install would retry quickly. The apply was rejected:

```
strict decoding error: unknown field "spec.retryInterval"
```

### Root cause

`spec.retryInterval` existed in Flux v1's `HelmRelease` CRD but was removed or
restructured in `helm.toolkit.fluxcd.io/v2`. In v2, retry timing is controlled
by `spec.interval` only. Setting `interval: 60m` means a failed install retries
after 60 minutes — far too slow for an e2e bootstrap.

### Fix

Dropped the field. The retry problem was made irrelevant by solving the
ordering issue at the Taskfile level (see Issue 3 and 4).

### Lesson

When working with Flux v2 APIs, the v1 documentation and many blog posts are
misleading. Always validate against the actual CRD schema in your cluster:

```bash
kubectl get crd helmreleases.helm.toolkit.fluxcd.io \
  -o jsonpath='{.spec.versions[0].schema.openAPIV3Schema}' | jq '.properties.spec.properties | keys'
```

---

## 6. Proxy crashed — Helm rendered an integer as scientific notation

### What happened

The proxy pod entered `CrashLoopBackOff`. The startup error was:

```
invalid value "1.048576e+06" for flag -max-audit-body-bytes: parse error
```

### Root cause

The Helm chart passes `--max-audit-body-bytes={{ .Values.audit.maxBodyBytes }}`
to the proxy binary. The default value in `values.yaml` is `1048576` (1 MiB).

Helm's Go template engine unmarshals YAML values through a JSON round-trip
internally. This converts all numbers to `float64`. When a large integer is
formatted back to a string via Go's default `%v` verb, values above a certain
threshold are rendered in scientific notation: `1.048576e+06`.

Go's `flag` package uses `strconv.ParseInt` to parse integer flags. It does not
accept scientific notation, so the proxy exited immediately on startup.

### Fix

Added `| int` to the template:

```yaml
- --max-audit-body-bytes={{ .Values.audit.maxBodyBytes | int }}
```

Helm's `int` function converts the `float64` back to a Go `int` before the
string conversion, producing `1048576` instead of `1.048576e+06`.

### Lesson

Any Helm value that is a large integer and will be passed as a command-line
argument or environment variable should be explicitly coerced with `| int` or
`| int64`. Helm's rendering of raw numbers is not guaranteed to match what a
CLI flag parser expects. Small integers (below ~10,000) are typically safe
because `%v` on a float64 only switches to scientific notation above a certain
magnitude.

---

## 7. Go test failed — kubectl deprecation warning mixed into JSON output

### What happened

The smoke test reached the Go test phase but failed immediately:

```
smoke_test.go:67: decode json: invalid character 'W' looking for beginning of value
Warning: wardle.example.com/v1alpha1 Flunder is deprecated in v1.3+
{
  "apiVersion": "wardle.example.com/v1alpha1",
  ...
}
```

### Root cause

The test helper `kubectlClient.run()` used `cmd.CombinedOutput()`. This Go
standard library method captures **both stdout and stderr into a single byte
slice**. kubectl prints API deprecation warnings to **stderr**. The JSON object
body goes to **stdout**. Combined, the test received:

```
Warning: wardle.example.com/v1alpha1 Flunder is deprecated in v1.3+\n{...JSON...}
```

`json.NewDecoder` starts reading, hits the `W` in `Warning`, and returns a
parse error before it ever reaches the JSON object.

### Fix

Changed `run()` to use `cmd.Output()` (stdout only) with a separate
`bytes.Buffer` attached to `cmd.Stderr`:

```go
var stderr bytes.Buffer
cmd.Stderr = &stderr
output, err := cmd.Output()
if err != nil {
    t.Fatalf("... %s\n%s", output, stderr.String())
}
```

This keeps stderr available for error diagnostics without contaminating the
return value.

### Lesson

`CombinedOutput()` is convenient for logging but dangerous when the output will
be parsed. Any time you run an external command and need to parse stdout (JSON,
YAML, structured text), use `Output()` with a separate stderr sink. kubectl in
particular writes warnings, progress messages, and `Warning:` annotations to
stderr, and these will only increase as APIs are deprecated.

---

## General Infrastructure Improvements

Beyond the crash fixes, several structural improvements were made to align this
project with the patterns proven in `gitops-reverser`.

### `hack/e2e/load-image.sh` — containerd GC pinning

The original script loaded images into k3d using plain `k3d image import`.
Containerd's garbage collector can evict images that are not referenced by
running pods. Between `image import` and `helm upgrade`, a GC cycle could
remove the freshly loaded image, causing `ImagePullBackOff` on the first pod
start.

The rewritten script pins each imported image using containerd's
`io.cri-containerd.pinned=pinned` label, which prevents GC eviction. It also
uses a stamp file to skip re-loading unchanged images, which speeds up
incremental runs significantly.

### `hack/e2e/wait-flux-resources.sh` — suspended resource skip

The original script called `kubectl wait` over all HelmReleases and
Kustomizations. If any resource was **suspended** (`spec.suspend: true`), the
`kubectl wait --for=condition=Ready` call would block indefinitely — a suspended
resource never becomes Ready. The script now filters out suspended resources
before waiting. It also guards against the case where no resources are found at
all, which would silently pass.

### `.golangci.yml` — comprehensive linter config

Replaced a near-empty stub with a full configuration aligned to
`gitops-reverser`. This enables a solid core of linters (`errcheck`, `govet`,
`staticcheck`, `gocritic`, `gosec`, `revive`) with appropriate exclusions for
generated code and test files. Running `task lint` now catches real issues
rather than only trivial formatting problems.

### `.devcontainer/devcontainer.json` — persistent kubeconfig volume

Added a named Docker volume for `~/.kube`:

```json
"source=auditproxydotkube,target=/home/vscode/.kube,type=volume"
```

Without this, the kubeconfig is written to the container's writable layer and
is lost every time the devcontainer rebuilds or the container restarts. With the
named volume, `kubectl` context and credentials survive across container
restarts, so you do not need to re-run `task e2e:cluster-up` to regenerate the
kubeconfig after a restart.

### `.devcontainer/Dockerfile` — docker-ce-cli in the CI stage

The `docker-ce-cli` package was only installed in the `dev` stage, not the
`ci` stage. The CI jobs that build and test the project run as the `ci` stage
target. Any CI step that shells out to `docker` (for example, `k3d image
import` or `docker build`) would fail with `docker: command not found`. Adding
the Docker apt repository and `docker-ce-cli` to the `ci` stage fixes this.

### `test/e2e/cluster/README.md` — inotify explanation

Added a README to the cluster directory explaining the inotify limit issue,
why it manifests in DooD environments, how the fix works, and what to expect
when running two clusters concurrently. This is the kind of non-obvious
infrastructure knowledge that tends to get lost between sessions.
