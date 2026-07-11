#!/usr/bin/env bash
set -euo pipefail

unit="${EPAR_RUNNER_SYSTEMD_UNIT:-actions-runner.service}"
pid_file="${EPAR_RUNNER_PID_FILE:-/var/run/actions-runner.pid}"
pid_start_file="${EPAR_RUNNER_PID_START_FILE:-${pid_file}.start}"
runner_work_dir="${EPAR_RUNNER_WORK_DIR:-/opt/actions-runner}"

process_start_time() {
  local pid="$1"
  local stat_line
  local stat_fields
  local start_time
  local -a fields
  stat_line="$(cat "/proc/${pid}/stat" 2>/dev/null)" || return 1
  [[ "${stat_line}" == *") "* ]] || return 1
  stat_fields="${stat_line##*) }"
  read -r -a fields <<<"${stat_fields}"
  start_time="${fields[19]:-}"
  [[ "${start_time}" =~ ^[0-9]+$ ]] || return 1
  printf '%s\n' "${start_time}"
}

validate_pid() {
  local pid="$1"
  local state
  local process_cwd
  local expected_cwd
  local stored_start_time
  local current_start_time

  if [[ ! "${pid}" =~ ^[1-9][0-9]*$ ]]; then
    echo "actions-runner PID is invalid: ${pid:-<empty>}" >&2
    return 1
  fi
  if ! kill -0 "${pid}" >/dev/null 2>&1; then
    echo "actions-runner process ${pid} is not running" >&2
    return 1
  fi
  state="$(ps -p "${pid}" -o stat= 2>/dev/null | tr -d '[:space:]' || true)"
  if [[ -z "${state}" ]]; then
    echo "actions-runner process ${pid} has no process state" >&2
    return 1
  fi
  if [[ "${state}" == Z* ]]; then
    echo "actions-runner process ${pid} is a zombie (state=${state})" >&2
    return 1
  fi
  process_cwd="$(readlink -f "/proc/${pid}/cwd" 2>/dev/null || true)"
  expected_cwd="$(readlink -f "${runner_work_dir}" 2>/dev/null || true)"
  if [[ -z "${expected_cwd}" ]]; then
    echo "actions-runner expected work directory is unavailable: ${runner_work_dir}" >&2
    return 1
  fi
  if [[ "${process_cwd}" != "${expected_cwd}" ]]; then
    echo "actions-runner process ${pid} cwd ${process_cwd:-<unavailable>} does not match runner work directory ${expected_cwd}" >&2
    return 1
  fi
  stored_start_time="$(cat "${pid_start_file}" 2>/dev/null || true)"
  if [[ ! "${stored_start_time}" =~ ^[0-9]+$ ]]; then
    echo "actions-runner process start marker is missing or invalid: ${pid_start_file}" >&2
    return 1
  fi
  current_start_time="$(process_start_time "${pid}" || true)"
  if [[ ! "${current_start_time}" =~ ^[0-9]+$ ]]; then
    echo "actions-runner process ${pid} start time is unavailable" >&2
    return 1
  fi
  if [[ "${current_start_time}" != "${stored_start_time}" ]]; then
    echo "actions-runner process ${pid} start time ${current_start_time} does not match stored start time ${stored_start_time}" >&2
    return 1
  fi
}

if [[ "${EPAR_DISABLE_SYSTEMD:-}" != "1" ]] && command -v systemctl >/dev/null 2>&1 && { [[ "${EPAR_FORCE_SYSTEMD:-}" == "1" ]] || [[ "$(ps -p 1 -o comm= 2>/dev/null || true)" == "systemd" ]]; }; then
  systemctl is-active --quiet "${unit}"
  main_pid="$(systemctl show "${unit}" --property=MainPID --value 2>/dev/null || true)"
  validate_pid "${main_pid}"
  exit 0
fi

pid="$(cat "${pid_file}" 2>/dev/null || true)"
if [[ -z "${pid}" ]]; then
  echo "actions-runner pid file is missing" >&2
  exit 1
fi

validate_pid "${pid}"
