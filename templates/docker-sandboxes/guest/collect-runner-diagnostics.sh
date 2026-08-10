#!/usr/bin/env bash
set -u

pid_file="${EPAR_RUNNER_PID_FILE:-/var/run/actions-runner.pid}"

echo "=== EPAR Docker Sandboxes runner diagnostics ==="
pid="$(cat "${pid_file}" 2>/dev/null || true)"
if [[ ! "${pid}" =~ ^[1-9][0-9]*$ ]]; then
  pid=""
fi
echo "runner_pid=${pid:-<missing>}"
if [[ -n "${pid}" ]]; then
  ps -p "${pid}" -o pid=,ppid=,stat=,etime= 2>/dev/null || echo "runner_process=<unavailable>"
fi
echo "dockerd_processes=$(pgrep -x dockerd 2>/dev/null | wc -l | tr -d '[:space:]')"
docker info --format 'docker_server={{.ServerVersion}} driver={{.Driver}}' 2>/dev/null || echo "docker_server=<unavailable> driver=<unavailable>"
echo "=== end diagnostics ==="
