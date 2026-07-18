#!/usr/bin/env bash
set -euo pipefail

chart_dir=${1:?chart directory is required}
upstream_chart=${2:?upstream chart URL is required}

rendered_wrapper=$(mktemp)
rendered_upstream=$(mktemp)
normalized_wrapper=$(mktemp)
normalized_upstream=$(mktemp)
package_dir=$(mktemp -d)

helm template external-dns "$chart_dir" --namespace external-dns > "$rendered_wrapper"
helm template external-dns "$upstream_chart" \
  --namespace external-dns \
  --values docs/external-dns-values.yaml > "$rendered_upstream"

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
# official chart. Helm source comments differ because one chart is a dependency.
grep -v '^# Source:' "$rendered_wrapper" > "$normalized_wrapper"
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
helm template external-dns "$package" --namespace external-dns >/dev/null

# Stored values from the deprecated standalone chart identify a potentially
# dangerous in-place migration. The new chart must stop instead of silently
# starting a second ExternalDNS controller with example ownership settings.
if helm template external-dns "$chart_dir" \
  --namespace external-dns \
  --is-upgrade \
  --set legacyStandalone.acceptRisk=true >/dev/null 2>&1; then
  echo 'legacy standalone upgrades must be rejected' >&2
  exit 1
fi
