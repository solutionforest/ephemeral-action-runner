#!/usr/bin/env bash
set -euo pipefail

unit="${EPAR_RUNNER_SYSTEMD_UNIT:-actions-runner.service}"
pid_file="${EPAR_RUNNER_PID_FILE:-/var/run/actions-runner.pid}"

if [[ "${EPAR_DISABLE_SYSTEMD:-}" != "1" ]] && command -v systemctl >/dev/null 2>&1 && { [[ "${EPAR_FORCE_SYSTEMD:-}" == "1" ]] || [[ "$(ps -p 1 -o comm= 2>/dev/null || true)" == "systemd" ]]; }; then
  systemctl is-active --quiet "${unit}"
  exit 0
fi

pid="$(cat "${pid_file}" 2>/dev/null || true)"
if [[ -z "${pid}" ]]; then
  echo "actions-runner pid file is missing" >&2
  exit 1
fi

if ! kill -0 "${pid}" >/dev/null 2>&1; then
  echo "actions-runner process ${pid} is not running" >&2
  exit 1
fi
