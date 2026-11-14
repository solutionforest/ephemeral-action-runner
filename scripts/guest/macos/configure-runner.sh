#!/bin/bash
set -euo pipefail
: "${RUNNER_URL:?RUNNER_URL is required}"
: "${RUNNER_NAME:?RUNNER_NAME is required}"
: "${RUNNER_LABELS:?RUNNER_LABELS is required}"
IFS= read -r token
test -n "$token"
cd /opt/actions-runner
# Registration is always fresh; reusable images must never carry credentials.
test ! -f .runner
test ! -f .credentials
args=(--url "$RUNNER_URL" --token "$token" --name "$RUNNER_NAME" --labels "$RUNNER_LABELS" --work _work --unattended --ephemeral)
if [[ -n "${RUNNER_GROUP:-}" ]]; then args+=(--runnergroup "$RUNNER_GROUP"); fi
if [[ "${RUNNER_NO_DEFAULT_LABELS:-false}" == true ]]; then args+=(--no-default-labels); fi
sudo -u eparrunner -H ./config.sh "${args[@]}"
