#!/usr/bin/env bash
set -euo pipefail

install -d /var/log/actions-runner
chown runner:runner /var/log/actions-runner
unit="${EPAR_RUNNER_SYSTEMD_UNIT:-actions-runner.service}"
unit_base="${unit%.service}"
pid_file="${EPAR_RUNNER_PID_FILE:-/var/run/actions-runner.pid}"

if command -v systemctl >/dev/null 2>&1 && [[ "$(ps -p 1 -o comm= 2>/dev/null || true)" == "systemd" ]]; then
  systemctl stop "${unit}" >/dev/null 2>&1 || true
  systemctl reset-failed "${unit}" >/dev/null 2>&1 || true
  systemd-run \
    --unit="${unit_base}" \
    --description="GitHub Actions ephemeral runner" \
    --property=User=runner \
    --property=Group=runner \
    --property=WorkingDirectory=/opt/actions-runner \
    --property=StandardOutput=append:/var/log/actions-runner/run.log \
    --property=StandardError=append:/var/log/actions-runner/run.log \
    /opt/actions-runner/run.sh
  sleep 1
  systemctl show "${unit}" --property=MainPID --value >"${pid_file}"
else
  old_pid="$(cat "${pid_file}" 2>/dev/null || true)"
  if [[ -n "${old_pid}" ]] && kill -0 "${old_pid}" >/dev/null 2>&1; then
    kill "${old_pid}" >/dev/null 2>&1 || true
  fi
  sudo -u runner -H bash -lc 'cd /opt/actions-runner && nohup ./run.sh >>/var/log/actions-runner/run.log 2>&1 & echo $!' >"${pid_file}"
fi
cat "${pid_file}"
