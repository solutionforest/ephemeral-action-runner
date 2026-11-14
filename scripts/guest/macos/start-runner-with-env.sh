#!/bin/bash
set -euo pipefail
export HOME=/Users/eparrunner
export PATH=/opt/homebrew/bin:/opt/homebrew/sbin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin
export LANG=en_US.UTF-8
if [[ -f /opt/epar/runner.env ]]; then
  # This image-owned file contains tool paths only, never registration secrets.
  source /opt/epar/runner.env
fi
cd /opt/actions-runner
exec ./run.sh
