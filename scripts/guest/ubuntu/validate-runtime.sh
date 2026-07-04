#!/usr/bin/env bash
set -euo pipefail

id runner >/dev/null
test -x /opt/actions-runner/config.sh
test -x /opt/actions-runner/run.sh
test -x /opt/epar/configure-runner.sh
test -x /opt/epar/run-runner.sh

if command -v systemctl >/dev/null 2>&1 && [[ "$(ps -p 1 -o comm= 2>/dev/null || true)" == "systemd" ]]; then
  state="$(systemctl is-system-running 2>/dev/null || true)"
  case "${state}" in
    running|degraded) ;;
    *) echo "systemd is not ready: ${state}" >&2; exit 1 ;;
  esac
fi

sudo -u runner -H bash -lc 'test -d "$HOME" && test -w "$HOME" && echo "Base runner validation passed"'

if [[ -f /opt/epar/features/docker-engine ]]; then
  bash /opt/epar/validate-docker-engine.sh
fi

if [[ -f /opt/epar/features/docker-browser ]]; then
  bash /opt/epar/validate-docker-browser.sh
fi

if [[ -f /opt/epar/features/rosetta-amd64 ]]; then
  bash /opt/epar/validate-rosetta-amd64.sh
fi

if [[ -f /opt/epar/features/web-e2e ]]; then
  bash /opt/epar/validate-web-e2e.sh
fi
