#!/usr/bin/env bash
set -euo pipefail

if [[ "$(id -u)" != "0" ]]; then
  echo "Docker Sandboxes native architecture helper must run as root" >&2
  exit 1
fi

expected_platform="${1:-}"
case "${expected_platform}" in
  linux/amd64) expected_arch=x86_64 ;;
  linux/arm64) expected_arch=aarch64 ;;
  *)
    echo "unsupported Docker Sandboxes native platform: ${expected_platform}" >&2
    exit 1
    ;;
esac

kernel_arch="$(uname -m)"
if [[ "${kernel_arch}" != "${expected_arch}" ]]; then
  echo "Docker Sandboxes guest architecture ${kernel_arch} does not match configured platform ${expected_platform}" >&2
  exit 1
fi

docker_arch="$(docker info --format '{{.Architecture}}')"
if [[ "${docker_arch}" != "${expected_arch}" ]]; then
  echo "Docker Sandboxes private Docker architecture ${docker_arch} does not match configured platform ${expected_platform}" >&2
  exit 1
fi

epar_handler_count=0
binfmt_root=/proc/sys/fs/binfmt_misc
if [[ -r "${binfmt_root}/status" ]]; then
  shopt -s nullglob
  for handler in "${binfmt_root}"/qemu-*; do
    [[ -f "${handler}" ]] || continue
    interpreter="$(awk '$1 == "interpreter" { print $2; exit }' "${handler}")"
    case "${interpreter}" in
      /opt/epar/emulation/qemu-*) epar_handler_count=$((epar_handler_count + 1)) ;;
    esac
  done
fi

printf '{"backend":"native","handlerCount":%d,"platform":"%s"}\n' "${epar_handler_count}" "${expected_platform}"
