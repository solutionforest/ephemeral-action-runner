#!/usr/bin/env bash
set -euo pipefail

if [[ "$(id -u)" != "0" ]]; then
  echo "quiesce-apt.sh must run as root" >&2
  exit 1
fi

# Docker Sandboxes starts one best-effort apt refresh during guest boot with a
# fixed command. On some hosts its network requests never finish, so merely
# waiting for the lock can permanently prevent runner registration. The
# template also masks Ubuntu's periodic units; terminate only the exact
# Docker-Sandboxes-owned refresh, then wait for every package-manager process
# to release its locks before registration. Never use a broad apt kill pattern:
# an unexpected package operation must remain visible and fail closed.
if command -v systemctl >/dev/null 2>&1; then
  systemctl stop apt-daily.timer apt-daily-upgrade.timer apt-daily.service apt-daily-upgrade.service unattended-upgrades.service >/dev/null 2>&1 || true
fi

docker_sandboxes_bootstrap_command='command -v apt-get > /dev/null 2>&1 && (apt-get update -qq -y > /dev/null 2>&1 || true) &'

read_process_arguments() {
  local pid="$1"
  local -n output_arguments="$2"
  output_arguments=()
  [[ -r "/proc/${pid}/cmdline" ]] || return 1
  mapfile -d '' -t output_arguments < "/proc/${pid}/cmdline"
}

is_docker_sandboxes_bootstrap_parent() {
  local pid="$1" executable
  local -a arguments=()
  read_process_arguments "${pid}" arguments || return 1
  executable="$(readlink -f "/proc/${pid}/exe" 2>/dev/null || true)"
  [[ "${#arguments[@]}" == "3" ]] \
    && [[ "${arguments[0]}" == "sh" || "${arguments[0]}" == "/bin/sh" || "${arguments[0]}" == "/usr/bin/sh" ]] \
    && [[ "${arguments[1]}" == "-c" ]] \
    && [[ "${arguments[2]}" == "${docker_sandboxes_bootstrap_command}" ]] \
    && [[ "${executable}" == "/usr/bin/dash" || "${executable}" == "/usr/bin/bash" ]]
}

is_docker_sandboxes_bootstrap_apt() {
  local pid="$1"
  local -a arguments=()
  read_process_arguments "${pid}" arguments || return 1
  [[ "${#arguments[@]}" == "4" ]] \
    && [[ "${arguments[0]}" == "apt-get" || "${arguments[0]}" == "/usr/bin/apt-get" ]] \
    && [[ "${arguments[1]}" == "update" ]] \
    && [[ "${arguments[2]}" == "-qq" ]] \
    && [[ "${arguments[3]}" == "-y" ]] \
    && [[ "$(readlink -f "/proc/${pid}/exe" 2>/dev/null || true)" == "/usr/bin/apt-get" ]]
}

docker_sandboxes_bootstrap_parents() {
  local process_path pid
  for process_path in /proc/[0-9]*; do
    pid="${process_path##*/}"
    if is_docker_sandboxes_bootstrap_parent "${pid}"; then
      printf '%s\n' "${pid}"
    fi
  done
}

terminate_docker_sandboxes_bootstrap_refresh() {
  local parent_pid child_pid attempt
  local -a parent_pids=() refresh_pids=() validated_refresh_pids=()
  mapfile -t parent_pids < <(docker_sandboxes_bootstrap_parents)
  for parent_pid in "${parent_pids[@]}"; do
    while IFS= read -r child_pid; do
      [[ -n "${child_pid}" ]] || continue
      if is_docker_sandboxes_bootstrap_apt "${child_pid}"; then
        refresh_pids+=("${child_pid}")
      fi
    done < <(pgrep -P "${parent_pid}" 2>/dev/null || true)
    refresh_pids+=("${parent_pid}")
  done

  for child_pid in "${refresh_pids[@]}"; do
    if is_docker_sandboxes_bootstrap_parent "${child_pid}" || is_docker_sandboxes_bootstrap_apt "${child_pid}"; then
      validated_refresh_pids+=("${child_pid}")
    fi
  done
  if [[ "${#validated_refresh_pids[@]}" == "0" ]]; then
    return 0
  fi

  echo "EPAR Docker Sandboxes template: stopping boot-time apt refresh pids=${validated_refresh_pids[*]}"
  kill -TERM "${validated_refresh_pids[@]}" 2>/dev/null || true
  for attempt in $(seq 1 10); do
    local -a remaining_pids=()
    for child_pid in "${validated_refresh_pids[@]}"; do
      if is_docker_sandboxes_bootstrap_parent "${child_pid}" || is_docker_sandboxes_bootstrap_apt "${child_pid}"; then
        remaining_pids+=("${child_pid}")
      fi
    done
    if [[ "${#remaining_pids[@]}" == "0" ]]; then
      return 0
    fi
    if [[ "${attempt}" == "10" ]]; then
      kill -KILL "${remaining_pids[@]}" 2>/dev/null || true
    else
      sleep 1
    fi
  done
}

terminate_docker_sandboxes_bootstrap_refresh

apt_processes() {
  {
    pgrep -x apt-get 2>/dev/null || true
    pgrep -x apt 2>/dev/null || true
    pgrep -x dpkg 2>/dev/null || true
    pgrep -x unattended-upgr 2>/dev/null || true
    pgrep -f -x '/usr/lib/apt/apt.systemd.daily.*' 2>/dev/null || true
  } | sort -u
}

for attempt in $(seq 1 180); do
  mapfile -t package_pids < <(apt_processes)
  if [[ "${#package_pids[@]}" == "0" ]]; then
    exit 0
  fi
  if [[ "${attempt}" == "180" ]]; then
    echo "EPAR Docker Sandboxes template: timed out waiting for unexpected package-manager processes: ${package_pids[*]}" >&2
    ps -o pid=,ppid=,stat=,comm=,args= -p "$(IFS=,; echo "${package_pids[*]}")" >&2 || true
    exit 1
  fi
  sleep 1
done
