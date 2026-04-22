#!/usr/bin/env bash

set -euo pipefail

: "${CTX:?CTX is required}"

FLUX_SERVICES_WAIT_TIMEOUT="${FLUX_SERVICES_WAIT_TIMEOUT:-600s}"

flux_ready_count=0
echo "waiting for Flux-managed resources in ${CTX}"

for kind in \
	helmreleases.helm.toolkit.fluxcd.io \
	kustomizations.kustomize.toolkit.fluxcd.io
do
	if [[ "${kind}" == "kustomizations.kustomize.toolkit.fluxcd.io" ]]; then
		# Skip suspended Kustomizations — waiting on them hangs forever.
		resources="$(kubectl --context "${CTX}" get "${kind}" --all-namespaces \
			-o custom-columns=NAMESPACE:.metadata.namespace,NAME:.metadata.name,SUSPEND:.spec.suspend \
			--no-headers 2>/dev/null \
			| awk '$3 != "true" {print $1 " " $2}')"
	else
		resources="$(kubectl --context "${CTX}" get "${kind}" --all-namespaces \
			-o jsonpath='{range .items[*]}{.metadata.namespace}{" "}{.metadata.name}{"\n"}{end}' \
			2>/dev/null)"
	fi

	[[ -z "${resources}" ]] && continue

	resource_count="$(printf '%s\n' "${resources}" | sed '/^$/d' | wc -l | tr -d ' ')"
	flux_ready_count="$((flux_ready_count + resource_count))"

	while IFS=' ' read -r namespace name; do
		[[ -n "${namespace}" ]] || continue
		kubectl --context "${CTX}" -n "${namespace}" \
			wait "${kind}/${name}" \
			--for=condition=Ready \
			--timeout="${FLUX_SERVICES_WAIT_TIMEOUT}"
	done <<<"${resources}"
done

if [[ "${flux_ready_count}" -le 0 ]]; then
	echo "ERROR: no Flux-managed resources found in ${CTX}" >&2
	exit 1
fi

echo "Flux-managed resources ready: ${flux_ready_count}"
