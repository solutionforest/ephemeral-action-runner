#!/usr/bin/env bash
set -euo pipefail

runner_dir="${EPAR_RUNNER_WORK_DIR:-/opt/actions-runner}"
pid_file="${EPAR_RUNNER_PID_FILE:-/var/run/actions-runner.pid}"
pid_start_file="${EPAR_RUNNER_PID_START_FILE:-${pid_file}.start}"
log_file="${EPAR_RUNNER_LOG_FILE:-/var/log/actions-runner/run.log}"
startup_check_seconds="${EPAR_RUNNER_STARTUP_CHECK_SECONDS:-1}"

process_start_time() {
  local pid="$1"
  local stat_line stat_fields
  local -a fields
  stat_line="$(cat "/proc/${pid}/stat" 2>/dev/null)" || return 1
  [[ "${stat_line}" == *") "* ]] || return 1
  stat_fields="${stat_line##*) }"
  read -r -a fields <<<"${stat_fields}"
  [[ "${fields[19]:-}" =~ ^[0-9]+$ ]] || return 1
  printf '%s\n' "${fields[19]}"
}

install -d -m 0755 -o agent -g agent "$(dirname "${log_file}")"
old_pid="$(cat "${pid_file}" 2>/dev/null || true)"
if [[ "${old_pid}" =~ ^[1-9][0-9]*$ ]] && kill -0 "${old_pid}" >/dev/null 2>&1; then
  echo "actions-runner is already running as PID ${old_pid}" >&2
  exit 1
fi
rm -f "${pid_file}" "${pid_start_file}"

[[ -s /opt/epar/host-trust-generation.json ]]
[[ -x /opt/epar/check-host-trust-generation.sh ]]
runner_environment=(
  "EPAR_RUNNER_WORK_DIR=${runner_dir}"
  "PATH=/opt/epar/hook-bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
  "ACTIONS_RUNNER_HOOK_JOB_STARTED=/opt/epar/check-host-trust-generation.sh"
)
sudo -u agent -H env "${runner_environment[@]}" /bin/bash -c 'cd "$1" || exit 1; nohup ./run.sh >>"$2" 2>&1 </dev/null & printf "%s\n" "$!"' bash "${runner_dir}" "${log_file}" >"${pid_file}"
sleep "${startup_check_seconds}"
pid="$(cat "${pid_file}" 2>/dev/null || true)"
if [[ ! "${pid}" =~ ^[1-9][0-9]*$ ]] || ! kill -0 "${pid}" >/dev/null 2>&1; then
  echo "actions-runner listener did not remain running" >&2
  exit 1
fi
process_cwd="$(readlink -f "/proc/${pid}/cwd" 2>/dev/null || true)"
expected_cwd="$(readlink -f "${runner_dir}")"
if [[ "${process_cwd}" != "${expected_cwd}" ]]; then
  echo "actions-runner PID ${pid} has unexpected working directory ${process_cwd:-<unavailable>}" >&2
  exit 1
fi
process_start_time "${pid}" >"${pid_start_file}"
printf '%s\n' "${pid}"
