#!/usr/bin/env bash
set -euo pipefail

install -d /var/log /var/run /var/lib/docker
rm -f /var/run/docker.pid

dockerd --host=unix:///var/run/docker.sock >/var/log/epar-dockerd.log 2>&1 &
dockerd_pid="$!"
echo "${dockerd_pid}" >/var/run/epar-dockerd.pid

cleanup() {
  runner_pid="$(cat /var/run/actions-runner.pid 2>/dev/null || true)"
  if [[ -n "${runner_pid}" ]]; then
    kill "${runner_pid}" >/dev/null 2>&1 || true
  fi
  kill "${dockerd_pid}" >/dev/null 2>&1 || true
  wait "${dockerd_pid}" >/dev/null 2>&1 || true
}
trap cleanup TERM INT EXIT

for attempt in $(seq 1 60); do
  if docker info >/dev/null 2>&1; then
    break
  fi
  if [[ "${attempt}" == "60" ]]; then
    cat /var/log/epar-dockerd.log >&2 || true
    echo "inner Docker daemon did not become ready" >&2
    exit 1
  fi
  sleep 1
done

tail -f /dev/null &
wait "$!"
