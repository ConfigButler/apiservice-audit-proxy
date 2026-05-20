# Helm Values Guide

This chart is intentionally explicit. `apiservice-audit-proxy` sits on a
security-sensitive path, so the chart does not try to guess certificate,
identity, or audit webhook settings.

Use this page with the full defaults in
[`values.yaml`](../charts/apiservice-audit-proxy/values.yaml).

## Main Decisions

| Decision | Values | What to choose |
|---|---|---|
| Proxy serving certificate | `server.tls.mode` | Use `cert-manager` when the cluster can issue serving certs, `existing-secret` when another platform owns the cert, or `self-signed` for a chart-managed CA with no external dependencies. |
| APIService registration | `server.apiService.*` | Enable it when the chart should register this proxy as the aggregated API backend. Leave it disabled if another controller owns the `APIService`. |
| Backend server trust | `backend.tls.caSecretName`, `backend.tls.serverName`, `backend.tls.insecureSkipVerify` | Prefer CA validation. Use `insecureSkipVerify=true` only for local/dev paths or while bootstrapping a demo. |
| Backend identity | `backend.identity.mode` | `impersonation` is the default. Use `requestheader` when the real backend trusts front-proxy `X-Remote-*` identity. Use `impersonation` when the backend can authorize Kubernetes impersonation and you do not want to provision a backend client private key. |
| Inbound requestheader trust | (no chart control) | Sourced live from the cluster's `kube-system/extension-apiserver-authentication` ConfigMap. The chart's kube-system RoleBinding grants the proxy ServiceAccount read access. |
| Audit webhook | `audit.webhook.*` | Point at a Secret containing the webhook kubeconfig for your receiver. `audit.testWebhookReceiver.enabled` is for demos. |

## Backend Identity Modes

### `requestheader`

`requestheader` preserves the historical behavior:

```text
kube-apiserver -> proxy:  X-Remote-* headers
proxy -> backend:         X-Remote-* headers
```

Use it when the real aggregated backend is designed to run behind a trusted
front proxy and accepts delegated requestheader identity. This is the natural
mode for many `k8s.io/apiserver`-style backends.

If the backend requires client certificate authentication on the backend hop,
set `backend.identity.requestheader.clientCert.secretName`. That Secret must
contain the proxy's client certificate and key. Avoid copying broad platform
private keys into this chart; prefer a dedicated client certificate scoped to
this proxy when the backend can trust one.

Minimal shape:

```yaml
backend:
  url: https://real-backend.example-system.svc:443
  tls:
    insecureSkipVerify: false
    caSecretName: backend-serving-ca
    serverName: real-backend.example-system.svc
  identity:
    mode: requestheader
    requestheader:
      clientCert:
        secretName: proxy-backend-client-cert
```

Inbound requestheader trust needs no configuration: the proxy reads the
front-proxy CA, the accepted client names, and the identity header names from
the cluster's `kube-system/extension-apiserver-authentication` ConfigMap. The
chart always renders a kube-system RoleBinding to the built-in
`extension-apiserver-authentication-reader` Role so the proxy ServiceAccount can
read it. Installing the chart therefore needs permission to create a
RoleBinding in `kube-system`.

### `impersonation`

`impersonation` is the default. It verifies the inbound requestheader identity,
then calls the backend as the proxy ServiceAccount with Kubernetes impersonation
headers:

```text
kube-apiserver -> proxy:  X-Remote-* headers
proxy -> backend:         Authorization: Bearer <proxy ServiceAccount token>
                          Impersonate-User / Group / Uid / Extra
```

Use it when the backend supports normal Kubernetes bearer-token auth and
impersonation authorization. This is useful on platforms such as CozyStack,
where taking ownership of the frontend proxy private key is undesirable.
`backend.identity.requestheader.clientCert.*` is structurally only meaningful
in requestheader mode and is ignored here -- the chart does not consult it.

Requirements:

- Inbound requestheader trust is cluster-sourced and always verified, so
  impersonation mode needs no inbound trust configuration. The accepted
  front-proxy client names come from the cluster's `requestheader-allowed-names`.
- The proxy ServiceAccount must be allowed to impersonate the users, groups,
  UIDs, and optional extras it forwards.

Minimal shape with chart-managed impersonation RBAC:

```yaml
backend:
  url: https://real-backend.example-system.svc:443
  tls:
    insecureSkipVerify: false
    caSecretName: backend-serving-ca
    serverName: real-backend.example-system.svc
  identity:
    mode: impersonation
    impersonation:
      forwardUid: true
      extras:
        mode: none
      rbac:
        create: true
```

For tightly managed clusters, leave
`backend.identity.impersonation.rbac.create=false` and provide your own
ClusterRole/ClusterRoleBinding. That keeps the impersonation grant visible in
the platform-owned RBAC layer.

## Extras In Impersonation Mode

User extras are powerful and open-ended. Start with:

```yaml
backend:
  identity:
    impersonation:
      extras:
        mode: none
```

Use `allowlist` only for extras your backend actually needs:

```yaml
backend:
  identity:
    impersonation:
      extras:
        mode: allowlist
        keys:
          - example.com/tenant
```

Avoid `extras.mode=all` unless the requestheader CA and allowed client names are
tightly controlled and the backend truly needs every extra. It renders broad
`userextras/*` impersonation RBAC when chart-managed RBAC is enabled.

## Certificate Modes

### `cert-manager`

Use this when cert-manager is installed and should issue the proxy serving
certificate:

```yaml
server:
  tls:
    mode: cert-manager
    certManager:
      issuerRef:
        group: cert-manager.io
        kind: Issuer
        name: apiservice-audit-proxy-issuer
```

In this mode the chart annotates the `APIService` for cert-manager CA injection.

### `existing-secret`

Use this when another platform component owns certificate issuance and rotation:

```yaml
server:
  tls:
    mode: existing-secret
    existingSecretName: apiservice-audit-proxy-serving-tls
  apiService:
    caBundle: <base64-ca-bundle>
```

This is often the right production mode when certificate lifecycle is centralized
outside the chart.

### `self-signed`

Use this when no external certificate authority is involved and the chart should
own the proxy serving cert end-to-end:

```yaml
server:
  tls:
    mode: self-signed
```

The chart generates a CA + leaf cert on first install (via Helm's `genCA` /
`genSignedCert`), writes them to the serving-cert Secret, and embeds the CA
into `APIService.spec.caBundle` so kube-apiserver verifies the proxy normally
-- no skip-verify needed. On subsequent renders the chart `lookup`s the existing
Secret and re-emits it verbatim, so the cert persists across `helm upgrade`.

This mode is the default and is appropriate for environments where running
cert-manager or wiring up an external Secret is overkill.

