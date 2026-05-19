# Front-proxy auth, requestheader auth, impersonation, and aggregated APIs

You are dealing with two Kubernetes identity-forwarding mechanisms that look similar from a distance but mean very different things:

**requestheader / front-proxy auth:**

> "A trusted proxy already authenticated the user. Trust these `X-Remote-*` headers because the caller is a trusted front proxy."

**impersonation:**

> "This authenticated caller wants the API server to treat the request as another user. First check that the caller is allowed to impersonate."

For an APIService / aggregated API setup, the usual Kubernetes-native path is:

```
kubectl / client
  |
  | user's real authn material: cert/token/OIDC/etc.
  v
front kube-apiserver
  |
  | mTLS from front kube-apiserver to aggregated-apiserver-proxy
  | X-Remote-User: alice
  | X-Remote-Group: devs
  v
your aggregated-apiserver-proxy
```

The official Kubernetes aggregation docs describe the flow as: the Kubernetes apiserver authenticates the requesting user, authorizes access to the requested API path, proxies the request to the extension apiserver, the extension apiserver authenticates the request from Kubernetes apiserver, authorizes the original user, and then executes the request.

That means your earlier instinct is correct: requestheader mode is the normal aggregation-layer contract between the front kube-apiserver and an extension apiserver.

Impersonation becomes interesting when your aggregated-apiserver-proxy is not really the final executor, but a pass-through proxy to another apiserver:

```
client
  -> front kube-apiserver
  -> your aggregated-apiserver-proxy
  -> aggregated-apiserver-backend
```

For that second hop, I would generally prefer impersonation:

```
your aggregated-apiserver-proxy
  |
  | Authorization: Bearer <proxy service account token>
  | Impersonate-User: alice
  | Impersonate-Group: devs
  v
aggregated-apiserver-backend
```

That keeps the final RBAC/admission decision inside the aggregated-apiserver-backend instead of forcing the aggregated-apiserver-proxy to become a miniature Kubernetes authorizer.

## Terminology used in this document

This document has three servers in the chain. The middle one is, in Kubernetes' own terms, just another aggregated/extension apiserver — to Kubernetes there is no difference. The names below are role names for *this* topology:

- **front kube-apiserver** — the cluster's main kube-apiserver that clients talk to. It authenticates the user and proxies aggregated API paths onward.
- **aggregated-apiserver-proxy** — the component you are building. It is registered with the front kube-apiserver via an `APIService`, so in Kubernetes' terms it *is* an aggregated/extension apiserver. This document calls it the *proxy* because it does not execute requests itself; it forwards them.
- **aggregated-apiserver-backend** — the apiserver behind the proxy that actually executes the request. Nothing about it is special to Kubernetes; the only thing that makes it the "backend" here is that the aggregated-apiserver-proxy sits in front of it. This document assumes it is a standard `k8s.io/apiserver`-based aggregated API server using delegated authentication and authorization — that assumption is what makes impersonation toward it work, and it is examined in [How the aggregated-apiserver-backend actually evaluates the request](#how-the-aggregated-apiserver-backend-actually-evaluates-the-request).

Where this document quotes the official Kubernetes contract it still uses the generic term *extension apiserver*, since that contract applies to any aggregated apiserver, proxy or not.

## The two mechanisms

### 1. Requestheader / authenticating proxy mode

Requestheader auth is about trusting a proxy to authenticate users and pass their identity in headers.

The incoming request to your aggregated-apiserver-proxy does not carry Alice's original token or client cert. Instead, the front kube-apiserver authenticates Alice, then forwards identity using configured headers such as:

```
X-Remote-User: alice
X-Remote-Group: devs
X-Remote-Extra-...
```

The aggregated-apiserver-proxy must not trust those headers blindly. It should trust them only when the incoming TLS connection is authenticated with a client certificate signed by the configured requestheader/front-proxy CA and, normally, with an allowed certificate common name. Kubernetes publishes the relevant CA, allowed names, and header names in the `extension-apiserver-authentication` ConfigMap.

The relevant kube-apiserver flags are roughly:

```
--requestheader-client-ca-file=<path to aggregator CA cert>
--requestheader-allowed-names=front-proxy-client
--requestheader-extra-headers-prefix=X-Remote-Extra-
--requestheader-group-headers=X-Remote-Group
--requestheader-username-headers=X-Remote-User
--proxy-client-cert-file=<path to aggregator proxy cert>
--proxy-client-key-file=<path to aggregator proxy key>
```

The kube-apiserver command reference says the proxy client certificate is used to prove the identity of the aggregator or kube-apiserver when it calls out during a request, including when proxying to a user API server; components receiving calls from kube-aggregator should use the CA from the `extension-apiserver-authentication` ConfigMap for their half of mutual TLS verification.

The scary part: whoever is trusted as an authenticating front proxy can assert user identity through headers. That is why the docs warn not to reuse a CA in another context unless you understand the risks; the authentication docs specifically say an authenticating proxy must present a valid client certificate before request headers are checked, to prevent header spoofing.

### 2. Impersonation mode

Impersonation is about an already-authenticated caller asking the kube-apiserver to evaluate the request as another identity.

Example headers:

```
Impersonate-User: alice
Impersonate-Group: devs
```

Kubernetes' impersonation flow is explicit:

1. Caller authenticates as itself.
2. API server checks whether caller has impersonation privileges.
3. API server replaces request user info with impersonated values.
4. Authorization evaluates the request as the impersonated user.

That is not just conceptual; the Kubernetes user impersonation docs state that impersonation requests authenticate as the requesting user first, then switch to the impersonated user info, and authorization acts on the impersonated user info.

So if your aggregated-apiserver-proxy calls the aggregated-apiserver-backend like this:

```
Authorization: Bearer <your-proxy-serviceaccount-token>
Impersonate-User: alice
Impersonate-Group: devs
```

then the aggregated-apiserver-backend does:

```
authenticate proxy service account
check proxy service account may impersonate alice/groups/etc.
replace request identity with alice/groups/etc.
authorize the actual request as alice
run admission
execute request
```

The aggregated-apiserver-proxy does not have to manually reproduce the backend's RBAC decision for the actual resource operation, provided it is truly just forwarding and not doing extra side-effect work.

## Who does the authorization check?

There are two different authorization checks in the architecture.

### Authorization check A: front kube-apiserver before the aggregated-apiserver-proxy

When the original client calls the aggregated API path, the front kube-apiserver performs normal Kubernetes authentication and authorization before it proxies to your aggregated-apiserver-proxy. The aggregation docs explicitly say the kube-apiserver authenticates the user and authorizes access to the requested API path before proxying.

So this happens before your aggregated-apiserver-proxy sees the request:

```
client -> front kube-apiserver
          authn user
          authz user against aggregated API path/resource
          proxy request to your aggregated-apiserver-proxy
```

That answers part of your question:

**Yes, some authorization has already happened in the front kube-apiserver.**

But that authorization is about whether the user may access the aggregated API path/resource as registered through APIService. It does not automatically prove that every downstream thing the aggregated-apiserver-proxy might do is safe.

### Authorization check B: aggregated-apiserver-backend when using impersonation

If the aggregated-apiserver-proxy forwards to the aggregated-apiserver-backend using impersonation, then the aggregated-apiserver-backend becomes the authority for the final resource operation.

```
your aggregated-apiserver-proxy -> aggregated-apiserver-backend
              authenticate proxy SA
              check proxy SA may impersonate user/groups
              authorize request as impersonated user
```

This is the key design win. You can let the aggregated-apiserver-backend answer:

> "May Alice actually get/list/watch/create/update/delete this backend resource?"

Kubernetes authorization is performed inside the API server, and access is denied by default unless some authorization mechanism allows it.

Exactly *how* the aggregated-apiserver-backend performs that check — and why it can, even though it is usually not a kube-apiserver and has no RBAC store of its own — is covered in [How the aggregated-apiserver-backend actually evaluates the request](#how-the-aggregated-apiserver-backend-actually-evaluates-the-request).

### Authorization check C: the aggregated-apiserver-proxy's own optional check

The official extension-apiserver model says the extension apiserver can authorize the original user by sending a SubjectAccessReview back to the Kubernetes apiserver, and Kubernetes includes the `system:auth-delegator` ClusterRole for allowing extension apiservers to submit those reviews.

For a normal extension apiserver that executes the operation itself, this is important:

```
front kube-apiserver -> extension apiserver
                        extension apiserver does SubjectAccessReview
                        extension apiserver executes
```

For your pass-through proxy case, I'd separate it like this:

```
If your aggregated-apiserver-proxy only forwards faithfully to the aggregated-apiserver-backend:
  backend impersonation authz is usually enough for the backend operation.

If your aggregated-apiserver-proxy performs its own side effects before/around forwarding:
  your aggregated-apiserver-proxy probably needs its own authorization decision too.
```

Examples where your aggregated-apiserver-proxy should probably do its own SubjectAccessReview:

- It writes audit events to a separate sink before the backend accepts the request.
- It mutates the request.
- It fans out one incoming request into multiple backend requests.
- It maps one API path to a different backend resource/namespace/verb.
- It performs cache writes, status writes, notifications, commits, or side effects.
- It accepts non-resource URLs and translates them.
- It hides backend errors or returns synthetic success.

If it is a transparent byte-ish HTTP proxy with identity forwarding and audit observation, you can probably keep the aggregated-apiserver-proxy's own authorization minimal and rely on the aggregated-apiserver-backend for final authorization.

### How the aggregated-apiserver-backend actually evaluates the request

So far this document has spoken of the aggregated-apiserver-backend "authorizing" the request and "running admission" as if it were a kube-apiserver. It is worth being precise, because the aggregated-apiserver-backend is usually *not* a kube-apiserver — it is itself an aggregated/extension apiserver (a `sample-apiserver`-style server, `cozystack-api`, and so on).

An aggregated API server built with `k8s.io/apiserver` — the "recommended options" / `genericapiserver` machinery — runs the *same request-handling filter chain* a kube-apiserver runs, because both are built from the same `k8s.io/apiserver` code:

```
WithAuthentication -> WithImpersonation -> WithAuthorization -> admission -> resource handler
```

The difference between a kube-apiserver and an aggregated-apiserver-backend is not the pipeline. It is what *backs* authentication and authorization:

| Stage | kube-apiserver | aggregated-apiserver-backend (generic apiserver) |
|---|---|---|
| Authentication | its own authenticators: client certs, tokens, OIDC, ... | **delegated** — a bearer token is verified with a `TokenReview` against the kube-apiserver; `X-Remote-*` is verified against the requestheader CA published in the `extension-apiserver-authentication` ConfigMap |
| Authorization | its own in-process RBAC authorizer, reading `Role`/`ClusterRole`/binding objects | **delegated** — every decision is a `SubjectAccessReview` sent to the kube-apiserver |
| Admission | local | local |

So when the aggregated-apiserver-proxy calls the aggregated-apiserver-backend with the proxy ServiceAccount token plus `Impersonate-User: alice`, the backend does this:

1. Its `WithAuthentication` filter authenticates the caller. The bearer token is resolved by a `TokenReview` to the kube-apiserver, yielding `system:serviceaccount:<ns>:<proxy-sa>`.
2. Its `WithImpersonation` filter sees the `Impersonate-*` headers and, for each, asks whether the caller may `impersonate` the resource `users` / `groups` / `uids` / `userextras`. Because authorization is delegated, that question is a `SubjectAccessReview` to the kube-apiserver, evaluated against cluster RBAC.
3. If allowed, the filter replaces the request identity with Alice.
4. Its `WithAuthorization` filter authorizes the actual resource operation as Alice — again a `SubjectAccessReview` to the kube-apiserver.
5. Its local admission chain runs, then the resource handler executes.

The correction to the mental model is this:

> The impersonation filter, and the *enforcement* of its result, live inside the aggregated-apiserver-backend.
> The impersonation *policy decision* — and the final resource RBAC decision — live in the kube-apiserver, reached over `SubjectAccessReview`.

The aggregated-apiserver-backend has no RBAC store of its own; it borrows the cluster's. So an error such as:

```
User "system:serviceaccount:audit-proxy:audit-proxy" cannot impersonate resource "users"
```

is produced *by the aggregated-apiserver-backend's own impersonation filter*, reporting a `SubjectAccessReview` that the kube-apiserver denied. That error is the useful signal that the backend recognised the impersonation header and ran the standard flow — it is a sign the mechanism works, not that it is broken.

#### This is a real prerequisite on the backend

Impersonation mode therefore only works if the aggregated-apiserver-backend is a delegated-authn/authz generic apiserver, *and* it is wired for it:

- it needs the `system:auth-delegator` ClusterRole, so it may submit `TokenReview` and `SubjectAccessReview` requests;
- it needs the `extension-apiserver-authentication-reader` role in `kube-system`, so it can read the requestheader configuration.

These are the standard bindings for any aggregated API server. The catch is that the requestheader-forwarding path does not exercise delegated *authorization* at all, so a backend that has only ever been driven in requestheader mode may never have had this path tested.

If the backend is *not* a generic apiserver — for example a hand-rolled HTTP server that simply trusts inbound `X-Remote-*` headers — then it has no `WithImpersonation` filter. It will silently ignore `Impersonate-*` headers, and impersonation mode will not behave as described here. In that case you are effectively back to requestheader forwarding, or to the backend trusting the proxy ServiceAccount directly with no per-user check.

## Recommended architecture for your case

For something like apiserver-audit-proxy, I would use this mental model:

```
                 authn/authz #1
client ───────> front kube-apiserver
                    |
                    | requestheader identity forwarding
                    | mTLS: front kube-apiserver proves it is trusted front proxy
                    | X-Remote-User / X-Remote-Group
                    v
                your aggregated-apiserver-proxy
                    |
                    | derive impersonation from trusted X-Remote-* identity
                    | strip untrusted headers
                    | use proxy service account/cert
                    v
                aggregated-apiserver-backend
                delegated authn / impersonation check / delegated authz #2 / local admission
```

The responsibilities split nicely:

**front kube-apiserver:**

- authenticate original client
- authorize access to aggregated API path
- proxy to aggregated-apiserver-proxy
- pass user info via requestheader headers

**aggregated-apiserver-proxy:**

- authenticate the front kube-apiserver via requestheader/front-proxy cert trust
- trust X-Remote-* only after that validation
- remove any user-supplied spoofing headers
- convert trusted X-Remote-* identity into backend Impersonate-* headers
- forward faithfully
- capture audit information
- avoid making extra authorization decisions unless it performs extra side effects

**aggregated-apiserver-backend:**

- authenticate the aggregated-apiserver-proxy identity
- check the aggregated-apiserver-proxy may impersonate the requested user/groups
- authorize the actual backend request as the impersonated user
- run admission
- execute or reject

When the aggregated-apiserver-backend is a standard `k8s.io/apiserver`-based server, the first three of these are *delegated* checks: it does not evaluate them from a local RBAC store but forwards them to the kube-apiserver as `TokenReview` and `SubjectAccessReview` calls. See [How the aggregated-apiserver-backend actually evaluates the request](#how-the-aggregated-apiserver-backend-actually-evaluates-the-request).

This is cleaner than making the aggregated-apiserver-proxy act as a full authorizer. It keeps Kubernetes RBAC and admission where operators expect them — admission local to the aggregated-apiserver-backend, and the RBAC decision in the cluster the backend delegates to — instead of in the proxy.

## Requestheader vs impersonation: comparison

| Topic | Requestheader / front-proxy | Impersonation |
|---|---|---|
| Main purpose | Forward an already-authenticated user identity from a trusted proxy to an API server | Let an authenticated caller ask the API server to evaluate a request as another identity |
| Common place in Kubernetes | kube-apiserver / aggregation layer → extension apiserver | Client/controller/proxy → kube-apiserver |
| Trust root | mTLS client cert from trusted front proxy CA | Kubernetes authentication + RBAC impersonate permission |
| Identity headers | `X-Remote-User`, `X-Remote-Group`, `X-Remote-Extra-*` or configured equivalents | `Impersonate-User`, `Impersonate-Group`, `Impersonate-Uid`, `Impersonate-Extra-*` |
| Who checks whether proxy is trusted? | Receiving extension apiserver validates front-proxy client certificate and allowed CN | Receiving kube-apiserver authenticates the real caller normally |
| Who checks whether identity substitution is allowed? | Trust is mostly in the front-proxy cert boundary | Kube-apiserver checks impersonate permission |
| Who authorizes the actual request? | Extension apiserver should authorize original user, often via SubjectAccessReview | Kube-apiserver authorizes as impersonated user |
| Failure mode | If front-proxy cert/key is compromised, attacker may assert users via headers | If impersonation RBAC is too broad, proxy can act as many users |
| Best fit | Implementing an aggregated API backend | Calling a kube-apiserver on behalf of another user |

## RBAC shape for impersonation

The basic broad role shape is:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: proxy-impersonator
rules:
- apiGroups: [""]
  resources: ["users", "groups", "serviceaccounts"]
  verbs: ["impersonate"]
```

That is powerful. The official docs show this general shape, but also point out that impersonation header values can be restricted with `resourceNames`.

A more constrained version might look like:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: proxy-impersonate-selected-identities
rules:
- apiGroups: [""]
  resources: ["users"]
  verbs: ["impersonate"]
  resourceNames:
  - "alice@example.com"
  - "bob@example.com"
- apiGroups: [""]
  resources: ["groups"]
  verbs: ["impersonate"]
  resourceNames:
  - "developers"
  - "platform-team"
```

The docs also warn that impersonating a user or group lets you perform any action as that user or group, which is why normal user/group impersonation is not namespace-scoped and requires ClusterRole/ClusterRoleBinding.

That point matters a lot. An aggregated-apiserver-proxy with broad impersonation over users and groups is effectively a high-trust component. You should treat it closer to control-plane infrastructure than a normal app.

## Important nuance: constrained impersonation

The current docs also describe constrained impersonation as a Kubernetes v1.36 beta feature. The idea is to split permission into:

1. permission to impersonate a specific identity
2. permission to perform specific actions while impersonating that identity

The docs describe modes such as `impersonate:user-info` and `impersonate-on:user-info:<verb>` for restricting what an impersonator may do while impersonating.

That is very relevant to your architecture, but I would not build your core design around it unless you know your target clusters support it and have the feature gate enabled. For broad compatibility, assume classic impersonation semantics:

```
Can impersonate Alice
  => can attempt anything Alice can do
  => backend RBAC still decides what Alice can do
```

Constrained impersonation could become a very nice hardening option later.
Track where it is heading in [KEP-5284](https://github.com/kubernetes/enhancements/issues/5284).

## Security checklist

### A. Incoming requestheader / APIService side

#### Validate the front-proxy client certificate

Your aggregated-apiserver-proxy must not trust `X-Remote-User` merely because the header exists.

It should validate that the incoming TLS client cert:

- is signed by the requestheader/front-proxy CA
- has an allowed Common Name, unless allowed names are intentionally empty
- is presented on the actual mTLS connection

The aggregation docs describe exactly this validation responsibility for extension apiservers.

#### Read trust configuration from extension-apiserver-authentication

> **Status: done.** The proxy sources its inbound trust live from this
> ConfigMap. See [`requestheader-trust-design.md`](requestheader-trust-design.md).

Your aggregated-apiserver-proxy should use the Kubernetes-published configuration where possible:

```
kube-system/configmap/extension-apiserver-authentication
  - requestheader-client-ca-file data
  - requestheader-allowed-names
  - requestheader-username-headers
  - requestheader-group-headers
  - requestheader-extra-headers-prefix
```

The docs say kube-apiserver creates this ConfigMap and that extension apiservers use it to validate requests.

The proxy reads it through the standard upstream dynamic controllers
(`dynamiccertificates.NewDynamicCAFromConfigMapController` plus
`headerrequest.NewRequestHeaderAuthRequestController`), so aggregator CA
rotation is adopted live. There is no per-install CA file, CA Secret, or
allowed-names list, and no flag whose omission produces an unverified proxy.

#### Bind extension-apiserver-authentication-reader

> **Status: done.** The chart always renders this RoleBinding
> (`templates/auth-reader-rbac.yaml`).

Your aggregated-apiserver-proxy's service account needs permission to read that ConfigMap. Kubernetes provides the `extension-apiserver-authentication-reader` role in `kube-system` for this purpose. The proxy fails startup loudly if it cannot read the ConfigMap — a missing RoleBinding is a clear `Forbidden` startup error, never a silent fallback to unverified trust.

#### Do not reuse the normal client CA as the requestheader CA

Keep these separate:

```
--client-ca-file
--requestheader-client-ca-file
```

The aggregation docs explicitly warn that these two CA uses can conflict if reused incorrectly.

#### Treat `--requestheader-allowed-names=""` as dangerous

An empty allowed-names setting means any CN signed by the requestheader CA is acceptable. That may be valid in a controlled setup, but for your aggregated-apiserver-proxy I'd prefer explicit names where possible. The docs state that empty allowed names means any CN is acceptable.

#### Reject direct access to the aggregated-apiserver-proxy

Ideally, your aggregated-apiserver-proxy should not be exposed as a general Service that clients can hit directly.

At minimum:

- require mTLS client cert validation
- reject requests without the front-proxy client cert
- reject requests with invalid CN
- do not accept plain HTTP
- consider NetworkPolicy to only allow kube-apiserver/control-plane source traffic

The main risk is someone bypassing the front kube-apiserver and sending their own `X-Remote-User` headers.

#### Strip inbound identity and impersonation headers before forwarding

On the request received by the aggregated-apiserver-proxy, consider these tainted unless they are produced by the validated front kube-apiserver:

- `X-Remote-User`
- `X-Remote-Group`
- `X-Remote-Extra-*`
- `Impersonate-User`
- `Impersonate-Group`
- `Impersonate-Uid`
- `Impersonate-Extra-*`
- `Authorization`

Practically: parse the trusted `X-Remote-*` identity after authenticating the front proxy, then construct a clean outgoing request to the aggregated-apiserver-backend.

Do not let the original client smuggle `Impersonate-*` headers through your component.

### B. Outgoing impersonation side

#### Confirm the aggregated-apiserver-backend can evaluate impersonation

Impersonation mode assumes the aggregated-apiserver-backend runs the standard `k8s.io/apiserver` request pipeline with delegated authentication and authorization. Before relying on it, confirm:

- the backend is a generic apiserver, so it actually has a `WithImpersonation` filter;
- the backend's service account holds the `system:auth-delegator` ClusterRole, so it may issue `TokenReview` and `SubjectAccessReview` calls;
- the backend can read the `extension-apiserver-authentication` ConfigMap via the `extension-apiserver-authentication-reader` role in `kube-system`.

A backend that merely trusts inbound `X-Remote-*` headers has no impersonation filter and will silently ignore `Impersonate-*`. See [How the aggregated-apiserver-backend actually evaluates the request](#how-the-aggregated-apiserver-backend-actually-evaluates-the-request).

#### Use a dedicated service account or client cert for the aggregated-apiserver-proxy

Do not use a general-purpose admin identity.

Use something like:

```
system:serviceaccount:<namespace>:apiserver-audit-proxy
```

That identity should have only the permissions needed to contact the aggregated-apiserver-backend and impersonate the expected identities.

#### Keep impersonation RBAC as narrow as possible

Avoid this unless you explicitly accept the risk:

```yaml
resources: ["users", "groups", "serviceaccounts"]
verbs: ["impersonate"]
```

Prefer `resourceNames` where realistic:

```yaml
resources: ["groups"]
verbs: ["impersonate"]
resourceNames: ["developers", "platform-team"]
```

Kubernetes supports restricting impersonation header values using `resourceNames`.

#### Be very careful with group impersonation

Groups can be more dangerous than users.

This is especially true for groups like:

```
system:masters
system:authenticated
system:serviceaccounts
system:serviceaccounts:<namespace>
```

In many clusters, group membership drives broad privileges. Accidentally allowing the aggregated-apiserver-proxy to impersonate privileged groups is effectively giving it broad administrative power.

#### Decide whether to forward extra fields and UID

Kubernetes supports:

```
Impersonate-Uid: ...
Impersonate-Extra-foo: ...
```

But do not forward these blindly unless you know the backend uses them and you have RBAC for them. Kubernetes treats impersonated UIDs and extra fields under the `authentication.k8s.io` API group for RBAC purposes.

For an initial implementation, I would likely forward only:

```
Impersonate-User
Impersonate-Group
```

Then add UID/extras later if you can prove they matter.

#### Expect the aggregated-apiserver-backend to audit impersonation

Kubernetes docs say an audit event is logged for each impersonation request.

That is good. It means you can later confirm:

```
real user: proxy service account
impersonated user: alice
request verb/resource/namespace/name
authorization result
```

But do not assume this replaces your own audit proxy's event model. It is one layer of evidence, not the whole story.

### C. Authorization model

#### Rely on aggregated-apiserver-backend RBAC for backend resources

If the aggregated-apiserver-proxy uses impersonation toward the aggregated-apiserver-backend, let the aggregated-apiserver-backend decide whether the impersonated user may perform the operation.

That gives you this clean invariant:

> If Alice could not do it directly on the aggregated-apiserver-backend,
> Alice should not be able to do it through the aggregated-apiserver-proxy.

#### Add proxy-side SubjectAccessReview only for proxy-owned behavior

Your aggregated-apiserver-proxy should consider a local/delegated authorization check when it does anything beyond transparent forwarding.

Use a SubjectAccessReview when the aggregated-apiserver-proxy itself is the actor for a side effect. Kubernetes' aggregation docs describe extension apiservers authorizing the original user through SubjectAccessReview, with `system:auth-delegator` as the built-in role for submitting those reviews.

Examples:

- "May Alice use this proxy feature?"
- "May Alice request this synthetic operation?"
- "May Alice cause this audit sink write?"
- "May Alice trigger this translation/fan-out?"

#### Do not confuse front authorization with backend authorization

The front kube-apiserver's authorization check says:

> Alice may access the aggregated API path.

The aggregated-apiserver-backend's authorization check says:

> Alice may perform this actual backend operation.

Those are not necessarily the same thing. Keep both in your mental model.

### D. Request integrity and proxy behavior

#### Forward method, path, query, body, and content type faithfully

For an audit/proxy component, avoid hidden semantic changes.

Be careful with:

- URL path rewriting
- query parameters: watch, timeoutSeconds, resourceVersion, fieldSelector, labelSelector
- subresources: status, scale, finalizers, logs, exec, attach, proxy
- verbs: get/list/watch/create/update/patch/delete/deletecollection
- content types: JSON, strategic merge patch, JSON patch, apply patch

Kubernetes RBAC often distinguishes subresources, and admission can behave differently based on verb and content type.

#### Be careful with streaming requests

Kubernetes has awkward edge cases around:

```
watch
exec
attach
portforward
logs with follow=true
proxy subresources
```

For an audit proxy, streaming and upgrade-like behavior can complicate both request handling and audit completion semantics.

#### Preserve failure semantics

If the backend says:

```
403 Forbidden
401 Unauthorized
404 NotFound
409 Conflict
422 Invalid
```

your aggregated-apiserver-proxy should generally return that faithfully.

Do not convert backend denials into proxy-level successes.

#### Do not perform optimistic side effects before backend authorization completes

If your aggregated-apiserver-proxy writes "accepted" audit records before the backend has actually accepted the request, label them carefully.

A clean audit model distinguishes:

```
request received by proxy
request forwarded to backend
backend response complete
backend rejected/accepted
```

That maps nicely to Kubernetes audit-stage thinking, but avoid claiming authorization success before the backend response.

### E. Operational hardening

#### Treat the aggregated-apiserver-proxy as control-plane-grade

If it can impersonate users toward the aggregated-apiserver-backend, it is not "just an app".

Protect:

- service account token
- mounted certificates
- pod exec access
- logs if they include headers
- crash dumps
- debug endpoints
- metrics labels

#### Avoid logging secrets and auth headers

Never log:

- `Authorization`
- `Cookie`
- client certificate material
- service account tokens
- `Impersonate-*` if that leaks sensitive identity context in your environment

You may want to log normalized identity separately:

```json
{
  "frontProxyUser": "alice",
  "frontProxyGroups": ["devs"],
  "backendImpersonateUser": "alice",
  "backendImpersonateGroups": ["devs"],
  "verb": "get",
  "resource": "pods",
  "namespace": "default"
}
```

#### Add explicit startup self-checks

On startup, your aggregated-apiserver-proxy can verify:

- can read extension-apiserver-authentication ConfigMap
- has loaded requestheader CA
- has non-empty / expected allowed names
- can reach the aggregated-apiserver-backend
- its backend identity is who you expect
- impersonation of a harmless test identity behaves as expected, if suitable

#### Use kubectl auth can-i for manual checks

For debugging, `kubectl auth can-i` pairs well with impersonation via `--as` / `--as-group`. The official `kubectl auth can-i` docs call out that it pairs with impersonation.

Example:

```bash
kubectl auth can-i list pods \
  --as=alice@example.com \
  --as-group=developers
```

That helps distinguish:

```
aggregated-apiserver-backend denies Alice
vs.
aggregated-apiserver-proxy is failing impersonation
vs.
front aggregation auth is failing
```

## Suggested implementation stance

For your situation, I would use this as the default design:

**Incoming side:**
requestheader/front-proxy auth, because this is the native APIService contract.

**Outgoing side:**
impersonation, because the aggregated-apiserver-backend should make the final RBAC/admission decision.

**Proxy-side authz:**
minimal for transparent forwarding; SubjectAccessReview only for proxy-owned side effects or semantic translations.

That gives you a defensible trust model:

- front kube-apiserver authenticates original users
- your aggregated-apiserver-proxy only trusts identity from a validated front proxy
- the aggregated-apiserver-backend enforces the final operation, delegating the decision to the kube-apiserver's RBAC
- impersonation RBAC describes exactly how powerful the aggregated-apiserver-proxy is
- audit data can show both proxy identity and impersonated identity

The design smell to avoid is this:

> "Headers came in, so I trusted them."

The better invariant is:

> "I trusted X-Remote-* only because the mTLS client was the configured front proxy.
> I created Impersonate-* myself.
> The aggregated-apiserver-backend then decided whether the impersonated user may act."

That is the cleanest model for your aggregated-apiserver-proxy + aggregated-apiserver-backend experiment.

## References

- Kubernetes — [Configure the Aggregation Layer](https://kubernetes.io/docs/tasks/extend-kubernetes/configure-aggregation-layer/)
- Kubernetes — [User impersonation: constrained impersonation](https://kubernetes.io/docs/reference/access-authn-authz/user-impersonation/#constrained-impersonation)
