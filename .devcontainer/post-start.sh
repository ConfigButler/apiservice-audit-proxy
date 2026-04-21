#!/usr/bin/env bash

set -euo pipefail

workspace_dir="${1:-${containerWorkspaceFolder:-${WORKSPACE_FOLDER:-$(pwd)}}}"

bash "${workspace_dir}/.devcontainer/ensure-home-permissions.sh" "${workspace_dir}"
bash "${workspace_dir}/.devcontainer/sync-signing-key.sh"
