#!/usr/bin/env bash
# This file is sourced by start-runner-with-env.sh.

# Require current image metadata as well as installed certificates. The
# metadata prevents residual files inherited from an older source image from
# enabling runtime overrides after its trust policy is removed.
if ! grep -Eq '"trustedCaCertificates"[[:space:]]*:[[:space:]]*\[|"hostTrust"[[:space:]]*:' /opt/epar/image-manifest.json >/dev/null 2>&1; then
  return 0
fi
if ! compgen -G '/usr/local/share/ca-certificates/epar/*.crt' >/dev/null &&
   ! compgen -G '/usr/local/share/ca-certificates/epar-host/*.crt' >/dev/null; then
  return 0
fi

system_ca_bundle='/etc/ssl/certs/ca-certificates.crt'

# Respect explicit source-image or operator settings while giving common
# language ecosystems the Ubuntu bundle that includes EPAR-installed roots.
if [[ -z "${NODE_EXTRA_CA_CERTS+x}" ]]; then
  NODE_EXTRA_CA_CERTS="${system_ca_bundle}"
fi
if [[ -z "${REQUESTS_CA_BUNDLE+x}" ]]; then
  REQUESTS_CA_BUNDLE="${system_ca_bundle}"
fi
if [[ -z "${PIP_CERT+x}" ]]; then
  PIP_CERT="${system_ca_bundle}"
fi
export NODE_EXTRA_CA_CERTS REQUESTS_CA_BUNDLE PIP_CERT
