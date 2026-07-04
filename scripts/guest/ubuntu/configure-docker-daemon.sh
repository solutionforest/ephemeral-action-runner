#!/usr/bin/env bash
set -euo pipefail

MIRRORS="${EPAR_DOCKER_REGISTRY_MIRRORS:-}"

if [[ -z "${MIRRORS}" ]]; then
  exit 0
fi

if ! command -v docker >/dev/null 2>&1; then
  echo "Docker is not installed; skipping Docker registry mirror configuration"
  exit 0
fi

if ! command -v jq >/dev/null 2>&1; then
  echo "jq is required to merge Docker daemon registry mirror configuration" >&2
  exit 1
fi

install -d -m 0755 /etc/docker

mirrors_json="$(printf '%s\n' "${MIRRORS}" | jq -R -s 'split("\n") | map(select(length > 0))')"
tmp="$(mktemp)"

if [[ -s /etc/docker/daemon.json ]]; then
  jq --argjson mirrors "${mirrors_json}" '. + {"registry-mirrors": $mirrors}' /etc/docker/daemon.json >"${tmp}"
else
  jq -n --argjson mirrors "${mirrors_json}" '{"registry-mirrors": $mirrors}' >"${tmp}"
fi

install -m 0644 "${tmp}" /etc/docker/daemon.json
rm -f "${tmp}"

if command -v systemctl >/dev/null 2>&1 && [[ "$(ps -p 1 -o comm= 2>/dev/null || true)" == "systemd" ]]; then
  systemctl restart docker.service
elif [[ -s /var/run/epar-dockerd.pid ]]; then
  kill -HUP "$(cat /var/run/epar-dockerd.pid)"
elif [[ -s /var/run/docker.pid ]]; then
  kill -HUP "$(cat /var/run/docker.pid)"
fi

for attempt in $(seq 1 30); do
  if docker info >/tmp/epar-docker-info.out 2>/tmp/epar-docker-info.err; then
    echo "Docker registry mirrors configured:"
    printf '%s\n' "${MIRRORS}" | sed 's/^/  - /'
    exit 0
  fi
  if [[ "${attempt}" == "30" ]]; then
    cat /tmp/epar-docker-info.err >&2 || true
    echo "Docker daemon did not become ready after registry mirror configuration" >&2
    exit 1
  fi
  sleep 1
done
