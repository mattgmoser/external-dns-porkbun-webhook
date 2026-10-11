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

if (( failures != 0 )); then
  echo "$failures verify-go-version case(s) failed" >&2
  exit 1
fi
echo "verify-go-version: all cases passed"
