# Case study: running `apiservice-audit-proxy` in front of CozyStack

This document captures a source-level analysis of whether
`apiservice-audit-proxy` can sit in front of CozyStack's `cozystack-api` in
**impersonation mode**, and which configuration knobs decide whether that
deployment succeeds.

It is a worked example of the general model in
[`background-authz.md`](background-authz.md) and
[`ARCHITECTURE.md`](ARCHITECTURE.md). The CozyStack sources
analysed are vendored under
[`external-resources/cozystack`](../external-resources/cozystack) (upstream
`github.com/cozystack/cozystack`, `k8s.io/apiserver v0.34.1`).

## Verdict

**It works.** `cozystack-api` is a textbook `k8s.io/apiserver` generic
aggregated API server with delegated authentication and authorization. It
accepts a bearer token, runs the standard `WithImpersonation` filter, and its
TLS layer does not require a client certificate. Nothing about CozyStack blocks
the impersonation hop.

The only things that decide success are configuration knobs — all of them on
*our* side or in cluster RBAC, none of them limitations of `cozystack-api`.
They are listed in [Knobs that matter](#knobs-that-matter).

## Topology

`cozystack-api` is itself registered as an aggregated API server for two API
groups (`apps.cozystack.io`, `core.cozystack.io`). Inserting the audit proxy
means the **proxy** becomes the `APIService` target and forwards to the real
`cozystack-api`:

```text
kubectl / tenant client
  |
  v
front kube-apiserver
  |  APIService v1alpha1.apps.cozystack.io  -> Service: apiservice-audit-proxy
  |  APIService v1alpha1.core.cozystack.io  -> Service: apiservice-audit-proxy
  |  requestheader identity: X-Remote-User / X-Remote-Group / ...
  v
apiservice-audit-proxy           (impersonation mode)
  |  Authorization: Bearer <proxy ServiceAccount token>
  |  Impersonate-User / Impersonate-Group derived from the verified X-Remote-*
  v
cozystack-api  (Service cozystack-api.cozy-system.svc:443)
  delegated authn  -> TokenReview to kube-apiserver
  WithImpersonation -> SubjectAccessReview "may proxy SA impersonate?"
  delegated authz   -> SubjectAccessReview "may the user do this?"
  admission + handler
```

`cozystack-api`'s own `APIService` objects
([`apiservice.yaml`](../external-resources/cozystack/packages/system/cozystack-api/templates/apiservice.yaml))
point `Service: cozystack-api / cozy-system`. To front CozyStack the proxy
takes over the Service target for **both** existing groups; see
[Knob: APIService handoff](#5-apiservice-handoff-both-groups).

## Evidence: `cozystack-api` is a generic apiserver

Startup path: [`cmd/cozystack-api/main.go`](../external-resources/cozystack/cmd/cozystack-api/main.go)
→ [`pkg/cmd/server/start.go`](../external-resources/cozystack/pkg/cmd/server/start.go)
→ [`pkg/apiserver/apiserver.go`](../external-resources/cozystack/pkg/apiserver/apiserver.go).

| Fact | Where |
|---|---|
| Built on `genericoptions.NewRecommendedOptions(...)` | `start.go:65` |
| Only `Etcd` is disabled — `Authentication` (`DelegatingAuthenticationOptions`) and `Authorization` (`DelegatingAuthorizationOptions`) stay enabled | `start.go:75` |
| `RecommendedOptions.ApplyTo(serverConfig)` wires delegated authn + authz | `start.go:257` |
| `GenericConfig.New("cozy-apiserver", NewEmptyDelegate())` — **default handler chain**, no custom `BuildHandlerChainFunc` | `apiserver.go:135` |

The default handler chain (`genericapiserver.DefaultBuildHandlerChain`)
unconditionally includes `WithImpersonation`. There is no feature gate for it
and no way to "turn it off" short of replacing the whole chain — which
CozyStack does not do.

CozyStack also ships the two RBAC bindings every delegated apiserver needs:

- [`auth-delegator.yaml`](../external-resources/cozystack/packages/system/cozystack-api/templates/auth-delegator.yaml)
  — `cozystack-api` SA → `system:auth-delegator` (issue `TokenReview` and
  `SubjectAccessReview`).
- [`auth-reader.yaml`](../external-resources/cozystack/packages/system/cozystack-api/templates/auth-reader.yaml)
  — `cozystack-api` SA → `extension-apiserver-authentication-reader` in
  `kube-system`.

Because `cozystack-api` functions as an aggregated apiserver at all, these must
already work — its own request authorization is a `SubjectAccessReview`, which
requires `system:auth-delegator`. The same ClusterRole also grants
`TokenReview`, which is what authenticates *our* proxy's bearer token.

## Why impersonation is not blocked by the ConfigMap

A common worry: `cozystack-api` reads
`kube-system/extension-apiserver-authentication`, so does it only trust
`X-Remote-*` and ignore everything else?

No. That ConfigMap configures the **requestheader authenticator** — one
authenticator inside a **union**. A `DelegatingAuthenticationOptions` server
runs, side by side:

1. requestheader authenticator — validates `X-Remote-*` *if* a CA-signed client
   cert is presented;
2. bearer-token authenticator — `TokenReview` for `Authorization: Bearer …`;
3. the `WithImpersonation` filter — always present in the chain.

They coexist. A server that reads that ConfigMap is *exactly* the kind of
server that supports impersonation, because both arrive together in
`RecommendedOptions`. Reading the ConfigMap and accepting impersonation are not
a trade-off.

## Why the TLS layer does not block a certless call

In impersonation mode the proxy presents a **bearer token and no client
certificate**. That only works if `cozystack-api`'s TLS layer does not
*require* a client cert.

[`k8s.io/apiserver/pkg/server/secure_serving.go:75-79`](/go/pkg/mod/k8s.io/apiserver@v0.34.1/pkg/server/secure_serving.go)
(the version CozyStack pins):

```go
if s.ClientCA != nil {
    // Populate PeerCertificates in requests, but don't reject connections without certificates
    // This allows certificates to be validated by authenticators, while still allowing other auth types
    tlsConfig.ClientAuth = tls.RequestClientCert
}
```

`tls.RequestClientCert` — the server *asks* for a client cert but never
requires or verifies it at the TLS layer. A connection with no client cert
completes the handshake; verification is deferred to the authenticator chain.
It is never `tls.RequireAndVerifyClientCert`. This is byte-identical in newer
`k8s.io/apiserver` releases, so it is not version-fragile.

Consequence: the proxy needs no client certificate for the
proxy → `cozystack-api` hop. The only TLS concern on that hop is the *server*
side — the proxy must trust `cozystack-api`'s serving certificate (see
[Knob 2](#2-backend-tls-trust)).

## Knobs that matter

Split into cluster-side prerequisites and proxy-side Helm values. CozyStack
itself needs no changes.

### Cluster-side prerequisites

#### A. `cozystack-api` delegated-auth RBAC — already satisfied

`system:auth-delegator` and `extension-apiserver-authentication-reader` ship
with CozyStack (links above). Nothing to do; verify only if a deploy misbehaves.

#### B. The proxy ServiceAccount needs `impersonate` RBAC — **you must enable this**

This is the single most important knob. Without it `cozystack-api`'s
`WithImpersonation` filter denies every request with a clean
`403 … cannot impersonate resource "users"`.

The chart renders the `ClusterRole` + `ClusterRoleBinding` from
[`impersonator-rbac.yaml`](../charts/apiservice-audit-proxy/templates/impersonator-rbac.yaml)
when `backend.identity.impersonation.rbac.create=true`. The default role grants
`impersonate` on `users`, `groups`, `serviceaccounts` cluster-wide — powerful;
treat the proxy as control-plane infrastructure. Where the set of tenant
identities is known, narrow it with `resourceNames` (see
[`background-authz.md`](background-authz.md) "RBAC shape for impersonation").

Note the aggregation layer periodically probes the `APIService` for health and
discovery; that probe is proxied and impersonated like any other request, so
the proxy's impersonation RBAC must also cover the identity that probe carries
— otherwise the `APIService` flips to `Available=False`
(`FailedDiscoveryCheck`). The e2e scenario `TestImpersonationRBACMissing`
documents exactly this signal.

### Proxy-side Helm values

#### 1. Backend identity mode and target

```yaml
backend:
  url: "https://cozystack-api.cozy-system.svc:443"
  identity:
    mode: impersonation
    impersonation:
      tokenFile: /var/run/secrets/kubernetes.io/serviceaccount/token  # projected SA token
      forwardUid: true
      rbac:
        create: true            # renders Knob B above
```

`backend.url` is the real `cozystack-api` Service. Both CozyStack API groups
resolve on the same host — the request path carries the group — so one
`backend.url` covers both.

#### 2. Backend TLS trust

The proxy must trust `cozystack-api`'s serving certificate. `cozystack-api`
serves with a cert-manager-issued cert
([`deployment.yaml`](../external-resources/cozystack/packages/system/cozystack-api/templates/deployment.yaml),
secret `cozystack-api-cert`).

```yaml
backend:
  tls:
    insecureSkipVerify: false        # prefer false outside prototyping
    caSecretName: "<ca-bundle>"      # CA that signed cozystack-api-cert
    serverName: "cozystack-api.cozy-system.svc"   # set if the SAN needs an explicit match
```

`insecureSkipVerify: true` is acceptable only for first-light prototyping.

#### 3. Inbound requestheader trust

In impersonation mode the proxy must verify the front kube-apiserver before
trusting `X-Remote-*`. That trust is sourced from the cluster's
`kube-system/extension-apiserver-authentication` ConfigMap rather than from
chart values:

```yaml
# No requestHeader: values are required.
# The chart renders the auth-reader RoleBinding the proxy needs.
```

The ConfigMap carries the cluster's aggregator/front-proxy CA, accepted client
names, and requestheader names. If the proxy cannot read it or cannot build a
usable trust snapshot, startup fails closed.

#### 4. Extras projection — default to `none`

`kubectl` requests carry apiserver-injected extras such as
`authentication.kubernetes.io/credential-id`. Forwarding an extra as
`Impersonate-Extra-*` requires matching `userextras/<key>` impersonation RBAC,
or `cozystack-api` rejects the request.

```yaml
backend:
  identity:
    impersonation:
      extras:
        mode: none        # recommended unless cozystack-api keys on a specific extra
```

`mode: none` drops un-needed extras and keeps the impersonation RBAC small. The
e2e scenario `TestImpersonationApiserverExtras` confirms a write still succeeds
with `mode: none` despite the injected extras. Use `allowlist` only for a
specific key CozyStack genuinely consumes; avoid `all`.

#### 5. APIService handoff (both groups)

CozyStack already creates the `APIService` objects. The proxy chart should not
try to own replacement definitions for them. Instead, install the proxy with
`server.apiService.enabled=false` and have the CozyStack-facing install layer
patch CozyStack's existing APIService objects so kube-apiserver sends those
groups to the proxy Service:

- `v1alpha1.apps.cozystack.io`
- `v1alpha1.core.cozystack.io`

Only the `spec.service` target and kube-apiserver-to-proxy CA trust need to
change. Preserve CozyStack's existing group, version, and priority fields.

```yaml
server:
  apiService:
    enabled: false
```

For each CozyStack `APIService`, the external install guide should set:

- `spec.service.name`: the proxy Service name
- `spec.service.namespace`: the proxy release namespace
- `spec.service.port`: the proxy Service port, normally `443`
- `spec.caBundle`: the CA that signs the proxy serving certificate, or
  `cert-manager.io/inject-ca-from` pointing at the proxy serving Certificate

With the chart's default `server.tls.mode=self-signed`, the CA is stored as
`ca.crt` in the proxy serving TLS Secret. With `server.tls.mode=cert-manager`,
the install layer can either copy the injected CA into the existing CozyStack
APIService objects or annotate those objects for cert-manager injection. With
`server.tls.mode=existing-secret`, the external Secret owner also owns the
matching APIService CA wiring.

This is not a multi-backend problem: both CozyStack API groups are served by
the same `cozystack-api` Service, and the request path carries the group. One
proxy `backend.url` is still correct.

#### 6. What stays the backend's job

Do **not** try to reproduce CozyStack's authorization in the proxy.
`cozystack-api` delegates every resource decision to the kube-apiserver via
`SubjectAccessReview`, so the clean invariant holds:

> If a tenant user could not perform the operation directly on `cozystack-api`,
> they cannot perform it through the proxy either.

The proxy stays a transparent forwarder; CozyStack's tenant RBAC is unchanged.

## Failure modes and how to read them

| Symptom | Cause | Fix |
|---|---|---|
| `403 … cannot impersonate resource "users"` | proxy SA lacks `impersonate` RBAC | Knob B — `rbac.create=true` |
| `APIService Available=False`, `FailedDiscoveryCheck`, message contains `403` | same — the discovery probe is impersonated and denied | Knob B |
| `401 Unauthorized` at the proxy | front-proxy cert not trusted / not presented | Knob 3 — cluster requestheader trust / auth-reader RoleBinding |
| TLS error proxy → backend | proxy does not trust `cozystack-api`'s serving cert | Knob 2 |
| `403` on a real operation, RBAC for impersonation is present | the *impersonated user* lacks RBAC on the CozyStack resource | expected — fix the tenant's RBAC, not the proxy |
| `403 … cannot impersonate … userextras/...` | an extra is forwarded without matching RBAC | Knob 4 — `extras.mode=none` |

Debugging tip: `kubectl auth can-i <verb> <resource> --as=<user> --as-group=<group>`
separates "CozyStack denies the user" from "the proxy's impersonation is
failing" from "front aggregation auth is failing".

## Pre-flight checklist

1. `cozystack-api` runs and its own `APIService`s are `Available` before
   inserting the proxy.
2. Proxy chart: `backend.identity.mode=impersonation`,
   `backend.url=https://cozystack-api.cozy-system.svc:443`,
   `backend.identity.impersonation.rbac.create=true`.
3. Backend TLS trust set (Knob 2).
4. The proxy ServiceAccount can read
   `kube-system/extension-apiserver-authentication` through the chart's
   auth-reader RoleBinding (Knob 3).
5. `extras.mode=none` unless a specific extra is required (Knob 4).
6. `server.apiService.enabled=false` in this chart, because CozyStack already
   owns its `APIService` objects.
7. CozyStack's two existing `APIService` objects (`apps` + `core`) point at the
   proxy Service with valid kube-apiserver-to-proxy CA trust (Knob 5).
8. Smoke test: a tenant `get`/`create` on a CozyStack resource succeeds, and the
   audit event records the **tenant** identity, never the proxy ServiceAccount.

## References

- CozyStack apiserver: [`apiserver.go`](../external-resources/cozystack/pkg/apiserver/apiserver.go),
  [`start.go`](../external-resources/cozystack/pkg/cmd/server/start.go)
- CozyStack RBAC: [`auth-delegator.yaml`](../external-resources/cozystack/packages/system/cozystack-api/templates/auth-delegator.yaml),
  [`auth-reader.yaml`](../external-resources/cozystack/packages/system/cozystack-api/templates/auth-reader.yaml)
- TLS `ClientAuth`: `k8s.io/apiserver/pkg/server/secure_serving.go:75-79`
- Background and checklist: [`background-authz.md`](background-authz.md)
- Current architecture: [`ARCHITECTURE.md`](ARCHITECTURE.md)
- Operator-facing values: [`HELM_VALUES.md`](HELM_VALUES.md)
