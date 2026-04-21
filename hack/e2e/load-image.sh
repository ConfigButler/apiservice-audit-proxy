#!/usr/bin/env bash

set -euo pipefail

: "${CLUSTER_NAME:?CLUSTER_NAME is required}"
: "${IMAGE:?IMAGE is required}"
: "${STAMP_FILE:?STAMP_FILE is required}"

DOCKER="${DOCKER:-docker}"
K3D="${K3D:-k3d}"

if ! "${DOCKER}" image inspect "${IMAGE}" >/dev/null 2>&1; then
  echo "image not found locally: ${IMAGE}" >&2
  exit 1
fi

mkdir -p "$(dirname "${STAMP_FILE}")"

image_id="$("${DOCKER}" image inspect --format='{{.Id}}' "${IMAGE}")"
server_node="k3d-${CLUSTER_NAME}-server-0"

if [[ -f "${STAMP_FILE}" ]] && [[ "$(<"${STAMP_FILE}")" == "${image_id}" ]]; then
  if "${DOCKER}" exec "${server_node}" ctr -n k8s.io images ls -q 2>/dev/null | grep -Fq "${IMAGE}"; then
    echo "reusing loaded image ${IMAGE}"
    exit 0
  fi
fi

echo "loading ${IMAGE} into k3d cluster ${CLUSTER_NAME}"
"${K3D}" image import -c "${CLUSTER_NAME}" "${IMAGE}"
printf '%s' "${image_id}" > "${STAMP_FILE}"
