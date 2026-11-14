#!/bin/bash
set -euo pipefail
test "$(uname -s)/$(uname -m)" = Darwin/arm64
test "$(id -u eparrunner)" -ne 0
for script in config.sh run.sh bin/Runner.Listener; do test -x "/opt/actions-runner/$script"; done
for script in configure-runner run-runner check-runner start-runner-with-env collect-runner-diagnostics; do test -x "/opt/epar/$script.sh"; done
sudo -u eparrunner -H bash -c 'test "$HOME" = /Users/eparrunner && test -w "$HOME" && test -w /opt/actions-runner'
if [[ -f /opt/epar/features/mobile ]]; then bash /opt/epar/validate-mobile.sh; fi
echo 'macOS ARM64 runner validation passed'
