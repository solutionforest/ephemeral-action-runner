#!/usr/bin/env bash
set -euo pipefail

cd /opt/epar
sha256sum --check helpers.sha256 >/dev/null

[[ "$(id -u agent)" == "1000" ]]
[[ "$(id -g agent)" == "1000" ]]
[[ "$(getent passwd agent | cut -d: -f6)" == "/home/agent" ]]
[[ "${HOME:-}" == "/home/agent" ]]
[[ "${USER:-}" == "agent" ]]
[[ "${LOGNAME:-}" == "agent" ]]
[[ -z "${SSH_AUTH_SOCK:-}" ]]
[[ -z "${SSH_AUTH_SOCK_GATEWAY:-}" ]]
[[ -z "${SSH_AGENT_PID:-}" ]]
[[ ! -e /run/ssh-agent.sock && ! -L /run/ssh-agent.sock ]]
[[ "${XDG_CONFIG_HOME:-}" == "/home/agent/.config" ]]
[[ "${XDG_CACHE_HOME:-}" == "/home/agent/.cache" ]]
[[ "${XDG_DATA_HOME:-}" == "/home/agent/.local/share" ]]
[[ "${XDG_STATE_HOME:-}" == "/home/agent/.local/state" ]]
[[ "${XDG_RUNTIME_DIR:-}" == "/run/user/1000" ]]
[[ "${DOCKER_CONFIG:-}" == "/home/agent/.docker" ]]
for private_directory in /home/agent/.docker /home/agent/.config /home/agent/.cache /home/agent/.local /home/agent/.local/share /home/agent/.local/state /run/user/1000; do
  [[ "$(stat -c '%U:%G:%a' "${private_directory}")" == "agent:agent:700" ]]
done
[[ ! -e /home/agent/.docker/config.json && ! -L /home/agent/.docker/config.json ]]
if ! passwd_entries="$(getent passwd)" || [[ -z "${passwd_entries}" ]]; then
  echo "failed to enumerate template passwd homes" >&2
  exit 1
fi
while IFS=: read -r _ _ _ _ _ account_home _; do
  [[ -n "${account_home}" && "${account_home}" == /* ]]
  normalized_home="$(readlink -m -- "${account_home}")"
  if [[ "${normalized_home}" == "/" ]]; then
    sudo -n test ! -e /.docker
    sudo -n test ! -L /.docker
    sudo -n test ! -e /.dockercfg
    sudo -n test ! -L /.dockercfg
    continue
  fi
  sudo -n test ! -e "${normalized_home}/.dockercfg"
  sudo -n test ! -L "${normalized_home}/.dockercfg"
  if [[ "${normalized_home}" != "/home/agent" ]]; then
    sudo -n test ! -e "${normalized_home}/.docker"
    sudo -n test ! -L "${normalized_home}/.docker"
  fi
done <<<"${passwd_entries}"
[[ ! -e /home/runner/.docker && ! -L /home/runner/.docker ]]
[[ ! -e /home/runner/.dockercfg && ! -L /home/runner/.dockercfg ]]
sudo -n test -f /etc/docker/daemon.json
sudo -n test ! -L /etc/docker/daemon.json
[[ "$(sudo -n stat -c '%U:%G:%a' /etc/docker/daemon.json)" == "root:root:644" ]]
sudo -n jq -e '
  .proxies == {
    "http-proxy": "http://gateway.docker.internal:3128",
    "https-proxy": "http://gateway.docker.internal:3128",
    "no-proxy": "*"
  }
  and ((keys - ["proxies", "registry-mirrors"]) | length == 0)
  and ((has("registry-mirrors") | not) or ((."registry-mirrors" | type) == "array" and all(."registry-mirrors"[]; type == "string")))
' /etc/docker/daemon.json >/dev/null
id -nG agent | tr ' ' '\n' | grep -Fx docker >/dev/null
sudo -u agent -H sudo -n true
[[ "$(pgrep -x dockerd | wc -l | tr -d '[:space:]')" == "1" ]]
docker info >/dev/null
[[ "$(docker info --format '{{.NoProxy}}')" == "*" ]]
[[ -x /opt/actions-runner/bin/Runner.Listener ]]
[[ -x /opt/epar/check-host-trust-generation.sh ]]
[[ -x /opt/epar/hook-bin/bash ]]
[[ "$(PATH=/opt/epar/hook-bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin command -v bash)" == "/opt/epar/hook-bin/bash" ]]
[[ -x /usr/bin/python3 ]]
[[ -s /opt/epar/actions-runner-version ]]
[[ "$(sudo -u agent -H /opt/actions-runner/bin/Runner.Listener --version)" == "$(cat /opt/epar/actions-runner-version)" ]]
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
