#!/usr/bin/env bash
set -euo pipefail

# Regression cases for scripts/verify-go-version.sh. Each case writes a
# Dockerfile against a go.mod selecting go1.27.2 and asserts pass or fail.

script="$(cd "$(dirname "$0")" && pwd)/verify-go-version.sh"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
printf 'module example.invalid/test\n\ngo 1.27.0\n\ntoolchain go1.27.2\n' > "$work/go.mod"

failures=0
check() {
  local expected=$1 name=$2 dockerfile=$3 actual=pass
  printf '%s\n' "$dockerfile" > "$work/Dockerfile"
  if ! "$script" "$work/Dockerfile" "$work/go.mod" >/dev/null 2>&1; then
    actual=fail
  fi
  if [[ "$actual" != "$expected" ]]; then
    echo "FAIL $name: expected $expected, got $actual" >&2
    failures=$((failures + 1))
  fi
}

check pass repository-dockerfile "$(cat "$(dirname "$script")/../Dockerfile")"
check pass platform-flag-and-stage 'FROM --platform=$BUILDPLATFORM golang:1.27.2-alpine AS build'
check pass digest-pinned 'FROM golang:1.27.2-alpine@sha256:f92b6ef800e499660581efdabdf25d9d817a9d124eaf900924f0504e7e27e12d AS build'
check pass lowercase 'from golang:1.27.2-alpine as build'
check pass fully-qualified 'FROM docker.io/library/golang:1.27.2 AS build'
check pass two-matching-stages $'FROM golang:1.27.2-alpine AS tools\nFROM golang:1.27.2-alpine AS build'
check fail patch-mismatch 'FROM golang:1.27.3-alpine AS build'
check fail arg-interpolated $'ARG GO_VERSION=1.27.2\nFROM golang:${GO_VERSION}-alpine AS build'
check fail floating-minor 'FROM golang:1.27-alpine AS build'
check fail untagged 'FROM golang AS build'
check fail second-stage-mismatch $'FROM golang:1.27.2-alpine AS tools\nFROM golang:1.27.1-alpine AS build'
check fail no-golang-stage 'FROM alpine:3.22'
check fail golang-only-in-flag-value 'FROM --platform=golang:1.27.2 alpine:3.22 AS build'
check pass continued-from $'FROM --platform=$BUILDPLATFORM \\\n  golang:1.27.2-alpine AS build'
check fail continued-second-stage-mismatch $'FROM golang:1.27.2-alpine AS tools\nFROM --platform=$BUILDPLATFORM \\\n  golang:1.27.1-alpine AS build'
check fail continued-with-comment-line $'FROM golang:1.27.2-alpine AS tools\nFROM \\\n# note\n  golang:1.27.1-alpine AS build'
check pass copy-from-stage $'FROM golang:1.27.2-alpine AS build\nFROM alpine:3.22\nCOPY --from=build /out /out'
check fail copy-from-external-golang $'FROM golang:1.27.2-alpine AS decoy\nFROM alpine:3.22 AS build\nCOPY --from=golang:1.27.1-alpine /usr/local/go /usr/local/go'
check fail mount-from-external-golang $'FROM golang:1.27.2-alpine AS decoy\nFROM alpine:3.22 AS build\nRUN --mount=type=bind,from=golang:1.27.1-alpine,source=/usr/local/go,target=/go true'
check pass copy-from-matching-golang $'FROM golang:1.27.2-alpine AS build\nFROM alpine:3.22\nCOPY --from=golang:1.27.2-alpine /usr/local/go /usr/local/go'
check fail flags-only-after-golang-stage $'FROM golang:1.27.1-alpine AS old\nFROM golang:1.27.2-alpine AS build\nFROM --platform=linux/amd64'
check fail escape-directive $'# escape=`\nFROM golang:1.27.2-alpine AS build'
check fail escape-directive-mixed-case $'# EsCaPe=`\nFROM golang:1.27.2-alpine AS build'
check fail escape-directive-indented $' # escape=`\nFROM golang:1.27.2-alpine AS build'
check fail syntax-directive $'# syntax=example.invalid/frontend:1\nFROM golang:1.27.2-alpine AS build'
check pass ordinary-leading-comments $'# Multi-stage build.\n# Builder: see go.mod.\nFROM golang:1.27.2-alpine AS build'
check fail quoted-from-mismatch $'FROM golang:1.27.2-alpine AS decoy\nFROM "golang:1.27.1-alpine" AS build'
check fail quoted-copy-from-mismatch $'FROM golang:1.27.2-alpine AS decoy\nFROM alpine:3.22\nCOPY --from="golang:1.27.1-alpine" /usr/local/go /usr/local/go'
check fail quoted-mount-from-mismatch $'FROM golang:1.27.2-alpine AS decoy\nFROM alpine:3.22\nRUN --mount=type=bind,"from=golang:1.27.1-alpine",target=/go true'
check fail empty-continuation-line $'FROM golang:1.27.2-alpine AS decoy\nFROM \\\n\n  golang:1.27.1-alpine AS build'
check fail arg-variable-image $'ARG GO_IMAGE=golang:1.27.1-alpine\nFROM golang:1.27.2-alpine AS decoy\nFROM ${GO_IMAGE} AS build'
check fail registry-prefixed-mismatch $'FROM golang:1.27.2-alpine AS decoy\nFROM registry-1.docker.io/library/golang:1.27.1-alpine AS build'
check fail registry-port-untagged $'FROM golang:1.27.2-alpine AS decoy\nFROM registry.example:5000/golang AS build'
check pass registry-port-matching $'FROM registry.example:5000/library/golang:1.27.2-alpine AS build'
check fail onbuild-copy-from-mismatch $'FROM golang:1.27.2-alpine AS build\nONBUILD COPY --from=golang:1.27.1 /usr/local/go /usr/local/go'
check pass stage-named-like-image $'FROM golang:1.27.2-alpine AS build\nFROM build AS test'

if (( failures != 0 )); then
  echo "$failures verify-go-version case(s) failed" >&2
  exit 1
fi
echo "verify-go-version: all cases passed"
