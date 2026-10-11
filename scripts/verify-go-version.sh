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
#   - Parser directives other than the official frontend, check, and the
#     default escape character can change how the file parses.
#   - An image operand containing $ is resolved by build arguments at build time.
#   - Any reference whose repository's last path component is golang is a Go
#     image, whatever its registry, and needs a literal MAJOR.MINOR.PATCH tag
#     and no digest: BuildKit resolves a digest even when it names a different
#     release than the tag beside it.
#   - Any other mention of golang in an instruction (a heredoc body, a quoted
#     or unusual flag, a shell argument) is rejected, because it may be an
#     image reference this check does not recognise.
# Image operands are the FROM operand after its --flags, and every --from= or
# --mount from= value. Earlier stage names are not images. Backslash
# continuations are joined exactly as Docker joins them, dropping the blank and
# comment lines it ignores inside them. Quotes around tokens are removed, and a
# leading BOM is stripped.

dockerfile=${1:-Dockerfile}
gomod=${2:-go.mod}

# One line per golang image: its version, or "!<reason> <detail>".
builder_versions="$(
  LC_ALL=C awk '
    function unquote(s) {
      gsub(/^["\047]+|["\047]+$/, "", s)
      return s
    }
    # Prints the version of a golang image reference, a rejection, or nothing
    # when the reference is a stage name or some other image. Returns 1 when
    # the reference was accounted for as a stage or golang image.
    function classify(ref,    name, tag, repo) {
      ref = unquote(ref)
      if (ref == "") return 0
      if (tolower(ref) in stage) return 1
      if (index(ref, "$") > 0) { print "!variable " ref; return 1 }
      name = ref
      if (index(name, "@") > 0) { digest = 1; sub(/@.*/, "", name) } else digest = 0
      tag = ""
      if (match(name, /:[^:\/]*$/)) {
        tag = substr(name, RSTART + 1)
        name = substr(name, 1, RSTART - 1)
      }
      repo = name
      sub(/^.*\//, "", repo)
      if (tolower(repo) != "golang") return 0
      if (digest) { print "!digest " ref; return 1 }
      sub(/-.*/, "", tag)
      if (tag ~ /^[0-9]+\.[0-9]+\.[0-9]+$/) print tag
      else print "!unversioned " ref
      return 1
    }
    NR == 1 { sub(/^\357\273\277/, "") }
    !instructions && /^[[:space:]]*#/ {
      if (match(tolower($0), /^[[:space:]]*#[[:space:]]*[a-z]+[[:space:]]*=/)) {
        key = tolower(substr($0, RSTART, RLENGTH))
        gsub(/[#=[:space:]]/, "", key)
        value = substr($0, RSTART + RLENGTH)
        gsub(/^[[:space:]]+|[[:space:]]+$/, "", value)
        if (key == "check") next
        if (key == "escape" && value == "\\") next
        if (key == "syntax" && value ~ /^(docker\.io\/)?docker\/dockerfile(:[0-9][0-9.]*)?(@sha256:[0-9a-f]+)?$/) next
        print "!directive " $0
        exit
      }
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
        # Docker joins continued lines directly, without adding whitespace,
        # so a backslash can split a single token across lines.
        line = line continuation
      }
      n = split(line, raw, /[[:space:]]+/)
      split("", token)
      split("", accounted)
      count = 0
      for (i = 1; i <= n; i++) if (raw[i] != "") token[++count] = raw[i]
      keyword = 1
      if (toupper(token[1]) == "ONBUILD") keyword = 2
      if (toupper(token[keyword]) == "FROM") {
        for (i = keyword + 1; i <= count && unquote(token[i]) ~ /^--/; i++) {}
        if (i > count) {
          print "!missing-image " line
        } else {
          accounted[i] = classify(token[i])
          if (i + 2 <= count && toupper(unquote(token[i + 1])) == "AS") {
            stage[tolower(unquote(token[i + 2]))] = 1
            accounted[i + 2] = 1
          }
        }
      }
      for (i = keyword + 1; i <= count; i++) {
        t = unquote(token[i])
        if (t !~ /^--/) break
        if (t ~ /^--from=/) {
          accounted[i] = classify(substr(t, 8))
        } else if (t ~ /^--mount=/) {
          m = split(substr(t, 9), option, ",")
          for (j = 1; j <= m; j++) {
            o = unquote(option[j])
            if (o ~ /^from=/) accounted[i] = classify(substr(o, 6))
          }
        }
      }
      for (i = 1; i <= count; i++) {
        if (!accounted[i] && tolower(token[i]) ~ /golang/) print "!unrecognised " line
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
    '!digest '*)
      echo "$dockerfile must pin golang images by tag only; a digest can name a different Go release than its tag: ${builder_version#!digest }" >&2
      status=1
      ;;
    '!unrecognised '*)
      echo "$dockerfile mentions golang in an instruction this check cannot verify: ${builder_version#!unrecognised }" >&2
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
