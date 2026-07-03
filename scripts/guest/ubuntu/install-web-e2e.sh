#!/usr/bin/env bash
set -euo pipefail

UPSTREAM_DIR="${UPSTREAM_DIR:-/opt/epar/upstream/runner-images}"
ARCH="$(dpkg --print-architecture)"

export DEBIAN_FRONTEND=noninteractive
export HELPER_SCRIPTS="${UPSTREAM_DIR}/images/ubuntu/scripts/helpers"
export INSTALLER_SCRIPT_FOLDER="/opt/epar"
export IMAGE_OS="${IMAGE_OS:-ubuntu24}"
export IMAGE_VERSION="${IMAGE_VERSION:-epar}"
export TOOLSET_JSON="${TOOLSET_JSON:-/opt/epar/toolset.json}"

bash /opt/epar/install-docker-browser.sh "${UPSTREAM_DIR}"

if [[ ! -f "${TOOLSET_JSON}" ]]; then
  install -d "$(dirname "${TOOLSET_JSON}")"
  if [[ "${ARCH}" == "arm64" && -f "${UPSTREAM_DIR}/images/ubuntu/toolsets/toolset-2404-arm64.json" ]]; then
    cp "${UPSTREAM_DIR}/images/ubuntu/toolsets/toolset-2404-arm64.json" "${TOOLSET_JSON}"
  elif [[ -f "${UPSTREAM_DIR}/images/ubuntu/toolsets/toolset-2404.json" ]]; then
    cp "${UPSTREAM_DIR}/images/ubuntu/toolsets/toolset-2404.json" "${TOOLSET_JSON}"
  else
    echo "Missing Ubuntu 24.04 runner-images toolset JSON" >&2
    exit 1
  fi
fi

apt-get update
apt-get install -y --no-install-recommends mysql-client rsync zip

bash "${UPSTREAM_DIR}/images/ubuntu/scripts/build/install-nodejs.sh"

install -d /opt/epar/features
touch /opt/epar/features/web-e2e
bash /opt/epar/validate-web-e2e.sh
