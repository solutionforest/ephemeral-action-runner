#!/usr/bin/env bash
set -euo pipefail

if [[ "$(id -u)" != "0" ]]; then
  echo "scrub-docker-auth.sh must run as root" >&2
  exit 1
fi

mode="${1:---build}"
case "${mode}" in
  --build|--runtime)
    ;;
  *)
    echo "scrub-docker-auth.sh accepts only --build or --runtime" >&2
    exit 1
    ;;
esac

# Docker Sandboxes can materialize a client configuration when a sandbox is
# created, even when the reusable template was scrubbed. Enumerate the actual
# passwd homes instead of assuming that the source image still uses ubuntu or
# that only agent and root exist. This helper is used both while building the
# reusable template and immediately before runner registration; the latter is
# deliberately before any workflow can create credentials.
credential_homes=(/root /home/runner /home/agent)
if ! passwd_entries="$(getent passwd)" || [[ -z "${passwd_entries}" ]]; then
  echo "failed to enumerate passwd homes for Docker credential scrubbing" >&2
  exit 1
fi
while IFS=: read -r _ _ _ _ _ account_home _; do
  if [[ -z "${account_home}" || "${account_home}" != /* ]]; then
    echo "passwd entry contains an invalid home ${account_home:-<empty>}" >&2
    exit 1
  fi
  normalized_home="$(readlink -m -- "${account_home}")"
  if [[ "${normalized_home}" == "/" ]]; then
    continue
  fi
  credential_homes+=("${normalized_home}")
done <<<"${passwd_entries}"

if [[ "${mode}" == "--build" ]]; then
  for root_home_docker_config in /.docker /.dockercfg; do
    if [[ -e "${root_home_docker_config}" || -L "${root_home_docker_config}" ]]; then
      echo "Docker client configuration exists in the root-filesystem home at ${root_home_docker_config}" >&2
      exit 1
    fi
  done
else
  if [[ -L /.docker ]]; then
    echo "Docker client configuration directory is a symlink at /.docker" >&2
    exit 1
  fi
  if [[ -d /.docker/config.json && ! -L /.docker/config.json ]]; then
    echo "Docker client authentication path is a directory at /.docker/config.json" >&2
    exit 1
  fi
  rm -f -- /.docker/config.json /.dockercfg
  for stale_root_docker_config in /.docker/config.json /.dockercfg; do
    if [[ -e "${stale_root_docker_config}" || -L "${stale_root_docker_config}" ]]; then
      echo "failed to scrub Docker client authentication at ${stale_root_docker_config}" >&2
      exit 1
    fi
  done
fi

for credential_home in "${credential_homes[@]}"; do
  # The image-build pass removes the entire source-image client directory.
  # The runtime pass is deliberately narrower: Docker Sandboxes may keep
  # private runtime state in .docker/sandbox/locks, so never remove that tree
  # after the sandbox has booted. Workflow-created files are never removed by
  # the runtime pass because it runs before registration and before any job.
  if [[ "${mode}" == "--build" ]]; then
    rm -rf -- "${credential_home}/.docker"
  else
    docker_config_dir="${credential_home}/.docker"
    if [[ -L "${docker_config_dir}" ]]; then
      echo "Docker client configuration directory is a symlink at ${docker_config_dir}" >&2
      exit 1
    fi
    runtime_config="${credential_home}/.docker/config.json"
    if [[ -d "${runtime_config}" && ! -L "${runtime_config}" ]]; then
      echo "Docker client authentication path is a directory at ${runtime_config}" >&2
      exit 1
    fi
    rm -f -- "${runtime_config}"
  fi
  rm -f -- "${credential_home}/.dockercfg"
  for stale_docker_config in "${credential_home}/.docker/config.json" "${credential_home}/.dockercfg"; do
    if [[ -e "${stale_docker_config}" || -L "${stale_docker_config}" ]]; then
      echo "failed to scrub Docker client authentication at ${stale_docker_config}" >&2
      exit 1
    fi
  done
done
