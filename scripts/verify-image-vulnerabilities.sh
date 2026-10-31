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

scripts/scan-image.sh "$image_ref" "$platform" "$report"
scripts/verify-trivy-report.sh "$report"
