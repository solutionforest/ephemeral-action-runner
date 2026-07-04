#!/usr/bin/env bash
set -euo pipefail

timeout="${EPAR_APT_LOCK_TIMEOUT:-600}"
deadline=$((SECONDS + timeout))

export DEBIAN_FRONTEND=noninteractive

if command -v systemctl >/dev/null 2>&1; then
  systemctl stop apt-daily.timer apt-daily-upgrade.timer apt-daily.service apt-daily-upgrade.service 2>/dev/null || true
fi

while fuser /var/lib/dpkg/lock-frontend /var/lib/dpkg/lock /var/cache/apt/archives/lock >/dev/null 2>&1; do
  if (( SECONDS >= deadline )); then
    echo "Timed out waiting for apt/dpkg locks to clear" >&2
    fuser -v /var/lib/dpkg/lock-frontend /var/lib/dpkg/lock /var/cache/apt/archives/lock >&2 || true
    exit 1
  fi
  sleep 2
done

echo "EPAR apt/dpkg locks are clear."
