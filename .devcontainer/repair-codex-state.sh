#!/usr/bin/env bash

set -euo pipefail

log() {
  echo "[repair-codex-state] $*"
}

codex_dir="${HOME}/.codex"
backup_root="${HOME}/.codex-repair-backups"
timestamp="$(date -u +%Y%m%dT%H%M%SZ)"
backup_dir="${backup_root}/${timestamp}"

if [ ! -d "${codex_dir}" ]; then
  log "No ${codex_dir} directory found; nothing to repair"
  exit 0
fi

mkdir -p "${backup_dir}"

move_if_exists() {
  local path="$1"
  if [ -e "${path}" ]; then
    mv "${path}" "${backup_dir}/"
    log "Moved $(basename "${path}") to ${backup_dir}"
  fi
}

log "Backing up volatile Codex state from ${codex_dir}"

find "${codex_dir}" -maxdepth 1 -type f \
  \( -name 'state_*.sqlite' -o -name 'state_*.sqlite-*' -o -name 'logs_*.sqlite' -o -name 'logs_*.sqlite-*' \) \
  -print0 | while IFS= read -r -d '' file; do
    mv "${file}" "${backup_dir}/"
    log "Moved $(basename "${file}") to ${backup_dir}"
  done

move_if_exists "${codex_dir}/models_cache.json"
move_if_exists "${codex_dir}/.tmp/app-server-remote-plugin-sync-v1"

if [ -d "${codex_dir}/.tmp/plugins" ]; then
  mv "${codex_dir}/.tmp/plugins" "${backup_dir}/plugins"
  log "Moved plugin cache to ${backup_dir}/plugins"
fi

if [ -f "${codex_dir}/.tmp/plugins.sha" ]; then
  mv "${codex_dir}/.tmp/plugins.sha" "${backup_dir}/"
  log "Moved plugins.sha to ${backup_dir}"
fi

mkdir -p "${codex_dir}/.tmp" "${codex_dir}/tmp"

log "Codex state repair complete"
log "Preserved files include auth.json, config.toml, installation_id, sessions, rules, and skills"
