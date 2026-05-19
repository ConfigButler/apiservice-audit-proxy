# Connecting to GitOps Reverser

This note explains how `apiservice-audit-proxy` should connect to
GitOps Reverser as its audit webhook receiver, and why this is a different
problem from requestheader trust.

The short version:

- The requestheader CA can come from Kubernetes'
  `kube-system/extension-apiserver-authentication` ConfigMap.
- The proxy-to-GitOps-Reverser audit webhook trust cannot come from that
  ConfigMap. It is ordinary webhook client TLS, configured through the proxy's
  webhook kubeconfig Secret.
- The native kube-apiserver-to-GitOps-Reverser audit webhook still requires a
  node-local kube-apiserver audit webhook kubeconfig. Kubernetes has no
  `APIService`-style or admission-webhook-style CA injection object for audit
  webhooks.

## The Three Trust Problems

There are three similar-looking CA questions, but they live in different
Kubernetes mechanisms.

| Path | Purpose | Dynamic cluster object? | Recommended source of trust |
|---|---|---|---|
| kube-apiserver/front-proxy -> apiservice-audit-proxy | Verify delegated `X-Remote-*` identity | Yes: `extension-apiserver-authentication` | Cluster requestheader ConfigMap |
| apiservice-audit-proxy -> GitOps Reverser | Send synthetic audit `EventList` payloads | No standard object | Proxy webhook kubeconfig Secret |
| kube-apiserver -> GitOps Reverser | Native Kubernetes audit webhook | No | Node-local `--audit-webhook-config-file` kubeconfig |

Do not reuse `extension-apiserver-authentication` for GitOps Reverser. That
ConfigMap is specifically the requestheader/front-proxy trust contract for
aggregated API servers. It says who may assert Kubernetes user identity through
headers. It does not say which audit webhook receiver a client should trust.

## Proxy to GitOps Reverser

The proxy sends synthetic audit events through `pkg/webhook`, which builds an
HTTP client from a kubeconfig-style file. The chart mounts that file from:

```yaml
webhook:
  kubeconfigSecretName: audit-pass-through-webhook-kubeconfig
  kubeconfigKey: kubeconfig
  timeout: 5s
```

For GitOps Reverser, that Secret should point at the GitOps Reverser audit
Service. In the common "additional body source" mode, use
`/audit-webhook-additional` so GitOps Reverser can join the proxy's
body-rich events with kube-apiserver's canonical audit stream.

Example shape:

```yaml
apiVersion: v1
kind: Config
clusters:
- name: gitops-reverser-audit
  cluster:
    server: https://gitops-reverser-audit.gitops-reverser.svc:9444/audit-webhook-additional
    certificate-authority-data: <base64 of GitOps Reverser audit root CA>
contexts:
- name: gitops-reverser-audit
  context:
    cluster: gitops-reverser-audit
    user: audit-proxy
current-context: gitops-reverser-audit
users:
- name: audit-proxy
  user:
    client-certificate-data: <optional proxy client cert>
    client-key-data: <optional proxy client key>
```

If GitOps Reverser does not require client certificates for this proxy path,
the `users` entry can use a bearer token or be empty, depending on the receiver
configuration. The server trust still belongs in the kubeconfig via
`certificate-authority-data`, `certificate-authority`, or, for development
only, `insecure-skip-tls-verify`.

This is the best place for a "ConfigMap-like" operational approach:

- GitOps Reverser owns a stable audit root CA Secret.
- cert-manager issues short-lived GitOps Reverser audit serving certificates
  from that root CA.
- A Helm template, Flux/Kustomize step, trust-manager Bundle, reflector rule,
  or small controller renders the proxy's webhook kubeconfig Secret from that
  root CA and the chosen receiver URL.
- The proxy pod restarts or reloads when the kubeconfig Secret changes.

That can be made mostly automatic inside the cluster, but it is not a
Kubernetes built-in standard like `extension-apiserver-authentication`.

## Native kube-apiserver Audit Webhook

GitOps Reverser can also receive the canonical Kubernetes audit stream directly
from kube-apiserver. That path is configured by kube-apiserver flags:

```text
--audit-policy-file=...
--audit-webhook-config-file=...
```

The webhook config file is a kubeconfig on the control-plane node filesystem.
It is not a Kubernetes API object, so cert-manager cannot inject CA data into
it. There is no audit-webhook equivalent of:

- `APIService.spec.caBundle`
- `cert-manager.io/inject-ca-from`
- admission webhook `clientConfig.caBundle`
- `extension-apiserver-authentication`

That means one control-plane bootstrap remains unavoidable:

1. Install GitOps Reverser and its certificate resources.
2. Wait for the audit root CA and any kube-apiserver audit client certificate
   to exist.
3. Generate the kube-apiserver audit webhook kubeconfig from those artifacts.
4. Copy that kubeconfig to each control-plane node.
5. Ensure kube-apiserver uses it through `--audit-webhook-config-file`.

The important lifecycle choice is to make kube-apiserver trust a stable root CA,
not the rotating serving certificate leaf:

```yaml
clusters:
- name: audit-webhook
  cluster:
    server: https://127.0.0.1:30444/audit-webhook
    certificate-authority-data: <base64 of GitOps Reverser audit root CA>
    tls-server-name: gitops-reverser-audit.gitops-reverser.svc
users:
- name: apiserver
  user:
    client-certificate-data: <base64 of long-lived audit client cert>
    client-key-data: <base64 of long-lived audit client key>
```

With that shape, normal GitOps Reverser serving certificate rotation does not
require kube-apiserver changes. Only root CA rotation or kube-apiserver audit
client credential rotation requires regenerating and redistributing the
node-local kubeconfig.

| Event | kube-apiserver file update needed? |
|---|---|
| Initial audit webhook enablement | Yes |
| First TLS bootstrap after GitOps Reverser install | Yes |
| Normal GitOps Reverser serving cert rotation | No |
| GitOps Reverser audit root CA rotation | Yes |
| kube-apiserver audit client cert rotation | Yes |

## APIService TLS Is Separate

The proxy's own `APIService` registration is different again. For
kube-apiserver to trust the proxy serving certificate, Kubernetes already has a
native place to put that trust:

```yaml
apiVersion: apiregistration.k8s.io/v1
kind: APIService
spec:
  caBundle: ...
```

When `certificates.mode=cert-manager`, this chart annotates the `APIService` so
cert-manager can inject the CA bundle. That solves kube-apiserver-to-proxy
serving TLS. It does not solve kube-apiserver-to-GitOps-Reverser audit webhook
TLS, because audit webhooks are not configured by an in-cluster API object.

## Recommendation

Use two different operating models:

1. **Proxy webhook delivery to GitOps Reverser**

   Manage the proxy's `webhook.kubeconfigSecretName` as an in-cluster Secret
   generated from GitOps Reverser's stable audit root CA. This can be automated
   by Helm, Flux, Kustomize, trust-manager, reflector, or a small controller.

2. **Native kube-apiserver audit delivery to GitOps Reverser**

   Accept the one-time control-plane kubeconfig step. Minimize future work by
   using a stable audit root CA and a deliberately long-lived kube-apiserver
   audit client credential, so normal serving cert rotation is invisible to the
   kube-apiserver.

This keeps the requestheader work clean: it removes the proxy's inbound
front-proxy CA configuration without pretending that audit-webhook kubeconfig
trust has the same Kubernetes-native distribution mechanism.
