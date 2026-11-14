#!/bin/bash
set -u
echo '=== macOS EPAR runner diagnostics ==='
sw_vers
launchctl print system/org.solutionforest.epar.runner 2>&1 || true
pid="$(cat /var/run/actions-runner.pid 2>/dev/null || true)"
if [[ "$pid" =~ ^[1-9][0-9]*$ ]]; then ps -p "$pid" -o pid=,ppid=,uid=,stat=,lstart=,comm=; fi
tail -n 80 /var/log/actions-runner/run.log 2>&1 || true
latest="$(ls -t /opt/actions-runner/_diag/Runner_*.log 2>/dev/null | head -n 1)"
if [[ -n "$latest" ]]; then tail -n 80 "$latest"; fi
