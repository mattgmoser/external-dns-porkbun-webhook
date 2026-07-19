#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 2 ]]; then
  echo "usage: $0 <image@sha256:digest> <report-directory>" >&2
  exit 2
fi

image_ref=$1
report_dir=$2

mkdir -p "$report_dir"

scan_status=0
for platform in linux/amd64 linux/arm64 linux/arm/v7; do
  report="$report_dir/${platform//\//-}.json"
  if ! scripts/verify-image-vulnerabilities.sh "$image_ref" "$platform" "$report"; then
    scan_status=1
  fi
done

exit "$scan_status"
