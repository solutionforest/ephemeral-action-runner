#!/usr/bin/env bash
set -euo pipefail

if [[ "$(id -u)" != "1000" || "$(id -g)" != "1000" || "${HOME}" != "/home/agent" ]]; then
  echo "EPAR Docker Sandboxes template: agent identity contract is not satisfied" >&2
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
