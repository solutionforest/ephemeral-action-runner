#!/usr/bin/env bash

set -euo pipefail

if [[ $# -ne 3 ]]; then
  echo "usage: $0 <package-repository> <catalog-reference> <output-file>" >&2
  exit 2
fi

repository="$1"
reference="$2"
output_file="$3"

if [[ ! "$repository" =~ ^[a-z0-9.-]+/[a-z0-9._/-]+$ ]]; then
  echo "invalid package repository: $repository" >&2
  exit 2
fi
if [[ "$reference" != "${repository}@sha256:"* ]]; then
  echo "catalog reference must be an exact digest in $repository" >&2
  exit 2
fi
manifest_digest="${reference#${repository}@}"
if [[ ! "$manifest_digest" =~ ^sha256:[0-9a-f]{64}$ ]]; then
  echo "catalog reference has an invalid manifest digest: $reference" >&2
  exit 2
fi
if [[ "$output_file" != /* || "$output_file" == */ ]]; then
  echo "an absolute output file is required" >&2
  exit 2
fi

output_directory="$(dirname "$output_file")"
mkdir -p "$output_directory"
if [[ -L "$output_file" ]]; then
  echo "catalog output must not be a symbolic link: $output_file" >&2
  exit 1
fi

work_directory="$(mktemp -d "$output_directory/.epar-catalog-fetch.XXXXXX")"
cleanup() {
  rm -rf -- "$work_directory"
}
trap cleanup EXIT

manifest_file="$work_directory/manifest.json"
catalog_file="$work_directory/catalog.json"
oras manifest fetch "$reference" --format json > "$manifest_file"

layer_digest="$({
  jq -er \
    --arg artifact "application/vnd.epar.prebuilt.catalog.v1" \
    --arg config "application/vnd.epar.prebuilt.catalog.config.v1+json" \
    --arg layer "application/vnd.epar.prebuilt.catalog.v1+json" \
    'select(.schemaVersion == 2)
     | select(.mediaType == "application/vnd.oci.image.manifest.v1+json")
     | select(.artifactType == $artifact)
     | select(.content.config.mediaType == $config)
     | select((.content.layers | length) == 1)
     | select(.content.layers[0].mediaType == $layer)
     | .content.layers[0].digest
     | select(test("^sha256:[0-9a-f]{64}$"))' \
    "$manifest_file"
} || true)"
if [[ ! "$layer_digest" =~ ^sha256:[0-9a-f]{64}$ ]]; then
  echo "catalog manifest has an invalid artifact, config, or layer descriptor" >&2
  exit 1
fi

oras blob fetch --output "$catalog_file" "${repository}@${layer_digest}"
actual_digest="sha256:$(sha256sum "$catalog_file" | awk '{print $1}')"
if [[ "$actual_digest" != "$layer_digest" ]]; then
  echo "catalog layer digest mismatch: expected $layer_digest, got $actual_digest" >&2
  exit 1
fi
jq -e '(.schemaVersion == 1) and (.artifactKind == "docker-sandboxes-template")' "$catalog_file" >/dev/null

mv -f -- "$catalog_file" "$output_file"
