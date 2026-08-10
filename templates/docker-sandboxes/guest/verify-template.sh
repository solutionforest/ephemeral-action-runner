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
sudo -n jq -e '((keys - ["proxies", "registry-mirrors"]) | length == 0) and ((has("registry-mirrors") | not) or ((."registry-mirrors" | type) == "array" and all(."registry-mirrors"[]; type == "string")))' /etc/docker/daemon.json >/dev/null
id -nG agent | tr ' ' '\n' | grep -Fx docker >/dev/null
sudo -u agent -H sudo -n true
[[ "$(pgrep -x dockerd | wc -l | tr -d '[:space:]')" == "1" ]]
docker info >/dev/null
marker_mode="$(jq -er '.mode | strings' /opt/epar/host-trust-generation.json)"
marker_host_os="$(jq -er '.hostOS | strings | ascii_downcase' /opt/epar/host-trust-generation.json)"
relay_required=false
if [[ "${marker_mode}" == "overlay" && "${marker_host_os}" == "windows" ]]; then
  relay_required=true
fi
relay_active=false
if sudo -n test -e /run/epar/egress-relay-active; then
  relay_active=true
fi
if [[ "${relay_required}" == "true" && "${relay_active}" != "true" ]]; then
  echo "Windows host-trust overlay requires the authenticated relay" >&2
  exit 1
fi
if [[ "${relay_active}" == "true" ]]; then
  sudo -n test -f /run/epar/egress-relay-active
  sudo -n test ! -L /run/epar/egress-relay-active
  [[ "$(sudo -n stat -c '%U:%G:%a' /run/epar/egress-relay-active)" == "root:root:444" ]]
  sudo -n test -f /run/epar/egress-relay.json
  sudo -n test ! -L /run/epar/egress-relay.json
  [[ "$(sudo -n stat -c '%U:%G:%a' /run/epar/egress-relay.json)" == "root:root:600" ]]
  sudo -n test -s /run/epar/egress-relay-ca.crt
  sudo -n test ! -L /run/epar/egress-relay-ca.crt
  [[ "$(sudo -n stat -c '%U:%G:%a' /run/epar/egress-relay-ca.crt)" == "root:root:444" ]]
  sudo -n test -s /run/epar/egress-relay-ca.key
  sudo -n test ! -L /run/epar/egress-relay-ca.key
  [[ "$(sudo -n stat -c '%U:%G:%a' /run/epar/egress-relay-ca.key)" == "root:root:600" ]]
  sudo -n test -s /usr/local/share/ca-certificates/epar/epar-egress-relay.crt
  sudo -n cmp -s /run/epar/egress-relay-ca.crt /usr/local/share/ca-certificates/epar/epar-egress-relay.crt
  [[ -z "$(docker info --format '{{.HTTPProxy}}')" ]]
  [[ "$(docker info --format '{{.HTTPSProxy}}')" == "http://127.0.0.1:3129" ]]
  [[ "$(docker info --format '{{.NoProxy}}')" != "*" ]]
  [[ "$(curl --silent --show-error --output /dev/null --write-out '%{http_code}' --max-time 15 http://127.0.0.1:3129/health)" == "204" ]]
  dockerd_pid="$(pgrep -x dockerd)"
  [[ "${dockerd_pid}" =~ ^[1-9][0-9]*$ ]]
  sudo -n bash -c "tr '\\0' '\\n' </proc/${dockerd_pid}/environ | grep -Fx 'GODEBUG=tlsmlkem=0,tlssecpmlkem=0' >/dev/null"
else
  sudo -n jq -e '.proxies == {"http-proxy": "http://gateway.docker.internal:3128", "https-proxy": "http://gateway.docker.internal:3128", "no-proxy": "*"}' /etc/docker/daemon.json >/dev/null
  [[ "$(docker info --format '{{.NoProxy}}')" == "*" ]]
fi
sudo -n test -s /opt/epar/trust/ca-bundle.pem
sudo -n test ! -L /opt/epar/trust/ca-bundle.pem
[[ "$(sudo -n stat -c '%U:%G:%a' /opt/epar/trust/ca-bundle.pem)" == "root:root:444" ]]
[[ -x /opt/actions-runner/bin/Runner.Listener ]]
[[ -x /opt/epar/check-host-trust-generation.sh ]]
[[ -x /opt/epar/prepare-job-start.sh ]]
[[ -x /opt/epar/scrub-docker-auth.sh ]]
sudo -n test -x /opt/epar/configure-egress-relay.sh
sudo -n test ! -L /opt/epar/configure-egress-relay.sh
sudo -n test -x /opt/epar/epar-egress-bridge
sudo -n test ! -L /opt/epar/epar-egress-bridge
[[ "$(sudo -n stat -c '%U:%G:%a' /opt/epar/epar-egress-bridge)" == "root:root:555" ]]
[[ -x /opt/epar/hook-bin/bash ]]
sudo -n test -x /opt/epar/enable-architecture-emulation
sudo -n test ! -L /opt/epar/enable-architecture-emulation
sudo -n cmp -s /opt/epar/enable-architecture-emulation.sh /opt/epar/enable-architecture-emulation
[[ "$(sudo -n stat -c '%U:%G:%a' /opt/epar/enable-architecture-emulation)" == "root:root:555" ]]
sudo -n test -x /opt/epar/verify-native-architecture
sudo -n test ! -L /opt/epar/verify-native-architecture
sudo -n cmp -s /opt/epar/verify-native-architecture.sh /opt/epar/verify-native-architecture
[[ "$(sudo -n stat -c '%U:%G:%a' /opt/epar/verify-native-architecture)" == "root:root:555" ]]
[[ "$(PATH=/opt/epar/hook-bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin command -v bash)" == "/opt/epar/hook-bin/bash" ]]
[[ -x /usr/bin/python3 ]]
sudo -n test -x /opt/epar/emulation/binfmt
sudo -n test ! -L /opt/epar/emulation/binfmt
sudo -n bash -c '[[ "$(stat -c "%U:%G:%a" /opt/epar/emulation/binfmt)" == "root:root:555" ]]'
sudo -n bash -c 'shopt -s nullglob; qemu=(/opt/epar/emulation/qemu-*); (( ${#qemu[@]} > 0 )); for interpreter in "${qemu[@]}"; do [[ -f "${interpreter}" && ! -L "${interpreter}" && -x "${interpreter}" && "$(stat -c "%U:%G:%a" "${interpreter}")" == "root:root:555" ]]; done'
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
