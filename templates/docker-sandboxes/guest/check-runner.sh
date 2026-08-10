#!/usr/bin/env bash
set -euo pipefail

runner_dir="${EPAR_RUNNER_WORK_DIR:-/opt/actions-runner}"
pid_file="${EPAR_RUNNER_PID_FILE:-/var/run/actions-runner.pid}"
pid_start_file="${EPAR_RUNNER_PID_START_FILE:-${pid_file}.start}"
pid="$(cat "${pid_file}" 2>/dev/null || true)"
stored_start="$(cat "${pid_start_file}" 2>/dev/null || true)"

if [[ ! "${pid}" =~ ^[1-9][0-9]*$ ]] || ! kill -0 "${pid}" >/dev/null 2>&1; then
  echo "actions-runner process is not running" >&2
  exit 1
fi
state="$(ps -p "${pid}" -o stat= 2>/dev/null | tr -d '[:space:]')"
if [[ -z "${state}" || "${state}" == Z* ]]; then
  echo "actions-runner process ${pid} has invalid state ${state:-<missing>}" >&2
  exit 1
fi
if [[ "$(readlink -f "/proc/${pid}/cwd" 2>/dev/null || true)" != "$(readlink -f "${runner_dir}")" ]]; then
  echo "actions-runner process ${pid} has an unexpected working directory" >&2
  exit 1
fi
stat_line="$(cat "/proc/${pid}/stat")"
stat_fields="${stat_line##*) }"
read -r -a fields <<<"${stat_fields}"
current_start="${fields[19]:-}"
if [[ ! "${stored_start}" =~ ^[0-9]+$ || "${current_start}" != "${stored_start}" ]]; then
  echo "actions-runner process ${pid} does not match its stored start marker" >&2
  exit 1
fi
