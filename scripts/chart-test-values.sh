#!/usr/bin/env bash
set -euo pipefail

values_file=${1:?values file is required}

# Convert only the documented placeholders into inert CI values. Keeping this
# transformation small means the chart's real defaults and documentation are
# what every lint, render, schema, and package check exercises.
sed \
  -e 's/value: "example.com"/value: "ci.example.test"/g' \
  -e 's/- example.com/- ci.example.test/g' \
  -e 's/txtOwnerId: change-me/txtOwnerId: ci-controller/g' \
  "$values_file"
