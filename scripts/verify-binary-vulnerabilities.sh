#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 1 ]]; then
  echo "usage: $0 <report.json>" >&2
  exit 2
fi

report=$1
scan_root="$(mktemp -d)"

if ! command -v go >/dev/null 2>&1; then
  echo "go is required" >&2
  exit 127
fi
if ! command -v trivy >/dev/null 2>&1; then
  echo "trivy is required" >&2
  exit 127
fi

CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
  -trimpath \
  -ldflags='-s -w -X main.Version=security-scan' \
  -o "$scan_root/external-dns-porkbun-webhook" ./

trivy --config /dev/null rootfs \
  --scanners vuln \
  --pkg-types library \
  --severity UNKNOWN,LOW,MEDIUM,HIGH,CRITICAL \
  --detection-priority comprehensive \
  --ignorefile /dev/null \
  --disable-telemetry \
  --no-progress \
  --skip-version-check \
  --format json \
  --output "$report" \
  "$scan_root"

scripts/verify-trivy-report.sh "$report"
