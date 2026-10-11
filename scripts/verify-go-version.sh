#!/usr/bin/env bash
set -euo pipefail

# CI builds and tests with the go.mod toolchain line (setup-go reads it), while
# the published image is built by the Dockerfile's golang stage. Dependabot
# bumps only the Dockerfile and never the toolchain line, so require every
# golang stage to name the toolchain's release; otherwise CI would test one Go
# and ship another.

dockerfile=${1:-Dockerfile}
gomod=${2:-go.mod}

# One line per golang stage: the version from its image reference, or the raw
# reference when it is not a literal MAJOR.MINOR.PATCH tag. Only the image
# operand is inspected, after any --flag options, so stage names, comments and
# flag values cannot be mistaken for it.
builder_versions="$(
  awk '
    toupper($1) == "FROM" {
      for (i = 2; i <= NF && $i ~ /^--/; i++) {}
      ref = $i
      if (ref !~ /^((docker\.io\/)?library\/)?golang(:|@|$)/) next
      version = ref
      sub(/^((docker\.io\/)?library\/)?golang:?/, "", version)
      sub(/@.*/, "", version)
      sub(/-.*/, "", version)
      print (version ~ /^[0-9]+\.[0-9]+\.[0-9]+$/ ? version : ref)
    }
  ' "$dockerfile"
)"
toolchain_version="$(awk '$1 == "toolchain" { sub(/^go/, "", $2); print $2; exit }' "$gomod")"

if [[ -z "$builder_versions" ]]; then
  echo "$dockerfile has no golang build stage" >&2
  exit 1
fi
if [[ ! "$toolchain_version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo "$gomod must declare an exact toolchain goMAJOR.MINOR.PATCH line, found: ${toolchain_version:-<none>}" >&2
  exit 1
fi

status=0
while IFS= read -r builder_version; do
  if [[ ! "$builder_version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    echo "$dockerfile must build from a literal golang:MAJOR.MINOR.PATCH image, found: $builder_version" >&2
    status=1
  elif [[ "$builder_version" != "$toolchain_version" ]]; then
    echo "Go version mismatch: $dockerfile builds with $builder_version but $gomod selects toolchain go$toolchain_version." >&2
    echo "Run: go mod edit -toolchain=go$builder_version (or align the Dockerfile), then rerun CI." >&2
    status=1
  fi
done <<< "$builder_versions"
if (( status != 0 )); then
  exit "$status"
fi

echo "Dockerfile and go.mod both use Go $toolchain_version"
