#!/bin/bash
set -euo pipefail
pid="$(cat /var/run/actions-runner.pid)"
[[ "$pid" =~ ^[1-9][0-9]*$ ]]
stored="$(cat /var/run/actions-runner.pid.start)"
test -n "$stored"
observed="$(LC_ALL=C ps -p "$pid" -o lstart=)"
test "$observed" = "$stored"
test "$(ps -p "$pid" -o uid= | tr -d ' ')" = "$(id -u eparrunner)"
state="$(ps -p "$pid" -o stat= | tr -d ' ')"
if [[ -z "$state" || "$state" == Z* ]]; then
  echo 'EPAR runner process is absent or a zombie' >&2
  exit 1
fi
service_pid="$(launchctl print system/org.solutionforest.epar.runner | awk '/^[[:space:]]*pid = / {print $3; exit}')"
test "$service_pid" = "$pid"
