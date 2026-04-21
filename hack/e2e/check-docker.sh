#!/usr/bin/env bash

set -euo pipefail

if docker info >/dev/null 2>&1; then
  exit 0
fi

cat >&2 <<'EOF'
Docker is required for the standalone k3d flow, but `docker info` failed.
Start the Docker daemon on your machine and then rerun the e2e task.
EOF
exit 1
