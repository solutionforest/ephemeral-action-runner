#!/usr/bin/env bash
set -euo pipefail

if [[ "$(id -u)" != "0" ]]; then
  echo "Docker Sandboxes architecture emulation helper must run as root" >&2
  exit 1
fi

emulation_root=/opt/epar/emulation
binfmt_root=/proc/sys/fs/binfmt_misc
install_output=""

if [[ "$(findmnt -T "${binfmt_root}" -n -o FSTYPE)" != "binfmt_misc" ]]; then
  if ! mount_output="$(mount -t binfmt_misc binfmt_misc "${binfmt_root}" 2>&1)"; then
    [[ -z "${mount_output}" ]] || printf '%s\n' "${mount_output}" >&2
    echo "failed to mount binfmt_misc for QEMU handler registration" >&2
    exit 1
  fi
fi

if ! install_output="$(env QEMU_BINARY_PATH="${emulation_root}" QEMU_PRESERVE_ARGV0=1 "${emulation_root}/binfmt" --install all 2>&1)"; then
  [[ -z "${install_output}" ]] || printf '%s\n' "${install_output}" >&2
  echo "failed to install bundled QEMU binfmt handlers" >&2
  exit 1
fi

if [[ ! -r "${binfmt_root}/status" ]] || [[ "$(<"${binfmt_root}/status")" != "enabled" ]]; then
  [[ -z "${install_output}" ]] || printf '%s\n' "${install_output}" >&2
  echo "binfmt_misc is unavailable or disabled after QEMU handler installation" >&2
  exit 1
fi

handler_count=0
shopt -s nullglob
for handler in "${binfmt_root}"/qemu-*; do
  [[ -f "${handler}" ]] || continue
  grep -Fx enabled "${handler}" >/dev/null || continue
  interpreter="$(awk '$1 == "interpreter" { print $2; exit }' "${handler}")"
  case "${interpreter}" in
    "${emulation_root}"/qemu-*) handler_count=$((handler_count + 1)) ;;
  esac
done

if (( handler_count == 0 )); then
  [[ -z "${install_output}" ]] || printf '%s\n' "${install_output}" >&2
  echo "no enabled bundled QEMU binfmt handlers were registered" >&2
  exit 1
fi

printf '{"backend":"qemu","handlerCount":%d}\n' "${handler_count}"
