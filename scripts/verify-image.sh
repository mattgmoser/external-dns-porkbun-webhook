#!/usr/bin/env bash
set -euo pipefail

image_ref=${1:?image reference is required}
expected_revision=${2:?expected source revision is required}
expected_version=${3:?expected image version is required}

image_json=$(docker buildx imagetools inspect "$image_ref" --format '{{json .Image}}')
jq -e \
  --arg revision "$expected_revision" \
  --arg version "$expected_version" \
  '((keys | sort) == ["linux/amd64", "linux/arm/v7", "linux/arm64"])
   and all(.[];
     .config.Labels["org.opencontainers.image.revision"] == $revision
     and .config.Labels["org.opencontainers.image.version"] == $version)' \
  <<< "$image_json" >/dev/null

manifest_json=$(docker buildx imagetools inspect "$image_ref" --format '{{json .Manifest}}')
digest=$(jq -r '.digest // empty' <<< "$manifest_json")
if [[ ! $digest =~ ^sha256:[0-9a-f]{64}$ ]]; then
  echo "could not resolve an OCI index digest for $image_ref" >&2
  exit 1
fi

mapfile -t image_digests < <(
  jq -r '.manifests[] | select(.platform.os == "linux") | .digest' <<< "$manifest_json" | sort
)
mapfile -t attestation_rows < <(
  jq -r '
    .manifests[]
    | select(.platform.os == "unknown")
    | select(.annotations["vnd.docker.reference.type"] == "attestation-manifest")
    | [.digest, .annotations["vnd.docker.reference.digest"]]
    | @tsv
  ' <<< "$manifest_json"
)
mapfile -t attested_image_digests < <(
  printf '%s\n' "${attestation_rows[@]}" | cut -f2 | sort
)
if [[ ${#attestation_rows[@]} -ne 3 || "${attested_image_digests[*]}" != "${image_digests[*]}" ]]; then
  echo "$image_ref does not have one attestation manifest per platform" >&2
  exit 1
fi

repository=${image_ref%@*}
last_component=${repository##*/}
if [[ $last_component == *:* ]]; then
  repository=${repository%:*}
fi
for row in "${attestation_rows[@]}"; do
  attestation_digest=${row%%$'\t'*}
  attestation_manifest=$(docker buildx imagetools inspect "${repository}@${attestation_digest}" --raw)
  mapfile -t predicate_types < <(
    jq -r '.layers[].annotations["in-toto.io/predicate-type"] // empty' <<< "$attestation_manifest" | sort
  )
  expected_predicates=(https://slsa.dev/provenance/v1 https://spdx.dev/Document)
  if [[ "${predicate_types[*]}" != "${expected_predicates[*]}" ]]; then
    echo "$image_ref is missing its SPDX SBOM or SLSA provenance attestation" >&2
    exit 1
  fi
done

printf '%s\n' "$digest"
