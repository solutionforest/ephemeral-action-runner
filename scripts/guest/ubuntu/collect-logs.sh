#!/usr/bin/env bash
set -euo pipefail

tar czf - \
  /var/log/actions-runner 2>/dev/null \
  /opt/actions-runner/_diag 2>/dev/null \
  || true
