#!/usr/bin/env bash
set -euo pipefail

attempts="${EPAR_DOCKER_HELLO_WORLD_ATTEMPTS:-4}"
delay="${EPAR_DOCKER_HELLO_WORLD_INITIAL_DELAY:-5}"
max_delay="${EPAR_DOCKER_HELLO_WORLD_MAX_DELAY:-40}"

case "${attempts}" in
  ''|*[!0-9]*) attempts=4 ;;
esac
case "${delay}" in
  ''|*[!0-9]*) delay=5 ;;
esac
case "${max_delay}" in
  ''|*[!0-9]*) max_delay=40 ;;
esac

if (( attempts < 1 )); then
  attempts=4
fi
if (( delay < 1 )); then
  delay=5
fi
if (( max_delay < delay )); then
  max_delay="${delay}"
fi

stdout="/tmp/epar-docker-hello-world.out"
stderr="/tmp/epar-docker-hello-world.err"
last_status=1

for attempt in $(seq 1 "${attempts}"); do
  rm -f "${stdout}" "${stderr}"
  set +e
  sudo -u runner -H docker run --rm hello-world >"${stdout}" 2>"${stderr}"
  last_status=$?
  set -e

  if (( last_status == 0 )); then
    cat "${stdout}" || true
    cat "${stderr}" >&2 || true
    exit 0
  fi

  echo "docker hello-world validation attempt ${attempt}/${attempts} failed (exit ${last_status})" >&2
  cat "${stdout}" >&2 || true
  cat "${stderr}" >&2 || true

  if (( attempt == attempts )); then
    echo "docker hello-world validation failed after ${attempts} attempts" >&2
    exit "${last_status}"
  fi

  echo "retrying docker hello-world validation in ${delay}s" >&2
  sleep "${delay}"
  delay=$((delay * 2))
  if (( delay > max_delay )); then
    delay="${max_delay}"
  fi
done

exit "${last_status}"
