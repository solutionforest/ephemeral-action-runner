#!/usr/bin/env bash
set -euo pipefail
unset SSH_AUTH_SOCK SSH_AUTH_SOCK_GATEWAY SSH_AGENT_PID

runner_dir="${EPAR_RUNNER_WORK_DIR:-/opt/actions-runner}"
tool_cache="${EPAR_RUNNER_TOOL_CACHE:-${runner_dir}/_work/_tool}"
pid_file="${EPAR_RUNNER_PID_FILE:-/var/run/actions-runner.pid}"
pid_start_file="${EPAR_RUNNER_PID_START_FILE:-${pid_file}.start}"
log_file="${EPAR_RUNNER_LOG_FILE:-/var/log/actions-runner/run.log}"
startup_check_seconds="${EPAR_RUNNER_STARTUP_CHECK_SECONDS:-1}"
agent_home="/home/agent"
agent_runtime_dir="/run/user/1000"
sandbox_forward_proxy="http://gateway.docker.internal:3128"

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
install -d -m 0700 -o agent -g agent \
  "${agent_home}/.docker" \
  "${agent_home}/.config" \
  "${agent_home}/.cache" \
  "${agent_home}/.local" \
  "${agent_home}/.local/share" \
  "${agent_home}/.local/state" \
  "${agent_runtime_dir}"
if [[ -e /home/runner/.docker || -L /home/runner/.docker ]]; then
  echo "refusing to start the runner with stale Docker client configuration under /home/runner" >&2
  exit 1
fi
old_pid="$(cat "${pid_file}" 2>/dev/null || true)"
if [[ "${old_pid}" =~ ^[1-9][0-9]*$ ]] && kill -0 "${old_pid}" >/dev/null 2>&1; then
  echo "actions-runner is already running as PID ${old_pid}" >&2
  exit 1
fi
rm -f "${pid_file}" "${pid_start_file}"

[[ -s /opt/epar/host-trust-generation.json ]]
[[ -s /opt/epar/trust/ca-bundle.pem && ! -L /opt/epar/trust/ca-bundle.pem ]]
[[ -x /opt/epar/check-host-trust-generation.sh ]]
[[ -x /opt/epar/prepare-job-start.sh ]]
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
  "HOME=${agent_home}"
  "USER=agent"
  "LOGNAME=agent"
  "XDG_CONFIG_HOME=${agent_home}/.config"
  "XDG_CACHE_HOME=${agent_home}/.cache"
  "XDG_DATA_HOME=${agent_home}/.local/share"
  "XDG_STATE_HOME=${agent_home}/.local/state"
  "XDG_RUNTIME_DIR=${agent_runtime_dir}"
  "DOCKER_CONFIG=${agent_home}/.docker"
  "EPAR_RUNNER_WORK_DIR=${runner_dir}"
  "RUNNER_TOOL_CACHE=${tool_cache}"
  "AGENT_TOOLSDIRECTORY=${tool_cache}"
  "DOTNET_INSTALL_DIR=${tool_cache}/dotnet"
  # The listener alone uses Docker Sandboxes' canonical forward proxy for
  # GitHub control-plane traffic. The job-start hook clears these variables
  # through GITHUB_ENV before workflow steps run.
  "HTTP_PROXY=${sandbox_forward_proxy}"
  "HTTPS_PROXY=${sandbox_forward_proxy}"
  "ALL_PROXY=${sandbox_forward_proxy}"
  "http_proxy=${sandbox_forward_proxy}"
  "https_proxy=${sandbox_forward_proxy}"
  "all_proxy=${sandbox_forward_proxy}"
	"SSL_CERT_FILE=/opt/epar/trust/ca-bundle.pem"
	"NODE_EXTRA_CA_CERTS=/opt/epar/trust/ca-bundle.pem"
	"REQUESTS_CA_BUNDLE=/opt/epar/trust/ca-bundle.pem"
	"PIP_CERT=/opt/epar/trust/ca-bundle.pem"
	"CURL_CA_BUNDLE=/opt/epar/trust/ca-bundle.pem"
	"GIT_SSL_CAINFO=/opt/epar/trust/ca-bundle.pem"
	"AWS_CA_BUNDLE=/opt/epar/trust/ca-bundle.pem"
  "PATH=/opt/epar/hook-bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
  "LANG=C.UTF-8"
)
for environment_name in JAVA_TOOL_OPTIONS NODE_USE_ENV_PROXY; do
  if [[ -n "${!environment_name+x}" ]]; then
    runner_environment+=("${environment_name}=${!environment_name}")
  fi
done
# Always install the preparation hook. It validates the host-trust lease when
# overlay mode is enabled (and accepts the explicit disabled marker otherwise)
# before clearing the listener proxy for workflow steps.
runner_environment+=("ACTIONS_RUNNER_HOOK_JOB_STARTED=/opt/epar/prepare-job-start.sh")
sudo -u agent -H env -i "${runner_environment[@]}" /bin/bash -c 'cd "$1" || exit 1; nohup ./run.sh >>"$2" 2>&1 </dev/null & printf "%s\n" "$!"' bash "${runner_dir}" "${log_file}" >"${pid_file}"
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
