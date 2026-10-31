#!/usr/bin/env bash
set -euo pipefail

# Move every reference to the current release from the chart's version to a
# new one: the chart version, appVersion and Artifact Hub image, the webhook
# image tag in both values files, the version-pinned commands and links in the
# READMEs, and CI's link-check exclusion for the not-yet-tagged blob. Historical
# mentions, such as CHANGELOG entries, are left alone, and any remaining mention
# of the old version is listed for review. All edits are staged in a scratch
# copy and written back only if every expected reference was found. The files
# must match the git index and HEAD, so git is the backup: if writing back
# fails, they are restored from the index without touching it.

usage() {
  echo "usage: $0 <new-version>" >&2
}

if [[ $# -ne 1 ]]; then
  usage
  exit 2
fi

new=$1
semver='^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$'
if [[ ! "$new" =~ $semver ]]; then
  echo "the new version must be stable core SemVer (MAJOR.MINOR.PATCH): $new" >&2
  exit 2
fi

root="$(git rev-parse --show-toplevel)"
cd "$root"
chart=charts/external-dns-porkbun-webhook
current="$(awk '$1 == "version:" { print $2; exit }' "$chart/Chart.yaml")"
if [[ ! "$current" =~ $semver ]]; then
  echo "could not read the current chart version from $chart/Chart.yaml" >&2
  exit 1
fi
# Compare in a plain assignment: inside a condition a failing sort would be
# ignored.
newest="$(printf '%s\n%s\n' "$current" "$new" | sort -V | tail -n1)"
if [[ "$new" == "$current" || "$newest" != "$new" ]]; then
  echo "the new version $new must be greater than the current $current" >&2
  exit 2
fi

files=(
  "$chart/Chart.yaml"
  "$chart/values.yaml"
  "$chart/README.md"
  docs/external-dns-values.yaml
  README.md
  .github/workflows/ci.yaml
)

for file in "${files[@]}"; do
  if [[ ! -f "$file" || -L "$file" ]]; then
    echo "$file must be a regular file" >&2
    exit 1
  fi
done
# Check the index and the working tree separately: a staged edit that the
# working tree has since reverted would otherwise pass.
if ! git diff --quiet -- "${files[@]}" || ! git diff --cached --quiet -- "${files[@]}"; then
  echo "commit or discard the changes to these files first, so git can restore them:" >&2
  git diff --name-only HEAD -- "${files[@]}" >&2
  git diff --name-only -- "${files[@]}" >&2
  exit 1
fi

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
for file in "${files[@]}"; do
  mkdir -p "$work/$(dirname "$file")"
  cp "$file" "$work/$file"
done

# replace FILE PREFIX OLD NEW SUFFIX [EXACT_COUNT]: replace PREFIX OLD SUFFIX
# with PREFIX NEW SUFFIX, all matched literally. OLD must not be followed by
# another digit, so 0.7.1 never matches inside 0.7.10. Fails unless the text
# occurs at least once, or exactly EXACT_COUNT times when given.
replace() {
  local file=$1 prefix=$2 old=$3 replacement=$4 suffix=$5 exact=${6:-} count
  count="$(PREFIX="$prefix" OLD="$old" NEW="$replacement" SUFFIX="$suffix" perl -e '
    my $path = $ARGV[0];
    open my $in, "<", $path or die "$path: $!\n";
    my $text = do { local $/; <$in> };
    close $in;
    my $count = $text =~ s/\Q$ENV{PREFIX}\E\Q$ENV{OLD}\E(?![0-9])\Q$ENV{SUFFIX}\E/$ENV{PREFIX}$ENV{NEW}$ENV{SUFFIX}/g;
    open my $out, ">", $path or die "$path: $!\n";
    print {$out} $text;
    close $out or die "$path: $!\n";
    print $count || 0;
  ' "$work/$file")"
  if [[ -n "$exact" && "$count" != "$exact" ]] || (( count == 0 )); then
    echo "$file: expected ${exact:-at least one} occurrence(s) of '${prefix}${old}${suffix}', found $count" >&2
    exit 1
  fi
  printf '%s: %s x %s\n' "$file" "$count" "${prefix//$'\n'/}${replacement}${suffix//$'\n'/}"
}

old_re="${current//./\\.}"
new_re="${new//./\\.}"

replace "$chart/Chart.yaml" $'\nversion: ' "$current" "$new" $'\n' 1
replace "$chart/Chart.yaml" $'\nappVersion: "' "$current" "$new" $'"\n' 1
replace "$chart/Chart.yaml" 'image: ghcr.io/mattgmoser/external-dns-porkbun-webhook:' "$current" "$new" $'\n' 1
replace "$chart/values.yaml" 'tag: "' "$current" "$new" '"' 1
replace docs/external-dns-values.yaml 'tag: "' "$current" "$new" '"' 1
replace "$chart/README.md" '--version ' "$current" "$new" ''
replace "$chart/README.md" 'install `' "$current" "$new" '`'
replace "$chart/README.md" '/external-dns-porkbun-webhook/blob/v' "$current" "$new" '/'
replace README.md '--version ' "$current" "$new" ''
replace README.md 'install `' "$current" "$new" '`'
replace README.md 'report chart `' "$current" "$new" '`'
replace .github/workflows/ci.yaml '/external-dns-porkbun-webhook/blob/v' "$old_re" "$new_re" '/' 1

# The same checks the release workflow applies to a tag, run on the staged copy.
# The webhook tag is read from its own image block, because both values files
# also pin the ExternalDNS controller image.
webhook_image_tag() {
  awk '
    /repository:[[:space:]]*ghcr\.io\/mattgmoser\/external-dns-porkbun-webhook[[:space:]]*$/ { found = 1; next }
    found && $1 == "tag:" { gsub(/"/, "", $2); print $2; exit }
  ' "$1"
}
staged_version="$(awk '$1 == "version:" { print $2; exit }' "$work/$chart/Chart.yaml")"
staged_app_version="$(awk '$1 == "appVersion:" { gsub(/"/, "", $2); print $2; exit }' "$work/$chart/Chart.yaml")"
staged_chart_tag="$(webhook_image_tag "$work/$chart/values.yaml")"
staged_example_tag="$(webhook_image_tag "$work/docs/external-dns-values.yaml")"
for staged in "$staged_version" "$staged_app_version" "$staged_chart_tag" "$staged_example_tag"; do
  if [[ "$staged" != "$new" ]]; then
    echo "a staged release reference reads '$staged', not $new" >&2
    exit 1
  fi
done

# If any write fails, put every file back as the index has it, which the
# precondition above made identical to HEAD.
restore() {
  if git checkout -- "${files[@]}"; then
    echo "Writing back failed; restored the files from git." >&2
  else
    echo "Writing back failed, and so did restoring from git. Run: git checkout -- ${files[*]}" >&2
  fi
}
trap 'restore; exit 1' ERR
for file in "${files[@]}"; do
  cp "$work/$file" "$file"
done
trap - ERR
echo "Moved the release references from $current to $new."

# Anything still naming the old version is either history or a reference this
# script does not know about yet; a person decides which.
# grep exits 1 when nothing is left and 2 when it could not search.
leftover_status=0
leftovers="$(grep -rnIE "(^|[^0-9.])${old_re}([^0-9]|$)" \
  --exclude=CHANGELOG.md --exclude=go.mod --exclude=go.sum --exclude-dir=.git \
  README.md CONTRIBUTING.md SECURITY.md docs charts .github)" || leftover_status=$?
if (( leftover_status > 1 )); then
  echo
  echo "Could not search for remaining mentions of $current; check them by hand." >&2
elif [[ -n "$leftovers" ]]; then
  echo
  echo "Review the remaining mentions of $current:"
  printf '%s\n' "$leftovers"
fi

if ! grep -Eq "^## ${new_re}([[:space:]]|$)" CHANGELOG.md; then
  echo
  echo "Add a '## $new' section to CHANGELOG.md; the release workflow rejects the tag without one."
fi
echo
echo "Rewrite artifacthub.io/changes and set artifacthub.io/containsSecurityUpdates in $chart/Chart.yaml for $new; they still describe $current."
