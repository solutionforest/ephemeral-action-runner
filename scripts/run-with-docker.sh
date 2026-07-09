#!/usr/bin/env bash
set -euo pipefail

# Runs EPAR from source with no local Go install: a containerized Go
# toolchain compiles and executes the source with `go run`, the same as the
# documented source-first path (docs/usage.md) — just inside a container
# instead of on the host. No binary is built or left on disk.
#
# Docker is still required (both for this wrapper and for EPAR's own
# Docker-DinD provider, reached here via the mounted host socket).
#
# Usage: scripts/run-with-docker.sh [epar-args...]

image="${GO_DOCKER_IMAGE:-golang:1.24}"
dev_image="${EPAR_DEV_IMAGE:-epar-dev-toolchain}"
gomod_volume="${EPAR_GOMOD_VOLUME:-epar-gomod}"
gocache_volume="${EPAR_GOCACHE_VOLUME:-epar-gocache}"
docker_sock="${EPAR_DOCKER_SOCK:-/var/run/docker.sock}"
host_name="${EPAR_HOST_NAME:-}"
if [[ -z "${host_name}" ]]; then
  host_name="$(hostname 2>/dev/null || true)"
fi
docker_env_flags=()
if [[ -n "${host_name}" ]]; then
  docker_env_flags=(-e "EPAR_HOST_NAME=${host_name}")
fi

if ! command -v docker >/dev/null 2>&1; then
  echo "docker command not found. Install Docker Desktop, Docker Engine, or a compatible Docker host." >&2
  exit 1
fi

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"

docker build --quiet \
  --build-arg "GO_IMAGE=${image}" \
  -t "$dev_image" \
  -f "${repo_root}/scripts/docker/dev.Dockerfile" \
  "${repo_root}/scripts/docker" >/dev/null

tty_flags=(--rm -i)
if [[ -t 0 ]]; then
  tty_flags=(--rm -it)
fi

# The container runs as root (needed: GOPATH/GOCACHE named volumes and /root
# are only writable by root inside the golang image, and re-pointing them at
# a writable path just to run as a non-root UID isn't worth the complexity).
# On real Linux hosts (unlike Docker Desktop, which already maps bind-mount
# ownership back to the host user) that leaves root-owned files under the
# bind-mounted .local/ and work/ dirs, so hand them back to the invoking user
# afterward.
docker run "${tty_flags[@]}" \
  "${docker_env_flags[@]}" \
  -v "${repo_root}:/app" -w /app \
  -v "${gomod_volume}:/go/pkg/mod" \
  -v "${gocache_volume}:/root/.cache/go-build" \
  -v "${docker_sock}:/var/run/docker.sock" \
  "$dev_image" \
  go run ./cmd/ephemeral-action-runner "$@" && status=0 || status=$?

if [[ "$(uname -s)" == "Linux" ]]; then
  chown -R "$(id -u):$(id -g)" "${repo_root}/.local" "${repo_root}/work" 2>/dev/null || true
fi

exit "$status"
