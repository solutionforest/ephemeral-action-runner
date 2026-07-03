#!/usr/bin/env bash
set -euo pipefail

for command_name in bash git curl wget jq tar unzip zip rsync mysql node npm; do
  if ! command -v "${command_name}" >/dev/null 2>&1; then
    echo "Required web/E2E command not found: ${command_name}" >&2
    exit 1
  fi
done

node --version
npm --version
zip --version >/dev/null
unzip -v >/dev/null
tar --version >/dev/null
rsync --version >/dev/null
mysql --version
