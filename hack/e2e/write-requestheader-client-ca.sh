#!/usr/bin/env bash

set -euo pipefail

: "${CTX:?CTX is required}"
: "${PROXY_NAMESPACE:?PROXY_NAMESPACE is required}"
: "${SECRET_NAME:?SECRET_NAME is required}"

tmpdir="$(mktemp -d "${TMPDIR:-/tmp}/audit-pass-through-requestheader-ca.XXXXXX")"
cleanup() {
  rm -rf "${tmpdir}"
}
trap cleanup EXIT

kubectl --context "${CTX}" -n kube-system get configmap extension-apiserver-authentication \
  -o jsonpath='{.data.requestheader-client-ca-file}' > "${tmpdir}/ca.crt"

kubectl --context "${CTX}" -n "${PROXY_NAMESPACE}" create secret generic "${SECRET_NAME}" \
  --from-file=ca.crt="${tmpdir}/ca.crt" \
  --dry-run=client -o yaml | kubectl --context "${CTX}" apply -f -
