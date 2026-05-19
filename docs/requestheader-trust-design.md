# Design: cluster-sourced requestheader trust for `apiservice-audit-proxy`

This document describes how the proxy should obtain its inbound trust
configuration — the requestheader CA, allowed client names, and identity
header names — from the cluster's `kube-system/extension-apiserver-authentication`
ConfigMap instead of from operator-supplied flags and a mounted Secret.

It is a follow-up to the security checklist in
[`background-authz.md`](background-authz.md), section A, items
"Read trust configuration from extension-apiserver-authentication" and
"Bind extension-apiserver-authentication-reader". It builds on the impersonation
work in [`impersonation-design.md`](impersonation-design.md).

## Goal

Make safe inbound trust the default, with no per-install configuration.

Today an operator must:

1. obtain the cluster's front-proxy CA certificate,
2. load it into a Secret,
3. set `requestHeader.clientCASecretName`, `requestHeader.clientCAFileName`,
   and `requestHeader.allowedNames`,
4. keep all of that in sync when the cluster rotates its aggregator CA.

Every one of those steps can be done wrong, and one wrong way — omitting
`--client-ca-file` — currently produces a proxy that trusts `X-Remote-*`
headers with no verification at all. "Safe" should not be opt-in.

Kubernetes already publishes exactly this trust configuration, for exactly
this purpose, in a ConfigMap every aggregated API server is expected to read.
The proxy should read it.

### Non-goals

- Delegated *authorization* (`SubjectAccessReview`). The proxy still does no
  authorization of its own; see `background-authz.md` section C.
- Delegated bearer-token authentication (`TokenReview`). The proxy's inbound
  contract is requestheader only; it is not a general apiserver.
- Changing the outbound (backend) identity model. Requestheader-forwarding and
  impersonation modes are unchanged.

## Background: what the ConfigMap contains

`kube-apiserver` creates and maintains `extension-apiserver-authentication` in
`kube-system`. The keys relevant to inbound requestheader trust:

| Key | Meaning |
|---|---|
| `requestheader-client-ca-file` | PEM bundle; the front-proxy CA the inbound client cert must chain to |
| `requestheader-allowed-names` | JSON array of accepted client-cert common names (empty = any) |
| `requestheader-username-headers` | header names carrying the user, e.g. `X-Remote-User` |
| `requestheader-uid-headers` | header names carrying the UID |
| `requestheader-group-headers` | header names carrying groups |
| `requestheader-extra-headers-prefix` | prefixes carrying extras, e.g. `X-Remote-Extra-` |

Two consequences:

- The proxy no longer needs a CA Secret, a CA file path, or an allowed-names
  list — all three come from the cluster.
- The proxy stops *hardcoding* `X-Remote-User` / `X-Remote-Group` / etc. It
  uses whatever header names the cluster actually configured. The current code
  in [`pkg/identity/requestheader.go`](../pkg/identity/requestheader.go) bakes
  those names in; that is a latent correctness bug this design also fixes.

Reading this ConfigMap is the documented contract — it is what
`k8s.io/apiserver`'s delegated-authentication machinery does for every
generic aggregated API server.

## Current behavior

- `cmd/server/main.go` exposes `--client-ca-file` and `--client-allowed-names`.
- `identity.NewExtractor(clientCAFile, allowedNames)` builds a `headerrequest`
  authenticator from a *static* CA file and *static* allowed names.
- When `clientCAFile == ""` the extractor trusts headers unverified
  (`requiresVerifiedHeaders == false`).
- The Helm chart mounts a `requestHeader.clientCASecretName` Secret and passes
  the two flags.
- Impersonation mode adds chart-level `fail` guards forcing both values to be
  set, because it specifically must not run unverified.

## Proposed behavior

The proxy builds its inbound authenticator from a live view of
`extension-apiserver-authentication`, using the same standard upstream pieces a
generic aggregated API server uses:

- `dynamiccertificates.NewDynamicCAFromConfigMapController` for the
  `requestheader-client-ca-file` CA bundle, and
- `headerrequest.NewRequestHeaderAuthRequestController` for the allowed client
  names and identity header names.

In `k8s.io/apiserver/pkg/server/options`, Kubernetes combines those two pieces
in a `DynamicRequestHeaderController`. That type is exported, but its
constructor is not — and importing the `options` package for a single type
pulls in the whole RecommendedOptions surface (etcd, admission, audit). The
proxy should instead mirror that type with its own small wrapper (below), so it
wires both halves of the requestheader configuration without the dependency
weight.

```text
                kube-system/extension-apiserver-authentication
                              |
                              | watch (informer)
                              v
        identity.RequestHeaderTrustController
          - dynamic requestheader CA bundle
          - dynamic allowed client names
          - dynamic username/uid/group/extra header names
          - strict initial validation for proxy startup
                              |
                              v
        headerrequest.NewDynamicVerifyOptionsSecure(...)  ← already used today
                              |
                              v
                    identity.Extractor
```

The proxy already calls `headerrequest.NewDynamicVerifyOptionsSecure`. The main
change inside `identity` is *where the dynamic providers come from*: today they
are static wrappers around a file, hardcoded header names, and a flag value; in
the new path they are live providers backed by the cluster ConfigMap.

### `identity` package API

Add a cluster-backed constructor alongside the existing file-backed one:

```go
// NewClusterExtractor builds an Extractor whose requestheader trust — CA,
// allowed names, and identity header names — is sourced live from the
// kube-system/extension-apiserver-authentication ConfigMap.
//
// The returned controller must complete a strict initial sync before the
// server starts, then run in the background to pick up trust rotation.
func NewClusterExtractor(client kubernetes.Interface) (*Extractor, *RequestHeaderTrustController, error)
```

- `NewExtractor(clientCAFile, allowedNames)` stays. It is still used by unit
  tests and remains a documented escape hatch (see "Escape hatch" below).
- A cluster-backed `Extractor` always verifies. There is no unverified path.
- `RequiresVerifiedHeaders()` returns `true` for it unconditionally.

`RequestHeaderTrustController` is a repo-owned wrapper with roughly this shape:

```go
type RequestHeaderTrustController struct {
    ca      *dynamiccertificates.ConfigMapCAController
    headers *headerrequest.RequestHeaderAuthRequestController
}

func (c *RequestHeaderTrustController) RunOnce(ctx context.Context) error
func (c *RequestHeaderTrustController) Run(ctx context.Context, workers int) // workers is cosmetic; the CA controller starts one regardless
func (c *RequestHeaderTrustController) Ready() bool
```

`RunOnce` must be stricter than the upstream controllers' default behavior. The
upstream dynamic CA controller intentionally ignores initial load failures and
the header controller treats a missing ConfigMap as non-fatal, because generic
apiserver authentication can fail closed while other authenticators work. This
proxy has no other inbound authentication path, so missing RBAC, a missing
ConfigMap, a missing CA bundle, malformed JSON header lists, or an empty
username header list are startup errors.

### CLI changes

Remove:

- `--client-ca-file`
- `--client-allowed-names`

The proxy always runs cluster-sourced trust. `main.go` builds an in-cluster
Kubernetes client (`rest.InClusterConfig`) and calls `NewClusterExtractor`.

`validateBackendIdentityFlags` loses its `--client-ca-file` /
`--client-allowed-names` preconditions for impersonation mode: cluster-sourced
trust is always present and always verified, so impersonation mode no longer
needs to assert them.

The in-cluster client is also a prerequisite the proxy already implicitly has —
it runs as a ServiceAccount and impersonation mode already reads that token.

### Inbound serving TLS

`buildServingTLSConfig` currently builds a static `tls.Config.ClientCAs` pool
from `--client-ca-file`. With cluster-sourced trust there is no such file.

Set `tls.Config.ClientAuth = tls.RequestClientCert`: the TLS layer requests a
client certificate but does not itself verify the chain. The `headerrequest`
x509 verifier — backed by the controller's *dynamic* CA — becomes the single
trust authority. This is exactly what `k8s.io/apiserver`'s `genericapiserver`
does — verified in `pkg/server/secure_serving.go:75-79`, analysed in
[`case-study-cozystack.md`](case-study-cozystack.md) — and it removes the
second, static, drift-prone copy of the CA.

Note this is the *inbound* hop only. `RequestClientCert` means a certless
connection completes the handshake; it does **not** mean a certless *request*
is trusted. A request whose client certificate is missing or does not chain to
the requestheader CA carries no verified identity, and the handler rejects it
(below). The TLS relaxation and the identity check are separate layers.

A request with no client certificate, or a certificate that does not chain to
the current requestheader CA, is rejected by the existing handler check
(`RequiresVerifiedHeaders() && !trustedIdentity` → 401).

### Startup and readiness

1. Build the client and controller.
2. **Strict initial trust load.** First do an explicit `Get` of the ConfigMap.
   Its job is diagnostics: `controller.RunOnce` swallows load errors — the
   upstream `ConfigMapCAController.RunOnce` is literally `_ = loadCABundle()` —
   so a bare `Get` is what surfaces *why* trust is missing (`Forbidden` from a
   missing RoleBinding versus `NotFound` for a missing ConfigMap) as a clear
   startup error. Then call `controller.RunOnce(ctx)` and gate on
   `controller.Ready()`: `ConfigMapCAController.VerifyOptions()` reports
   `ok == false` until a CA bundle actually parses — covering a missing key, an
   absent ConfigMap, denied RBAC, and malformed PEM alike — and
   `UsernameHeaders()` must be non-empty. If either fails, **fail startup
   loudly**: the proxy must not start in a state where it cannot verify anyone.
3. `go controller.Run(ctx, workers)` — keep watching for CA rotation and
   requestheader configuration changes.
4. `/readyz` reports not-ready until that strict initial load has succeeded
   (`controller.Ready()` is true). After that, transient watch/list failures do
   not make the proxy forget last-known-good trust.

Rotation is handled for free: when `kube-apiserver` rotates the aggregator CA,
the informer delivers the new bundle and in-flight verification picks it up
with no restart and no Secret update.

The readiness rule is intentionally about "do we have a usable trust snapshot?"
rather than "is the watch currently healthy?". Losing the watch should be
logged and retried, while existing verified traffic continues under the
last-known-good trust bundle. A process restart during the same outage still
fails startup, because it cannot establish any initial trust snapshot.

### RBAC

The proxy ServiceAccount needs read access to one ConfigMap. Kubernetes ships a
built-in Role for exactly this:

```yaml
# Built-in, already present in every cluster:
#   Role kube-system/extension-apiserver-authentication-reader
```

The chart adds one `RoleBinding` in `kube-system`:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: {{ include "apiservice-audit-proxy.fullname" . }}-auth-reader
  namespace: kube-system
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: extension-apiserver-authentication-reader
subjects:
  - kind: ServiceAccount
    name: {{ include "apiservice-audit-proxy.serviceAccountName" . }}
    namespace: {{ .Release.Namespace }}
```

This is the standard binding every aggregated API server already creates; the
checklist in `background-authz.md` calls it out explicitly.

Because the binding is created in `kube-system`, the Helm installer needs
permission to create a `RoleBinding` outside the release namespace. That is an
expected requirement for a cluster-integrated aggregated API component, but the
chart documentation should call it out so an RBAC denial is easy to diagnose.

### Helm chart changes

Removed from `values.yaml` and the templates:

- `requestHeader.clientCASecretName`
- `requestHeader.clientCAFileName`
- `requestHeader.allowedNames`
- the `requestheader-client-ca` volume, volumeMount, and `--client-*` args in
  `deployment.yaml`
- the `requestHeader.*` `fail` guards in `deployment.yaml`

Added:

- the `kube-system` RoleBinding above (always rendered — the proxy always
  needs it).

The entire `requestHeader:` block disappears from `values.yaml`. Net effect for
an operator: **three settings and one Secret removed, nothing added.** This is
the configuration reduction the request asked for, and it makes the only
remaining behavior the safe one.

## Failure modes and safety

| Situation | Behavior |
|---|---|
| RoleBinding missing / ConfigMap unreadable at startup | startup fails; proxy never serves |
| ConfigMap present but CA/header data missing or malformed at startup | startup fails; proxy never serves |
| ConfigMap unreadable later (transient) | last-known-good trust retained; `Run` retries; logged |
| Aggregator CA rotated | new bundle adopted live, no restart |
| Inbound request with no client cert | 401 (unchanged) |
| Client cert not chaining to current CA | 401 (unchanged) |
| `requestheader-allowed-names` empty in cluster | any CN under the CA accepted — this is the cluster's own stated policy; the proxy honors it rather than overriding it |

The key safety property: there is no flag whose omission yields an unverified
proxy. The insecure mode is deleted, not defaulted-off.

## Why there is no "additional trusted caller" knob

It is tempting — having seen `cozystack-api` accept bearer tokens, client
certs, and requestheader identity all at once (see
[`case-study-cozystack.md`](case-study-cozystack.md)) — to let the proxy take
callers other than the front kube-apiserver, perhaps behind an off-by-default
flag. This design deliberately does not, for four reasons.

**1. Requestheader trust is the power to be anyone.** A caller trusted on the
requestheader path can set `X-Remote-User` to any value and the proxy will
believe it. "Add another trusted caller" is therefore identical to "add another
entity that can impersonate every user in the cluster." There is no small
version of this knob, default-off or otherwise.

**2. The cluster already owns the knob — at the right level.** The set of
trusted front proxies is `requestheader-allowed-names` in
`extension-apiserver-authentication`. Because this design *reads that
ConfigMap*, the proxy already honors whatever front proxies the cluster trusts.
To let another component call the proxy as a front proxy, a cluster
administrator adds it to the cluster's requestheader configuration — no proxy
change, no redeploy. That decision belongs to whoever controls cluster
aggregation trust, not to whoever runs `helm install`.

**3. A proxy-local knob would be a privilege-escalation path.** An extra-CA or
extra-allowed-name setting in `values.yaml` would let a namespace-scoped chart
installer widen a cluster-scoped trust boundary. The person installing the
audit proxy is frequently not the person who should decide who may impersonate
cluster users. Keeping the trust set in the cluster ConfigMap keeps that
decision with the right party.

**4. The proxy's position in the chain is not `cozystack-api`'s.**
`cozystack-api` is permissive because it is the *executor*: direct clients,
controllers, and kube-apiserver health probes all authenticate *to* it, so it
runs the full union authenticator. The audit proxy sits *inside* the
aggregation path; its one legitimate client is the front kube-apiserver that
fronts its `APIService`. Copying `cozystack-api`'s permissiveness would not
make the proxy more capable — it would turn a narrow aggregation component into
an unauthenticated identity-assertion endpoint. Position in the chain dictates
trust posture; see `background-authz.md` section A, "Reject direct access to
the aggregated-apiserver-proxy."

If a future use case genuinely needs a caller to act *as itself* — rather than
to assert other identities — that is direct authentication: a different
mechanism, with its own authorization obligations (`background-authz.md`
section C), and a separate design, not a relaxation of this one.

The one fallback this design keeps — the file-backed `NewExtractor` below — is
**not** a relaxation. It is for a proxy that cannot reach the cluster at all,
and it makes the trust set *narrower* (a single operator-pinned CA), never
wider.

## Escape hatch

`NewExtractor(clientCAFile, allowedNames)` — the file-backed constructor — is
kept for:

- unit tests, which already construct it directly with generated fixtures, and
- running the binary outside a cluster (no API access).

It stays a *library* API. It is intentionally **not** re-exposed as a CLI flag,
so a normal in-cluster install has exactly one path. If a future air-gapped or
non-cluster deployment needs it, re-adding a single explicit
`--requestheader-client-ca-file` flag is a deliberate, reviewable change — not
the default.

## Phased implementation plan (all will land in a single PR)

1. **`identity` package** — add `NewClusterExtractor` and
   `RequestHeaderTrustController`, mirroring upstream
   `k8s.io/apiserver/pkg/server/options/authentication_dynamic_request_header.go`
   (~80 lines: the same two embedded controllers, `RunOnce`, and `Run`). Wire
   the CA controller's `VerifyOptions` and the requestheader-name providers into
   the existing `NewDynamicVerifyOptionsSecure` call. Unit-test against a fake
   clientset serving a synthetic ConfigMap.
2. **`cmd/server`** — build the in-cluster client; start the controller with
   strict `RunOnce` + `Run`; gate `/readyz` on usable trust state; remove the
   two flags and their validation.
3. **Serving TLS** — switch `buildServingTLSConfig` to `tls.RequestClientCert`;
   drop the static `ClientCAs` pool.
4. **Helm chart** — add the `kube-system` RoleBinding; delete the
   `requestHeader.*` values, volume, mount, args, and guards.
5. **Docs** — update `background-authz.md` (checklist A items now satisfied),
   `HELM_VALUES.md`, and `CONNECTIONS_AND_TLS.md`.
6. **e2e** — assert the proxy comes up with no requestheader Secret, that a
   write still succeeds, and that deleting the RoleBinding makes startup fail.

## Testing strategy

- **Unit** — `NewClusterExtractor` against a `fake.Clientset` whose
  `extension-apiserver-authentication` ConfigMap carries a generated CA: a cert
  under that CA verifies; a cert under a foreign CA is rejected; a ConfigMap
  update to a new CA is observed without rebuilding the `Extractor`.
- **Unit** — startup fails when the ConfigMap is absent, the CA key is missing
  or invalid, header-name JSON is malformed, or username headers are empty.
- **Unit** — the live providers retain last-known-good state after a later bad
  ConfigMap update, and accept a later good update.
- **e2e** — a fresh install with no `requestHeader.*` values performs an
  audited write end-to-end; removing the RoleBinding fails the proxy's startup
  / readiness rather than silently disabling verification.

## Open questions

- **Watch vs. periodic resync** — the upstream controllers use informers and
  periodic refresh. Confirm the proxy's RBAC also allows `watch` on the
  ConfigMap. The built-in reader Role grants `get`/`list`/`watch`, so this is
  expected to be fine.
