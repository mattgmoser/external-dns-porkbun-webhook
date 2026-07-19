#!/usr/bin/env bash
set -euo pipefail

if (( $# != 5 )); then
  echo "usage: $0 INDEX CHART VERSION SHA256 URL" >&2
  exit 2
fi

index=$1
chart=$2
version=$3
expected_digest=$4
expected_url=$5

if [[ ! -f "$index" ]]; then
  echo "chart index not found: $index" >&2
  exit 1
fi
if [[ ! "$expected_digest" =~ ^[0-9a-f]{64}$ ]]; then
  echo "invalid expected SHA-256: $expected_digest" >&2
  exit 2
fi

entry=$(mktemp)
trap 'rm -f "$entry"' EXIT

# Isolate exactly one matching version from the requested chart. Chart-releaser
# preserves existing versions, so accepting the first match could hide a stale
# duplicate with a different digest.
awk -v chart="$chart" -v version="$version" '
  function finish_entry() {
    if (entry != "" && entry_version) {
      matches++
      selected = entry
    }
    entry = ""
    entry_version = 0
  }

  $0 == "  " chart ":" {
    in_chart = 1
    next
  }

  in_chart && (/^[^ ]/ || /^  [^ -][^:]*:[[:space:]]*$/) {
    finish_entry()
    in_chart = 0
  }

  in_chart && /^  - / {
    finish_entry()
    entry = $0 ORS
    next
  }

  in_chart {
    entry = entry $0 ORS
    if ($0 == "    version: " version) {
      entry_version = 1
    }
  }

  END {
    finish_entry()
    if (matches != 1) {
      printf "expected exactly one %s %s index entry, found %d\n", chart, version, matches > "/dev/stderr"
      exit 1
    }
    printf "%s", selected
  }
' "$index" > "$entry"

if [[ "$(grep -Fxc "    version: $version" "$entry" || true)" != 1 ]]; then
  echo "chart index entry does not contain exactly the expected version" >&2
  exit 1
fi

if [[ "$(grep -Fxc "    digest: $expected_digest" "$entry" || true)" != 1 ]]; then
  echo "chart index entry does not contain the expected package digest" >&2
  exit 1
fi

awk -v expected="$expected_url" '
  $0 == "    urls:" {
    blocks++
    in_urls = 1
    next
  }
  in_urls && /^    - / {
    urls++
    if (substr($0, 7) == expected) {
      matches++
    }
    next
  }
  in_urls {
    in_urls = 0
  }
  END {
    if (blocks != 1 || urls != 1 || matches != 1) {
      print "chart index entry does not contain exactly the expected package URL" > "/dev/stderr"
      exit 1
    }
  }
' "$entry"
