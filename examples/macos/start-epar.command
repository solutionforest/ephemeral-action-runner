#!/usr/bin/env bash
set -euo pipefail

# Finder/Open at Login launches .command files with a minimal environment.
export PATH="/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin:${PATH:-}"

script_path="${BASH_SOURCE[0]:-$0}"
script_dir="$(cd -- "$(dirname -- "${script_path}")" && pwd -P)"

find_repo_root() {
  if [[ -n "${EPAR_ROOT:-}" ]]; then
    printf '%s\n' "${EPAR_ROOT}"
    return
  fi

  local dir="${script_dir}"
  for _ in $(seq 1 8); do
    if [[ -d "${dir}/configs" && -d "${dir}/scripts" && -f "${dir}/go.mod" ]]; then
      printf '%s\n' "${dir}"
      return
    fi
    dir="$(dirname -- "${dir}")"
  done

  echo "Unable to find EPAR root. Set EPAR_ROOT in this script." >&2
  return 1
}

EPAR_ROOT="$(find_repo_root)"
CONFIG_PATH="${EPAR_CONFIG:-${EPAR_ROOT}/.local/config.yml}"
if [[ -z "${EPAR_BIN:-}" ]]; then
  EPAR_BIN="${EPAR_ROOT}/bin/ephemeral-action-runner"
fi
MIRROR_CONTAINER="${EPAR_MIRROR_CONTAINER:-epar-dockerhub-cache}"
WAIT_FOR_DOCKER="${EPAR_WAIT_FOR_DOCKER:-1}"
DOCKER_WAIT_ATTEMPTS="${EPAR_DOCKER_WAIT_ATTEMPTS:-120}"

cd "${EPAR_ROOT}"
mkdir -p work/logs

if [[ ! -x "${EPAR_BIN}" ]]; then
  echo "EPAR binary not found or not executable: ${EPAR_BIN}" >&2
  echo "Build it first: go build -o ./bin/ephemeral-action-runner ./cmd/ephemeral-action-runner" >&2
  exit 1
fi

if [[ "${WAIT_FOR_DOCKER}" != "0" ]]; then
  echo "[$(date '+%Y-%m-%d %H:%M:%S')] waiting for Docker to become ready..."
  for _ in $(seq 1 "${DOCKER_WAIT_ATTEMPTS}"); do
    if docker info >/dev/null 2>&1; then
      echo "[$(date '+%Y-%m-%d %H:%M:%S')] Docker is ready"
      break
    fi
    sleep 2
  done

  docker info >/dev/null

  if docker container inspect "${MIRROR_CONTAINER}" >/dev/null 2>&1; then
    echo "[$(date '+%Y-%m-%d %H:%M:%S')] ensuring mirror container ${MIRROR_CONTAINER} is running..."
    docker start "${MIRROR_CONTAINER}" >/dev/null 2>&1 || true
  else
    echo "[$(date '+%Y-%m-%d %H:%M:%S')] mirror container ${MIRROR_CONTAINER} not found; continuing"
  fi
fi

echo "[$(date '+%Y-%m-%d %H:%M:%S')] starting EPAR..."
exec "${EPAR_BIN}" start --config "${CONFIG_PATH}" "$@"
