#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 1 ]]; then
  echo "usage: $0 <trivy-report.json>" >&2
  exit 2
fi

report=$1

if [[ ! -s "$report" ]]; then
  echo "Trivy report does not exist or is empty: $report" >&2
  exit 2
fi

if ! command -v trivy >/dev/null 2>&1; then
  echo "trivy is required" >&2
  exit 127
fi
if ! command -v jq >/dev/null 2>&1; then
  echo "jq is required" >&2
  exit 127
fi

# A syntactically valid but empty report must not turn a scanner failure into a
# clean result. Both the CI preflight and final image contain this Go binary.
if ! jq -e '
  .SchemaVersion == 2
  and ((.Results | type) == "array" and (.Results | length) > 0)
  and any(.Results[];
    .Class == "lang-pkgs"
    and .Type == "gobinary"
    and ((.Target | type) == "string" and (.Target | length) > 0))
' "$report" >/dev/null; then
  echo "Trivy report is missing the expected Go binary result: $report" >&2
  exit 2
fi

# Render every finding for the workflow log. Explicit empty configuration and
# ignore files prevent repository-level defaults from suppressing results.
trivy --config /dev/null convert \
  --ignorefile /dev/null \
  --scanners vuln \
  --format table \
  "$report"

fixable_count="$(
  jq '[.Results[]?.Vulnerabilities[]?
       | select(((.FixedVersion // "") | length) > 0)]
      | length' "$report"
)"

if (( fixable_count != 0 )); then
  echo "Release gate rejected $fixable_count vulnerability finding(s) with an available fix:" >&2
  jq -r '.Results[]?.Vulnerabilities[]?
         | select(((.FixedVersion // "") | length) > 0)
         | "- \(.VulnerabilityID) in \(.PkgName) \(.InstalledVersion) (fix: \(.FixedVersion), severity: \(.Severity))"' \
    "$report" >&2
  exit 1
fi

echo "No vulnerability findings with an available fix"
