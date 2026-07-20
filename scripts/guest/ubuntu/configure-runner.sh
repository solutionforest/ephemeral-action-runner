#!/usr/bin/env bash
set -euo pipefail

: "${RUNNER_URL:?RUNNER_URL is required}"
: "${RUNNER_NAME:?RUNNER_NAME is required}"
: "${RUNNER_LABELS:?RUNNER_LABELS is required}"
RUNNER_EPHEMERAL="${RUNNER_EPHEMERAL:-true}"
RUNNER_GROUP="${RUNNER_GROUP:-}"
RUNNER_NO_DEFAULT_LABELS="${RUNNER_NO_DEFAULT_LABELS:-false}"
EPAR_ACTIONS_RUNNER_DIR="${EPAR_ACTIONS_RUNNER_DIR:-/opt/actions-runner}"

if ! IFS= read -r RUNNER_TOKEN || [[ -z "${RUNNER_TOKEN}" ]]; then
  echo "RUNNER_TOKEN must be provided as one nonempty line on stdin" >&2
  exit 1
fi

cd "${EPAR_ACTIONS_RUNNER_DIR}"
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
if [[ -n "${RUNNER_GROUP}" ]]; then
  args+=(--runnergroup "${RUNNER_GROUP}")
fi
if [[ "${RUNNER_NO_DEFAULT_LABELS}" == "true" ]]; then
  args+=(--no-default-labels)
fi

sudo -u runner ./config.sh "${args[@]}"
