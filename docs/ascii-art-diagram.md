┌─────────────────────────────────────────────────────────────────────────┐
│  Kubernetes API Server (kube-apiserver)                                 │
│                                                                         │
│  APIService: v1alpha1.wardle.example.com                                │
│  → routes to apiservice-audit-proxy:443 (namespace: wardle)            │
└────────────────────────────┬────────────────────────────────────────────┘
                             │ 1. client request
                             │    (kubectl get flunder …)
                             ▼
┌────────────────────────────────────────────────────────────────────────┐
│  apiservice-audit-proxy  (namespace: wardle)                           │
│                                                                        │
│  • receives front-proxy request from kube-apiserver                    │
│  • verifies X-Remote-User/X-Remote-Group headers via requestheader CA │
│  • spools request + response bodies for audit                          │
│  • forwards request upstream ──────────────────────────────────────┐  │
│  • on response: fires audit event to webhook (best-effort)  ───┐   │  │
└────────────────────────────────────────────────────────────────┼───┼──┘
                                                                 │   │ 2. proxied request
                                                                 │   ▼
                                                    ┌────────────────────────────┐
                                                    │  wardle-server             │
                                                    │  (sample aggregated API)   │
                                                    │  namespace: wardle         │
                                                    │                            │
                                                    │  • stores Flunder objects  │
                                                    │  • backed by etcd sidecar  │
                                                    └────────────────────────────┘
                                                                 │ 3. audit webhook POST
                                                                 ▼
                                          ┌──────────────────────────────────────────┐
                                          │  mock-audit-webhook                      │
                                          │  (namespace: audit-pass-through-e2e)     │
                                          │                                          │
                                          │  • receives audit events over HTTP       │
                                          │  • stores them in memory                 │
                                          │  • exposes GET /events for assertions    │
                                          └──────────────────────────────────────────┘
                                                                 ▲
                                          ┌──────────────────────────────────────────┐
                                          │  e2e smoke test (Go)                     │
                                          │                                          │
                                          │  1. waits for APIService to be Available │
                                          │  2. creates a Flunder via kube-apiserver │
                                          │  3. port-forwards to mock-audit-webhook  │
                                          │  4. polls GET /events until it sees the  │
                                          │     Flunder's audit event with           │
                                          │     requestObject + responseObject set   │
                                          └──────────────────────────────────────────┘
