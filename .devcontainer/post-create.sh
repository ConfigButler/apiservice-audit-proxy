#!/usr/bin/env bash

set -euo pipefail

log() {
  echo "[post-create] $*"
}

fail() {
  echo "[post-create] ERROR: $*" >&2
  exit 1
}

workspace_dir="${1:-${containerWorkspaceFolder:-${WORKSPACE_FOLDER:-$(pwd)}}}"
log "Using workspace directory: ${workspace_dir}"

git_name="$(git config --get user.name || true)"
git_email="$(git config --get user.email || true)"

if [ -z "${git_name}" ] && [ -n "${GIT_USER_NAME:-}" ]; then
  git_name="${GIT_USER_NAME}"
fi

if [ -z "${git_email}" ] && [ -n "${GIT_USER_EMAIL:-}" ]; then
  git_email="${GIT_USER_EMAIL}"
fi

if [ -z "${git_name}" ] || [ -z "${git_email}" ]; then
  fail "Missing Git identity. Set user.name and user.email in Git, or provide GIT_USER_NAME and GIT_USER_EMAIL."
fi

if ! git config --global --get user.name >/dev/null 2>&1; then
  git config --global user.name "${git_name}"
fi

if ! git config --global --get user.email >/dev/null 2>&1; then
  git config --global user.email "${git_email}"
fi

log "Refreshing Git SSH signing configuration"
bash "${workspace_dir}/.devcontainer/ensure-home-permissions.sh" "${workspace_dir}"
bash "${workspace_dir}/.devcontainer/sync-signing-key.sh"

