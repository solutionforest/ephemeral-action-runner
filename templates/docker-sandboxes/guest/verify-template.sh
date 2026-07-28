#!/usr/bin/env bash
set -euo pipefail

cd /opt/epar
sha256sum --check helpers.sha256 >/dev/null

[[ "$(id -u agent)" == "1000" ]]
[[ "$(id -g agent)" == "1000" ]]
[[ "$(getent passwd agent | cut -d: -f6)" == "/home/agent" ]]
id -nG agent | tr ' ' '\n' | grep -Fx docker >/dev/null
sudo -u agent -H sudo -n true
[[ "$(pgrep -x dockerd | wc -l | tr -d '[:space:]')" == "1" ]]
docker info >/dev/null
[[ -x /opt/actions-runner/bin/Runner.Listener ]]
[[ -x /opt/epar/check-host-trust-generation.sh ]]
[[ -x /opt/epar/hook-bin/bash ]]
[[ "$(PATH=/opt/epar/hook-bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin command -v bash)" == "/opt/epar/hook-bin/bash" ]]
[[ -x /usr/bin/python3 ]]
[[ "$(sudo -u agent -H /opt/actions-runner/bin/Runner.Listener --version)" == "2.332.0" ]]
[[ "${EPAR_TEMPLATE_SBX_VERSION}" == "0.35.0" ]]
case "${EPAR_TEMPLATE_PLATFORM}" in
  linux/amd64)
    [[ "$(uname -m)" == "x86_64" ]]
    ;;
  linux/arm64)
    [[ "$(uname -m)" == "aarch64" ]]
    ;;
  *)
    echo "unsupported EPAR template platform: ${EPAR_TEMPLATE_PLATFORM}" >&2
    exit 1
    ;;
esac
