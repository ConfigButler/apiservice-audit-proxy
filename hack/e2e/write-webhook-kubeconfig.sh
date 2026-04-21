#!/usr/bin/env bash

set -euo pipefail

: "${CTX:?CTX is required}"
: "${PROXY_NAMESPACE:?PROXY_NAMESPACE is required}"
: "${SECRET_NAME:?SECRET_NAME is required}"
: "${WEBHOOK_NAMESPACE:?WEBHOOK_NAMESPACE is required}"
: "${WEBHOOK_SERVICE_NAME:?WEBHOOK_SERVICE_NAME is required}"

WEBHOOK_PATH="${WEBHOOK_PATH:-/audit-webhook/e2e}"
TMP_KUBECONFIG="$(mktemp "${TMPDIR:-/tmp}/audit-pass-through-webhook.XXXXXX")"

cleanup() {
  rm -f "${TMP_KUBECONFIG}"
}
trap cleanup EXIT

cat > "${TMP_KUBECONFIG}" <<EOF
apiVersion: v1
kind: Config
preferences: {}
clusters:
- name: mock-audit-webhook
  cluster:
    server: http://${WEBHOOK_SERVICE_NAME}.${WEBHOOK_NAMESPACE}.svc.cluster.local:9444${WEBHOOK_PATH}
contexts:
- name: webhook
  context:
    cluster: mock-audit-webhook
    user: default
current-context: webhook
users:
- name: default
  user: {}
EOF

kubectl --context "${CTX}" -n "${PROXY_NAMESPACE}" \
  create secret generic "${SECRET_NAME}" \
  --from-file=kubeconfig="${TMP_KUBECONFIG}" \
  --dry-run=client -o yaml \
  | kubectl --context "${CTX}" apply -f -
