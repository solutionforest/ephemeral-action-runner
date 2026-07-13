#!/usr/bin/env bash
set -euo pipefail

TRUST_DIR="/usr/local/share/ca-certificates/epar"
if [[ ! -d "${TRUST_DIR}" ]] || ! find "${TRUST_DIR}" -type f -name '*.crt' -print -quit | grep -q .; then
  exit 0
fi

if ! command -v update-ca-certificates >/dev/null 2>&1; then
  echo "update-ca-certificates is required to install image.trustedCaCertificatePaths" >&2
  exit 1
fi

update-ca-certificates
