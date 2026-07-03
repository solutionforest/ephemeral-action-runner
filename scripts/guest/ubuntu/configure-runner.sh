#!/usr/bin/env bash
set -euo pipefail

: "${RUNNER_URL:?RUNNER_URL is required}"
: "${RUNNER_TOKEN:?RUNNER_TOKEN is required}"
: "${RUNNER_NAME:?RUNNER_NAME is required}"
: "${RUNNER_LABELS:?RUNNER_LABELS is required}"
RUNNER_EPHEMERAL="${RUNNER_EPHEMERAL:-true}"

cd /opt/actions-runner
if [[ -f .runner ]]; then
  sudo -u runner ./config.sh remove --token "${RUNNER_TOKEN}" || true
fi

args=(
  --url "${RUNNER_URL}"
  --token "${RUNNER_TOKEN}"
  --name "${RUNNER_NAME}"
  --labels "${RUNNER_LABELS}"
  --work "_work"
  --unattended
  --replace
)
if [[ "${RUNNER_EPHEMERAL}" == "true" ]]; then
  args+=(--ephemeral)
fi

sudo -u runner ./config.sh "${args[@]}"
