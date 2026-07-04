#!/usr/bin/env bash
set -euo pipefail

RUNNER_VERSION="${1:-latest}"
ARCH="$(uname -m)"
case "${ARCH}" in
  aarch64|arm64) RUNNER_ARCH="arm64" ;;
  x86_64|amd64) RUNNER_ARCH="x64" ;;
  *) echo "Unsupported runner architecture ${ARCH}" >&2; exit 1 ;;
esac

export DEBIAN_FRONTEND=noninteractive
export NEEDRESTART_MODE=l
export NEEDRESTART_SUSPEND=1
bash /opt/epar/wait-apt-ready.sh
apt-get update
apt-get install -y --no-install-recommends ca-certificates curl jq sudo tar

if [[ "${RUNNER_VERSION}" == "latest" ]]; then
  RUNNER_VERSION="$(curl -fsSL https://api.github.com/repos/actions/runner/releases/latest | jq -r '.tag_name' | sed 's/^v//')"
fi

id -u runner >/dev/null 2>&1 || useradd --create-home --shell /bin/bash runner
usermod -aG docker runner 2>/dev/null || true

install -d -o runner -g runner /opt/actions-runner
cd /opt/actions-runner
RUNNER_TGZ="actions-runner-linux-${RUNNER_ARCH}-${RUNNER_VERSION}.tar.gz"
curl -fL "https://github.com/actions/runner/releases/download/v${RUNNER_VERSION}/${RUNNER_TGZ}" -o "/tmp/${RUNNER_TGZ}"
tar xzf "/tmp/${RUNNER_TGZ}"
chown -R runner:runner /opt/actions-runner

./bin/installdependencies.sh

install -d /var/log/actions-runner
chown runner:runner /var/log/actions-runner
