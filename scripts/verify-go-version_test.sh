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
check fail digest-pinned-rejected 'FROM golang:1.27.2-alpine@sha256:f92b6ef800e499660581efdabdf25d9d817a9d124eaf900924f0504e7e27e12d AS build'
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
check fail split-token-continuation $'FROM golang:1.27.2-alpine AS decoy\nFROM gol\\\nang:1.27.1-alpine AS build'
check pass split-token-continuation-matching $'FROM gol\\\nang:1.27.2-alpine AS build'
check fail arg-variable-image $'ARG GO_IMAGE=golang:1.27.1-alpine\nFROM golang:1.27.2-alpine AS decoy\nFROM ${GO_IMAGE} AS build'
check fail inner-quotes-from $'FROM golang:1.27.2-alpine AS decoy\nFROM gol"ang":1.27.1-alpine AS build'
check fail inner-single-quotes-from $'FROM golang:1.27.2-alpine AS decoy\nFROM gol\'ang\':1.27.1-alpine AS build'
check fail inner-escape-from $'FROM golang:1.27.2-alpine AS decoy\nFROM gol\\ang:1.27.1-alpine AS build'
check fail empty-variable-from $'FROM golang:1.27.2-alpine AS decoy\nFROM gol${EMPTY}ang:1.27.1-alpine AS build'
check fail inner-quotes-copy-from $'FROM golang:1.27.2-alpine AS decoy\nFROM alpine:3.22\nCOPY --from=gol"ang":1.27.1-alpine /usr/local/go /usr/local/go'
check fail inner-escape-mount-from $'FROM golang:1.27.2-alpine AS decoy\nFROM alpine:3.22\nRUN --mount=type=bind,from=gol\\ang:1.27.1-alpine,target=/go true'
check pass outer-quotes-only $'FROM "golang:1.27.2-alpine" AS build'
check fail registry-prefixed-mismatch $'FROM golang:1.27.2-alpine AS decoy\nFROM registry-1.docker.io/library/golang:1.27.1-alpine AS build'
check fail registry-port-untagged $'FROM golang:1.27.2-alpine AS decoy\nFROM registry.example:5000/golang AS build'
check pass registry-port-matching $'FROM registry.example:5000/library/golang:1.27.2-alpine AS build'
check fail onbuild-copy-from-mismatch $'FROM golang:1.27.2-alpine AS build\nONBUILD COPY --from=golang:1.27.1 /usr/local/go /usr/local/go'
check pass stage-named-like-image $'FROM golang:1.27.2-alpine AS build\nFROM build AS test'
check fail tag-digest-mismatch 'FROM golang:1.27.2-alpine@sha256:8a5910f31396cd4d89662f56c68b3ae31d374308270a1c3bd96672ee5ed43414 AS build'
check fail bom-hides-first-stage $'\xEF\xBB\xBFFROM golang:1.27.1-alpine AS actual\nFROM golang:1.27.2-alpine AS decoy'
check pass bom-matching $'\xEF\xBB\xBFFROM golang:1.27.2-alpine AS build'
check pass hadolint-comment $'# hadolint ignore=DL3006\nFROM golang:1.27.2-alpine AS build'
check pass syntax-official-frontend $'# syntax=docker/dockerfile:1\nFROM golang:1.27.2-alpine AS build'
check pass syntax-official-spaced $'# syntax = docker/dockerfile:1.7\nFROM golang:1.27.2-alpine AS build'
check pass check-directive $'# check=skip=JSONArgsRecommended\nFROM golang:1.27.2-alpine AS build'
check pass escape-default-backslash $'# escape=\\\nFROM golang:1.27.2-alpine AS build'
check pass stage-named-golang $'FROM golang:1.27.2-alpine AS golang\nFROM alpine:3.22\nCOPY --from=golang /usr/local/go /usr/local/go'
# Deliberate fail-closed limitations: a golang mention the check cannot place.
check fail heredoc-mention $'FROM golang:1.27.2-alpine AS build\nCOPY <<EOF /notes\nFROM golang:1.27.1-alpine\nEOF'
check fail shell-argument-from-flag $'FROM golang:1.27.2-alpine AS build\nRUN echo --from=golang:1.27.1-alpine'
check fail shell-argument-mention $'FROM golang:1.27.2-alpine AS build\nRUN apk add --no-cache golang'

if (( failures != 0 )); then
  echo "$failures verify-go-version case(s) failed" >&2
  exit 1
fi
echo "verify-go-version: all cases passed"
