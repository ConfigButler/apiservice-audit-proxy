#!/usr/bin/env bash

set -euo pipefail

workspace_dir="${1:-${containerWorkspaceFolder:-${WORKSPACE_FOLDER:-$(pwd)}}}"

bash "${workspace_dir}/.devcontainer/ensure-home-permissions.sh" "${workspace_dir}"
bash "${workspace_dir}/.devcontainer/sync-signing-key.sh"

# Optional .env in the workspace root — see CONTRIBUTORS.md.
[[ -f "${workspace_dir}/.env" ]] || echo "hint: ${workspace_dir}/.env is absent — see CONTRIBUTORS.md if you want gh CLI access."
