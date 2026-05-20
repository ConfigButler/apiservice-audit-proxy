# Design: backend impersonation mode for `apiservice-audit-proxy`

This document describes the design behind the implemented
`apiservice-audit-proxy` backend impersonation mode. Sections such as
"Current behavior" and "Proposed behavior" preserve the design chronology from
before the feature landed; use [`HELM_VALUES.md`](HELM_VALUES.md) for the
operator-facing configuration guide.

The immediate target is the CozyStack flow:

```text
kube-apiserver -> apiservice-audit-proxy -> cozystack-api
```

The design is based on the current source in this repository.

## Goal

Make the proxy usable in front of aggregated API servers without giving it a
front-proxy client certificate signed by the cluster aggregator CA.

In the new mode:

1. kube-apiserver calls the proxy with the normal requestheader identity
   surface: `X-Remote-User`, `X-Remote-Group`, `X-Remote-Uid`, and
   `X-Remote-Extra-*`.
2. The proxy verifies that delegated identity on the inbound hop, as it already
   does from the cluster's `extension-apiserver-authentication` ConfigMap.
3. The proxy calls the real backend as its own Kubernetes ServiceAccount.
4. The proxy translates the verified user into Kubernetes impersonation
   headers: `Impersonate-User`, repeated `Impersonate-Group`,
   `Impersonate-Uid`, and `Impersonate-Extra-*`.
5. The backend authorizes the proxy ServiceAccount to impersonate the requested
   user through normal Kubernetes RBAC.

For CozyStack, this avoids:

- a backend client cert signed by the cluster aggregator CA,
- a copied kube-apiserver proxy-client cert,
- a dedicated `cozystack-api` requestheader CA bundle, and
- live patches to a CozyStack-managed control-plane Deployment.

## Current behavior

The current proxy is a requestheader-forwarding proxy.

Relevant code:

- [`cmd/server/main.go`](../cmd/server/main.go)
  parses backend TLS flags and builds an `http.Transport`. There is no backend
  authentication mode beyond TLS client certificates.
- [`pkg/identity/requestheader.go`](../pkg/identity/requestheader.go)
  extracts Kubernetes requestheader identity from inbound requests. The server
  path uses the cluster-backed extractor from
  [`pkg/identity/cluster.go`](../pkg/identity/cluster.go), which verifies the
  inbound client certificate against the cluster's published requestheader CA
  and accepted client names before trusting those headers.
- [`pkg/proxy/handler.go`](../pkg/proxy/handler.go)
  extracts `authnv1.UserInfo` at the start of `ServeHTTP`.
- `serveAudited` calls `buildUpstreamRequest`, which clones the inbound request
  headers, strips only hop-by-hop headers, appends `X-Forwarded-For`, and sends
  the result to the backend.
- Non-audited requests, including `GET`, list, watch, and non-resource
  requests, currently go through `httputil.NewSingleHostReverseProxy`.

That means the proxy preserves inbound `X-Remote-*` headers on the backend hop.
This works only when the backend sees the proxy as a trusted front proxy. For
`cozystack-api`, that requires a requestheader client certificate trusted by
the backend.

## Proposed behavior

Add a backend identity mode:

```text
--backend-identity-mode=requestheader     # binary default; forwards X-Remote-*
--backend-identity-mode=impersonation     # chart default; emits Impersonate-*
```

The binary keeps `requestheader` as its flag default for compatibility. The
Helm chart defaults to `impersonation`, because the chart can make the
ServiceAccount-token path explicit and avoids requiring a backend client
certificate for the common install.

In `impersonation` mode, the proxy must apply the same backend identity logic
to audited and non-audited requests:

- remove all inbound `X-Remote-*` headers before calling the backend,
- remove all inbound `Impersonate-*` headers before calling the backend,
- remove inbound `Authorization` before calling the backend,
- attach the proxy's backend bearer token,
- set impersonation headers from the already-verified `authnv1.UserInfo`, and
- keep existing reverse-proxy mechanics such as URL rewriting, hop-by-hop
  header stripping, and `X-Forwarded-For`.

This mode requires verified inbound requestheader trust. That trust is no
longer configured with proxy-local flags; the proxy reads the cluster's
requestheader CA, accepted client names, and header names from
`kube-system/extension-apiserver-authentication`.

## Inbound identity trust

Impersonation mode turns verified `X-Remote-*` headers into authorized backend
impersonation, so trust in the inbound client certificate is now a direct grant
of impersonation power. A CA check alone does not narrow that trust.

Standard Kubernetes aggregator trust pins both a dedicated
`requestheader-client-ca-file` and a `requestheader-allowed-names` list
(commonly the kube-apiserver front-proxy client name). The proxy now consumes
that same trust directly from `kube-system/extension-apiserver-authentication`
via `identity.NewClusterExtractor`. Startup fails if the ConfigMap is
unreadable or does not yield a usable trust snapshot, so impersonation mode
cannot start by silently trusting unverified headers.

## Backend identity interface

Introduce a small request decorator in `pkg/proxy`, for example:

```go
type BackendIdentity interface {
    Apply(req *http.Request, user authnv1.UserInfo)
}
```

Then add it to `HandlerConfig`:

```go
type HandlerConfig struct {
    // existing fields...
    BackendIdentity BackendIdentity
}
```

The default implementation can be a no-op or an explicit
`RequestHeaderForwarder`, preserving current behavior. The impersonation
implementation should mutate only the outbound request clone, never the inbound
request that is later used to build the audit event.

`Apply` has no error return. Header projection and stripping cannot fail, and
the one fallible operation — obtaining the backend bearer token — is handled by
the transport, not by `Apply` (see [Backend bearer token](#backend-bearer-token)).
An infallible `Apply` is what lets the same decorator run from the
`httputil.ReverseProxy` `Rewrite` hook, which has no error channel.

This keeps the proxy package independent from CLI flag parsing and Helm values.
`cmd/server` decides which implementation to construct.

## Header handling

In impersonation mode, build outbound headers as a controlled projection of the
verified `authnv1.UserInfo`.

Mapping:

| `authnv1.UserInfo` field | Backend header |
|--------------------------|----------------|
| `Username` | `Impersonate-User` |
| each `Groups[]` value | `Impersonate-Group` |
| `UID` when non-empty | `Impersonate-Uid` |
| each `Extra[key][]` value | `Impersonate-Extra-<escaped key>` |

Use Kubernetes' header names from `k8s.io/client-go/transport` where possible:

- `transport.ImpersonateUserHeader`
- `transport.ImpersonateGroupHeader`
- `transport.ImpersonateUIDHeader`
- `transport.ImpersonateUserExtraHeaderPrefix`

`client-go` has `NewImpersonatingRoundTripper`, but it is not sufficient by
itself because it is configured with one static identity. This proxy needs a
different impersonated identity per request. It also deliberately skips
overwriting an existing `Impersonate-User` header, while this proxy must strip
user-supplied impersonation headers and set its own controlled values.

Stripping inbound headers must scan keys by prefix. `http.Header.Del` matches a
single canonicalized key, so the decorator iterates every header key and deletes
any whose canonical form starts with `X-Remote-` or `Impersonate-`, plus the
exact `Authorization` key. A fixed `Del("X-Remote-User")` list is insufficient
because `X-Remote-Extra-*` and `Impersonate-Extra-*` are open-ended.

The extra-key escaping must match Kubernetes. In `client-go`, the unexported
`headerKeyEscape` percent-encodes bytes that are illegal in HTTP header keys
and also encodes `%`. The apiserver side lower-cases the encoded suffix before
`url.PathUnescape`. The proxy should copy that small behavior into its own
tested helper instead of inventing a different escaping scheme.

Test cases should include at least:

- `scopes`
- `example.com/tenant`, proving `/` is percent-encoded and round-trips
- `Test.example.com/thing.thing`, proving the backend receives the lower-cased
  extra key `test.example.com/thing.thing`
- a key containing `%20`, proving the literal percent is encoded as `%25`

Avoid tests that depend on exact MIME header canonicalization. Go's
`http.Header` may display `Impersonate-Extra-Test.example.com%2fthing.thing`
even when the helper emitted a different suffix casing. The important contract
is Kubernetes-compatible decoding.

## Extra key forwarding policy

Inbound `X-Remote-Extra-*` headers are not a fixed set. Recent kube-apiserver
versions attach their own extras to every authenticated request and forward them
to aggregated API servers, for example `authentication.kubernetes.io/credential-id`,
and, under node restriction, `authentication.kubernetes.io/node-name`,
`node-uid`, `pod-name`, and `pod-uid`. The exact set changes across Kubernetes
releases.

If the proxy blindly converts every inbound extra into an `Impersonate-Extra-*`
header, then every install needs RBAC for `userextras/<key>` covering keys it
never deliberately chose, and that RBAC silently breaks the next time the
cluster is upgraded and a new apiserver-injected key appears. A normal `kubectl`
request already carries `authentication.kubernetes.io/credential-id`, so this is
not a corner case — it is the common path.

Therefore extra forwarding is an explicit allowlist:

- `--backend-impersonation-extra-keys` is a comma-separated allowlist of decoded
  extra keys. Only these keys are projected into `Impersonate-Extra-*`.
- The default allowlist is empty: the proxy forwards no extras and drops
  apiserver-injected extras. `Impersonate-User`, `Impersonate-Group`, and
  `Impersonate-Uid` still carry the meaningful identity, so the common path
  works with only `users`/`groups`/`uids` RBAC.
- `--backend-impersonation-forward-all-extras` is an escape hatch for backends
  that genuinely authorize on extras. It is off by default.

The allowlist also drives RBAC: the chart enumerates one `userextras/<key>` rule
per allowlisted key. `forward-all-extras` instead needs the bare
`authentication.k8s.io` resource wildcard.

## Backend bearer token

Add a token-file flag:

```text
--backend-impersonation-token-file=/var/run/secrets/kubernetes.io/serviceaccount/token
```

The default should be the standard projected ServiceAccount token path. The
flag is used only when `--backend-identity-mode=impersonation`.

Prefer `k8s.io/client-go/transport.NewBearerAuthWithRefreshRoundTripper` for
token refresh behavior. It already rereads the token file through a cached file
token source and handles projected-token rotation.

Constructed with an empty initial token and a token-file path, the wrapper reads
the file once during construction to confirm it exists, and returns an error if
it cannot. A missing or unreadable token file is therefore a **startup failure**,
not a per-request error: `cmd/server` builds the backend transport before
serving, so it should surface this and exit non-zero. After startup, a later
refresh failure is tolerated — the cached token source logs the error and keeps
serving the last good token, which is the correct behavior for projected-token
rotation hiccups.

One important caveat: the client-go bearer wrapper also refuses to overwrite an
existing `Authorization` header. Therefore `Authorization` must be stripped from
the outbound request before the bearer wrapper runs, or the user's original
header could suppress the proxy ServiceAccount token.

## Proxy path refactor

The largest code seam is the split between audited and passthrough requests.

Today:

- audited requests go through `buildUpstreamRequest` and `h.transport.RoundTrip`,
- non-audited requests go through `h.passthrough.ServeHTTP(w, r)`.

Backend impersonation must apply to both paths. A `GET` is not audited, but it
still has to succeed against the backend when the APIService is switched to the
proxy.

Two workable implementation shapes:

1. Put `userInfo` in the request context before calling the reverse proxy, and
   configure the reverse proxy `Rewrite` hook to call the same backend identity
   decorator.
2. Replace the reverse proxy passthrough path with a `servePassthrough` method
   that builds an upstream request through the same helper as `serveAudited`,
   then streams the response back without body capture or audit delivery.

The first option preserves more of `httputil.ReverseProxy` behavior, especially
for streaming and upgrades. The second option is easier to reason about, but it
must be checked carefully for watch requests and any upgraded connection paths.

Recommended first implementation: keep `httputil.ReverseProxy`, but construct it
directly with a `Rewrite` hook instead of `httputil.NewSingleHostReverseProxy`.
`Rewrite` and `Director` are mutually exclusive, and `Rewrite` exposes both the
inbound request (`ProxyRequest.In`) and the outbound clone (`ProxyRequest.Out`),
which matches the rule that only the outbound clone may be mutated.

`ProxyRequest.SetURL` reproduces the scheme, host, and path rewriting that
`NewSingleHostReverseProxy` did, but it does **not** touch `X-Forwarded-*`. The
legacy `Director` path appends `X-Forwarded-For` automatically; the `Rewrite`
path does not. To honor the `X-Forwarded-For` requirement in the
[Proposed behavior](#proposed-behavior) list, the `Rewrite` hook must set it
explicitly — reuse the same `appendForwardedFor` helper the audited path already
uses, so both paths produce identical `X-Forwarded-For` behavior. Do **not**
call `ProxyRequest.SetXForwarded`: it would additionally inject
`X-Forwarded-Host` and `X-Forwarded-Proto`, which neither the current audited
path nor the current passthrough path sends, and which this backend set does not
need (see [Decisions](#decisions)).

Add a typed context value for the verified `authnv1.UserInfo`, read it in the
`Rewrite` hook, and apply the same outbound decoration helper used by the
audited path. Then add tests proving both `POST` and `GET` backend requests
receive the same impersonation headers and the same `X-Forwarded-For`.

## CLI changes

Extend `config` in `cmd/server/main.go`:

```go
type config struct {
    // existing fields...
    backendIdentityMode                  string
    backendImpersonationTokenFile        string
    backendImpersonationExtraKeys        string
    backendImpersonationForwardAllExtras bool
    backendImpersonationForwardUID       bool
}
```

New flags:

```text
--backend-identity-mode=requestheader|impersonation
--backend-impersonation-token-file=/var/run/secrets/kubernetes.io/serviceaccount/token
--backend-impersonation-extra-keys=scopes,example.com/tenant
--backend-impersonation-forward-all-extras=false
--backend-impersonation-forward-uid=true
```

Validation:

- `--backend-identity-mode` must be either `requestheader` or `impersonation`.
- Inbound trust is always cluster-sourced and always verified on the server
  path; there are no proxy-local requestheader CA or allowed-name flags to
  validate.
- `--backend-impersonation-token-file`, `--backend-impersonation-extra-keys`,
  `--backend-impersonation-forward-all-extras`, and
  `--backend-impersonation-forward-uid` are only meaningful in `impersonation`
  mode. In `requestheader` mode they are rejected or ignored consistently.
- `--backend-impersonation-extra-keys` and
  `--backend-impersonation-forward-all-extras` are mutually exclusive: an
  allowlist plus "forward everything" is a contradiction.
- Initially reject `--backend-client-cert-file` and
  `--backend-client-key-file` in `impersonation` mode. They can be allowed
  later if a real mTLS-plus-bearer use case appears, but rejecting them avoids a
  confused identity model in the first implementation.

TLS server verification remains separate. `--backend-ca-file`,
`--backend-server-name`, and `--backend-insecure-skip-verify` still mean only
"how does the proxy verify the backend serving certificate?"

## Helm chart changes

Extend
[`charts/apiservice-audit-proxy/values.yaml`](../charts/apiservice-audit-proxy/values.yaml):

```yaml
backend:
  identity:
    mode: impersonation
    impersonation:
      tokenFile: /var/run/secrets/kubernetes.io/serviceaccount/token
      forwardUid: true
      extras:
        mode: none # none | allowlist | all
        keys: []
      rbac:
        create: false
```

`backend.identity.mode` is the single backend identity strategy selector.
`backend.identity.impersonation.*` is only active when that mode is
`impersonation`; the chart fails if it is asked to render impersonation RBAC in
`requestheader` mode.
Deprecated sibling paths such as `backend.identityMode` and
`backend.impersonation` fail rendering instead of being silently ignored.

`extras.mode=allowlist` makes `extras.keys` the single source of truth for
extras: it controls both which `Impersonate-Extra-*` headers the proxy emits and
which `userextras/<key>` RBAC rules the chart renders. `extras.mode=none` drops
all extras. `extras.mode=all` forwards every extra and renders broad extra RBAC.

There is no `requestHeader:` chart section. Inbound trust comes from the
cluster ConfigMap, and the chart always renders the kube-system
`extension-apiserver-authentication-reader` RoleBinding the proxy needs to read
it.

Render args in
[`templates/deployment.yaml`](../charts/apiservice-audit-proxy/templates/deployment.yaml):

```yaml
- --backend-identity-mode={{ .Values.backend.identity.mode }}
{{- if eq .Values.backend.identity.mode "impersonation" }}
- --backend-impersonation-token-file={{ .Values.backend.identity.impersonation.tokenFile }}
- --backend-impersonation-forward-uid={{ .Values.backend.identity.impersonation.forwardUid }}
{{- if eq .Values.backend.identity.impersonation.extras.mode "all" }}
- --backend-impersonation-forward-all-extras=true
{{- else if eq .Values.backend.identity.impersonation.extras.mode "allowlist" }}
- --backend-impersonation-extra-keys={{ join "," .Values.backend.identity.impersonation.extras.keys }}
{{- end }}
{{- end }}
```

Add an optional RBAC template only when
`backend.identity.mode=impersonation` and
`backend.identity.impersonation.rbac.create=true`.

The chart should not enable broad impersonation RBAC by default. Installing the
proxy with `backend.identity.mode=impersonation` but without RBAC should fail
clearly at runtime with a Kubernetes authorization error, which is safer than
silently granting broad impersonation.

## RBAC shape

A functional ClusterRole for `extras.mode=allowlist`,
`extras.keys: [scopes, example.com/tenant]`, and `forwardUid: true` looks like:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: apiservice-audit-proxy-impersonator
rules:
  - apiGroups: [""]
    resources: ["users", "groups", "serviceaccounts"]
    verbs: ["impersonate"]
  # rendered only when forwardUid: true
  - apiGroups: ["authentication.k8s.io"]
    resources: ["uids"]
    verbs: ["impersonate"]
  # one entry per extras.keys value; omitted unless extras.mode=allowlist
  - apiGroups: ["authentication.k8s.io"]
    resources:
      - "userextras/scopes"
      - "userextras/example.com/tenant"
    verbs: ["impersonate"]
```

The `userextras` rule is a direct projection of `extras.keys` when
`extras.mode=allowlist`. `extras.mode=none` renders no `userextras` rule at all,
which is the safe default. When `extras.mode=all` the chart replaces that rule
with the bare resource wildcard `*` in `authentication.k8s.io` instead of
enumerating keys.

Bind it to the chart ServiceAccount:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: apiservice-audit-proxy-impersonator
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: apiservice-audit-proxy-impersonator
subjects:
  - kind: ServiceAccount
    name: apiservice-audit-proxy
    namespace: apiservice-audit-proxy
```

Notes:

- RBAC does not support `userextras/*` as a wildcard subresource. It must be
  enumerated as `userextras/<key>` (`extras.mode=allowlist`), or the rule must
  use the bare resource wildcard `*` in `authentication.k8s.io`
  (`extras.mode=all`).
- RBAC uses the decoded, lower-case extra key as the subresource. The backend
  header uses an escaped suffix such as `Impersonate-Extra-example.com%2Ftenant`,
  but the RBAC resource is `userextras/example.com/tenant`.
- Restricting `resourceNames` is possible for known users, groups, service
  accounts, UIDs, and extra values, but it is awkward for a generic APIService
  proxy because the identity set is usually not fixed.
- This broad RBAC is the main security cost of the design. It is still a better
  fit than giving the proxy a front-proxy client cert, because the permission is
  visible in Kubernetes RBAC and scoped to authentication impersonation rather
  than to trusted requestheader injection.

## CozyStack compatibility

This should not require a CozyStack chart or `cozystack-api` code change.

`cozystack-api` is built on Kubernetes recommended API server options. In the
live cluster, a direct request to `cozystack-api` with the proxy ServiceAccount
token and `Impersonate-User: test-user` was rejected with:

```text
User "system:serviceaccount:apiservice-audit-proxy:apiservice-audit-proxy" cannot impersonate resource "users"
```

That failure is the useful signal: `cozystack-api` recognized the impersonation
header and invoked normal Kubernetes authorization. After granting the proxy
ServiceAccount the right RBAC, the same mechanism should allow the proxied
request.

## Security requirements

Impersonation mode must enforce these rules:

- Require verified inbound delegated identity through cluster-sourced
  requestheader trust. The CA bundle, accepted client names, and header names
  all come from `kube-system/extension-apiserver-authentication`; startup fails
  closed if that trust cannot be loaded.
- Strip inbound `Impersonate-*` headers before setting proxy-controlled
  impersonation headers.
- Strip inbound `Authorization` before applying the backend ServiceAccount
  token.
- Strip inbound `X-Remote-*` before calling the backend, because the backend
  should authenticate this as an ordinary bearer-token request, not as a
  requestheader proxy request.
- Project only allowlisted extras into `Impersonate-Extra-*`. Apiserver-injected
  extras the operator did not opt into must not be impersonated.
- Never log bearer tokens.
- Keep the audit event user as the real delegated user from inbound
  `X-Remote-*`; do not replace audit event user with the proxy ServiceAccount.

The backend's own audit trail may show the proxy ServiceAccount as the
authenticated user and the original user as the impersonated user. That is
expected for Kubernetes impersonation and differs from pure requestheader
forwarding.

## Tests

### Unit tests in `pkg/proxy`

- `POST` audited request in `impersonation` mode sends:
  - backend bearer token,
  - `Impersonate-User`,
  - repeated `Impersonate-Group`,
  - `Impersonate-Uid` when present,
  - encoded `Impersonate-Extra-*` for allowlisted keys only.
- `GET` passthrough request receives the same backend identity handling.
- Inbound `Authorization`, `Impersonate-*`, and `X-Remote-*` are stripped from
  the backend request, including open-ended `X-Remote-Extra-*` and
  `Impersonate-Extra-*` keys (prefix-scan stripping, not a fixed key list).
- Extras outside the allowlist are dropped; `forward-all-extras` projects every
  inbound extra.
- `forward-uid=false` suppresses `Impersonate-Uid` even when the identity has a
  UID.
- The original inbound request still produces an audit event with the delegated
  user, never the proxy ServiceAccount.
- Both `POST` and `GET` backend requests carry `X-Forwarded-For` (the `Rewrite`
  path does not append it automatically).
- Extra-key escaping matches Kubernetes for `/`, `%`, spaces, and mixed case.
- Cluster-sourced requestheader trust verifies the inbound client certificate
  and honors the cluster's accepted front-proxy client names.

### Unit tests in `cmd/server`

- invalid `--backend-identity-mode` is rejected,
- backend client certificate flags are rejected in `impersonation` mode,
- token file, extra-keys, forward-all-extras, and forward-uid flags are accepted
  in `impersonation` mode,
- `--backend-impersonation-extra-keys` together with
  `--backend-impersonation-forward-all-extras` is rejected,
- impersonation-only flags in `requestheader` mode are rejected or ignored
  consistently,
- a missing or unreadable `--backend-impersonation-token-file` fails transport
  construction so the process exits non-zero, rather than starting and serving
  errors per request.

### Helm tests or rendered-manifest tests

- default values render `--backend-identity-mode=impersonation` and the
  configured impersonation token/UID flags,
- impersonation values render the new flags,
- optional RBAC is not rendered by default (`rbac.create=false`),
- RBAC only renders when `backend.identity.mode=impersonation`,
- `rbac.create=true` fails in `requestheader` mode,
- RBAC renders one enumerated `userextras/<key>` rule per `extras.keys` value
  when `extras.mode=allowlist`,
- `extras.mode=none` renders no `userextras` rule,
- `extras.mode=all` renders the bare `authentication.k8s.io` resource
  wildcard and never renders `userextras/*`,
- the `uids` rule is rendered only when `forwardUid=true`.

### End-to-end scenarios

The existing harness (`Taskfile.e2e.yml`, `test/e2e`) installs a k3d cluster,
the Wardle sample-apiserver backend (`api-backend`), the proxy, and a
webhook-tester that captures audit events. Each scenario below follows the
existing pattern: a values file under `test/e2e/values/`, a `TestXxx` function
under `test/e2e/`, and an `e2e:test-xxx` Taskfile task that depends on
`e2e:portforward-webhook-tester` with the matching `E2E_PROXY_VALUES_FILE`.

The Wardle sample-apiserver is a suitable impersonation backend: it uses
delegated authentication and authorization, so it accepts the proxy
ServiceAccount bearer token via `TokenReview` and resolves the `impersonate`
verb via `SubjectAccessReview` against kube-apiserver. The impersonation lane
therefore needs no backend client certificate. Confirm the backend Deployment is
wired for delegated authn/authz (`extension-apiserver-authentication` reader and
`system:auth-delegator`); the requestheader lane does not exercise that path, so
this may be a harness prerequisite to add.

Two new values files:

- `proxy-impersonation.yaml`: `backend.identity.mode=impersonation`,
  `backend.identity.impersonation.rbac.create=true`, no
  `backend.identity.requestheader.clientCert.secretName`.
- `proxy-impersonation-no-rbac.yaml`: same, but `rbac.create=false` and no
  operator-supplied impersonation RBAC.

Both rely on cluster-sourced inbound requestheader trust. The e2e harness no
longer copies a requestheader CA Secret into the proxy namespace; instead, the
chart-created kube-system RoleBinding lets the proxy read
`extension-apiserver-authentication` directly. The requestheader-trust e2e lane
also verifies that deleting that RoleBinding makes startup fail closed.

**Scenario 1 — audited write (`TestImpersonationWrite`).**

1. Install with `proxy-impersonation.yaml`; point the `wardle.example.com`
   APIService at the proxy.
2. `kubectl create` a Wardle `Flunder`.
3. Assert the create succeeds (HTTP 201 reaches the client).
4. Assert webhook-tester received a `ResponseComplete` event whose
   `user.username` is the real delegated kubectl identity, **not**
   `system:serviceaccount:<ns>:apiservice-audit-proxy`, with captured request
   and response bodies.

**Scenario 2 — non-audited passthrough (`TestImpersonationPassthrough`).**

1. With the same install, run `kubectl get flunders` (list) and
   `kubectl get flunders --watch` for a short window.
2. Assert both succeed. A `GET` is not audited, so it exercises the
   `ReverseProxy` path; if impersonation were applied only on the audited path,
   the backend would authorize the request as the bare proxy ServiceAccount and
   reject it. Success proves the `Rewrite` hook applies the same identity.
3. Assert the watch streams an event from a concurrent write, proving streaming
   survives the impersonation `Rewrite` hook.
4. Assert no audit event is emitted for the `GET` or watch.

**Scenario 3 — missing RBAC fails cleanly (`TestImpersonationRBACMissing`).**

1. Install with `proxy-impersonation-no-rbac.yaml`.
2. `kubectl create` a `Flunder`.
3. Assert the client receives `403 Forbidden` whose message names the
   impersonation authorization failure (`cannot impersonate resource "users"`),
   proxied straight through from the backend.
4. Assert the proxy stays healthy and that any audit event records the `403`;
   the failure must be observable, not a confusing `500`.

**Scenario 4 — apiserver-injected extras (`TestImpersonationApiserverExtras`).**

1. With the default `proxy-impersonation.yaml` (`extras.mode=none`), perform a
   write.
2. A normal kubectl request already carries an apiserver-injected extra such as
   `authentication.kubernetes.io/credential-id`. Assert the write still succeeds
   with only `users`/`groups`/`uids` RBAC, proving the proxy dropped the
   un-allowlisted extra rather than requiring
   `userextras/authentication.kubernetes.io/credential-id`.
3. Optionally switch to `extras.mode=allowlist` and add that key without adding
   RBAC, then assert the write now fails, proving the allowlist is what drives
   the impersonation set.

**Scenario 5 — requestheader regression.**

The existing `TestSmoke` and `TestAggregatedAPIAuditGap` lanes run unchanged
with default values (`backend.identity.mode=requestheader`). They are the regression
guard that the new mode did not alter default behavior. No new test, but the
design is not done until they still pass.

**Header sanitization** (stripping inbound `Impersonate-*`, `Authorization`, and
`X-Remote-*`) is verified precisely in `pkg/proxy` unit tests. A full-cluster
test cannot easily inject those headers, because kube-apiserver consumes
`Impersonate-*` and `Authorization` before aggregating, and only the proxy's own
`X-Remote-*` projection reaches the backend. An optional advanced e2e can hit
the proxy `Service` directly with a forged-but-CA-valid requestheader client
certificate (the harness already holds that CA) carrying junk
`Impersonate-*`/`Authorization`/`X-Remote-Extra-*` headers, pointed at a
header-echo backend, to assert end-to-end stripping. Treat that as hardening,
not a gate.

**Token rotation** (projected ServiceAccount token refresh via
`NewBearerAuthWithRefreshRoundTripper`) belongs in a nightly or soak lane: set
the projected token `expirationSeconds` low and assert a write succeeds both
before and after a refresh interval. It is too slow for the inline e2e suite.

New Taskfile tasks mirror `e2e:test-smoke`: `e2e:test-impersonation`
(scenarios 1, 2, 4) and `e2e:test-impersonation-no-rbac` (scenario 3).

## Rollout plan

1. Add the proxy package abstraction and unit tests.
2. Add the CLI flags with `requestheader` as the default.
3. Add chart values and optional RBAC templates.
4. Test against the existing sample-apiserver demo.
5. Test against CozyStack with no backend client cert and explicit
   impersonation RBAC.
6. Document the mode in the upstream proxy README and keep requestheader mode
   as the default until the new path has an end-to-end lane.

## Decisions

These were open questions during design; the resolutions below are now part of
the plan.

**Backend client cert flags — rejected in the first implementation only.**
`--backend-client-cert-file` is a transport-layer mTLS concern, orthogonal to
identity. A backend fronted by mTLS *and* using bearer-token identity is a
legitimate setup, so the rejection is not a permanent prohibition. It is
rejected initially to keep the identity model unambiguous and the scope small;
revisit when a concrete mTLS-plus-bearer backend appears. Frame the rejection in
docs as "not yet wired", not "conceptually forbidden".

**Broad RBAC — the chart ships optional RBAC, gated and not broad by default.**
RBAC is rendered only when `rbac.create=true` (default `false`). The supported
path is enumerated `userextras/<key>` rules driven by `extras.mode=allowlist`
and `extras.keys`; `extras.mode=none` renders no `userextras` rule at all.
`extras.mode=all` is the only switch that renders a wildcard, and it is off by
default. The chart never renders broad impersonation RBAC implicitly. Shipping
*something* matters — otherwise operators hand-roll worse RBAC — but "allow all"
must never be the path of least resistance.

**Passthrough — keep `httputil.ReverseProxy`.** Watch and streaming are
first-class APIService paths; re-implementing flush and upgrade handling in a
custom `RoundTrip` is risk for little gain. The shared decoration is factored
into one helper used by both the audited path and the `Rewrite` hook, so a
custom passthrough buys no deduplication either.

**UID — forward `Impersonate-Uid` by default when the identity has a non-empty
UID.** Controlled by `--backend-impersonation-forward-uid`, default on. UID
impersonation is GA on any Kubernetes new enough to run a modern aggregated
backend; the compatibility risk is the *backend* version, so the flag exists to
disable forwarding for an old backend rather than as a routine knob.

**`X-Forwarded-*` — preserve `X-Forwarded-For` only, on both paths.** The
current proxy sends `X-Forwarded-For` and neither `X-Forwarded-Host` nor
`X-Forwarded-Proto` — the legacy `NewSingleHostReverseProxy` `Director` path
auto-appends only `X-Forwarded-For`, and the audited path's `appendForwardedFor`
does the same. Switching the passthrough to a `Rewrite` hook must therefore set
`X-Forwarded-For` explicitly via the shared `appendForwardedFor` helper, not via
`ProxyRequest.SetXForwarded`. Calling `SetXForwarded` would newly inject
`X-Forwarded-Host`/`X-Forwarded-Proto`; that is a behavior change, not parity,
and the aggregated-API-server backend set does not need those headers. If a
future backend does, add them deliberately rather than as a side effect of the
refactor.

## Open questions

- Should the chart eventually offer per-identity `resourceNames`-scoped RBAC for
  installs with a known, fixed identity set, or is the broad
  `verbs: ["impersonate"]` role the only supported shape?
