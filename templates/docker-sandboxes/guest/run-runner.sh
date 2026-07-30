#!/usr/bin/env bash
set -euo pipefail

runner_dir="${EPAR_RUNNER_WORK_DIR:-/opt/actions-runner}"
tool_cache="${EPAR_RUNNER_TOOL_CACHE:-${runner_dir}/_work/_tool}"
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

install -d -m 0755 -o agent -g agent "$(dirname "${log_file}")" "${tool_cache}" "${tool_cache}/dotnet"
old_pid="$(cat "${pid_file}" 2>/dev/null || true)"
if [[ "${old_pid}" =~ ^[1-9][0-9]*$ ]] && kill -0 "${old_pid}" >/dev/null 2>&1; then
  echo "actions-runner is already running as PID ${old_pid}" >&2
  exit 1
fi
rm -f "${pid_file}" "${pid_start_file}"

[[ -s /opt/epar/host-trust-generation.json ]]
[[ -x /opt/epar/check-host-trust-generation.sh ]]
if [[ ! -x /usr/bin/python3 ]]; then
  echo "EPAR runner trust policy: python3 is required" >&2
  exit 1
fi
trust_mode="$(/usr/bin/env -i PATH=/usr/bin:/bin LANG=C.UTF-8 /usr/bin/python3 -I -S - /opt/epar/host-trust-generation.json <<'PY'
import json
import sys

try:
    with open(sys.argv[1], "r", encoding="utf-8") as handle:
        marker = json.load(handle)
except Exception as exc:
    raise SystemExit(f"EPAR runner trust policy: invalid image marker: {exc}")
if not isinstance(marker, dict) or marker.get("schemaVersion") != 1:
    raise SystemExit("EPAR runner trust policy: unsupported image marker schema")
mode = marker.get("mode")
if mode == "disabled":
    if marker.get("generation") != "disabled" or marker.get("hostOS") not in ("", None) or marker.get("scopes") != [] or marker.get("certificateCount") != 0:
        raise SystemExit("EPAR runner trust policy: malformed disabled policy")
elif mode == "overlay":
    if not isinstance(marker.get("generation"), str) or not marker["generation"] or not isinstance(marker.get("hostOS"), str) or not marker["hostOS"] or not isinstance(marker.get("scopes"), list) or not marker["scopes"] or not isinstance(marker.get("certificateCount"), int) or marker["certificateCount"] < 1:
        raise SystemExit("EPAR runner trust policy: malformed overlay policy")
else:
    raise SystemExit(f"EPAR runner trust policy: unknown mode {mode!r}")
print(mode)
PY
)"
runner_environment=(
  "EPAR_RUNNER_WORK_DIR=${runner_dir}"
  "RUNNER_TOOL_CACHE=${tool_cache}"
  "AGENT_TOOLSDIRECTORY=${tool_cache}"
  "DOTNET_INSTALL_DIR=${tool_cache}/dotnet"
  "PATH=/opt/epar/hook-bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
)
if [[ "${trust_mode}" == "overlay" ]]; then
  runner_environment+=("ACTIONS_RUNNER_HOOK_JOB_STARTED=/opt/epar/check-host-trust-generation.sh")
fi
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
