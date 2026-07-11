#!/usr/bin/env bash
set -u

unit="${EPAR_RUNNER_SYSTEMD_UNIT:-actions-runner.service}"
pid_file="${EPAR_RUNNER_PID_FILE:-/var/run/actions-runner.pid}"
pid_start_file="${EPAR_RUNNER_PID_START_FILE:-${pid_file}.start}"
runner_log="${EPAR_RUNNER_LOG_FILE:-/var/log/actions-runner/run.log}"
dockerd_log="${EPAR_DOCKERD_LOG_FILE:-/var/log/epar-dockerd.log}"
requested_tail_lines="${EPAR_DIAGNOSTIC_TAIL_LINES:-50}"
max_tail_lines=200
if [[ "${requested_tail_lines}" =~ ^[1-9][0-9]*$ ]]; then
  if (( ${#requested_tail_lines} > 3 )) || (( 10#${requested_tail_lines} > max_tail_lines )); then
    tail_lines="${max_tail_lines}"
  else
    tail_lines="$((10#${requested_tail_lines}))"
  fi
else
  tail_lines=50
fi

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

echo "=== EPAR runner readiness diagnostics ==="
echo "captured_at=$(date -u +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || true)"

echo "--- runner process state ---"
pid="$(cat "${pid_file}" 2>/dev/null || true)"
echo "pid_file=${pid_file} pid=${pid:-<missing>}"
stored_start_time="$(cat "${pid_start_file}" 2>/dev/null || true)"
current_start_time=""
if [[ -n "${pid}" ]]; then
  current_start_time="$(process_start_time "${pid}" || true)"
  ps -p "${pid}" -o pid=,ppid=,stat=,etime=,cmd= 2>&1 || true
fi
echo "pid_start_file=${pid_start_file} stored_start=${stored_start_time:-<missing>} current_start=${current_start_time:-<unavailable>}"
if command -v systemctl >/dev/null 2>&1; then
  echo "systemd_unit=${unit} active=$(systemctl is-active "${unit}" 2>/dev/null || true) main_pid=$(systemctl show "${unit}" --property=MainPID --value 2>/dev/null || true)"
fi

echo "--- ${runner_log} (last ${tail_lines} lines) ---"
tail -n "${tail_lines}" "${runner_log}" 2>&1 || true

latest_diag="$(ls -1t /opt/actions-runner/_diag/Runner_*.log 2>/dev/null | head -n 1 || true)"
echo "--- latest runner diagnostic: ${latest_diag:-<none>} (last ${tail_lines} lines) ---"
if [[ -n "${latest_diag}" ]]; then
  tail -n "${tail_lines}" "${latest_diag}" 2>&1 || true
fi

echo "--- ${dockerd_log} (last ${tail_lines} lines) ---"
tail -n "${tail_lines}" "${dockerd_log}" 2>&1 || true
echo "=== end EPAR runner readiness diagnostics ==="
