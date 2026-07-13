#!/usr/bin/env bash
set -euo pipefail

if [[ "${EPAR_CONTAINER_IMAGE_BUILD:-false}" == "true" ]]; then
  docker --version
  docker compose version
  docker buildx version
else
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
  bash /opt/epar/validate-docker-hello-world.sh
fi

BROWSER=""
for candidate in epar-browser chromium chromium-browser google-chrome /snap/bin/chromium; do
  if command -v "${candidate}" >/dev/null 2>&1; then
    BROWSER="$(command -v "${candidate}")"
    break
  elif [[ -x "${candidate}" ]]; then
    BROWSER="${candidate}"
    break
  fi
done

if [[ -z "${BROWSER}" ]]; then
  echo "No Chromium-compatible browser found" >&2
  exit 1
fi

BROWSER_VALIDATION_HTML="$(mktemp /tmp/epar-browser-validation.XXXXXX.html)"
trap 'rm -f "${BROWSER_VALIDATION_HTML}"' EXIT
printf '%s\n' '<!doctype html><html><body>EPAR browser validation marker</body></html>' >"${BROWSER_VALIDATION_HTML}"
chmod 0644 "${BROWSER_VALIDATION_HTML}"
BROWSER_VALIDATION_URL="file://${BROWSER_VALIDATION_HTML}"

for attempt in 1 2 3; do
  if sudo -u runner -H "${BROWSER}" --headless --no-sandbox --disable-gpu --dump-dom "${BROWSER_VALIDATION_URL}" >/tmp/epar-browser-dom.html 2>/tmp/epar-browser.err &&
    grep -Fq 'EPAR browser validation marker' /tmp/epar-browser-dom.html; then
    echo "Browser validation passed with ${BROWSER}"
    exit 0
  fi
  echo "Browser validation attempt ${attempt} failed" >&2
  cat /tmp/epar-browser.err >&2 || true
  sleep $((attempt * 5))
done

echo "Browser validation failed after 3 attempts" >&2
exit 1
