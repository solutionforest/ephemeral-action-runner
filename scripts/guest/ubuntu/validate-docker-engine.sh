#!/usr/bin/env bash
set -euo pipefail

if [[ "${EPAR_CONTAINER_IMAGE_BUILD:-false}" == "true" ]]; then
  docker --version
  docker compose version
  docker buildx version
  exit 0
fi

if command -v systemctl >/dev/null 2>&1 && [[ "$(ps -p 1 -o comm= 2>/dev/null || true)" == "systemd" ]]; then
  systemctl stop docker.service docker.socket >/dev/null 2>&1 || true
  rm -f /var/lib/docker/network/files/local-kv.db
  systemctl reset-failed containerd.service docker.service docker.socket >/dev/null 2>&1 || true
  systemctl enable containerd.service docker.service >/dev/null 2>&1 || true
  systemctl start containerd.service >/dev/null 2>&1 || true
  systemctl start docker.socket >/dev/null 2>&1 || true
  systemctl start docker.service >/dev/null 2>&1 || true
fi

for attempt in $(seq 1 45); do
  if docker info >/tmp/docker-info.out 2>/tmp/docker-info.err; then
    cat /tmp/docker-info.out
    break
  fi
  if [[ "${attempt}" == "45" ]]; then
    cat /tmp/docker-info.err >&2 || true
    if command -v systemctl >/dev/null 2>&1; then
      systemctl status containerd.service docker.service --no-pager --full >&2 || true
      journalctl -u containerd.service -u docker.service -n 120 --no-pager >&2 || true
    fi
    echo "Docker daemon did not become ready" >&2
    exit 1
  fi
  sleep 2
done

sudo -u runner -H docker version
sudo -u runner -H docker compose version
sudo -u runner -H docker buildx version
sudo -u runner -H docker run --rm hello-world

