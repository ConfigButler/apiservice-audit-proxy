# E2E Test Suite Summary

This document explains what each end-to-end test in
[test/e2e/](../test/e2e/) actually proves, where the coverage is solid,
where it is thin, and a couple of known gaps with the reasons we have not
closed them yet.

All e2e tests live under the `e2e` build tag and run against a real `k3d`
cluster spun up by `task e2e:cluster-up`. Each suite has its own
`task e2e:test-*` target; a single aggregate `task e2e:test-all` runs them
all in the order CI uses.

## TL;DR

| Area | Test | Verdict | Runtime |
|---|---|---|---|
| Audited write | `TestSmoke` | Strong | <1s |
| Audited write — explicit backend CA | `TestSmoke` (re-run via `e2e:test-smoke-backend-ca`) | Strong | <1s + redeploy |
| Native vs proxy audit comparison | `TestAggregatedAPIAuditGap` | Strong | ~5s |
| Collection delete is audited | `TestAggregatedAPIDeleteCollectionAudit` | Strong | ~1s |
| Image lifecycle | `TestImageRefresh*` (3 tests) | Strong | ~15s |
| Long watch survives | `TestWatchStaysOpenThroughProxy` | Strong (PR #8 regression guard) | ~45s |
| Metrics scrape + stream classification | `TestProxyMetricsScrapeAfterWatch` | Strong | ~2s |
| Requestheader trust loaded from cluster | `TestRequestHeaderTrustFromCluster` | Strong but slow | ~90s |
| Impersonation — write, passthrough, apiserver extras | `TestImpersonationWrite/Passthrough/ApiserverExtras` | Strong | ~4s |
| Impersonation — missing RBAC | `TestImpersonationRBACMissing` | Strong | <1s |

Total wall-clock for `task e2e:test-all` against a warm cluster: roughly
**3 minutes**.

## How to run them

```bash
# Run everything (this is what CI calls):
task e2e:test-all

# Run a single suite while iterating:
task e2e:test-watch-streams
task e2e:test-metrics-scrape
task e2e:test-smoke
```

`task e2e:test-all` is wired into [.github/workflows/ci.yml](../.github/workflows/ci.yml)
as the only e2e command, so adding a new e2e suite means adding a
`task: e2e:test-…` line to the `e2e:test-all` cmds list — CI picks it up
automatically.

## What each test proves

### `TestSmoke`
Creates a `Flunder` through the aggregated API, fetches it back, and waits
for the proxy to emit the matching `ResponseComplete` audit event into the
test webhook receiver. Re-run with a different deployment via
`e2e:test-smoke-backend-ca` to cover the explicit backend-CA-trust path.

**Strong because**: it is the canonical golden path. If this breaks,
everything else is suspect.

### `TestAggregatedAPIAuditGap`
The "demo" test. Drives one aggregated-API write and asserts:
- Lane B (proxy webhook) sees a *complete* event with `RequestObject` and
  `ResponseObject` populated.
- Lane A (kube-apiserver native audit, recorded by the test webhook
  receiver) sees a *hollow* event — `ObjectRef.Name` missing, no decoded
  objects — which is the gap this proxy exists to close.

**Strong because**: it pins the *motivation*, not just the mechanism. If
kube-apiserver ever started returning rich aggregated-API events, this test
would fail loudly and we would re-evaluate.

### `TestAggregatedAPIDeleteCollectionAudit`
Covers the `deletecollection` verb, which is distinct from `delete` in
Kubernetes `RequestInfo` (a `DELETE` with no object name is rewritten to
`deletecollection`) and was silently dropped before it was added to the
`shouldAudit` allow-list. The test seeds two flunders and issues a single raw
collection `DELETE` (`kubectl delete --raw`, which deterministically produces
the verb — `kubectl delete --all` instead lists and deletes each member by
name), then asserts the proxy (Lane B) emits a complete event with no
`ObjectRef.Name` (a collection op has no single object), the correct resource
coordinates, `ResponseStatus.Code` 200, and a captured response body.

**Strong because**: it is a regression guard for the verb gap, verified to go
red against a pre-fix binary. The body shape is caller-dependent — a raw delete
returns the deleted-items list, whereas a namespace-teardown `deletecollection`
returns an empty body — so the test keys its hard assertions off `ObjectRef`
and status, matching what downstream consumers rely on. Both real wire forms
are checked in under [examples/](examples/) as paired Lane A/Lane B payloads.

### `TestImageRefresh*` (3 tests)
Validates that `task e2e:load-image` correctly invalidates pod image
digests when the source changes, and stays a no-op when nothing changed.
This is infrastructure-level confidence in the iteration loop, not the
proxy itself, but a regression here makes every other e2e flaky.

### `TestWatchStaysOpenThroughProxy`
Opens a `kubectl get flunders --watch`, sleeps 45 seconds (deliberately
past the old 30s `WriteTimeout` regression window), then creates an object
and asserts the streamed `ADDED` event arrives. Stderr is drained
concurrently and scanned at the end for `INTERNAL_ERROR`, `stream error`,
`unexpected EOF` — the literal HTTP/2 reset symptoms from the original
production incident.

**Strong because**: it is a regression guard against the exact incident
PR #8 was opened to fix. The 45-second sleep is principled — anything
shorter cannot reach the regression window.

### `TestProxyMetricsScrapeAfterWatch`
Reaches `/metrics` through the chart Service port using
`kubectl get --raw /api/v1/namespaces/.../services/...:metrics/proxy/metrics`
(no local port-forward needed). Drives one chunked list and one watch,
then asserts:
- `requests_total{verb="list",streaming="false"}` is present (chunked
  responses are not misclassified as streams).
- `requests_total{verb="watch",streaming="true"}` is present.
- `stream_duration_seconds{kind="watch"}_count` increased by at least one
  after closing our watch (per-scenario delta — robust against other
  in-flight watches owned by kube-controller-manager).
- `transport_bytes_total{leg="backend",streaming="true",direction="read"}`
  is non-zero.

**Strong because**: it is the only test that exercises the metrics
plumbing end-to-end through the chart, and it pins the Section 2
narrowing (chunked != streaming) on the live wire instead of just in unit
mocks.

### `TestRequestHeaderTrustFromCluster`
Confirms the proxy boots without a CA Secret mounted: it must read
requestheader CA + username headers live from
`kube-system/extension-apiserver-authentication`. Then deletes the
required RoleBinding and asserts the proxy *fails* to start — i.e., it
does not silently fall back to an insecure mode.

**Strong because**: it covers a security-critical bootstrap path and a
negative case. **Slow (~90s)** because it has to do a real proxy restart
with a deliberately-broken cluster, then revert.

### `TestImpersonationWrite / Passthrough / ApiserverExtras`
Re-deploys the proxy with `backend.identity.mode=impersonation` and the
operator-supplied RBAC. Covers:
- An audited write through the impersonation path.
- A non-audited GET through impersonation.
- Apiserver-injected `extra` fields surviving end-to-end.

**Strong because**: impersonation is the harder of the two identity modes
and is easy to break with header-handling edits.

### `TestImpersonationRBACMissing`
Deploys impersonation mode *without* the necessary `ClusterRoleBinding`
and asserts that a write fails with a clean 403 (proxied from the
backend), not a confusing 5xx from the proxy itself.

**Strong because**: covers the "operator forgot the RBAC" failure mode,
which is the most likely real-world misconfiguration.

## Areas that could become better

### Reduce wall-clock on `TestRequestHeaderTrustFromCluster`
~90s is dominated by a proxy redeploy + readiness wait + revert cycle.
Possible improvements:
- Pre-bake an alternate deployment so the revert is a kubectl apply, not a
  redeploy.
- Replace the post-revert readiness wait with a tighter probe.

Not urgent — the test exists, it is correct, and the slowness only hits
CI once per run.

### Tighten `TestProxyMetricsScrapeAfterWatch` against accidental fan-in
The test currently anchors on per-scenario deltas (which is the right
choice — the cluster has other watches in flight). But if a future
addition records the *same* `verb=list,resource=flunders` combination from
unrelated traffic, the positive assertion could become noise-driven. If
that ever happens, the fix is to assert the *counter delta* between two
scrapes around our list, not absolute presence.

### Watch test could carry more shape assertions
`TestWatchStaysOpenThroughProxy` proves the stream survived, but it does
not assert the response framing matched
`Content-Type: application/json;stream=watch`. A future addition could
diff the response headers proxy-side vs backend-side to lock in the
passthrough posture.

### No e2e for the upgrade passthrough fix (Section 1 of the hardening plan)
We have unit coverage that drives a raw `101 Switching Protocols` through
the proxy via `net.Dial` (see
[`TestHandler_PassthroughUpgradeRequest`](../pkg/proxy/handler_test.go)),
but no e2e. See the next section for why.

### No e2e for the Helm chart `fail` guards
We have shell-level proof (`helm template --set …` exits non-zero with the
expected message), but no Go test. If chart-side bugs become a recurring
problem, a small `chart_guards_test.go` that shells out to `helm template`
and asserts on stderr would close that.

## Known gaps and why they are still gaps

### Why we do not have an e2e for HTTP upgrade (websocket / SPDY) passthrough

PR #8's Section 1 fix (`metricResponseWriter.Unwrap` + leaving the
`io.ReadWriteCloser` body intact on `101 Switching Protocols`) is covered
in **unit** tests with a real raw HTTP/1.1 conversation. We did not add an
e2e equivalent because there is no upgrade-speaking backend reachable
through this proxy in the test stack:

1. **The proxy fronts an aggregated API, not the core API.** The standard
   upgrade endpoints in Kubernetes — `pods/exec`, `pods/portforward`,
   `pods/attach` — live in the core API and are served by kube-apiserver
   directly, not via any `APIService`. The proxy never sees them.

2. **The aggregated test backend (`wardle`) does not implement an upgrade
   subresource.** It is a toy aggregated API server that registers a
   `Flunder` CRD-shaped resource for testing the audit gap. It has no
   handlers that respond with `101 Switching Protocols`, so even if we
   pointed `kubectl exec` at it, there is nothing to exec into.

3. **Adding an upgrade-speaking backend is invasive.** We would need to:
   - Build a new test-only aggregated API server that exposes a
     subresource which responds with `101 Switching Protocols` + raw
     duplex IO, OR
   - Repoint the proxy at the core kube API for the duration of the test
     (which contradicts the proxy's deployment model).

The unit test
[`TestHandler_PassthroughUpgradeRequest`](../pkg/proxy/handler_test.go)
drives the proxy via `httptest.NewServer` + `net.Dial` and a manually
hand-crafted backend that hijacks its connection and writes a real `101`
+ post-upgrade payload. Asserting that the proxy returns
`HTTP/1.1 101 Switching Protocols` and never `502 Bad Gateway` is
wire-level coverage of the same code path the e2e would have exercised —
the difference is just whether the bytes flow through `k3d` or through
loopback.

If this ever becomes a recurring regression area, the cheapest e2e
upgrade would be to ship a small SPDY/websocket-speaking aggregated API
in the test stack and add one `TestUpgradePassthrough` that dials it
through the proxy. Until then, the unit test is the canonical guard.

### Why we do not yet exercise the chart `fail` guards in Go

The three template guards added to
[`charts/.../templates/deployment.yaml`](../charts/apiservice-audit-proxy/templates/deployment.yaml)
are tested by shelling out to `helm template --set …` with the
red-condition values and confirming a non-zero exit + the expected error
text. We have not promoted that to a Go test because chart rendering is
not a frequent regression site and `helm template` is a perfectly fine
oracle. If we add more guards or start parameterising them, that calculus
changes.
