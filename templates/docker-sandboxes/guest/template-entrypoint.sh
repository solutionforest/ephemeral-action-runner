#!/usr/bin/env bash
set -euo pipefail

if [[ "$(id -u)" != "1000" || "$(id -g)" != "1000" || "${HOME:-}" != "/home/agent" || "${USER:-}" != "agent" || "${LOGNAME:-}" != "agent" ]]; then
  echo "EPAR Docker Sandboxes template: agent identity contract is not satisfied" >&2
  exit 1
fi
if [[ -n "${SSH_AUTH_SOCK:-}" || -n "${SSH_AUTH_SOCK_GATEWAY:-}" || -n "${SSH_AGENT_PID:-}" || -e /run/ssh-agent.sock || -L /run/ssh-agent.sock ]]; then
  echo "EPAR Docker Sandboxes template: host SSH-agent forwarding is not permitted; restart the Sandboxes daemon without SSH-agent variables" >&2
  exit 1
fi
unset SSH_AUTH_SOCK SSH_AUTH_SOCK_GATEWAY SSH_AGENT_PID
unset http_proxy https_proxy no_proxy HTTP_PROXY HTTPS_PROXY NO_PROXY
sudo -n /opt/epar/install-trusted-ca-certificates.sh
sudo -n install -d -m 0755 -o root -g root /run/epar /var/log/epar
bridge_started=false
bridge_pid=""
cleanup_bridge_on_failure() {
  status="$?"
  trap - EXIT
  set +e
  if [[ "${status}" != "0" && "${bridge_started}" == "true" ]]; then
    mapfile -t cleanup_bridge_pids < <(sudo -n pgrep -f -x '/opt/epar/epar-egress-bridge' || true)
    if [[ "${#cleanup_bridge_pids[@]}" == "1" ]] && [[ "$(sudo -n readlink -f "/proc/${cleanup_bridge_pids[0]}/exe" 2>/dev/null || true)" == "/opt/epar/epar-egress-bridge" ]]; then
      sudo -n kill -TERM "${cleanup_bridge_pids[0]}" >/dev/null 2>&1 || true
    fi
    sudo -n rm -f /run/epar/egress-bridge.pid
  fi
  exit "${status}"
}
trap cleanup_bridge_on_failure EXIT
mapfile -t existing_bridge_pids < <(sudo -n pgrep -f -x '/opt/epar/epar-egress-bridge' || true)
if [[ "${#existing_bridge_pids[@]}" != "0" ]]; then
  echo "EPAR Docker Sandboxes template: egress bridge is already running" >&2
  exit 1
fi
sudo -n rm -f /run/epar/egress-bridge.pid
sudo -n /bin/bash -c 'exec env -i HOME=/root PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin LANG=C.UTF-8 /opt/epar/epar-egress-bridge >>/var/log/epar/egress-bridge.log 2>&1 </dev/null' &
bridge_started=true
for _ in $(seq 1 30); do
  mapfile -t bridge_pids < <(sudo -n pgrep -f -x '/opt/epar/epar-egress-bridge' || true)
  if [[ "${#bridge_pids[@]}" == "1" ]]; then
    break
  fi
  sleep 1
done
if [[ "${#bridge_pids[@]}" != "1" ]]; then
  echo "EPAR Docker Sandboxes template: expected exactly one egress bridge process" >&2
  exit 1
fi
bridge_pid="${bridge_pids[0]}"
if [[ "$(sudo -n readlink -f "/proc/${bridge_pid}/exe" 2>/dev/null || true)" != "/opt/epar/epar-egress-bridge" ]]; then
  echo "EPAR Docker Sandboxes template: egress bridge has an unexpected executable" >&2
  exit 1
fi
printf '%s\n' "${bridge_pid}" | sudo -n tee /run/epar/egress-bridge.pid >/dev/null
sudo -n chown root:root /run/epar/egress-bridge.pid
sudo -n chmod 0444 /run/epar/egress-bridge.pid
[[ "$(sudo -n cat /run/epar/egress-bridge.pid)" == "${bridge_pid}" ]]
sudo -n install -d -m 0700 -o agent -g agent /run/user/1000
if [[ "${XDG_CONFIG_HOME:-}" != "/home/agent/.config" || "${XDG_CACHE_HOME:-}" != "/home/agent/.cache" || "${XDG_DATA_HOME:-}" != "/home/agent/.local/share" || "${XDG_STATE_HOME:-}" != "/home/agent/.local/state" || "${XDG_RUNTIME_DIR:-}" != "/run/user/1000" || "${DOCKER_CONFIG:-}" != "/home/agent/.docker" ]]; then
  echo "EPAR Docker Sandboxes template: agent configuration-path contract is not satisfied" >&2
  exit 1
fi
if [[ -e /home/runner/.docker || -L /home/runner/.docker ]]; then
  echo "EPAR Docker Sandboxes template: stale Docker client configuration exists under /home/runner" >&2
  exit 1
fi

if [[ "${EPAR_SKIP_DOCKER_READY_CHECK:-0}" != "1" ]]; then
  echo "EPAR Docker Sandboxes template: waiting for the sandbox-private Docker daemon"
  for attempt in $(seq 1 120); do
    daemon_count="$( (pgrep -x dockerd 2>/dev/null || true) | wc -l | tr -d '[:space:]')"
    if [[ "${daemon_count}" == "1" ]] && docker info >/dev/null 2>&1; then
      if [[ "$(docker info --format '{{.NoProxy}}')" != "*" ]]; then
        echo "EPAR Docker Sandboxes template: sandbox-private Docker daemon is not using policy-enforced transparent egress" >&2
        exit 1
      fi
      echo "EPAR Docker Sandboxes template: one sandbox-private Docker daemon is ready"
      break
    fi
    if [[ "${attempt}" == "120" ]]; then
      echo "EPAR Docker Sandboxes template: expected exactly one ready dockerd process; observed ${daemon_count}" >&2
      exit 1
    fi
    sleep 1
  done
fi

if [[ "$#" == "0" ]]; then
  set -- sleep infinity
fi
exec "$@"
