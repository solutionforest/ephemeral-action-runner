#!/usr/bin/env bash
set -euo pipefail

runner_dir="${EPAR_ACTIONS_RUNNER_DIR:-/opt/actions-runner}"
file_commands_dir="${runner_dir}/_work/_temp/_runner_file_commands"

# Keep the host-trust check first. A stale or missing overlay lease must stop
# the job before this hook mutates GITHUB_ENV; the check accepts the explicit
# disabled image marker so the preparation hook remains unconditional.
/opt/epar/check-host-trust-generation.sh

workflow_https_proxy=""
workflow_no_proxy="*"
marker_mode="$(jq -er '.mode | strings' /opt/epar/host-trust-generation.json)"
marker_host_os="$(jq -er '.hostOS | strings | ascii_downcase' /opt/epar/host-trust-generation.json)"
relay_required=false
if [[ "${marker_mode}" == "overlay" && "${marker_host_os}" == "windows" ]]; then
  relay_required=true
fi
relay_active=false
if sudo -n test -e /run/epar/egress-relay-active; then
  relay_active=true
fi
if [[ "${relay_required}" == "true" && "${relay_active}" != "true" ]]; then
  echo "EPAR job-start preparation: Windows host-trust overlay requires the authenticated relay" >&2
  exit 1
fi
if [[ "${relay_active}" == "true" ]]; then
  if ! sudo -n test -f /run/epar/egress-relay-active \
    || ! sudo -n test ! -L /run/epar/egress-relay-active \
    || [[ "$(sudo -n stat -c '%U:%G:%a' /run/epar/egress-relay-active 2>/dev/null || true)" != "root:root:444" ]]; then
    echo "EPAR job-start preparation: host-trust relay activation marker is invalid" >&2
    exit 1
  fi
  if [[ "$(curl --silent --show-error --output /dev/null --write-out '%{http_code}' --max-time 15 http://127.0.0.1:3129/health)" != "204" ]]; then
    echo "EPAR job-start preparation: host-trust relay is unavailable" >&2
    exit 1
  fi
  workflow_https_proxy="http://127.0.0.1:3130"
  workflow_no_proxy="localhost,127.0.0.1,::1,host.docker.internal,gateway.docker.internal,kubernetes.docker.internal,host.containers.internal,10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,169.254.0.0/16,.local"
fi

if [[ "$(id -u)" != "1000" || "$(id -g)" != "1000" ]]; then
  echo "EPAR job-start preparation: hook must run as agent (UID/GID 1000)" >&2
  exit 1
fi
if [[ -z "${GITHUB_ENV:-}" || "${GITHUB_ENV}" != /* || "${GITHUB_ENV}" == *$'\n'* || "${GITHUB_ENV}" == *$'\r'* ]]; then
  echo "EPAR job-start preparation: GITHUB_ENV must be an absolute path without newlines" >&2
  exit 1
fi
for runner_parent in "${runner_dir}" "${runner_dir}/_work" "${runner_dir}/_work/_temp" "${file_commands_dir}"; do
  if [[ -L "${runner_parent}" || ! -d "${runner_parent}" ]]; then
    echo "EPAR job-start preparation: runner file-command parent is not a real directory" >&2
    exit 1
  fi
done
if [[ -L "${file_commands_dir}" || ! -d "${file_commands_dir}" ]]; then
  echo "EPAR job-start preparation: runner file-command directory is not a real directory" >&2
  exit 1
fi
runner_dir_real="$(readlink -f -- "${runner_dir}")"
file_commands_dir_real="$(readlink -f -- "${file_commands_dir}")"
if [[ -z "${runner_dir_real}" || "${runner_dir_real}" == "/" || -z "${file_commands_dir_real}" || "${file_commands_dir_real}" == "/" ]]; then
  echo "EPAR job-start preparation: runner file-command directory has no stable path" >&2
  exit 1
fi
if [[ "${file_commands_dir_real}" != "${runner_dir_real}/_work/_temp/_runner_file_commands" ]]; then
  echo "EPAR job-start preparation: runner file-command directory escaped the runner temp root" >&2
  exit 1
fi
if [[ "$(stat -c '%u:%g' "${file_commands_dir_real}" 2>/dev/null || true)" != "1000:1000" ]]; then
  echo "EPAR job-start preparation: runner file-command directory ownership is invalid" >&2
  exit 1
fi
if [[ -L "${GITHUB_ENV}" || ! -f "${GITHUB_ENV}" ]]; then
  echo "EPAR job-start preparation: GITHUB_ENV must be a non-symlink regular file" >&2
  exit 1
fi
github_env_real="$(readlink -f -- "${GITHUB_ENV}")"
case "${github_env_real}" in
  "${file_commands_dir_real}"/*)
    ;;
  *)
    echo "EPAR job-start preparation: GITHUB_ENV escaped the runner file-command directory" >&2
    exit 1
    ;;
esac
if [[ "$(stat -c '%u:%g' "${github_env_real}" 2>/dev/null || true)" != "1000:1000" ]]; then
  echo "EPAR job-start preparation: GITHUB_ENV ownership is invalid" >&2
  exit 1
fi

# The listener's Docker Sandboxes gateway proxy is never inherited by workflow
# steps. Windows overlay mode requires EPAR's credential-free local bridge;
# other modes clear proxy variables and keep their provider-native route.
{
  printf 'HTTP_PROXY=\n'
  printf 'HTTPS_PROXY=%s\n' "${workflow_https_proxy}"
  printf 'ALL_PROXY=\n'
  printf 'http_proxy=\n'
  printf 'https_proxy=%s\n' "${workflow_https_proxy}"
  printf 'all_proxy=\n'
  printf 'NO_PROXY=%s\n' "${workflow_no_proxy}"
  printf 'no_proxy=%s\n' "${workflow_no_proxy}"
} >>"${github_env_real}"
