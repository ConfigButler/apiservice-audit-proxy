# Connections and TLS

This document maps every network hop used by `apiservice-audit-proxy`, what
TLS or identity material is involved, which Helm values influence it, and how
the local demo currently behaves.

The easiest way to reason about the certificates is to name the client and
server for each hop:

- the **server certificate** lets the server prove its identity to the client
- the **client certificate** lets the client prove its identity to the server
- a **CA bundle** is how one side decides which certificates to trust

## Hop Map

| Hop | Client | Server | Current demo transport | Main controls |
|---|---|---|---|---|
| Kubernetes API aggregation | kube-apiserver | apiservice-audit-proxy | HTTPS | `certificates.*`, `apiService.*` |
| Delegated user identity | kube-apiserver/front-proxy | apiservice-audit-proxy | same HTTPS request | `requestHeader.*` |
| Backend API call | apiservice-audit-proxy | sample-apiserver | HTTPS | `backend.*`, `testApiserver.backendServingCert.*`, `testApiserver.backendClientCert.*` |
| Proxy audit webhook, Lane B | apiservice-audit-proxy | webhook-tester | HTTP Service | `webhook.*`, `webhookTester.*` |
| Native kube-apiserver audit webhook, Lane A | kube-apiserver | Traefik, then webhook-tester | HTTPS to Traefik, HTTP to webhook-tester | `test/e2e/cluster/audit/webhook-config.yaml`, Traefik Flux values |
| Human browser UI | developer browser | webhook-tester | HTTP port-forward | `e2e:portforward-webhook-tester` |

## 1. kube-apiserver to Proxy

The proxy is an aggregated API server. When `apiService.enabled=true`, the chart
registers an `APIService` that points kube-apiserver at the proxy Service.

```text
kube-apiserver  --->  apiservice-audit-proxy
                 proxy presents server cert
```

The proxy's serving certificate is controlled by:

```yaml
certificates:
  mode: cert-manager | dev-self-signed | existing-secret
```

The serving certificate is mounted into the proxy pod and used by:

```text
--tls-cert-file
--tls-private-key-file
```

The kube-apiserver trust side is controlled by `apiService.*`:

- `apiService.caBundle`: explicit CA bundle in the `APIService`
- `apiService.insecureSkipTLSVerify`: disables backend verification for this APIService
- `certificates.mode=cert-manager`: chart annotates the `APIService` for cert-manager CA injection
- `certificates.mode=dev-self-signed`: chart forces `insecureSkipTLSVerify=true` for local development

Templates:

- `templates/certificates.yaml`
- `templates/dev-serving-cert-secret.yaml`
- `templates/deployment.yaml`
- `templates/apiservice.yaml`

## 2. Delegated User Identity into the Proxy

Aggregated API requests arrive with front-proxy identity headers such as
`X-Remote-User`. Those headers are powerful, so the proxy must only trust them
from a verified kube-apiserver/front-proxy client.

```text
kube-apiserver  --->  apiservice-audit-proxy
                 request has X-Remote-* headers
                 proxy verifies client cert against requestHeader CA
```

The chart control is:

```yaml
requestHeader:
  clientCASecretName: audit-pass-through-requestheader-client-ca
  clientCAFileName: ca.crt
```

When set, the proxy receives:

```text
--client-ca-file=/var/run/audit-pass-through/requestheader-client-ca/ca.crt
```

This is not the proxy's serving certificate. It is the CA used to decide whether
the incoming client is allowed to provide delegated identity headers.

In local e2e, `task e2e:prepare-requestheader-client-ca` copies the cluster's
requestheader client CA into the proxy namespace before Helm installs the proxy.

## 3. Proxy to Backend

The proxy forwards the actual aggregated API request to the real backend.

```text
apiservice-audit-proxy  --->  sample-apiserver backend
```

There are two independent TLS questions on this hop.

### Backend Serving Certificate

The backend serving certificate lets the backend prove its identity to the
proxy.

```text
proxy  --->  backend
        backend presents server cert
        proxy verifies it
```

Proxy-side controls:

```yaml
backend:
  insecureSkipVerify: false
  caSecretName: audit-pass-through-backend-serving-ca
  caFileName: ca.crt
  serverName: api-backend.wardle.svc.cluster.local
```

Demo backend controls:

```yaml
testApiserver:
  backendServingCert:
    enabled: true
    selfSignedIssuerName: audit-pass-through-backend-serving-selfsigned
    caSecretName: audit-pass-through-backend-serving-ca
    caIssuerName: audit-pass-through-backend-serving-ca-issuer
    certSecretName: audit-pass-through-backend-serving-cert
    dnsNames: []
```

When enabled, the chart creates a backend-serving CA, a backend serving
certificate, mounts it into the sample-apiserver pod, and starts the backend
with:

```text
--tls-cert-file=/etc/wardle/serving-cert/tls.crt
--tls-private-key-file=/etc/wardle/serving-cert/tls.key
```

The proxy trusts that certificate through `backend.caSecretName` and validates
the DNS name through `backend.serverName`.

The default smoke values keep this simpler:

```yaml
backend:
  insecureSkipVerify: true
```

The backend-CA smoke values exercise the stricter path.

### Backend Client Certificate

The backend client certificate lets the proxy prove its identity to the backend.

```text
proxy presents client cert  --->  backend
backend verifies proxy cert
```

Proxy-side controls:

```yaml
backend:
  clientCertSecretName: audit-pass-through-backend-client-cert
  clientCertFileName: tls.crt
  clientKeyFileName: tls.key
```

Demo backend controls:

```yaml
testApiserver:
  backendClientCert:
    selfSignedIssuerName: audit-pass-through-backend-selfsigned
    caSecretName: audit-pass-through-backend-client-ca
    caIssuerName: audit-pass-through-backend-client-ca-issuer
    clientSecretName: audit-pass-through-backend-client-cert
```

The chart creates a client-auth CA and a proxy client certificate. It mounts the
CA into the sample-apiserver pod and starts the backend with:

```text
--client-ca-file=/etc/wardle/proxy-client-ca/ca.crt
```

The proxy mounts `backend.clientCertSecretName` and sends that certificate when
connecting to the backend.

Short version:

```text
backendServingCert = backend proves identity to proxy
backendClientCert  = proxy proves identity to backend
```

Using both is mutual TLS for the proxy-to-backend hop.

## 4. Proxy to Audit Webhook, Lane B

The proxy emits synthetic audit events using a kubeconfig-style webhook client.
The chart always mounts the Secret named by:

```yaml
webhook:
  kubeconfigSecretName: audit-pass-through-webhook-kubeconfig
  kubeconfigKey: kubeconfig
  timeout: 5s
```

When `webhookTester.enabled=true`, the chart generates that Secret and points
the proxy directly at the in-cluster webhook-tester Service:

```yaml
server: http://<webhook-tester-service>.<namespace>.svc.cluster.local:8080/<proxy-session-uuid>
```

So the current Lane B demo path is:

```text
apiservice-audit-proxy  --->  webhook-tester Service
                         HTTP
```

There is no webhook-tester serving certificate and no proxy client certificate
on this demo path today.

For a production HTTPS audit webhook, the TLS settings belong in the supplied
kubeconfig Secret. A kubeconfig can carry or reference CA data, client
certificates, client keys, bearer tokens, and other standard client auth
material. In that mode, leave `webhookTester.enabled=false` or override
`webhook.kubeconfigSecretName` with a Secret you manage.

Template:

- `templates/webhook-tester-kubeconfig-secret.yaml`

## 5. kube-apiserver to Audit Webhook, Lane A

The e2e cluster also configures kube-apiserver's native audit webhook so the
tests can show the aggregated API audit gap side by side.

This is not configured by the Helm chart. It is baked into the k3d server at
cluster creation time through:

- `test/e2e/cluster/audit/policy.yaml`
- `test/e2e/cluster/audit/webhook-config.yaml`
- `test/e2e/cluster/start-cluster.sh`

The webhook kubeconfig points kube-apiserver at Traefik's fixed NodePort:

```yaml
server: https://127.0.0.1:30444/<kube-apiserver-session-uuid>
insecure-skip-tls-verify: true
```

The path is:

```text
kube-apiserver  --->  Traefik websecure NodePort 30444  --->  webhook-tester Service
                 HTTPS with Traefik's default cert            HTTP inside cluster
```

`insecure-skip-tls-verify` is intentional in the current e2e setup because
Traefik uses its default self-signed certificate. The goal of Lane A is not to
test webhook TLS; it is to capture kube-apiserver-native audit events for the
same API calls.

## 6. Human Browser to Webhook-Tester

For local development, the Taskfile opens a port-forward:

```bash
task e2e:portforward-webhook-tester
```

Then the browser talks to webhook-tester over HTTP on localhost:

```text
browser  --->  localhost:18090  --->  webhook-tester Service
          HTTP port-forward
```

The Helm chart also renders an optional Ingress when:

```yaml
webhookTester:
  ingress:
    enabled: true
    className: traefik
```

In the local e2e stack, Traefik is installed by Flux and exposes its `websecure`
entrypoint on NodePort `30444`. The kube-apiserver native audit lane uses that
entrypoint. The browser workflow currently uses port-forwarding because it is
predictable from the developer machine and does not require remembering host or
NodePort details.

## Current Demo Behavior

The standard smoke demo uses these choices:

| Area | Standard smoke | Backend-CA smoke |
|---|---|---|
| Proxy serving cert | cert-manager | cert-manager |
| kube-apiserver trusts proxy | cert-manager APIService CA injection | cert-manager APIService CA injection |
| Proxy verifies requestheader client | yes | yes |
| Backend verifies proxy client cert | yes | yes |
| Proxy verifies backend server cert | no, `insecureSkipVerify=true` | yes, `backend.caSecretName` + `serverName` |
| Proxy audit webhook lane | HTTP direct to webhook-tester Service | HTTP direct to webhook-tester Service |
| kube-apiserver audit webhook lane | HTTPS to Traefik, then HTTP to webhook-tester | same |
| Browser UI | HTTP port-forward | HTTP port-forward |

The demo intentionally uses separate certificate authorities for separate trust
questions:

- proxy serving certificate CA
- backend client-auth CA
- optional backend serving certificate CA
- requestheader client CA copied from the cluster

That is more verbose than a single demo CA, but it mirrors real Kubernetes
systems better. In production, these trust domains are often owned by different
controllers, clusters, or platform teams.

## Uniformity Question: Should Lane B Also Use Traefik?

Today the two audit lanes are intentionally different:

```text
Lane A: kube-apiserver  ->  Traefik  ->  webhook-tester
Lane B: proxy           ->             webhook-tester Service
```

This is operationally simple, but visually confusing. If the goal is to make
the demo as easy to explain as possible, it would be reasonable to route Lane B
through the same Traefik Ingress:

```text
Lane A: kube-apiserver  ->  Traefik  ->  webhook-tester
Lane B: proxy           ->  Traefik  ->  webhook-tester
```

That would make the demo topology more uniform. It would also make Lane B depend
on Traefik and the Ingress path, even though production users can and often will
send audit webhooks directly to an in-cluster Service or an external endpoint.

A clean way to support this would be an explicit demo value, not a hidden change
to all webhook-tester installs. For example:

```yaml
webhookTester:
  proxyDelivery:
    mode: service | ingress
    insecureSkipTLSVerify: true
```

Or, more generally, a value that overrides only the generated kubeconfig server:

```yaml
webhookTester:
  generatedKubeconfig:
    server: https://traefik.traefik-system.svc.cluster.local:443/<session>
    insecureSkipTLSVerify: true
```

The current chart does not implement either option. Right now, the generated
proxy webhook kubeconfig always points at the webhook-tester Service over HTTP.

## Recommendation

Keep the default proxy-to-webhook-tester path on HTTP for the first smoke path,
but treat production audit webhook delivery as security-sensitive.

For `gitops-reverser`, audit events are input to downstream behavior. A random
pod or network peer must not be able to submit audit events that look like they
came from this proxy. In production, the audit webhook receiver should verify
the sender, and the proxy should verify the receiver.

The first smoke path can still stay simple because it is trying to prove the
aggregated API proxy behavior. The other security-sensitive paths it already
exercises are:

- kube-apiserver to proxy
- proxy verification of requestheader identity
- proxy to backend

Those paths already exercise the certificate material that matters for the
aggregated API proxy story. Keeping proxy audit delivery as HTTP in the first
smoke test keeps that test focused and avoids making every e2e run depend on
Ingress behavior.

It is worth adding an explicit HTTPS webhook mode, but it should be a separate
scenario rather than the first smoke path. The value of that mode would be:

- prove that the proxy correctly consumes webhook kubeconfig TLS settings
- document how a production HTTPS audit receiver should be configured
- catch regressions in webhook delivery when CA data or `insecure-skip-tls-verify`
  is involved
- cover sender authentication, ideally with mTLS or another receiver-enforced
  client identity

It would not prove much about the core aggregated API audit gap, but it would
prove an important production property: the audit receiver can distinguish this
proxy from arbitrary clients.

Recommended path:

| Choice | Recommendation | Why |
|---|---|---|
| Default smoke demo | Keep HTTP direct to webhook-tester Service | Smallest moving parts; focuses on proxy/backend audit behavior |
| Visual demo topology | Optionally add `proxyDelivery.mode=ingress` | Makes Lane A and Lane B easier to explain side by side |
| TLS webhook coverage | Add a separate HTTPS webhook e2e variant | Tests kubeconfig CA / skip-verify behavior without complicating smoke |
| Webhook sender authentication | Add a production-style variant | Prevents forged audit events from arbitrary clients |
| Webhook mTLS | Prefer for the production-style variant when the receiver supports it | Gives the receiver a concrete client identity to authorize |

If we add `proxyDelivery.mode=ingress`, it should be an explicit demo choice.
The chart should continue to make `service` the default because it is the most
natural in-cluster Kubernetes path:

```yaml
webhookTester:
  proxyDelivery:
    mode: service
```

Then a topology-focused demo can opt into:

```yaml
webhookTester:
  proxyDelivery:
    mode: ingress
    server: https://127.0.0.1:30444/<proxy-session-uuid>
    insecureSkipTLSVerify: true
```

That keeps the story clean: the normal e2e verifies the proxy's core behavior,
and the optional ingress mode exists to show both audit lanes entering
webhook-tester through the same front door.

For a production-oriented webhook test, the better target is not just "same
Ingress path"; it is "authenticated audit delivery." A good future test would
stand up a receiver that requires a trusted proxy client certificate, configure
the proxy's webhook kubeconfig with client cert/key and CA data, and assert that
unauthenticated requests are rejected.

## Naming Guide

| Name | Meaning |
|---|---|
| `certificates.*` | How the proxy gets its own HTTPS serving certificate |
| `apiService.*` | How kube-apiserver reaches and trusts the proxy as an aggregated API server |
| `requestHeader.*` | How the proxy verifies the kube-apiserver/front-proxy client before trusting delegated identity headers |
| `backend.*` | How the proxy connects to and authenticates with the real backend |
| `testApiserver.backendServingCert.*` | Demo-only resources that give the sample backend a server certificate |
| `testApiserver.backendClientCert.*` | Demo-only resources that give the proxy a client certificate and the sample backend a client-auth CA |
| `webhook.*` | Which kubeconfig Secret the proxy uses for audit webhook delivery |
| `webhookTester.*` | Demo-only webhook receiver and UI |
