#!/usr/bin/env bash
set -euo pipefail

rosetta_mount="${EPAR_ROSETTA_MOUNT:-/run/rosetta}"

if [[ -x /opt/epar/setup-rosetta.sh ]]; then
  /opt/epar/setup-rosetta.sh
fi

mountpoint -q "${rosetta_mount}"
test -x "${rosetta_mount}/rosetta"

if [[ ! -f /proc/sys/fs/binfmt_misc/rosetta ]]; then
  echo "Rosetta binfmt registration is missing" >&2
  exit 1
fi
if ! grep -q '^enabled$' /proc/sys/fs/binfmt_misc/rosetta; then
  echo "Rosetta binfmt registration is not enabled" >&2
  cat /proc/sys/fs/binfmt_misc/rosetta >&2
  exit 1
fi

if command -v docker >/dev/null 2>&1; then
  if command -v systemctl >/dev/null 2>&1; then
    systemctl start containerd.service 2>/dev/null || true
    systemctl start docker.service
  fi
  arch="$(sudo -u runner -H docker run --rm --platform linux/amd64 alpine:3.20 sh -c 'uname -m')"
  if [[ "${arch}" != "x86_64" ]]; then
    echo "Expected linux/amd64 container uname -m to be x86_64, got ${arch}" >&2
    exit 1
  fi
  echo "Rosetta amd64 Docker validation passed"
else
  echo "Rosetta amd64 registration validation passed"
fi
