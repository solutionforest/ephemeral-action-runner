#!/usr/bin/env bash
set -euo pipefail
umask 077

if [[ "$(id -u)" != "0" ]]; then
  echo "configure-runner.sh must run as root" >&2
  exit 1
fi
unset SSH_AUTH_SOCK SSH_AUTH_SOCK_GATEWAY SSH_AGENT_PID

: "${RUNNER_URL:?RUNNER_URL is required}"
: "${RUNNER_NAME:?RUNNER_NAME is required}"
: "${RUNNER_LABELS:?RUNNER_LABELS is required}"
RUNNER_EPHEMERAL="${RUNNER_EPHEMERAL:-true}"
RUNNER_GROUP="${RUNNER_GROUP:-}"
RUNNER_NO_DEFAULT_LABELS="${RUNNER_NO_DEFAULT_LABELS:-false}"
runner_dir="${EPAR_ACTIONS_RUNNER_DIR:-/opt/actions-runner}"
sandbox_forward_proxy="http://gateway.docker.internal:3128"
[[ -s /opt/epar/trust/ca-bundle.pem && ! -L /opt/epar/trust/ca-bundle.pem ]]

/opt/epar/quiesce-apt.sh

# Docker Sandboxes may inject a host-authenticated client configuration while
# creating the sandbox. Scrub only the authentication files immediately before
# registration; preserve .docker/sandbox/locks and any other runtime state.
# This runs before workflow steps exist, so it cannot remove later job-created
# credentials.
/opt/epar/scrub-docker-auth.sh --runtime

install -d -m 0700 -o agent -g agent \
  /home/agent/.docker \
  /home/agent/.config \
  /home/agent/.cache \
  /home/agent/.local \
  /home/agent/.local/share \
  /home/agent/.local/state \
  /run/user/1000

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

configuration_environment=(
  "HOME=/home/agent"
  "USER=agent"
  "LOGNAME=agent"
  "XDG_CONFIG_HOME=/home/agent/.config"
  "XDG_CACHE_HOME=/home/agent/.cache"
  "XDG_DATA_HOME=/home/agent/.local/share"
  "XDG_STATE_HOME=/home/agent/.local/state"
  "XDG_RUNTIME_DIR=/run/user/1000"
  "DOCKER_CONFIG=/home/agent/.docker"
  "HTTP_PROXY=${sandbox_forward_proxy}"
  "HTTPS_PROXY=${sandbox_forward_proxy}"
  "ALL_PROXY=${sandbox_forward_proxy}"
  "http_proxy=${sandbox_forward_proxy}"
  "https_proxy=${sandbox_forward_proxy}"
  "all_proxy=${sandbox_forward_proxy}"
	"SSL_CERT_FILE=/opt/epar/trust/ca-bundle.pem"
	"NODE_EXTRA_CA_CERTS=/opt/epar/trust/ca-bundle.pem"
	"REQUESTS_CA_BUNDLE=/opt/epar/trust/ca-bundle.pem"
	"PIP_CERT=/opt/epar/trust/ca-bundle.pem"
	"CURL_CA_BUNDLE=/opt/epar/trust/ca-bundle.pem"
	"GIT_SSL_CAINFO=/opt/epar/trust/ca-bundle.pem"
	"AWS_CA_BUNDLE=/opt/epar/trust/ca-bundle.pem"
  "PATH=/opt/epar/hook-bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
  "LANG=C.UTF-8"
)
for environment_name in JAVA_TOOL_OPTIONS NODE_USE_ENV_PROXY; do
  if [[ -n "${!environment_name+x}" ]]; then
    configuration_environment+=("${environment_name}=${!environment_name}")
  fi
done

sudo -u agent -H env -i "${configuration_environment[@]}" ./config.sh "${args[@]}"
unset runner_token args
