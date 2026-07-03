#!/usr/bin/env bash
set -euo pipefail

install -d /var/log/actions-runner
chown runner:runner /var/log/actions-runner
systemctl stop actions-runner.service >/dev/null 2>&1 || true
systemctl reset-failed actions-runner.service >/dev/null 2>&1 || true
systemd-run \
  --unit=actions-runner \
  --description="GitHub Actions ephemeral runner" \
  --property=User=runner \
  --property=Group=runner \
  --property=WorkingDirectory=/opt/actions-runner \
  --property=StandardOutput=append:/var/log/actions-runner/run.log \
  --property=StandardError=append:/var/log/actions-runner/run.log \
  /opt/actions-runner/run.sh
sleep 1
systemctl show actions-runner.service --property=MainPID --value >/var/run/actions-runner.pid
cat /var/run/actions-runner.pid
