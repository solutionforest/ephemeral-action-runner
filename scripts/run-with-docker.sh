#!/usr/bin/env bash
set -euo pipefail

case "$(uname -s)" in
  MINGW*|MSYS*|CYGWIN*)
    script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
    ps_script="$script_dir/run-with-docker.ps1"
    command -v cygpath >/dev/null 2>&1 && ps_script="$(cygpath -w "$ps_script")"
    exec powershell.exe -NoLogo -NoProfile -ExecutionPolicy Bypass -File "$ps_script" "$@"
    ;;
esac

# Runs EPAR from source with no local Go install: a containerized Go
# toolchain compiles and executes the source with `go run`, the same as the
# documented source-first path (docs/usage.md) — just inside a container
# instead of on the host. No binary is built or left on disk.
#
# Docker is still required (both for this wrapper and for EPAR's own
# Docker-DinD provider, reached here via the mounted host socket).
#
# Usage: scripts/run-with-docker.sh [epar-args...]

image="${GO_DOCKER_IMAGE:-golang:1.25}"
dev_image="${EPAR_DEV_IMAGE:-epar-dev-toolchain}"
gomod_volume="${EPAR_GOMOD_VOLUME:-epar-gomod}"
gocache_volume="${EPAR_GOCACHE_VOLUME:-epar-gocache}"
docker_sock="${EPAR_DOCKER_SOCK:-/var/run/docker.sock}"
export DOCKER_CLI_HINTS="${DOCKER_CLI_HINTS:-false}"
host_name="${EPAR_HOST_NAME:-}"
if [[ -z "${host_name}" ]]; then
  host_name="$(hostname 2>/dev/null || true)"
fi
docker_env_flags=(-e "DOCKER_CLI_HINTS=${DOCKER_CLI_HINTS}")
if [[ -n "${EPAR_CONFIG:-}" ]]; then docker_env_flags+=(-e "EPAR_CONFIG=${EPAR_CONFIG}"); fi
if [[ -n "${host_name}" ]]; then
  docker_env_flags+=(-e "EPAR_HOST_NAME=${host_name}")
fi

if ! command -v docker >/dev/null 2>&1; then
  echo "docker command not found. Install Docker Desktop, Docker Engine, or a compatible Docker host." >&2
  exit 1
fi

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
EPAR_HOST_TRUST_HELPER="${repo_root}/scripts/host-trust/host-trust-feed.sh"
# shellcheck disable=SC1091
. "${repo_root}/scripts/host-trust/wrapper-lib.sh"
controller_command="${1:-}"
config_path="$(epar_host_trust_config_path "${repo_root}" "$@")"
implicit_init=0
if [[ "${controller_command:-start}" == "start" && ! -f "$config_path" ]]; then
  implicit_init=1
fi
if [[ "${controller_command}" == "init" || "$implicit_init" == 1 ]]; then
  docker_env_flags+=(-e "EPAR_HOST_TRUST_INIT_DEFERRED=1")
  docker_env_flags+=(-e "EPAR_CONTROLLER_HOST_OS=$(epar_host_trust_host_os)")
fi

docker build --quiet \
  --build-arg "GO_IMAGE=${image}" \
  -t "$dev_image" \
  -f "${repo_root}/scripts/docker/dev.Dockerfile" \
  "${repo_root}/scripts/docker" >/dev/null

tty_flags=(--rm -i)
if [[ -t 0 ]]; then
  tty_flags=(--rm -it)
fi

host_trust_docker_flags=()
run_controller() {
  docker run "${tty_flags[@]}" \
    "${docker_env_flags[@]}" \
    "${host_trust_docker_flags[@]}" \
    -v "${repo_root}:/app" -w /app \
    -v "${gomod_volume}:/go/pkg/mod" \
    -v "${gocache_volume}:/root/.cache/go-build" \
    -v "${docker_sock}:/var/run/docker.sock" \
    "$dev_image" \
    go run ./cmd/ephemeral-action-runner "$@"
}

trap epar_host_trust_cleanup EXIT INT TERM
status=0
if [[ "$implicit_init" == 1 ]]; then
  init_args=()
  while IFS= read -r -d '' argument; do init_args+=("$argument"); done < <(epar_host_trust_init_arguments "$@")
  run_controller "${init_args[@]}" || status=$?
  if [[ "$status" == 0 ]]; then
    EPAR_HOST_TRUST_POST_INIT_CONFIG="$config_path"
    epar_host_trust_post_init "${repo_root}" || status=$?
  fi
fi

if [[ "$status" == 0 ]]; then
  epar_host_trust_prepare "${repo_root}" "${controller_command:-start}" "$@" || status=$?
fi
if [[ "$status" == 0 && -n "${EPAR_HOST_TRUST_FEED_DIR}" ]]; then
  host_trust_docker_flags+=(
    -e "EPAR_CONTROLLER_HOST_OS=$(epar_host_trust_host_os)"
    -e "EPAR_HOST_TRUST_FEED=/run/epar-host-trust/current.json"
    -v "${EPAR_HOST_TRUST_FEED_DIR}:/run/epar-host-trust:ro"
  )
fi

# The container runs as root (needed: GOPATH/GOCACHE named volumes and /root
# are only writable by root inside the golang image, and re-pointing them at
# a writable path just to run as a non-root UID isn't worth the complexity).
# On real Linux hosts (unlike Docker Desktop, which already maps bind-mount
# ownership back to the host user) that leaves root-owned files under the
# bind-mounted .local/ and work/ dirs, so hand them back to the invoking user
# afterward.
if [[ "$status" == 0 ]]; then
  run_controller "$@" || status=$?
fi

if [[ "$(uname -s)" == "Linux" ]]; then
  chown -R "$(id -u):$(id -g)" "${repo_root}/.local" "${repo_root}/work" 2>/dev/null || true
fi

if [[ "$status" == "0" && "$controller_command" == "init" ]]; then
  epar_host_trust_post_init "${repo_root}" || status=$?
fi
epar_host_trust_cleanup
trap - EXIT INT TERM

exit "$status"
