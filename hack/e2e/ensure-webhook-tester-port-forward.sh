#!/usr/bin/env bash
set -euo pipefail

KUBECTL="${KUBECTL:-kubectl}"
KUBE_CONTEXT="${CTX:-${KUBECONTEXT:-}}"
NAMESPACE="${WEBHOOK_TESTER_NAMESPACE:-wardle}"
RELEASE_NAME="${E2E_RELEASE_NAME:-apiservice-audit-proxy}"
SERVICE="${WEBHOOK_TESTER_SERVICE:-${RELEASE_NAME}-webhook-tester}"
DEPLOYMENT="${WEBHOOK_TESTER_DEPLOYMENT:-${SERVICE}}"
LOCAL_PORT="${WEBHOOK_TESTER_LOCAL_PORT:-18090}"
SERVICE_PORT="${WEBHOOK_TESTER_SERVICE_PORT:-8080}"
BASE_URL="http://127.0.0.1:${LOCAL_PORT}"
STAMPS_DIR="${STAMPS_DIR:-.stamps/e2e/port-forwards}"
PID_FILE="${WEBHOOK_TESTER_PORT_FORWARD_PID_FILE:-${STAMPS_DIR}/webhook-tester-${LOCAL_PORT}.pid}"
LOG_FILE="${WEBHOOK_TESTER_PORT_FORWARD_LOG_FILE:-${STAMPS_DIR}/webhook-tester-${LOCAL_PORT}.log}"

if [[ -z "${KUBE_CONTEXT}" ]]; then
  KUBE_CONTEXT="$("${KUBECTL}" config current-context 2>/dev/null || true)"
fi

if [[ -z "${KUBE_CONTEXT}" ]]; then
  echo "Kubernetes context is required (set CTX or KUBECONTEXT)" >&2
  exit 1
fi

health_ready() {
  curl -fsS "${BASE_URL}/healthz" >/dev/null 2>&1
}

pid_matches_forward() {
  local pid="$1"
  local args

  args="$(ps -p "${pid}" -o args= 2>/dev/null || true)"
  [[ "${args}" == *"port-forward"* && "${args}" == *"svc/${SERVICE}"* && "${args}" == *"${LOCAL_PORT}:${SERVICE_PORT}"* ]]
}

if health_ready; then
  echo "webhook-tester is already reachable at ${BASE_URL}"
  exit 0
fi

if [[ -f "${PID_FILE}" ]]; then
  existing_pid="$(tr -d '\n\r' < "${PID_FILE}" || true)"
  if [[ -n "${existing_pid}" ]] && kill -0 "${existing_pid}" 2>/dev/null; then
    if pid_matches_forward "${existing_pid}"; then
      kill "${existing_pid}" 2>/dev/null || true
    fi
  fi
  rm -f "${PID_FILE}"
fi

mkdir -p "${STAMPS_DIR}"

"${KUBECTL}" --context "${KUBE_CONTEXT}" -n "${NAMESPACE}" \
  rollout status "deploy/${DEPLOYMENT}" --timeout=180s

echo "Starting webhook-tester port-forward: ${NAMESPACE}/${SERVICE} ${LOCAL_PORT}:${SERVICE_PORT}"
setsid "${KUBECTL}" --context "${KUBE_CONTEXT}" -n "${NAMESPACE}" \
  port-forward --address 127.0.0.1 "svc/${SERVICE}" "${LOCAL_PORT}:${SERVICE_PORT}" \
  >"${LOG_FILE}" 2>&1 < /dev/null &
echo $! > "${PID_FILE}"

for _ in {1..30}; do
  if health_ready; then
    echo "webhook-tester: ${BASE_URL}"
    echo "proxy session: ${BASE_URL}/aabbccdd-0000-4000-0000-000000000002"
    echo "kube-apiserver session: ${BASE_URL}/aabbccdd-0000-4000-0000-000000000001"
    exit 0
  fi
  sleep 1
done

echo "webhook-tester did not become reachable at ${BASE_URL}" >&2
echo "Port-forward log: ${LOG_FILE}" >&2
exit 1
