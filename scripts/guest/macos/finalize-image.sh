#!/bin/bash
set -euo pipefail
# Remove only the EPAR service and its exact registration/work paths.
if launchctl print system/org.solutionforest.epar.runner >/dev/null 2>&1; then
  launchctl bootout system/org.solutionforest.epar.runner
fi
rm -f /Library/LaunchDaemons/org.solutionforest.epar.runner.plist
rm -f /var/run/actions-runner.pid /var/run/actions-runner.pid.start
for name in .runner .credentials .credentials_rsaparams .env .path; do rm -f "/opt/actions-runner/$name"; done
rm -rf /opt/actions-runner/_work /opt/actions-runner/_diag
rm -f /var/log/actions-runner/run.log
sync
