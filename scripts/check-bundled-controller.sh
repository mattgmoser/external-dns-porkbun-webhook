#!/usr/bin/env bash
set -Eeuo pipefail

# Scan the ExternalDNS controller image a chart release pins, and compare it
# with the newest upstream release. Artifact Hub grades this package on both
# runtime images, so the controller's findings reach users too. Nothing else
# watches the image: Dependabot's Helm job tracks only the chart dependency,
# because its registry.k8s.io lookups fail.
#
# Exit status: 0 when there is nothing to adopt; 1 when a newer upstream
# release has a better scan and the pin should move; 2 when the check itself
# failed, including any unexpected command failure (the ERR trap below).
# Residual findings with no better upstream release are reported as warnings
# only: until upstream ships a fix there is nothing to adopt.

trap 'exit 2' ERR

usage() {
  echo "usage: $0 <values.yaml> <report-directory>" >&2
}

if [[ $# -ne 2 ]]; then
  usage
  exit 2
fi

values=$1
report_dir=$2

for tool in trivy jq gh docker; do
  if ! command -v "$tool" >/dev/null 2>&1; then
    echo "$tool is required" >&2
    exit 2
  fi
done

repository=registry.k8s.io/external-dns/external-dns
platforms=(linux/amd64 linux/arm64 linux/arm/v7)
mkdir -p "$report_dir"
summary="$report_dir/summary.md"
: > "$summary"

fail() {
  echo "$1" >&2
  exit 2
}

# version_gt A B: A is a strictly newer version than B.
version_gt() {
  [[ "$1" != "$2" && "$(printf '%s\n%s\n' "$1" "$2" | sort -V | tail -n1)" == "$1" ]]
}

pinned_tag="$(awk '
  /repository:[[:space:]]*registry\.k8s\.io\/external-dns\/external-dns[[:space:]]*$/ { found = 1; next }
  found && $1 == "tag:" { gsub(/"/, "", $2); print $2; exit }
' "$values")"
if [[ ! "$pinned_tag" =~ ^(v[0-9]+\.[0-9]+\.[0-9]+)@sha256:[0-9a-f]{64}$ ]]; then
  fail "$values must pin the ExternalDNS controller image to an exact digest, found: ${pinned_tag:-<none>}"
fi
pinned_version="${BASH_REMATCH[1]}"

# Stable controller releases only. The repository also publishes chart
# releases (external-dns-helm-chart-X.Y.Z), and a prerelease's image is not
# necessarily promoted to registry.k8s.io.
if ! releases="$(gh api --paginate 'repos/kubernetes-sigs/external-dns/releases?per_page=100' \
  --jq '.[] | select(.draft == false and .prerelease == false) | .tag_name')"; then
  fail 'could not list the ExternalDNS releases'
fi
latest_version="$(grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' <<< "$releases" | sort -V | tail -n1 || true)"
if [[ -z "$latest_version" ]]; then
  fail 'the ExternalDNS release list has no stable controller release'
fi

# scan LABEL IMAGE_REF: scan every published platform and write
# LABEL-summary.json with the distinct findings, how many have a fix, and the
# most severe one (-1 when there are none).
scan() {
  local label=$1 image_ref=$2 platform report
  local reports=()
  for platform in "${platforms[@]}"; do
    report="$report_dir/$label-${platform//\//-}.json"
    if ! scripts/scan-image.sh "$image_ref" "$platform" "$report"; then
      fail "could not scan $image_ref for $platform"
    fi
    # An empty but valid report must not read as a clean image.
    if ! jq -e '
      .SchemaVersion == 2
      and ((.Results | type) == "array" and (.Results | length) > 0)
      and any(.Results[]; .Class == "lang-pkgs" and .Type == "gobinary")
    ' "$report" >/dev/null; then
      fail "the Trivy report for $image_ref ($platform) is missing the controller binary"
    fi
    trivy --config /dev/null convert --ignorefile /dev/null --scanners vuln --format table "$report"
    reports+=("$report")
  done
  jq -s '
    def rank:
      {"UNKNOWN": 0, "LOW": 1, "MEDIUM": 2, "HIGH": 3, "CRITICAL": 4}[.]
      // error("unexpected Trivy severity: \(.)");
    [.[] | .Results[]?.Vulnerabilities[]?
      | {id: .VulnerabilityID, package: .PkgName, installed: .InstalledVersion,
         fixed: (.FixedVersion // ""), severity: .Severity, rank: (.Severity | rank)}]
    | unique_by([.id, .package, .installed])
    | {findings: ., fixable: (map(select(.fixed != "")) | length), worst: (map(.rank) | max // -1)}
  ' "${reports[@]}" > "$report_dir/$label-summary.json"
}

# Artifact Hub's grade comes from the most severe finding across the images a
# chart lists. With only unrated findings it shows no grade.
grade() {
  case "$1" in
    -1) echo 'A' ;;
    0) echo 'unrated' ;;
    1) echo 'B' ;;
    2) echo 'C' ;;
    3) echo 'D' ;;
    4) echo 'F' ;;
    *) fail "unexpected severity rank: $1" ;;
  esac
}

describe() {
  local label=$1 title=$2 file="$report_dir/$1-summary.json"
  {
    printf '### %s\n\n' "$title"
    jq -r '"\(.findings | length) finding(s), \(.fixable) with a fix."' "$file"
    printf 'Grade from this image alone: %s.\n\n' "$(grade "$(jq '.worst' "$file")")"
    jq -r '
      if (.findings | length) > 0 then
        "| Finding | Package | Installed | Fixed in | Severity |",
        "|---|---|---|---|---|",
        (.findings | sort_by(-.rank, .package, .id)[]
          | "| \(.id) | \(.package) | \(.installed) | \(if .fixed == "" then "-" else .fixed end) | \(.severity) |"),
        ""
      else empty end
    ' "$file"
  } >> "$summary"
}

pinned_ref="$repository:$pinned_tag"
scan pinned "$pinned_ref"
describe pinned "Pinned controller $pinned_version"
pinned_worst="$(jq '.worst' "$report_dir/pinned-summary.json")"
pinned_fixable="$(jq '.fixable' "$report_dir/pinned-summary.json")"

# action: the one change worth failing the run for. notes: everything else
# worth knowing, reported as warnings.
action=
notes=()

if version_gt "$latest_version" "$pinned_version"; then
  latest_tag_ref="$repository:$latest_version"
  inspect_errors="$report_dir/inspect-latest.err"
  if ! manifest="$(docker buildx imagetools inspect "$latest_tag_ref" --format '{{json .Manifest}}' 2> "$inspect_errors")"; then
    # Only a missing tag counts as "not published"; any other lookup error,
    # such as a credential helper that is "not found", fails the check.
    if grep -Fq "$latest_tag_ref: not found" "$inspect_errors" \
      || grep -Fqi 'manifest unknown' "$inspect_errors"; then
      notes+=("ExternalDNS $latest_version is released, but $latest_tag_ref is not published yet.")
    else
      cat "$inspect_errors" >&2
      fail "could not resolve $latest_tag_ref"
    fi
  else
    latest_digest="$(jq -r '.digest' <<< "$manifest")"
    if [[ ! "$latest_digest" =~ ^sha256:[0-9a-f]{64}$ ]]; then
      fail "$latest_tag_ref resolved to an invalid digest: $latest_digest"
    fi
    missing="$(jq -r '
      [.manifests[]?.platform | select(. != null)
        | "\(.os)/\(.architecture)\(if (.variant // "") == "" or (.architecture == "arm64" and .variant == "v8") then "" else "/\(.variant)" end)"]
      as $published
      | $ARGS.positional - $published | join(", ")
    ' --args "${platforms[@]}" <<< "$manifest")"
    if [[ -n "$missing" ]]; then
      notes+=("ExternalDNS $latest_version does not publish $missing, which this chart supports; it cannot be adopted as is.")
    else
      latest_ref="$repository:$latest_version@$latest_digest"
      scan latest "$latest_ref"
      describe latest "Latest upstream controller $latest_version"
      latest_worst="$(jq '.worst' "$report_dir/latest-summary.json")"
      latest_fixable="$(jq '.fixable' "$report_dir/latest-summary.json")"
      if (( latest_worst < pinned_worst || (latest_worst == pinned_worst && latest_fixable < pinned_fixable) )); then
        action="ExternalDNS $latest_version scans better than the pinned $pinned_version (grade $(grade "$latest_worst") vs $(grade "$pinned_worst"), $latest_fixable vs $pinned_fixable finding(s) with a fix). Pin \`$latest_version@$latest_digest\` once its rendered arguments and breaking changes are checked."
      else
        notes+=("ExternalDNS $latest_version is available, but its scan is no better than the pinned $pinned_version.")
      fi
    fi
  fi
elif (( pinned_fixable > 0 )); then
  notes+=("The pinned controller has $pinned_fixable finding(s) with a fix, and $pinned_version is the newest ExternalDNS release. Nothing to adopt until upstream ships one.")
fi

{
  printf '## Bundled ExternalDNS controller\n\n'
  printf -- '- Pinned: %s\n' "\`$pinned_ref\`"
  printf -- '- Newest upstream release: %s\n\n' "$latest_version"
  for note in ${action:+"$action"} ${notes[@]+"${notes[@]}"}; do
    printf -- '- %s\n' "$note"
  done
  printf '\n'
} > "$report_dir/header.md"
cat "$report_dir/header.md" "$summary" > "$report_dir/report.md"
cat "$report_dir/report.md"
if [[ -n "${GITHUB_STEP_SUMMARY:-}" ]]; then
  cat "$report_dir/report.md" >> "$GITHUB_STEP_SUMMARY"
fi

for note in ${notes[@]+"${notes[@]}"}; do
  echo "::warning title=Bundled controller::$note"
done
if [[ -n "$action" ]]; then
  echo "::error title=Bundled controller::$action"
  exit 1
fi
