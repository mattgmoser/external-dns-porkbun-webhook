#!/usr/bin/env bash
set -euo pipefail

# CI builds and tests with the go.mod toolchain line (setup-go reads it), while
# the published image is built by the Dockerfile's golang stage. Dependabot
# bumps only the Dockerfile and never the toolchain line, so require the two to
# name the same release; otherwise CI would test one Go and ship another.

dockerfile=${1:-Dockerfile}
gomod=${2:-go.mod}

builder_version="$(
  awk '
    toupper($1) == "FROM" {
      for (i = 2; i <= NF; i++) {
        if ($i ~ /^golang:/) {
          ref = $i
          sub(/^golang:/, "", ref)
          sub(/@.*/, "", ref)
          sub(/-.*/, "", ref)
          print ref
          exit
        }
      }
    }
  ' "$dockerfile"
)"
toolchain_version="$(awk '$1 == "toolchain" { sub(/^go/, "", $2); print $2; exit }' "$gomod")"

if [[ ! "$builder_version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo "$dockerfile must build from a literal golang:MAJOR.MINOR.PATCH image, found: ${builder_version:-<none>}" >&2
  exit 1
fi
if [[ ! "$toolchain_version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo "$gomod must declare an exact toolchain goMAJOR.MINOR.PATCH line, found: ${toolchain_version:-<none>}" >&2
  exit 1
fi
if [[ "$builder_version" != "$toolchain_version" ]]; then
  echo "Go version mismatch: $dockerfile builds with $builder_version but $gomod selects toolchain go$toolchain_version." >&2
  echo "Run: go mod edit -toolchain=go$builder_version (or align the Dockerfile), then rerun CI." >&2
  exit 1
fi

echo "Dockerfile and go.mod both use Go $builder_version"
