# Helm Values Guide

This chart is intentionally explicit. `apiservice-audit-proxy` sits on a
security-sensitive path, so the chart does not try to guess certificate,
identity, or audit webhook settings.

Use this page with the full defaults in
[`values.yaml`](../charts/apiservice-audit-proxy/values.yaml).

## Main Decisions

| Decision | Values | What to choose |
|---|---|---|
| Proxy serving certificate | `certificates.mode` | Use `cert-manager` when the cluster can issue serving certs, `existing-secret` when another platform owns the cert, and `dev-self-signed` only for local throwaway demos. |
| APIService registration | `apiService.*` | Enable it when the chart should register this proxy as the aggregated API backend. Leave it disabled if another controller owns the `APIService`. |
| Backend server trust | `backend.caSecretName`, `backend.serverName`, `backend.insecureSkipVerify` | Prefer CA validation. Use `insecureSkipVerify=true` only for local/dev paths or while bootstrapping a demo. |
| Backend identity | `backend.identity.mode` | Use `requestheader` when the real backend trusts front-proxy `X-Remote-*` identity. Use `impersonation` when the backend can authorize Kubernetes impersonation and you do not want to provision a backend client private key. |
| Inbound requestheader trust | `requestHeader.*` | Set `clientCASecretName` in production. Set `allowedNames` whenever possible; it is required for `impersonation` mode. |
| Audit webhook | `webhook.*` | Point at a Secret containing the webhook kubeconfig for your receiver. `webhookTester.enabled` is for demos. |

## Backend Identity Modes

### `requestheader`

`requestheader` is the default and preserves the historical behavior:

```text
kube-apiserver -> proxy:  X-Remote-* headers
proxy -> backend:         X-Remote-* headers
```

Use it when the real aggregated backend is designed to run behind a trusted
front proxy and accepts delegated requestheader identity. This is the natural
mode for many `k8s.io/apiserver`-style backends.

If the backend requires client certificate authentication on the backend hop,
set `backend.clientCertSecretName`. That Secret must contain the proxy's client
certificate and key. Avoid copying broad platform private keys into this chart;
prefer a dedicated client certificate scoped to this proxy when the backend can
trust one.

Minimal shape:

```yaml
backend:
  url: https://real-backend.example-system.svc:443
  insecureSkipVerify: false
  caSecretName: backend-serving-ca
  serverName: real-backend.example-system.svc
  clientCertSecretName: proxy-backend-client-cert
  identity:
    mode: requestheader
```

Inbound requestheader trust needs no configuration: the proxy reads the
front-proxy CA, the accepted client names, and the identity header names from
the cluster's `kube-system/extension-apiserver-authentication` ConfigMap. The
chart always renders a kube-system RoleBinding to the built-in
`extension-apiserver-authentication-reader` Role so the proxy ServiceAccount can
read it. Installing the chart therefore needs permission to create a
RoleBinding in `kube-system`.

### `impersonation`

`impersonation` mode verifies the inbound requestheader identity, then calls the
backend as the proxy ServiceAccount with Kubernetes impersonation headers:

```text
kube-apiserver -> proxy:  X-Remote-* headers
proxy -> backend:         Authorization: Bearer <proxy ServiceAccount token>
                          Impersonate-User / Group / Uid / Extra
```

Use it when the backend supports normal Kubernetes bearer-token auth and
impersonation authorization. This is useful on platforms such as CozyStack,
where taking ownership of the frontend proxy private key is undesirable. The
proxy does not need `backend.clientCertSecretName` in this mode; in fact the
chart rejects that combination today so the identity model stays clear.

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
certificates:
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
certificates:
  mode: existing-secret
  existingSecretName: apiservice-audit-proxy-serving-tls

apiService:
  caBundle: <base64-ca-bundle>
```

This is often the right production mode when certificate lifecycle is centralized
outside the chart.

### `dev-self-signed`

Use this only for local demos:

```yaml
certificates:
  mode: dev-self-signed
```

The chart creates a self-signed serving cert Secret and forces the rendered
`APIService` to skip TLS verification. That is convenient for local development
and inappropriate for production.

