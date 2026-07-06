#!/usr/bin/env bash
set -euo pipefail

if [[ -f /opt/epar/source-image.env ]]; then
  set -a
  # shellcheck disable=SC1091
  . /opt/epar/source-image.env
  set +a
fi

cd /opt/actions-runner
exec ./run.sh
