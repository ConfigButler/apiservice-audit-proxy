# Mutual TLS Explained For This Repository

This document is meant as a practical guide to mutual TLS (mTLS) in the context
of `apiservice-audit-proxy`.

If you already have the intuition that "TLS lets the client trust the server"
and "mTLS lets both sides prove who they are", you are off to a very good
start. The missing piece is understanding what is actually being proven, how
that proof works, and where this repository uses each trust mechanism.

For the broader project shape, see [ARCHITECTURE.md](./ARCHITECTURE.md).

## Short Version

Normal TLS gives you:

- encryption
- integrity
- server identity, if the client validates the server certificate correctly

Mutual TLS adds:

- client identity at the transport layer, because the server also asks for and
  validates a client certificate

That means mTLS is not "stronger encryption". The encryption is already there
with normal TLS. What mTLS adds is symmetric identity proof: both sides
cryptographically prove they possess a private key tied to a certificate that a
trusted CA issued.

## The Building Blocks

Before the handshake makes sense, it helps to name the moving parts.

### Private key

A private key is the secret. Whoever possesses it can prove ownership of the
certificate bound to it.

If the private key leaks, the identity leaks with it.

### Certificate

A certificate is a signed statement that binds a public key to an identity.

That identity depends on context:

- for a server certificate, it is usually a DNS name such as
  `api-backend.wardle.svc.cluster.local`
- for a client certificate, it may be a subject name, a URI, or simply "this
  key was issued by a CA I trust for client auth"

### Certificate Authority (CA)

A CA is an issuer that signs certificates. Trusting a CA means:

"I am willing to trust certificates signed by this issuer for the purposes I
configured."

That is why CA scope matters so much. If you trust a CA too broadly, you may
accidentally trust more peers than intended.

### Trust store / CA bundle

A trust store is the set of CA certificates a client or server uses when
deciding what to trust.

Examples in Kubernetes:

- a kubeconfig can contain CA data for a server it talks to
- an `APIService` can contain a `caBundle` so kube-apiserver trusts the
  aggregated server's serving certificate
- a component can load a `--client-ca-file` so it knows which client
  certificates to trust

### SAN

For server certificates, the Subject Alternative Name (SAN) extension is where
DNS names live. Modern TLS hostname validation checks SANs, not the old Common
Name field by itself.

If a client connects to `api-backend.wardle.svc.cluster.local`, the server
certificate must be valid for that name.

### Key usages and extended key usages

Certificates can be constrained for intended purposes:

- `server auth`
- `client auth`
- signing other certificates, for a CA

That is one reason a server certificate and a client certificate are not always
interchangeable.

## How TLS Works

At a high level, a normal TLS connection does this:

1. The client opens a connection to the server.
2. The server sends its certificate.
3. The client checks:
   - is the certificate signed by a CA I trust?
   - is it valid for the hostname I intended to reach?
   - is it currently valid and usable for server auth?
4. The handshake establishes shared session keys.
5. The client talks to the server over an encrypted, integrity-protected
   channel.

If those checks pass, the client has good reason to believe:

- it is talking to the intended server
- nobody on the network can casually read or tamper with the traffic

That is already a big security win.

## How mTLS Extends TLS

Mutual TLS adds one extra step:

1. The server asks the client for a certificate.
2. The client sends its certificate.
3. The server checks:
   - is the client certificate signed by a CA I trust?
   - is it valid for client auth?
   - does the client prove possession of the matching private key?
4. Only then does the server treat the peer as an authenticated client.

So the asymmetry disappears:

- the client authenticates the server
- the server authenticates the client

This is why people often summarize mTLS as "both sides authenticate each
other", but the important detail is *how*: both sides validate certificate
chains and proof of private-key possession against their configured trust roots.

## Why This Is Safe

mTLS improves safety because it combines three things:

- confidentiality: traffic is encrypted
- integrity: tampering is detectable
- identity: only peers with trusted certificates and matching private keys are
  accepted

That makes several attacks much harder:

- passive sniffing
- active tampering
- connecting to an impostor server
- unauthorized clients calling a server that requires client certs

But the safety depends on correct configuration. mTLS is not magic. It is only
as strong as:

- protection of private keys
- narrow trust in the right CA bundles
- correct hostname validation for servers
- sensible certificate rotation and expiry handling
- avoiding `insecureSkipVerify` outside dev scenarios

## What mTLS Does Not Automatically Solve

mTLS is powerful, but it does not solve every trust problem by itself.

It does not automatically tell your application:

- which RBAC role a client should have
- whether a forwarded identity header is trustworthy
- whether the authenticated client is allowed to perform a specific action

It answers a narrower question very well:

"Did this peer prove possession of a private key for a certificate issued by a
CA I trust for this purpose?"

Authorization is still a separate step.

## The Three TLS Trust Edges In This Repository

This repository has three different trust relationships. Keeping them separate
in your head helps a lot.

## 1. kube-apiserver -> apiservice-audit-proxy

This is the aggregated API hop.

The proxy serves HTTPS. kube-apiserver acts as the client for the
`APIService`-registered backend and needs to trust the proxy's serving
certificate.

In this repo, that trust is configured through the `APIService` resource:

- `spec.caBundle` means "kube-apiserver should trust this CA for the proxy's
  serving certificate"
- `insecureSkipTLSVerify: true` means "skip server verification", which is only
  acceptable for development and testing

You can see that wiring in:

- [charts/apiservice-audit-proxy/templates/apiservice.yaml](../charts/apiservice-audit-proxy/templates/apiservice.yaml)
- [charts/apiservice-audit-proxy/templates/deployment.yaml](../charts/apiservice-audit-proxy/templates/deployment.yaml)

Important nuance:

This hop is not automatically mTLS just because certificates are involved.
Server TLS is always in play here. Client-certificate verification is optional
and separate.

## 2. apiservice-audit-proxy -> aggregated backend

This is the proxy's outbound connection to the real aggregated API server.

There are two independent questions here:

### Does the proxy trust the backend server?

That is standard server-side TLS validation:

- `--backend-ca-file` tells the proxy which CA to trust for the backend's
  serving certificate
- `--backend-server-name` lets the proxy validate the expected server name
- `--backend-insecure-skip-verify` disables proper server verification and is a
  dev-only escape hatch

### Does the backend trust the proxy as a client?

That is where mTLS enters:

- `--backend-client-cert-file`
- `--backend-client-key-file`

When those are configured, the proxy presents a client certificate to the
backend. If the backend is configured with a client CA, it can verify that the
caller is a trusted client and not just any pod that can reach the service.

This is the cleanest "true mTLS" example in the repo.

You can see it in the e2e setup:

- the backend trusts a client CA:
  [test-apiserver-deployment.yaml](../charts/apiservice-audit-proxy/templates/test-apiserver-deployment.yaml)
- cert-manager issues a client certificate for the proxy with `client auth`
  usage:
  [test-apiserver-client-certs.yaml](../charts/apiservice-audit-proxy/templates/test-apiserver-client-certs.yaml)
- the proxy mounts and uses that certificate:
  [test/e2e/values/proxy-cert-manager.yaml](../test/e2e/values/proxy-cert-manager.yaml)

There is also a stricter backend-serving-cert path in the e2e setup where the
backend gets its own serving certificate and the proxy validates it via an
explicit CA instead of skipping verification:

- [test-apiserver-serving-certs.yaml](../charts/apiservice-audit-proxy/templates/test-apiserver-serving-certs.yaml)
- [proxy-cert-manager-backend-ca.yaml](../test/e2e/values/proxy-cert-manager-backend-ca.yaml)

## 3. apiservice-audit-proxy -> audit webhook

The proxy sends synthetic audit events to a webhook using a kubeconfig-style
client configuration.

That kubeconfig can carry:

- server trust material, so the proxy validates the webhook server certificate
- client credentials, including a client certificate and key if the webhook
  expects mTLS

The key point is that this trust relationship is independent from the backend
connection. A component can use:

- plain HTTP to a webhook
- one-way TLS to a webhook
- or mTLS to a webhook

without changing the backend hop.

The relevant code is:

- [pkg/webhook/client.go](../pkg/webhook/client.go)

## The Requestheader / Front-Proxy Nuance

This is the Kubernetes-specific piece that is easy to confuse with mTLS.

When kube-apiserver proxies an aggregated API request, it can forward delegated
identity headers such as:

- `X-Remote-User`
- `X-Remote-Uid`
- `X-Remote-Group`
- `X-Remote-Extra-*`

Those headers are powerful. If a backend trusted them from any random caller,
an attacker could forge user identity just by sending fake headers.

So the real question is not "are headers present?" It is:

"Do I trust the peer that sent these headers?"

That is why Kubernetes commonly pairs delegated requestheader auth with a
front-proxy client certificate. The backend verifies that the immediate caller
is a trusted front proxy before it trusts the delegated headers.

In this repository, the equivalent control is `--client-ca-file`:

- if it is not set, the proxy will read delegated headers without extra client
  certificate verification
- if it is set, the proxy only trusts delegated headers when the inbound client
  certificate validates against that CA

The implementation is here:

- [pkg/identity/requestheader.go](../pkg/identity/requestheader.go)
- [cmd/server/main.go](../cmd/server/main.go)

Important nuance:

This is adjacent to mTLS, but not exactly the same as a strict
"require-and-verify client cert for every connection" posture.

The current server TLS config uses `VerifyClientCertIfGiven`, which means:

- the HTTPS server can verify a client cert if one is presented
- delegated headers are only trusted when the presented cert chains to the
  configured requestheader CA

That is still a valuable protection because it stops the proxy from trusting
identity headers from an unverified peer. It is just worth distinguishing from
the stricter policy of rejecting every connection that lacks a valid client
certificate at the transport layer.

## Where kubectl Fits

`kubectl` is related, but usually on a different hop.

When you run `kubectl`, at minimum it typically relies on normal TLS to verify
the kube-apiserver's serving certificate. That protects you from talking to an
impostor API server.

Authentication of the user behind `kubectl` can happen in different ways:

- client certificate auth
- bearer token
- OIDC
- exec-based plugins

So `kubectl` does not always use mTLS for user auth. Sometimes it does, if the
kubeconfig contains a client cert and key. Sometimes it uses one-way TLS plus a
different auth method.

That is a useful mental model:

- TLS and mTLS are transport trust mechanisms
- Kubernetes authentication and authorization sit on top of them

## Pros Of mTLS

- Strong machine-to-machine identity without relying only on network location.
- Good fit for service-to-service traffic inside a cluster.
- Removes the need to send bearer secrets on every request when certificate
  identity is sufficient.
- Limits who can connect successfully when the server requires trusted client
  certs.
- Works well with short-lived certificates and automated rotation.

## Cons And Tradeoffs

- More operational complexity than one-way TLS.
- Certificate issuance, rotation, and revocation need real discipline.
- CA scoping mistakes can create overly broad trust.
- Debugging handshake failures can be confusing.
- Some systems validate only the CA and key possession, but not a narrower set
  of allowed subject names, which can be too permissive if the CA is shared.
- If people fall back to `insecureSkipVerify` or oversized trust bundles, much
  of the security value is lost.

## Common Failure Modes

- Trusting the wrong CA bundle.
- Forgetting server name validation.
- Reusing one CA for too many unrelated purposes.
- Treating "has a cert from this CA" as sufficient authorization.
- Trusting `X-Remote-*` headers without verifying the front proxy.
- Assuming "certificates are present" means "mTLS is active everywhere".

## A Good Mental Model For This Project

If you want a simple way to remember the design, think of it like this:

- `APIService caBundle` answers:
  "Should kube-apiserver trust the proxy's serving certificate?"
- `--backend-ca-file` answers:
  "Should the proxy trust the backend's serving certificate?"
- `--backend-client-cert-file` and `--backend-client-key-file` answer:
  "Should the backend trust the proxy as a client?"
- `--client-ca-file` answers:
  "Should the proxy trust delegated identity headers from the caller?"
- the webhook kubeconfig answers:
  "How should the proxy trust and authenticate to the audit webhook?"

Those are related, but they solve different trust questions.

## Bottom Line

TLS makes the channel private and lets the client verify the server.

mTLS extends that by letting the server verify the client too.

In Kubernetes, that transport-layer trust often sits next to a second idea:
delegated identity, where one trusted component forwards user identity headers
to another. That is why this repository cares both about mTLS-style certificate
checks and about requestheader trust.

If you keep the trust questions separate, the whole picture gets much easier:

- who am I connecting to?
- who is allowed to connect to me?
- which CA do I trust for that decision?
- am I trusting a network peer, a forwarded user identity, or both?

That is the core of how certificates, CAs, TLS, mTLS, `APIService`, and audit
webhook delivery fit together here.
