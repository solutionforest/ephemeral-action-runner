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
