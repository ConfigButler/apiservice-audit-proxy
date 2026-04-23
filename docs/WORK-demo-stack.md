# Work Plan: Demo Stack & Audit Event Viewer

## Goal

Make it trivially easy for someone evaluating `apiservice-audit-proxy` to deploy a full working demo — including a backend API server and a live viewer for the audit events the proxy emits. This turns the project from "here's a component" into "here's a running thing you can see working in five minutes."

---

## Current State

| Component | Status |
|---|---|
| `apiservice-audit-proxy` | Deployed via Helm chart |
| `sample-apiserver` (backend) | kustomize only, used in e2e tests |
| `mock-audit-webhook` | Binary + Dockerfile, kustomize only, used in e2e tests |
| Web UI for audit events | Does not exist |

The e2e setup already wires all three together, so the logic is proven. The gap is a user-facing path to get there.

---

## Phase 1 — Helm Chart: Optional Demo Deployments

Add two optional sub-deployments to the existing Helm chart, both **disabled by default**.

### 1a. `testApiserver` (sample-apiserver)

The upstream Kubernetes sample-apiserver (`registry.k8s.io/e2e-test-images/sample-apiserver`) is a public image — no build needed. Include it as an optional Deployment + Service + RBAC in the chart.

Proposed `values.yaml` additions:

```yaml
testApiserver:
  enabled: false
  image:
    repository: registry.k8s.io/e2e-test-images/sample-apiserver
    tag: "1.33.8"
  replicaCount: 1
  resources: {}
```

When `enabled: true`, the chart should also auto-configure:
- `backend.url` pointing to the in-cluster service
- The `apiService` group/version to match the sample server

### 1b. `mockAuditWebhook`

The `mock-audit-webhook` binary already exists at `cmd/mock-audit-webhook/` with its own Dockerfile target and kustomize manifests in `test/e2e/`. Promote it to an optional Helm deployment.

Proposed `values.yaml` additions:

```yaml
mockAuditWebhook:
  enabled: false
  image:
    repository: ghcr.io/configbutler/mock-audit-webhook
    tag: ""  # defaults to chart appVersion
  replicaCount: 1
  listenAddress: ":9444"
  maxStoredEvents: 200
  service:
    type: ClusterIP
    port: 9444
  resources: {}
```

When `enabled: true`, the chart should also auto-configure:
- `webhook.kubeconfigSecretName` pointing at a generated kubeconfig for the in-cluster mock webhook service

### Convenience: `demo` preset

Consider a top-level `demo.enabled: true` flag that sets sensible defaults for both of the above, so a full demo install is a single override.

---

## Phase 2 — Audit Event Viewer UI

Extend `mock-audit-webhook` with a minimal real-time web UI so audit events are immediately visible in a browser.

### What it should do

- Serve a static HTML page at `/`
- Stream incoming audit events to the browser via **Server-Sent Events (SSE)** — no WebSocket complexity, no frontend framework, no build step
- Display events as formatted, syntax-highlighted JSON with newest-first ordering
- Show key fields prominently: `verb`, `requestURI`, `user.username`, `objectRef`, `responseStatus.code`, timestamp
- Include a clear/filter control

### Why SSE over WebSockets

SSE is HTTP/1.1 compatible, works through standard ingresses and kubectl port-forward without any special handling, and requires zero client-side setup. For a read-only event stream it is the right tool.

### Implementation sketch

The Go side already has the ring buffer and the POST handler. The additions are:

1. An SSE endpoint (`GET /events/stream`) that registers a channel and broadcasts each received event
2. A single embedded HTML file (using Go's `embed` package) served at `/ui`
3. A minimal `<script>` block using the native `EventSource` API — no npm, no bundler

This keeps the binary self-contained and avoids any frontend toolchain dependency.

---

## Phase 3 — Decision: Separate Repo for `audit-webhook-receiver`?

This is the substantive architectural question. Here is the full tradeoff analysis.

### What the tool would be if extracted

A standalone Kubernetes audit webhook receiver that:
- Accepts `audit.k8s.io/v1` event batches from any audit source (kube-apiserver, this proxy, or any other)
- Stores and streams events via SSE
- Provides a browser UI for live inspection

It would be useful beyond this project — anyone who wants to inspect raw audit events during development or debugging could use it.

---

### Arguments FOR a separate repo

**1. Independent utility**
The tool is genuinely useful without `apiservice-audit-proxy`. Any Kubernetes cluster with a webhook audit backend could point at it. That user base is much larger than users of this proxy.

**2. Independent release cycle**
UI improvements, new event visualization features, and bug fixes are unrelated to proxy logic. Coupling them to this repo means the audit viewer gets a new version only when the proxy does.

**3. Cleaner focus for this repo**
`apiservice-audit-proxy` is a production infrastructure component. Bundling a dev/demo tool muddies that identity, especially if the UI grows in complexity.

**4. Discoverability**
A standalone tool with its own name, README, and GitHub topic tags is more findable than a subdirectory inside another project.

**5. Reuse in other projects**
If you ever build other aggregated API server tooling, you'd want to reuse the audit viewer without taking a dependency on the proxy repo.

---

### Arguments AGAINST a separate repo (keep it here)

**1. Premature extraction**
The tool is currently ~150 lines of Go and serves one purpose: validating that this proxy emits correct events. Extracting it before it has proven standalone value adds maintenance overhead (CI, releases, versioning, issues) for uncertain gain.

**2. Helm chart coupling**
The most user-visible integration point is the Helm chart. With a separate repo, the chart would need to reference an external image with its own release tag. That's a versioning surface to keep in sync.

**3. Development friction**
Right now you can change the proxy and the mock webhook in one PR and test them together. A split means coordinating across repos for changes that span both.

**4. No external demand yet**
The right time to extract is when external users ask for it or when it clearly grows beyond this project's scope. Neither has happened yet. Optimizing the repo structure for hypothetical future users is premature.

**5. Shared Dockerfile**
Both binaries share the same multi-stage Dockerfile and build pipeline. Splitting means duplicating or significantly restructuring the build.

---

### Recommendation

**Keep it in this repo for now. Revisit after Phase 2 ships.**

The extraction threshold should be: external users are asking to use the viewer with sources other than this proxy, *or* the UI grows to the point where it warrants its own toolchain (i.e. it can no longer be maintained as a single embedded HTML file).

A practical middle ground: give the binary a distinct name (`audit-event-viewer` or `audit-webhook-receiver`) and document it explicitly in the README as a standalone tool that happens to ship with this project. That improves discoverability without the cost of a repo split. If demand materializes, extraction becomes a clean cut rather than a messy untangling.

---

## CI: Publishing a Second Container Image

The `mock-audit-webhook` image can be published from the existing CI pipeline with minimal changes. The infrastructure is already mostly in place.

### What already works

- **Dockerfile is ready.** The `BINARY` build arg already switches between the proxy (`./cmd/server`) and the webhook (`./cmd/mock-audit-webhook`). No Dockerfile changes needed to produce a second image.
- **The `publish` reusable workflow is parameterized.** `multi-arch-publish.yml` accepts `image-name` as an input, so it can be called a second time with `configbutler/mock-audit-webhook`.
- **Jobs can run in parallel.** A new `docker-build-mock-webhook` job is independent of the existing `docker-build` job.

### What the change looks like

1. Add `MOCK_WEBHOOK_IMAGE_NAME: configbutler/mock-audit-webhook` to the top-level `env:` in `ci.yml`.
2. Duplicate the `docker-build` job → `docker-build-mock-webhook`, with `build-args: BINARY=mock-audit-webhook` and the new image name.
3. Duplicate the `publish` job → `publish-mock-webhook`, same reusable workflow, different `image-name`.
4. If the webhook image is needed in `e2e-smoke`, add `docker-build-mock-webhook` as a dependency and expose the image via an env var.

### One unknown to verify first

The shared action `configbutler/.github/actions/docker-build@main` is called today **without** a `build-args` input. Confirm it exposes that parameter before starting. If not, either add it to the shared action (one-line change) or use `docker/build-push-action` directly for this job.

### Minor Dockerfile wart

The builder always outputs to `/out/apiservice-audit-proxy` and the entrypoint is hardcoded to `/apiservice-audit-proxy` regardless of which binary is built. This is confusing but functional. Worth a two-line fix when doing this work.

---

## Alternative Demo Receiver: `webhook-tester`

[tarampampam/webhook-tester](https://github.com/tarampampam/webhook-tester) is an open-source Go project that accepts arbitrary webhook payloads, stores them in memory, and provides a browser UI to inspect them. It is worth considering as a zero-maintenance alternative to `mock-audit-webhook` for the quick-start demo path.

### What makes it attractive

- Public image, no build required — drops straight into the Helm chart as an optional deployment.
- In-memory mode means no external storage dependency.
- Generic receiver: the proxy's `audit.k8s.io/v1` EventList payload arrives as JSON and is displayed as-is.
- Maintained upstream; bug fixes and UI improvements come for free.

### The tradeoff

webhook-tester is a generic payload inspector — it has no knowledge of Kubernetes audit event structure. It will not highlight `verb`, `user.username`, `objectRef`, or response codes. For "does the proxy emit events?" it is sufficient. For a polished demo that communicates _what_ the events mean, the planned `mock-audit-webhook` SSE UI (Phase 2) is the better choice.

### TLS requirement for the outbound webhook

The kubeconfig spec documents `cluster.server` as an `https://` address, and all surrounding options (`certificate-authority`, `insecure-skip-tls-verify`, `client-certificate`, `client-key`) are TLS-oriented. **Plain `http://` is not a supported configuration** — treat it as fragile even if it happens to work in a given client-go version.

The documented escape hatch for a demo where you do not want to manage a CA is:

```yaml
clusters:
  - cluster:
      server: https://webhook-tester:port/api/<uuid>
      insecure-skip-tls-verify: true
```

webhook-tester serves HTTPS, so this works out of the box. The proxy already has `--backend-insecure-skip-verify` for the analogous backend case, so the pattern is established. The Helm chart can generate a kubeconfig with `insecure-skip-tls-verify: true` when deploying webhook-tester in demo mode.

This also reinforces why mTLS is the right default for production: it is exactly the shape Kubernetes audit webhooks expect.

---

## The "Side-by-Side" Demo: Showing Why This Tool Exists

The most compelling demo is not "the proxy forwards events" — it is "here is what you are _missing_ without this proxy."

### The concept

Run two webhook receivers simultaneously:

| Lane | Webhook source | Shows |
|---|---|---|
| **A** | Native K8s apiserver audit policy | Events from the main apiserver; aggregated API server requests **absent** |
| **B** | This proxy's outbound webhook | Events forwarded from the aggregated API server |

A user does a `kubectl get foos.example.com`. Lane A is silent. Lane B lights up. That is the "why does this exist" story in one visual, no explanation needed.

### Why it works as a demo

webhook-tester's UUID-per-namespace model means you get two independent endpoints from one deployment. Open two browser tabs, one per UUID. The contrast between them makes the value proposition self-evident.

### The constraint: apiserver audit policy requires cluster-level configuration

Lane A requires configuring the K8s apiserver with an audit policy file and a webhook backend config. In k3d, this means extra flags at cluster creation time (`--k3s-arg`). This cannot be done from inside the cluster — it is not something a Helm chart can set up.

**Conclusion:** the side-by-side demo belongs in the **Taskfile/Tilt dev environment**, not the Helm chart. Script the k3d cluster creation to include the native audit webhook config, and the two-lane view comes for free. It is the right demo for a developer evaluating the proxy; the Helm-only path is for operators deploying it.

---

## Sequencing

| Step | Effort | Value | Dependency |
|---|---|---|---|
| 1a. Helm: testApiserver subchart | Small | High — completes the demo path | None |
| 1b. Helm: mockAuditWebhook subchart | Small | High — closes the audit event loop | None |
| 2. SSE stream + browser UI | Medium | High — makes the demo visual and compelling | 1b |
| 3. Repo extraction decision | Low (decision) | — | Phase 2 shipped and usage observed |

Phases 1a and 1b can be done in parallel. Phase 2 builds directly on 1b being merged and the image being published.
