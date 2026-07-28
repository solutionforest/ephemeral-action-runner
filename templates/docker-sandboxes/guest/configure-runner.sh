#!/usr/bin/env bash
set -euo pipefail
umask 077

: "${RUNNER_URL:?RUNNER_URL is required}"
: "${RUNNER_NAME:?RUNNER_NAME is required}"
: "${RUNNER_LABELS:?RUNNER_LABELS is required}"
RUNNER_EPHEMERAL="${RUNNER_EPHEMERAL:-true}"
RUNNER_GROUP="${RUNNER_GROUP:-}"
RUNNER_NO_DEFAULT_LABELS="${RUNNER_NO_DEFAULT_LABELS:-false}"
runner_dir="${EPAR_ACTIONS_RUNNER_DIR:-/opt/actions-runner}"

if ! IFS= read -r runner_token || [[ -z "${runner_token}" ]]; then
  echo "RUNNER_TOKEN must be provided as one nonempty line on stdin" >&2
  exit 1
fi
if IFS= read -r extra_line; then
  echo "RUNNER_TOKEN input must contain exactly one line" >&2
  exit 1
fi

cd "${runner_dir}"
if [[ -f .runner ]]; then
  echo "refusing to configure a Docker Sandboxes template that already contains runner registration state" >&2
  exit 1
fi

args=(
  --url "${RUNNER_URL}"
  --token "${runner_token}"
  --name "${RUNNER_NAME}"
  --labels "${RUNNER_LABELS}"
  --work _work
  --unattended
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

sudo -u agent -H ./config.sh "${args[@]}"
unset runner_token args
