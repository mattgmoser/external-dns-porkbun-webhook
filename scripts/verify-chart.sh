#!/usr/bin/env bash
set -euo pipefail

chart_dir=${1:?chart directory is required}
upstream_chart=${2:?upstream chart URL is required}

rendered_wrapper=$(mktemp)
rendered_upstream=$(mktemp)
normalized_wrapper=$(mktemp)
normalized_upstream=$(mktemp)
wrapper_values=$(mktemp)
upstream_values=$(mktemp)
package_dir=$(mktemp -d)

cleanup() {
  rm -f \
    "$rendered_wrapper" \
    "$rendered_upstream" \
    "$normalized_wrapper" \
    "$normalized_upstream" \
    "$wrapper_values" \
    "$upstream_values"
  rm -rf "$package_dir"
}
trap cleanup EXIT

bash scripts/chart-test-values.sh "$chart_dir/values.yaml" > "$wrapper_values"
bash scripts/chart-test-values.sh docs/external-dns-values.yaml > "$upstream_values"

helm lint --strict "$chart_dir" --values "$wrapper_values"
helm template external-dns "$chart_dir" \
  --namespace external-dns \
  --values "$wrapper_values" > "$rendered_wrapper"
helm template external-dns "$upstream_chart" \
  --namespace external-dns \
  --values "$upstream_values" > "$rendered_upstream"

if command -v kubeconform >/dev/null 2>&1; then
  kubeconform_cmd=(kubeconform)
else
  kubeconform_cmd=(go run github.com/yannh/kubeconform/cmd/kubeconform@v0.8.0)
fi
"${kubeconform_cmd[@]}" \
  -strict \
  -summary \
  -kubernetes-version 1.24.0 < "$rendered_wrapper"

# The wrapper must stay a transparent, version-pinned rendering of the
# official chart apart from its release-owned migration marker. Helm source
# comments also differ because one chart is a dependency.
sed '/^# Source: external-dns-porkbun-webhook\/templates\/migration-state.yaml$/,/^---$/d' \
  "$rendered_wrapper" | grep -v '^# Source:' > "$normalized_wrapper"
grep -v '^# Source:' "$rendered_upstream" > "$normalized_upstream"
diff -u "$normalized_upstream" "$normalized_wrapper"

require_literal() {
  if ! grep -Fq -- "$1" "$rendered_wrapper"; then
    echo "missing chart invariant: $1" >&2
    exit 1
  fi
}

require_literal '        - name: external-dns'
require_literal '        - name: webhook'
require_literal '              value: 127.0.0.1:8888'
require_literal '      targetPort: http-webhook'
require_literal '  topology: official-external-dns-same-pod-sidecar'

if grep -Eq '^[[:space:]]+(port|containerPort):[[:space:]]+8888$' "$rendered_wrapper"; then
  echo 'provider port 8888 must not be exposed by a Service or declared as a public container port' >&2
  exit 1
fi

if [ "$(grep -Fc 'mountPath: /var/run/secrets/kubernetes.io/serviceaccount' "$rendered_wrapper")" -ne 1 ]; then
  echo 'the projected Kubernetes API token must be mounted into exactly one container' >&2
  exit 1
fi

if [ "$(grep -Fc 'automountServiceAccountToken: false' "$rendered_wrapper")" -lt 2 ]; then
  echo 'ServiceAccount and Pod token automounting must both be disabled' >&2
  exit 1
fi

helm package "$chart_dir" --destination "$package_dir" >/dev/null
chart_version=$(awk '$1 == "version:" {print $2; exit}' "$chart_dir/Chart.yaml")
package="$package_dir/external-dns-porkbun-webhook-${chart_version}.tgz"
test -f "$package"
archive_listing=$(tar tzf "$package")
dependency_chart='external-dns-porkbun-webhook/charts/external-dns/Chart.yaml'
grep -Fqx "$dependency_chart" <<< "$archive_listing"
dependency_version=$(tar xOf "$package" "$dependency_chart" | awk '$1 == "version:" {print $2; exit}')
dependency_app_version=$(tar xOf "$package" "$dependency_chart" | awk '$1 == "appVersion:" {print $2; exit}')
test "$dependency_version" = '1.21.1'
test "$dependency_app_version" = '0.21.0'
if grep -Fq 'artifacthub-repo.yml' <<< "$archive_listing"; then
  echo 'Artifact Hub repository metadata belongs beside index.yaml, not inside the chart' >&2
  exit 1
fi

# Rendering the packaged archive proves it is self-contained and does not need
# network access to resolve its pinned dependency at install time.
helm template external-dns "$package" \
  --namespace external-dns \
  --values "$wrapper_values" >/dev/null

# Supplying a fresh values file drops the old chart's keys. This exact upgrade
# shape must still stop instead of silently starting a second controller.
if helm template external-dns "$chart_dir" \
  --namespace external-dns \
  --is-upgrade \
  --values "$wrapper_values" >/dev/null 2>&1; then
  echo 'unacknowledged topology-changing upgrades must be rejected' >&2
  exit 1
fi

# A maintainer who has completed the documented controller handoff must have a
# deliberate escape hatch for an in-place migration.
helm template external-dns "$chart_dir" \
  --namespace external-dns \
  --is-upgrade \
  --values "$wrapper_values" \
  --set migration.acknowledgeControllerReplacement=true >/dev/null

# Helm's --set-string values are non-empty strings and therefore truthy in Go
# templates. Only the exact YAML boolean true may acknowledge this migration.
for string_acknowledgement in false true; do
  if helm template external-dns "$chart_dir" \
    --namespace external-dns \
    --is-upgrade \
    --values "$wrapper_values" \
    --set-string "migration.acknowledgeControllerReplacement=$string_acknowledgement" >/dev/null 2>&1; then
    echo "string migration acknowledgement must be rejected: $string_acknowledgement" >&2
    exit 1
  fi
done

# Placeholder values must fail before any workload reaches the cluster.
if helm template external-dns "$chart_dir" --namespace external-dns >/dev/null 2>&1; then
  echo 'default placeholder values must not be installable' >&2
  exit 1
fi

# Exercise every fail-closed placeholder independently so an earlier check
# cannot mask a regression in a later one.
placeholder_overrides=(
  'external-dns.provider.webhook.env[2].value=example.com'
  'external-dns.provider.webhook.env[2].value='
  'external-dns.provider.webhook.env[3].value=example.com'
  'external-dns.provider.webhook.env[3].value='
  'external-dns.domainFilters[0]=example.com'
  'external-dns.domainFilters[0]='
  'external-dns.txtOwnerId=change-me'
  'external-dns.txtOwnerId=CHANGE-ME'
  'external-dns.txtOwnerId='
)
for override in "${placeholder_overrides[@]}"; do
  if helm template external-dns "$chart_dir" \
    --namespace external-dns \
    --values "$wrapper_values" \
    --set-string "$override" >/dev/null 2>&1; then
    echo "placeholder must be rejected: $override" >&2
    exit 1
  fi
done

if helm template external-dns "$chart_dir" \
  --namespace external-dns \
  --values "$wrapper_values" \
  --set-string 'external-dns.provider.name=aws' >/dev/null 2>&1; then
  echo 'the wrapper must reject providers that remove the Porkbun webhook sidecar' >&2
  exit 1
fi

# The wrapper's security boundary depends on the mutating webhook remaining on
# loopback and the Service targeting only the separate ops listener. Reject
# overrides that would expose mutations or disconnect probes and metrics.
listener_overrides=(
  'external-dns.provider.webhook.env[4].value=:8080'
  'external-dns.provider.webhook.env[4].value=0.0.0.0:8888'
  'external-dns.provider.webhook.env[5].value=127.0.0.1:8080'
  'external-dns.extraArgs.webhook-provider-url=http://external-dns-webhook:8888'
  'external-dns.extraArgs.txt-wildcard-replacement='
  'external-dns.extraArgs.txt-wildcard-replacement=*'
  'external-dns.extraArgs.txt-wildcard-replacement=bad.label'
  'external-dns.extraArgs.txt-wildcard-replacement=-bad'
)
for override in "${listener_overrides[@]}"; do
  if helm template external-dns "$chart_dir" \
    --namespace external-dns \
    --values "$wrapper_values" \
    --set-string "$override" >/dev/null 2>&1; then
    echo "unsafe listener override must be rejected: $override" >&2
    exit 1
  fi
done

if helm template external-dns "$chart_dir" \
  --namespace external-dns \
  --values "$wrapper_values" \
  --set-string 'external-dns.provider.webhook.env[10].name=WEBHOOK_LISTEN' \
  --set-string 'external-dns.provider.webhook.env[10].value=127.0.0.1:8888' >/dev/null 2>&1; then
  echo 'duplicate WEBHOOK_LISTEN entries must be rejected' >&2
  exit 1
fi
