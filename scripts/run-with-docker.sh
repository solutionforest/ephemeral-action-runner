#!/usr/bin/env bash
set -euo pipefail

export EPAR_INVOCATION="${EPAR_INVOCATION:-run-with-docker}"

case "$(uname -s)" in
  MINGW*|MSYS*|CYGWIN*)
    script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
    ps_script="$script_dir/run-with-docker.ps1"
    command -v cygpath >/dev/null 2>&1 && ps_script="$(cygpath -w "$ps_script")"
    exec powershell.exe -NoLogo -NoProfile -ExecutionPolicy Bypass -File "$ps_script" "$@"
    ;;
esac

# The legacy controller-in-Docker path bypassed host-native diagnostics and
# Docker Sandboxes support. It is deliberately removed: Docker is now only a
# compiler backend for the project-local native controller.
if [[ "${EPAR_LEGACY_CONTROLLER_IN_DOCKER:-0}" == "1" ]]; then
  echo 'EPAR_LEGACY_CONTROLLER_IN_DOCKER=1 is no longer supported. Remove it and run ./start so EPAR builds and executes its project-local native controller.' >&2
  exit 1
fi

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
exec env EPAR_NATIVE_CONTROLLER_BACKEND=docker "$script_dir/build-native-controller.sh" "$@"
