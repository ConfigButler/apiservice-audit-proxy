#!/usr/bin/env bash

set -euo pipefail

CLUSTER_NAME="${CLUSTER_NAME:-audit-pass-through-e2e}"
K3D="${K3D:-k3d}"
KUBECTL="${KUBECTL:-kubectl}"
K3S_IMAGE="${K3S_IMAGE:-rancher/k3s:v1.35.2-k3s1}"
K3D_HTTP_PORT="${K3D_HTTP_PORT:-8081}"
K3D_HTTPS_PORT="${K3D_HTTPS_PORT:-8443}"
DISABLE_K3S_TRAEFIK="${DISABLE_K3S_TRAEFIK:-true}"
DISABLE_K3S_SERVICELB="${DISABLE_K3S_SERVICELB:-true}"

cluster_context_name() {
  printf 'k3d-%s' "${CLUSTER_NAME}"
}

cluster_exists() {
  "${K3D}" cluster list -o json 2>/dev/null | grep -q "\"name\":\"${CLUSTER_NAME}\""
}

merge_kubeconfig() {
  "${K3D}" kubeconfig merge "${CLUSTER_NAME}" \
    --kubeconfig-switch-context \
    --kubeconfig-merge-default >/dev/null
}

rewrite_kubeconfig_for_devcontainer() {
  local cluster_entry server host port

  cluster_entry="$("${KUBECTL}" config view --minify -o jsonpath='{.clusters[0].name}')"
  server="$("${KUBECTL}" config view --minify -o jsonpath='{.clusters[0].cluster.server}')"

  if [[ "${server}" =~ ^https://([^:/]+):([0-9]+)$ ]]; then
    host="${BASH_REMATCH[1]}"
    port="${BASH_REMATCH[2]}"
  else
    return 0
  fi

  if getent hosts host.docker.internal >/dev/null 2>&1; then
    if [[ "${host}" == "0.0.0.0" || "${host}" == "127.0.0.1" || "${host}" == "localhost" ]]; then
      "${KUBECTL}" config set-cluster "${cluster_entry}" \
        --server="https://host.docker.internal:${port}" \
        --tls-server-name=localhost >/dev/null
    fi
  fi
}

create_cluster() {
  local k3s_args=()

  if [[ "${DISABLE_K3S_TRAEFIK}" == "true" ]]; then
    k3s_args+=(--k3s-arg "--disable=traefik@server:0")
  fi
  if [[ "${DISABLE_K3S_SERVICELB}" == "true" ]]; then
    k3s_args+=(--k3s-arg "--disable=servicelb@server:0")
  fi

  "${K3D}" cluster create "${CLUSTER_NAME}" \
    --image "${K3S_IMAGE}" \
    --servers 1 \
    --agents 1 \
    --api-port 6550 \
    --port "${K3D_HTTP_PORT}:80@loadbalancer" \
    --port "${K3D_HTTPS_PORT}:443@loadbalancer" \
    --kubeconfig-update-default \
    --kubeconfig-switch-context \
    "${k3s_args[@]}"
}

mkdir -p "${HOME}/.kube"

if cluster_exists; then
  echo "reusing k3d cluster ${CLUSTER_NAME}"
else
  echo "creating k3d cluster ${CLUSTER_NAME}"
  create_cluster
fi

merge_kubeconfig
"${KUBECTL}" config use-context "$(cluster_context_name)" >/dev/null
rewrite_kubeconfig_for_devcontainer
"${KUBECTL}" --context "$(cluster_context_name)" wait --for=condition=Ready node --all --timeout=180s
