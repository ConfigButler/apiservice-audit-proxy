#!/usr/bin/env bash

set -euo pipefail

CLUSTER_NAME="${CLUSTER_NAME:-audit-pass-through-e2e}"
K3D="${K3D:-k3d}"
KUBECTL="${KUBECTL:-kubectl}"
K3S_IMAGE="${K3S_IMAGE:-rancher/k3s:v1.35.2-k3s1}"
DISABLE_K3S_TRAEFIK="${DISABLE_K3S_TRAEFIK:-true}"
DISABLE_K3S_SERVICELB="${DISABLE_K3S_SERVICELB:-true}"
K3D_API_PORT="${K3D_API_PORT:-}"
# Relative path from the repo root to the directory containing policy.yaml
# and webhook-config.yaml. When the files exist there the kube-apiserver is
# configured with the audit webhook automatically; leave unset to skip.
AUDIT_DIR_REL="${AUDIT_DIR_REL:-test/e2e/cluster/audit}"

AUDIT_POLICY_FILE="policy.yaml"
AUDIT_WEBHOOK_CONFIG_FILE="webhook-config.yaml"

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

# Verify that the Docker daemon can bind-mount the given host path and that
# the audit files are visible inside it. Required for Docker-outside-of-Docker
# devcontainer setups where the daemon's filesystem root differs from pwd.
docker_can_mount_repo() {
  local candidate="$1"
  docker run --rm \
    -v "${candidate}:/hostproj:ro" \
    busybox:1.36.1 sh -c \
    "test -f /hostproj/${AUDIT_DIR_REL}/${AUDIT_POLICY_FILE} && \
     test -f /hostproj/${AUDIT_DIR_REL}/${AUDIT_WEBHOOK_CONFIG_FILE}" \
    >/dev/null 2>&1
}

# Return the repo path that the Docker daemon can actually mount. Tries the
# current working directory first, then HOST_PROJECT_PATH if set.
resolve_host_project_path() {
  local repo_pwd candidates=()
  repo_pwd="$(pwd -P)"
  candidates+=("${repo_pwd}")
  if [[ -n "${HOST_PROJECT_PATH:-}" ]]; then
    candidates+=("${HOST_PROJECT_PATH}")
  fi

  local candidate
  for candidate in "${candidates[@]}"; do
    if docker_can_mount_repo "${candidate}"; then
      printf '%s' "${candidate}"
      return 0
    fi
  done

  echo "ERROR: cannot determine a mountable path for audit files." >&2
  echo "Tried:" >&2
  for candidate in "${candidates[@]}"; do
    echo "  ${candidate}" >&2
  done
  echo "Fix: set HOST_PROJECT_PATH to the repo path visible to the Docker daemon." >&2
  return 1
}

# k3d stat()-checks the volume source path locally before creating containers.
# In Docker-outside-of-Docker setups HOST_PROJECT_PATH may only exist on the
# daemon host. Creating a local symlink silences the k3d preflight warning.
ensure_k3d_stat_compat_path() {
  local host_project_path="$1"
  local repo_pwd
  repo_pwd="$(pwd -P)"

  if [[ "${host_project_path}" == "${repo_pwd}" ]]; then
    return 0
  fi

  local parent_dir
  parent_dir="$(dirname "${host_project_path}")"

  mkdir -p "${parent_dir}" 2>/dev/null || sudo -n mkdir -p "${parent_dir}" 2>/dev/null || {
    echo "warning: could not create '${parent_dir}' for HOST_PROJECT_PATH compat symlink" >&2
    return 0
  }

  if [[ -L "${host_project_path}" ]]; then
    ln -sfn "${repo_pwd}" "${host_project_path}" 2>/dev/null \
      || sudo -n ln -sfn "${repo_pwd}" "${host_project_path}" 2>/dev/null \
      || echo "warning: could not refresh HOST_PROJECT_PATH compat symlink" >&2
  elif [[ ! -e "${host_project_path}" ]]; then
    ln -s "${repo_pwd}" "${host_project_path}" 2>/dev/null \
      || sudo -n ln -s "${repo_pwd}" "${host_project_path}" 2>/dev/null \
      || echo "warning: could not create HOST_PROJECT_PATH compat symlink" >&2
  fi
}

audit_files_present() {
  [[ -f "${AUDIT_DIR_REL}/${AUDIT_POLICY_FILE}" && \
     -f "${AUDIT_DIR_REL}/${AUDIT_WEBHOOK_CONFIG_FILE}" ]]
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

  local volume_args=()
  if audit_files_present; then
    local host_project_path
    host_project_path="$(resolve_host_project_path)"
    ensure_k3d_stat_compat_path "${host_project_path}"

    local audit_host_dir="${host_project_path}/${AUDIT_DIR_REL}"
    volume_args+=(-v "${audit_host_dir}:/etc/kubernetes/audit@server:0")
    k3s_args+=(
      --k3s-arg "--kube-apiserver-arg=audit-policy-file=/etc/kubernetes/audit/${AUDIT_POLICY_FILE}@server:0"
      --k3s-arg "--kube-apiserver-arg=audit-webhook-config-file=/etc/kubernetes/audit/${AUDIT_WEBHOOK_CONFIG_FILE}@server:0"
      --k3s-arg "--kube-apiserver-arg=audit-webhook-batch-max-wait=1s@server:0"
      --k3s-arg "--kube-apiserver-arg=audit-webhook-batch-max-size=10@server:0"
    )
    echo "audit webhook enabled — mounting ${audit_host_dir}"
  else
    echo "audit files not found at ${AUDIT_DIR_REL}; starting cluster without audit webhook"
  fi

  echo "creating k3d cluster ${CLUSTER_NAME} (api-port ${api_port})"
  "${K3D}" cluster create "${CLUSTER_NAME}" \
    --image "${K3S_IMAGE}" \
    --servers 1 \
    --agents 3 \
    --api-port "${api_port}" \
    --kubeconfig-update-default \
    --kubeconfig-switch-context \
    "${volume_args[@]}" \
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
  echo "Delete it and retry: ${K3D} cluster delete ${CLUSTER_NAME}" >&2
  exit 1
fi

echo "cluster $(cluster_context_name) is ready"
