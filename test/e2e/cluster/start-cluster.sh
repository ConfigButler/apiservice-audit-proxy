#!/usr/bin/env bash

set -euo pipefail

CLUSTER_NAME="${CLUSTER_NAME:-audit-pass-through-e2e}"
K3D="${K3D:-k3d}"
KUBECTL="${KUBECTL:-kubectl}"
K3S_IMAGE="${K3S_IMAGE:-rancher/k3s:v1.35.2-k3s1}"
DISABLE_K3S_TRAEFIK="${DISABLE_K3S_TRAEFIK:-true}"
DISABLE_K3S_SERVICELB="${DISABLE_K3S_SERVICELB:-true}"
K3D_API_PORT="${K3D_API_PORT:-}"

cluster_context_name() {
  printf 'k3d-%s' "${CLUSTER_NAME}"
}

cluster_exists() {
  "${K3D}" cluster list -o json 2>/dev/null | grep -q "\"name\":\"${CLUSTER_NAME}\""
}

cluster_is_healthy() {
  local context_name
  context_name="$(cluster_context_name)"

  if ! "${KUBECTL}" config get-contexts "${context_name}" >/dev/null 2>&1; then
    return 1
  fi

  "${KUBECTL}" --context "${context_name}" --request-timeout=10s get ns >/dev/null 2>&1
}

merge_kubeconfig() {
  if "${K3D}" kubeconfig merge "${CLUSTER_NAME}" \
    --kubeconfig-switch-context \
    --kubeconfig-merge-default >/dev/null; then
    return 0
  fi

  echo "falling back to writing kubeconfig directly for ${CLUSTER_NAME}" >&2
  "${K3D}" kubeconfig get "${CLUSTER_NAME}" > "${HOME}/.kube/config"
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
      echo "rewriting kubeconfig server to host.docker.internal:${port}"
      "${KUBECTL}" config set-cluster "${cluster_entry}" \
        --server="https://host.docker.internal:${port}" \
        --tls-server-name=localhost >/dev/null
    fi
  fi
}

port_in_use() {
  ss -Hltn "sport = :$1" 2>/dev/null | grep -q . && return 0
  docker ps --format '{{.Ports}}' | grep -Eq "(^|[ ,])0\\.0\\.0\\.0:$1->" && return 0
  return 1
}

pick_api_port() {
  local candidate

  if [[ -n "${K3D_API_PORT}" ]]; then
    printf '%s' "${K3D_API_PORT}"
    return 0
  fi

  for candidate in 6550 6551 6552 6553 6554; do
    if ! port_in_use "${candidate}"; then
      printf '%s' "${candidate}"
      return 0
    fi
  done

  echo "unable to find a free K3D API port in 6550-6554; set K3D_API_PORT explicitly" >&2
  return 1
}

create_cluster() {
  local api_port k3s_args=()

  if [[ "${DISABLE_K3S_TRAEFIK}" == "true" ]]; then
    k3s_args+=(--k3s-arg "--disable=traefik@server:0")
  fi
  if [[ "${DISABLE_K3S_SERVICELB}" == "true" ]]; then
    k3s_args+=(--k3s-arg "--disable=servicelb@server:0")
  fi

  api_port="$(pick_api_port)"

  echo "creating k3d cluster ${CLUSTER_NAME} (api-port ${api_port})"
  "${K3D}" cluster create "${CLUSTER_NAME}" \
    --image "${K3S_IMAGE}" \
    --servers 1 \
    --agents 3 \
    --api-port "${api_port}" \
    --kubeconfig-update-default \
    --kubeconfig-switch-context \
    "${k3s_args[@]}"
}

ensure_inotify_limits() {
  # k3s uses inotify watchers for containerd image import; the default
  # max_user_instances of 128 is exhausted when multiple k3d clusters run
  # concurrently. A privileged container can write host kernel parameters.
  local current
  current="$(cat /proc/sys/fs/inotify/max_user_instances 2>/dev/null || echo 0)"
  if [[ "${current}" -lt 512 ]]; then
    echo "bumping fs.inotify.max_user_instances from ${current} to 512"
    docker run --rm --privileged alpine \
      sysctl -w fs.inotify.max_user_instances=512 >/dev/null
  fi
}

mkdir -p "${HOME}/.kube"
ensure_inotify_limits

if cluster_exists; then
  echo "reusing k3d cluster ${CLUSTER_NAME}"
else
  create_cluster
fi

merge_kubeconfig
"${KUBECTL}" config use-context "$(cluster_context_name)" >/dev/null
rewrite_kubeconfig_for_devcontainer

if ! cluster_is_healthy; then
  echo "ERROR: cluster $(cluster_context_name) is not healthy after setup" >&2
  echo "Delete it and retry: k3d cluster delete ${CLUSTER_NAME}" >&2
  exit 1
fi

echo "cluster $(cluster_context_name) is ready"
