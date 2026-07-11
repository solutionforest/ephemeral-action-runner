#!/usr/bin/env bash
set -euo pipefail

install -d /var/log/actions-runner
chown runner:runner /var/log/actions-runner
unit="${EPAR_RUNNER_SYSTEMD_UNIT:-actions-runner.service}"
unit_base="${unit%.service}"
pid_file="${EPAR_RUNNER_PID_FILE:-/var/run/actions-runner.pid}"
pid_start_file="${EPAR_RUNNER_PID_START_FILE:-${pid_file}.start}"
log_file="${EPAR_RUNNER_LOG_FILE:-/var/log/actions-runner/run.log}"
runner_work_dir="${EPAR_RUNNER_WORK_DIR:-/opt/actions-runner}"
startup_check_seconds="${EPAR_RUNNER_STARTUP_CHECK_SECONDS:-1}"

fail_startup() {
  echo "actions-runner startup validation failed: $*" >&2
  if [[ -f "${log_file}" ]]; then
    echo "--- ${log_file} (last 80 lines) ---" >&2
    tail -n 80 "${log_file}" >&2 || true
  else
    echo "runner log is not available at ${log_file}" >&2
  fi
  exit 1
}

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

record_pid_start() {
  local pid="$1"
  local start_time
  start_time="$(process_start_time "${pid}")" || fail_startup "cannot read start time for process ${pid}"
  if ! printf '%s\n' "${start_time}" >"${pid_start_file}"; then
    fail_startup "cannot write process start marker ${pid_start_file}"
  fi
}

validate_pid() {
  local pid="$1"
  local state
  local process_cwd
  local expected_cwd
  if [[ ! "${pid}" =~ ^[1-9][0-9]*$ ]]; then
    fail_startup "invalid PID ${pid:-<empty>}"
  fi
  if ! kill -0 "${pid}" >/dev/null 2>&1; then
    fail_startup "process ${pid} is not running"
  fi
  state="$(ps -p "${pid}" -o stat= 2>/dev/null | tr -d '[:space:]' || true)"
  if [[ -z "${state}" ]]; then
    fail_startup "process ${pid} has no process state"
  fi
  if [[ "${state}" == Z* ]]; then
    fail_startup "process ${pid} is a zombie (state=${state})"
  fi
  process_cwd="$(readlink -f "/proc/${pid}/cwd" 2>/dev/null || true)"
  expected_cwd="$(readlink -f "${runner_work_dir}" 2>/dev/null || true)"
  if [[ -z "${expected_cwd}" ]]; then
    fail_startup "expected runner work directory is unavailable: ${runner_work_dir}"
  fi
  if [[ "${process_cwd}" != "${expected_cwd}" ]]; then
    fail_startup "process ${pid} cwd ${process_cwd:-<unavailable>} does not match runner work directory ${expected_cwd}"
  fi
}

if [[ "${EPAR_DISABLE_SYSTEMD:-}" != "1" ]] && command -v systemctl >/dev/null 2>&1 && { [[ "${EPAR_FORCE_SYSTEMD:-}" == "1" ]] || [[ "$(ps -p 1 -o comm= 2>/dev/null || true)" == "systemd" ]]; }; then
  rm -f "${pid_start_file}"
  systemctl stop "${unit}" >/dev/null 2>&1 || true
  systemctl reset-failed "${unit}" >/dev/null 2>&1 || true
  systemd-run \
    --unit="${unit_base}" \
    --description="GitHub Actions ephemeral runner" \
    --property=User=runner \
    --property=Group=runner \
    "--setenv=EPAR_RUNNER_WORK_DIR=${runner_work_dir}" \
    --property=WorkingDirectory="${runner_work_dir}" \
    --property=StandardOutput="append:${log_file}" \
    --property=StandardError="append:${log_file}" \
    /opt/epar/start-runner-with-env.sh
  sleep "${startup_check_seconds}"
  if ! systemctl is-active --quiet "${unit}"; then
    fail_startup "systemd unit ${unit} is not active"
  fi
  main_pid="$(systemctl show "${unit}" --property=MainPID --value 2>/dev/null || true)"
  validate_pid "${main_pid}"
  printf '%s\n' "${main_pid}" >"${pid_file}"
  record_pid_start "${main_pid}"
else
  old_pid="$(cat "${pid_file}" 2>/dev/null || true)"
  if [[ -n "${old_pid}" ]] && kill -0 "${old_pid}" >/dev/null 2>&1; then
    kill "${old_pid}" >/dev/null 2>&1 || true
  fi
  rm -f "${pid_start_file}"
  sudo -u runner -H env "EPAR_RUNNER_WORK_DIR=${runner_work_dir}" \
    bash -c 'nohup /opt/epar/start-runner-with-env.sh >>"$1" 2>&1 & echo $!' bash "${log_file}" >"${pid_file}"
  sleep "${startup_check_seconds}"
  pid="$(cat "${pid_file}" 2>/dev/null || true)"
  validate_pid "${pid}"
  record_pid_start "${pid}"
fi
cat "${pid_file}"
