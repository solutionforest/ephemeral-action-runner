#!/usr/bin/env bash
set -euo pipefail

if [[ -f /opt/epar/source-image.env ]]; then
  set -a
  # shellcheck disable=SC1091
  . /opt/epar/source-image.env
  set +a
fi

runner_work_dir="${EPAR_RUNNER_WORK_DIR:-/opt/actions-runner}"

cd "${runner_work_dir}"
exec ./run.sh
