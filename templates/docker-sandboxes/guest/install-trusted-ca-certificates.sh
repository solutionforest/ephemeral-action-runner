#!/usr/bin/env bash
set -euo pipefail

trust_dirs=(
  "/usr/local/share/ca-certificates/epar"
  "/usr/local/share/ca-certificates/epar-host"
)
has_certificates=false
for trust_dir in "${trust_dirs[@]}"; do
  if [[ -d "${trust_dir}" ]] && find "${trust_dir}" -type f -name '*.crt' -print -quit | grep -q .; then
    has_certificates=true
    break
  fi
done
if [[ "${has_certificates}" == "true" ]]; then
  if ! command -v update-ca-certificates >/dev/null 2>&1; then
    echo "update-ca-certificates is required to install EPAR trusted CA certificates" >&2
    exit 1
  fi
  update-ca-certificates
fi

# Give every EPAR-owned TLS client one stable bundle path. Runtime host-trust
# refreshes replace this file atomically after update-ca-certificates.
system_bundle="/etc/ssl/certs/ca-certificates.crt"
canonical_dir="/opt/epar/trust"
canonical_bundle="${canonical_dir}/ca-bundle.pem"
[[ -s "${system_bundle}" && ! -L "${system_bundle}" ]]
install -d -m 0755 -o root -g root "${canonical_dir}"
install -m 0444 -o root -g root "${system_bundle}" "${canonical_bundle}.tmp"
mv -f "${canonical_bundle}.tmp" "${canonical_bundle}"
[[ -s "${canonical_bundle}" && ! -L "${canonical_bundle}" ]]
[[ "$(stat -c '%U:%G:%a' "${canonical_bundle}")" == "root:root:444" ]]
