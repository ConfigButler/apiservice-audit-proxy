# Audit Pass-Through APIServer OSS Step 2 Plan

## Goal

This plan describes the second step after [`OSS-PLAN.md`](./OSS-PLAN.md) is executed.

Step 1 makes this folder easy to build, test, lint, package, and release where it currently lives.
Step 2 turns it into a genuinely strong standalone project by doing three things:

- extract it into its own repository cleanly
- add k3d-backed end-to-end coverage as a first-class quality gate
- make certificate setup and rotation much easier for first-time users

The intent is to keep the project small, but remove the two biggest adoption risks:

- "does this really work in a cluster?"
- "how do I get the TLS and trust model right without hand-rolling everything?"

## Why This Is Step 2

This should happen only after the packaging baseline from `OSS-PLAN.md` exists.

Reasons:

- extraction is much easier once the project already has its own `Taskfile`, Helm chart, CI, and
  devcontainer
- e2e wiring benefits from having a stable chart and release artifact flow first
- certificate UX is much easier to document and test once the packaging surface is settled

## Current Repo Status

The repository now has the first concrete slice of this step implemented:

- the shell smoke path has been replaced by a Go e2e package under `test/e2e/`
- the local task surface includes both `task e2e:test-smoke` and
  `task e2e:test-smoke-backend-ca`
- the proxy can verify delegated requestheader identity with `--client-ca-file`
- the chart can mount a requestheader client CA Secret for that trust path

That shifts the remaining Step 2 work away from basic feature wiring and toward:

- stabilizing the live k3d execution environment
- deciding how much of the e2e lane becomes CI-gated before extraction
- finishing standalone-repo cutover details

## Desired End State

At the end of Step 2, the project should support this story:

1. the code lives in its own repository
2. contributors can run unit checks and k3d e2e locally through one task entrypoint
3. CI runs at least one k3d-backed e2e scenario on pull requests or on a protected branch path
4. first-time users get a supported certificate path with rotation guidance instead of needing to
   invent one
5. Helm values and docs clearly separate:
   - easy local/dev install
   - recommended production-ish install

## Workstreams

### 1. Extract Into Its Own Repository

Promote this folder from "well-packaged subproject" to "standalone repo".

Scope:

- copy or move the contents of this folder into a new repository
- preserve history if practical, but do not block on perfect history surgery
- keep the module name, image name, chart name, and release artifacts aligned
- remove references that only make sense inside `gitops-reverser`

Likely cleanup needed during extraction:

- replace links back into the parent repo where they are currently hard-coded
- vendor or recreate any helper scripts that the standalone repo still needs
- move any borrowed docs into the new repo if they remain operationally relevant
- make the README stand on its own without assuming knowledge of GitOps Reverser internals

Recommended extraction strategy:

1. finish Step 1 inside this repo
2. create the new standalone repo
3. copy the already-working project layout into it
4. fix repo-local paths, org names, image names, and release locations
5. get the full local + CI flow green in the new repo

Recommendation:

- optimize for clean standalone maintenance over preserving every internal link
- if history extraction becomes expensive, prefer a fresh repo with clear attribution

### 2. k3d E2E As A First-Class Gate

For a tool like this, unit tests are not enough. The most important risk is integration:

- `APIService` routing
- proxy insertion into the aggregated API path
- backend TLS behavior
- delegated identity propagation
- webhook delivery
- real certificate/trust wiring

So Step 2 should add a real k3d-backed e2e lane modeled after the main project’s strongest e2e
patterns.

What to borrow from the main repo:

- k3d cluster lifecycle
- local image build and image load flow
- task-driven e2e orchestration
- explicit cluster prep scripts
- reproducible test fixtures instead of one-off manual commands

Recommended standalone e2e layout:

- `test/e2e/`
- `test/e2e/cluster/`
- `hack/e2e/`
- `Taskfile.e2e.yml` or a dedicated `task e2e:*` namespace

Recommended first e2e scenario:

- one smoke path centered on `APIService` proxy behavior, not a broad matrix

Concrete first flow:

1. create the k3d cluster and install cert-manager
2. deploy the real sample-apiserver behind a distinct backend `Service` such as `api-backend`
3. deploy the audit pass-through proxy as the `APIService` target `Service`
4. let cert-manager issue the proxy serving certificate and mount that Secret into the proxy pod
5. let cert-manager inject the serving CA into `APIService.caBundle` so kube-apiserver validates
   the proxy over HTTPS without `insecureSkipTLSVerify`
6. let cert-manager issue the proxy's backend client certificate and mount that Secret into the
   proxy pod so the real sample-apiserver accepts the proxied hop
7. reuse the existing audit webhook receiver kubeconfig model for the proxy's outbound webhook POST
8. issue one mutating Wardle request such as `kubectl apply -f flunder.yaml`
9. assert:
   - the request succeeds through the aggregated API path
   - `apiservice/v1alpha1.wardle.example.com` stays `Available`
   - the real backend receives the request successfully
   - the webhook receiver gets one synthetic `EventList`
   - that event contains the recovered fields this project exists to restore:
     `objectRef.name`, `requestObject`, and `responseObject`

Recommended first CI posture:

- run unit/lint on every PR
- run a slim k3d e2e smoke scenario on PRs if runtime is acceptable
- if PR runtime is too expensive, run smoke e2e on merge to main plus manual PR dispatch

Recommended local task surface:

- `task e2e:cluster-up`
- `task e2e:build-image`
- `task e2e:load-image`
- `task e2e:test`
- `task e2e:test-smoke`
- `task e2e:cluster-down`

Important:

- keep the first e2e lane narrow and reliable
- do not try to reproduce every edge case in v1
- prefer one trustworthy smoke path over a broad but flaky suite
- base that first smoke path directly on cert-manager-issued certificates so the supported TLS story
  is exercised from day one

### 3. Certificate UX For First-Time Users

This is the biggest usability gap after packaging.

Today, a new user has to understand several tricky things at once:

- the proxy’s own serving certificate
- the backend trust configuration
- the webhook trust/configuration model
- certificate rotation implications
- where CA material needs to live

Step 2 should turn that into a documented and supported installation model instead of a collection
of advanced operator assumptions.

Recommended product direction:

- support one simple dev path
- support one recommended rotating-cert path
- document the trust model clearly for both

For Step 2 e2e specifically, prefer the rotating-cert path first.

Reason:

- the key integration risk is not "can we mount any cert Secret at all?"
- the key integration risk is "does `APIService` proxying work when the proxy is fronted by
  cert-manager-issued serving TLS and real trust injection?"

Recommended supported modes:

1. Local/dev mode
   - optimized for k3d and quick experimentation
   - can use simplified trust where appropriate
   - clearly marked as non-production

2. Managed certificate mode
   - optimized for real cluster installs
   - uses cert-manager for the proxy serving certificate
   - documents how trust is established and what rotates automatically

### 4. Simple Dev Certificate Story

The dev story should be intentionally easy.

Recommendation:

- ship a chart mode that can generate or mount a simple self-signed serving cert for the proxy
- allow a clearly labeled dev-only path that uses `APIService.spec.insecureSkipTLSVerify: true`
  where acceptable for local testing
- provide a task or script that prepares the minimal secrets/config needed for a local cluster

This is still worth shipping, but it should not be the first e2e gate.

Why:

- it is a convenience path
- it does not prove the recommended cert-manager trust flow
- it can follow after the cert-manager-backed smoke scenario is stable

Success criteria:

- a first-time user can get a working local install without needing to understand cert-manager
  internals
- the docs say plainly that this is for local/dev use only

### 5. Recommended Rotating-Cert Story

For the "real" path, the project should help users adopt auto-rotating certificates correctly.

Recommendation:

- add optional chart support for cert-manager-managed serving certificates
- create a small, opinionated default that users can enable rather than forcing them to compose it
  all themselves
- document the exact ownership model:
  - what cert-manager rotates automatically
  - what does not rotate automatically
  - what trust anchor remains stable

The goal is not to hide TLS complexity completely. The goal is to give users a paved road.

Recommended chart behavior:

- `certificates.mode: dev-self-signed | cert-manager | existing-secret`
- `existing-secret` stays available for advanced users
- `cert-manager` mode creates the minimum resources needed for a sane default
- values and notes explain what Secret names and mounts are expected

For the first e2e lane, keep the cert-manager shape minimal:

- one namespace-scoped `Issuer` for the proxy serving certificate
- one `Certificate` for the proxy serving Secret mounted into the proxy pod
- one `APIService` annotation or equivalent wiring so cert-manager injects the serving CA bundle
- one backend client CA plus one proxy client `Certificate` if the sample-apiserver continues to
  require client authentication from the immediate caller
- no extra certificate modes in the first smoke path beyond what is needed to make the proxy work

Minimal manifest split to target in e2e:

- `APIService v1alpha1.wardle.example.com` points at the proxy `Service`
- the proxy forwards to a distinct backend `Service`
- the proxy mounts:
  - the cert-manager-issued serving Secret
  - the cert-manager-issued backend client cert Secret
  - the webhook kubeconfig Secret
- the real sample-apiserver trusts the backend client CA Secret

This keeps the e2e story concrete:

- cert-manager owns certificate issuance
- kube-apiserver validates the proxy with an injected CA bundle
- the proxy authenticates to the backend with a mounted client cert
- the test proves the real `APIService` proxy topology rather than a dev-only shortcut

Recommended docs to include:

- "local/dev install"
- "cert-manager install"
- "bring your own certs"
- troubleshooting for expired or mismatched certs

### 6. Trust Model Documentation

Certificate UX is not just about generating certs. It is also about helping users understand which
connections are being authenticated.

The standalone repo should document at least these trust boundaries:

- kube-apiserver to audit pass-through proxy
- audit pass-through proxy to real aggregated backend
- audit pass-through proxy to audit webhook receiver
- delegated header trust assumptions

Recommended outcome:

- one short operational TLS guide in the standalone repo
- one diagram or flow section in the README or docs
- no silent trust assumptions hiding in example values

### 7. Release And Artifact Improvements

Once the project is standalone and has e2e + certificate modes, the release bundle should help
users choose the right install path.

Recommended release outputs:

- container image
- Helm chart
- `install-dev.yaml` or dev-focused chart example
- `install-cert-manager.yaml` or equivalent documented chart values example

This does not require two separate products. It just means the release should surface the supported
paths clearly.

## Suggested Implementation Order

1. Complete `OSS-PLAN.md`.
2. Add a narrow k3d smoke e2e path while the project is still in this repo.
3. Stabilize the Helm chart values needed by e2e.
4. Add the first user-friendly dev certificate path.
5. Add the cert-manager-backed rotating-cert path.
6. Extract to the standalone repository.
7. Re-point CI, release publishing, image names, and chart metadata in the new repo.
8. Make the standalone repo’s unit, lint, Helm, and e2e flows pass end-to-end.

## Definition Of Done

Step 2 is complete when:

- the project lives in its own repository
- local contributors can run a documented k3d smoke e2e flow
- CI runs at least one real k3d-backed e2e scenario
- the Helm chart supports an easy dev certificate path
- the Helm chart supports a recommended cert-manager-based rotating-cert path
- the docs explain which certificate path new users should choose
- the docs clearly explain the remaining trust limitations around delegated identity

## Explicit Non-Goals

Keep Step 2 focused. Do not turn it into a full platform rewrite.

Out of scope:

- full production hardening of every TLS/auth path
- solving every possible PKI environment
- generalized multi-cluster installation logic
- replacing kube-apiserver audit webhook mechanics
- broad feature expansion unrelated to extraction, e2e, or certificate usability

## Notes On Fit With The Current Prototype

This Step 2 plan fits the current prototype direction well:

- the binary already has the key TLS flags needed for meaningful e2e
- the existing docs already discuss trust and TLS tradeoffs
- the main repo already contains proven k3d and cert-manager patterns worth borrowing

That means Step 2 is not a direction change. It is a maturity pass:

- from prototype packaging
- to standalone validation
- to user-friendly deployment guidance
