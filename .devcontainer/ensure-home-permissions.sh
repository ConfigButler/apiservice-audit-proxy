#!/usr/bin/env bash

set -euo pipefail

log() {
  echo "[ensure-home-permissions] $*"
}

home_dir="${HOME:-/home/vscode}"
workspace_dir="${1:-${containerWorkspaceFolder:-${WORKSPACE_FOLDER:-$(pwd)}}}"

log "Ensuring writable home-mounted directories for $(id -un)"

sudo mkdir -p \
  "${home_dir}/.cache/go-build" \
  "${home_dir}/.cache/goimports" \
  "${home_dir}/.cache/golangci-lint" \
  "${home_dir}/.codex" \
  "${home_dir}/.claude" \
  "${home_dir}/.config" \
  "${home_dir}/.ssh"

sudo chown -R vscode:vscode \
  "${home_dir}/.cache" \
  "${home_dir}/.codex" \
  "${home_dir}/.claude" \
  "${home_dir}/.config" \
  "${home_dir}/.ssh" \
  "${home_dir}" || true

if [ -d "${workspace_dir}" ]; then
  sudo chown -R vscode:vscode "${workspace_dir}" || true
fi

chmod 700 "${home_dir}/.codex" "${home_dir}/.claude" "${home_dir}/.ssh" || true

log "Home-mounted directory ownership refreshed"
