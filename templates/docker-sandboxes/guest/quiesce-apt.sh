#!/usr/bin/env bash
set -euo pipefail

if [[ "$(id -u)" != "0" ]]; then
  echo "quiesce-apt.sh must run as root" >&2
  exit 1
fi

# Ubuntu may start apt-daily early during a Docker Sandbox boot. A warm runner
# can otherwise reach its first package operation before that refresh releases
# /var/lib/apt/lists/lock. The template masks future starts at build time; stop
# any unit already activated during boot and wait before runner registration.
if command -v systemctl >/dev/null 2>&1; then
  systemctl stop apt-daily.timer apt-daily-upgrade.timer apt-daily.service apt-daily-upgrade.service unattended-upgrades.service >/dev/null 2>&1 || true
fi

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
    echo "EPAR Docker Sandboxes template: timed out waiting for boot-time apt processes: ${package_pids[*]}" >&2
    ps -o pid=,ppid=,stat=,comm=,args= -p "$(IFS=,; echo "${package_pids[*]}")" >&2 || true
    exit 1
  fi
  sleep 1
done
