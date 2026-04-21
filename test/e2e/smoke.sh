#!/usr/bin/env bash

set -euo pipefail

: "${CTX:?CTX is required}"
: "${WEBHOOK_NAMESPACE:?WEBHOOK_NAMESPACE is required}"
: "${WEBHOOK_SERVICE_NAME:?WEBHOOK_SERVICE_NAME is required}"

tmpdir="$(mktemp -d "${TMPDIR:-/tmp}/audit-pass-through-smoke.XXXXXX")"
pf_pid=""
smoke_namespace="audit-pass-through-smoke"
flunder_name="smoke-$(date +%s)"
webhook_port="19444"

cleanup() {
  if [[ -n "${pf_pid}" ]]; then
    kill "${pf_pid}" >/dev/null 2>&1 || true
  fi
  rm -rf "${tmpdir}"
}
trap cleanup EXIT

kubectl --context "${CTX}" wait apiservice/v1alpha1.wardle.example.com \
  --for=jsonpath='{.status.conditions[?(@.type=="Available")].status}'=True \
  --timeout=240s

kubectl --context "${CTX}" api-resources --api-group=wardle.example.com | grep -q flunders

kubectl --context "${CTX}" create namespace "${smoke_namespace}" --dry-run=client -o yaml \
  | kubectl --context "${CTX}" apply -f -

cat > "${tmpdir}/flunder.yaml" <<EOF
apiVersion: wardle.example.com/v1alpha1
kind: Flunder
metadata:
  name: ${flunder_name}
  namespace: ${smoke_namespace}
spec:
  reference: smoke-reference
EOF

kubectl --context "${CTX}" apply -f "${tmpdir}/flunder.yaml"
kubectl --context "${CTX}" -n "${smoke_namespace}" get flunder "${flunder_name}" -o json | jq -e \
  '.apiVersion == "wardle.example.com/v1alpha1" and .spec.reference == "smoke-reference"' >/dev/null

kubectl --context "${CTX}" -n "${WEBHOOK_NAMESPACE}" port-forward "svc/${WEBHOOK_SERVICE_NAME}" "${webhook_port}:9444" \
  > "${tmpdir}/port-forward.log" 2>&1 &
pf_pid=$!

for _ in $(seq 1 60); do
  if curl -fsS "http://127.0.0.1:${webhook_port}/events" >/dev/null 2>&1; then
    break
  fi
  sleep 1
done

for _ in $(seq 1 90); do
  if curl -fsS "http://127.0.0.1:${webhook_port}/events" | jq -e --arg name "${flunder_name}" '
    [
      .items[].eventList.items[]
      | select(.objectRef.name == $name)
      | select(.requestObject != null)
      | select(.responseObject != null)
    ] | length > 0
  ' >/dev/null; then
    echo "smoke flow passed for ${flunder_name}"
    exit 0
  fi
  sleep 2
done

curl -fsS "http://127.0.0.1:${webhook_port}/events" | jq .
echo "timed out waiting for a recovered audit payload for ${flunder_name}" >&2
exit 1
