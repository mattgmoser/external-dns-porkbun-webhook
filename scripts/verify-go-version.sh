#!/usr/bin/env bash
set -euo pipefail

# CI builds and tests with the go.mod toolchain line (setup-go reads it), while
# the published image is built by the Dockerfile's golang stage. Dependabot
# bumps only the Dockerfile and never the toolchain line, so require every
# golang image the Dockerfile uses to name the toolchain's release; otherwise
# CI would test one Go and ship another.
#
# This is not a Dockerfile parser, so it fails closed: anything it cannot
# classify with certainty is rejected rather than skipped.
#   - Parser directives (escape, syntax, ...) can change how the file parses.
#   - An image operand containing $ is resolved by build arguments at build time.
#   - Any reference whose repository's last path component is golang is a Go
#     image, whatever its registry, and needs a literal MAJOR.MINOR.PATCH tag.
# Image operands are the FROM operand after its --flags, and every --from= or
# --mount from= value in any instruction. Backslash continuations are joined,
# dropping the blank and comment lines Docker ignores inside them, and quotes
# around tokens are removed.

dockerfile=${1:-Dockerfile}
gomod=${2:-go.mod}

# One line per golang image: its version, or "!<reason> <reference>".
builder_versions="$(
  awk '
    function unquote(s) {
      gsub(/^["\047]+|["\047]+$/, "", s)
      return s
    }
    # Prints the version of a golang image reference, a rejection, or nothing
    # when the reference is some other image or a stage name.
    function classify(ref,    name, tag, repo) {
      ref = unquote(ref)
      if (ref == "") return
      if (index(ref, "$") > 0) { print "!variable " ref; return }
      name = ref
      sub(/@.*/, "", name)
      tag = ""
      if (match(name, /:[^:\/]*$/)) {
        tag = substr(name, RSTART + 1)
        name = substr(name, 1, RSTART - 1)
      }
      repo = name
      sub(/^.*\//, "", repo)
      if (tolower(repo) != "golang") return
      sub(/-.*/, "", tag)
      if (tag ~ /^[0-9]+\.[0-9]+\.[0-9]+$/) print tag
      else print "!unversioned " ref
    }
    !instructions && /^[[:space:]]*#/ {
      directive = tolower($0)
      gsub(/[[:space:]]/, "", directive)
      if (directive ~ /^#[a-z]+=/) { print "!directive " $0; exit }
      next
    }
    /^[[:space:]]*(#|$)/ { next }
    {
      instructions = 1
      line = $0
      while (line ~ /\\[[:space:]]*$/) {
        sub(/\\[[:space:]]*$/, "", line)
        while ((more = getline continuation) > 0 && continuation ~ /^[[:space:]]*(#|$)/) {}
        if (more <= 0) break
        line = line " " continuation
      }
      n = split(line, raw, /[[:space:]]+/)
      split("", token)
      count = 0
      for (i = 1; i <= n; i++) if (raw[i] != "") token[++count] = raw[i]
      if (toupper(token[1]) == "FROM") {
        for (i = 2; i <= count && unquote(token[i]) ~ /^--/; i++) {}
        if (i <= count) classify(token[i])
        else print "!missing-image " line
      }
      for (i = 2; i <= count; i++) {
        t = unquote(token[i])
        if (t ~ /^--from=/) {
          classify(substr(t, 8))
        } else if (t ~ /^--mount=/) {
          m = split(substr(t, 9), option, ",")
          for (j = 1; j <= m; j++) {
            o = unquote(option[j])
            if (o ~ /^from=/) classify(substr(o, 6))
          }
        }
      }
    }
  ' "$dockerfile"
)"
toolchain_version="$(awk '$1 == "toolchain" { sub(/^go/, "", $2); print $2; exit }' "$gomod")"

if [[ ! "$toolchain_version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo "$gomod must declare an exact toolchain goMAJOR.MINOR.PATCH line, found: ${toolchain_version:-<none>}" >&2
  exit 1
fi
if [[ -z "$builder_versions" ]]; then
  echo "$dockerfile has no golang build stage" >&2
  exit 1
fi

status=0
while IFS= read -r builder_version; do
  case "$builder_version" in
    '!directive '*)
      echo "$dockerfile uses a parser directive this check cannot follow: ${builder_version#!directive }" >&2
      status=1
      ;;
    '!variable '*)
      echo "$dockerfile must name images literally; build arguments make this check unverifiable: ${builder_version#!variable }" >&2
      status=1
      ;;
    '!unversioned '*)
      echo "$dockerfile must build from a literal golang:MAJOR.MINOR.PATCH image, found: ${builder_version#!unversioned }" >&2
      status=1
      ;;
    '!missing-image '*)
      echo "$dockerfile has a FROM instruction without an image: ${builder_version#!missing-image }" >&2
      status=1
      ;;
    *)
      if [[ "$builder_version" != "$toolchain_version" ]]; then
        echo "Go version mismatch: $dockerfile builds with $builder_version but $gomod selects toolchain go$toolchain_version." >&2
        echo "Run: go mod edit -toolchain=go$builder_version (or align the Dockerfile), then rerun CI." >&2
        status=1
      fi
      ;;
  esac
done <<< "$builder_versions"
if (( status != 0 )); then
  exit "$status"
fi

echo "Dockerfile and go.mod both use Go $toolchain_version"
