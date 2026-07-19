#!/usr/bin/env bash
set -euo pipefail

usage() {
  echo "usage: $0 <image@sha256:digest> <linux/amd64|linux/arm64|linux/arm/v7> <report.json>" >&2
}

if [[ $# -ne 3 ]]; then
  usage
  exit 2
fi

image_ref=$1
platform=$2
report=$3

if [[ ! "$image_ref" =~ @sha256:[0-9a-f]{64}$ ]]; then
  echo "image reference must use an immutable sha256 digest: $image_ref" >&2
  exit 2
fi

case "$platform" in
  linux/amd64)
    expected_arch=amd64
    expected_variant=
    ;;
  linux/arm64)
    expected_arch=arm64
    expected_variant=
    ;;
  linux/arm/v7)
    expected_arch=arm
    expected_variant=v7
    ;;
  *)
    echo "unsupported release platform: $platform" >&2
    usage
    exit 2
    ;;
esac

if ! command -v trivy >/dev/null 2>&1; then
  echo "trivy is required" >&2
  exit 127
fi
if ! command -v jq >/dev/null 2>&1; then
  echo "jq is required" >&2
  exit 127
fi

echo "Scanning $image_ref ($platform)"
trivy --config /dev/null \
  image \
  --scanners vuln \
  --pkg-types os,library \
  --severity UNKNOWN,LOW,MEDIUM,HIGH,CRITICAL \
  --detection-priority comprehensive \
  --image-src remote \
  --platform "$platform" \
  --ignorefile /dev/null \
  --disable-telemetry \
  --no-progress \
  --skip-version-check \
  --timeout 10m \
  --format json \
  --output "$report" \
  "$image_ref"

if ! jq -e \
  --arg image_ref "$image_ref" \
  --arg arch "$expected_arch" \
  --arg variant "$expected_variant" \
  '.ArtifactName == $image_ref
   and .ArtifactType == "container_image"
   and .Metadata.ImageConfig.os == "linux"
   and .Metadata.ImageConfig.architecture == $arch
   and ((.Metadata.ImageConfig.variant // "") == $variant)' \
  "$report" >/dev/null; then
  echo "Trivy report does not describe $image_ref for $platform" >&2
  exit 2
fi

scripts/verify-trivy-report.sh "$report"
