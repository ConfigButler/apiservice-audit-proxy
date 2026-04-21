#!/usr/bin/env bash

set -euo pipefail

: "${CTX:?CTX is required}"

FLUX_SERVICES_WAIT_TIMEOUT="${FLUX_SERVICES_WAIT_TIMEOUT:-600s}"

echo "waiting for Flux-managed resources in ${CTX}"

for kind in helmreleases.helm.toolkit.fluxcd.io kustomizations.kustomize.toolkit.fluxcd.io; do
  resources="$(kubectl --context "${CTX}" get "${kind}" --all-namespaces \
    -o jsonpath='{range .items[*]}{.metadata.namespace}{" "}{.metadata.name}{"\n"}{end}' 2>/dev/null)"
  [[ -n "${resources}" ]] || continue

  while IFS=' ' read -r namespace name; do
    [[ -n "${namespace}" ]] || continue
    kubectl --context "${CTX}" -n "${namespace}" wait "${kind}/${name}" \
      --for=condition=Ready --timeout="${FLUX_SERVICES_WAIT_TIMEOUT}"
  done <<< "${resources}"
done
