#!/usr/bin/env bash
set -euo pipefail

# Example EPAR image customization script.
#
# Add only non-secret, reusable machine dependencies here. Project packages,
# language dependency caches, credentials, and private keys should stay in the
# workflow or in GitHub secrets, not in the runner image.

export DEBIAN_FRONTEND=noninteractive

apt-get update
apt-get install -y --no-install-recommends \
  make \
  pkg-config \
  shellcheck

make --version
pkg-config --version
shellcheck --version
