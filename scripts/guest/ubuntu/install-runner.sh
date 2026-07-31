#!/usr/bin/env bash
set -euo pipefail

RUNNER_VERSION="${1:-}"
RUNNER_PACKAGE="${2:-}"
RUNNER_SHA256="${3:-}"
if [[ -z "${RUNNER_VERSION}" || -z "${RUNNER_PACKAGE}" || ! -f "${RUNNER_PACKAGE}" || ! "${RUNNER_SHA256}" =~ ^sha256:[0-9a-f]{64}$ ]]; then
  echo "Usage: install-runner.sh <exact-version> <verified-package-path> <sha256:digest>" >&2
  exit 1
fi
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
apt-get install -y --no-install-recommends ca-certificates sudo tar

id -u runner >/dev/null 2>&1 || useradd --create-home --shell /bin/bash runner
usermod -aG docker runner 2>/dev/null || true

install -d -o runner -g runner /opt/actions-runner
cd /opt/actions-runner
echo "${RUNNER_SHA256#sha256:}  ${RUNNER_PACKAGE}" | sha256sum --check -
tar xzf "${RUNNER_PACKAGE}"
rm -f "${RUNNER_PACKAGE}"
chown -R runner:runner /opt/actions-runner

INSTALLED_RUNNER_VERSION="$(sudo -u runner -H ./bin/Runner.Listener --version | tr -d '\r' | tail -n 1)"
if [[ "${INSTALLED_RUNNER_VERSION}" != "${RUNNER_VERSION}" ]]; then
  echo "Actions runner package version ${INSTALLED_RUNNER_VERSION:-<empty>} does not match expected version ${RUNNER_VERSION}" >&2
  exit 1
fi

./bin/installdependencies.sh

install -d /var/log/actions-runner
chown runner:runner /var/log/actions-runner
