#!/usr/bin/env bash
set -euo pipefail

TRUST_DIRS=(
  "/usr/local/share/ca-certificates/epar"
  "/usr/local/share/ca-certificates/epar-host"
)
has_certificates=false
for trust_dir in "${TRUST_DIRS[@]}"; do
  if [[ -d "${trust_dir}" ]] && find "${trust_dir}" -type f -name '*.crt' -print -quit | grep -q .; then
    has_certificates=true
    break
  fi
done
if [[ "${has_certificates}" != "true" ]]; then
  exit 0
fi

if ! command -v update-ca-certificates >/dev/null 2>&1; then
  echo "update-ca-certificates is required to install EPAR trusted CA certificates" >&2
  exit 1
fi

update-ca-certificates
