#!/usr/bin/env bash
set -euo pipefail

systemctl stop docker.service docker.socket >/dev/null 2>&1 || true
rm -f /var/lib/docker/network/files/local-kv.db
systemctl reset-failed containerd.service docker.service docker.socket >/dev/null 2>&1 || true
systemctl enable containerd.service docker.service >/dev/null 2>&1 || true
systemctl start containerd.service >/dev/null 2>&1 || true
systemctl start docker.socket >/dev/null 2>&1 || true
systemctl start docker.service >/dev/null 2>&1 || true

for attempt in $(seq 1 45); do
  if docker info >/tmp/docker-info.out 2>/tmp/docker-info.err; then
    cat /tmp/docker-info.out
    break
  fi
  if [[ "${attempt}" == "45" ]]; then
    cat /tmp/docker-info.err >&2 || true
    systemctl status containerd.service docker.service --no-pager --full >&2 || true
    journalctl -u containerd.service -u docker.service -n 120 --no-pager >&2 || true
    echo "Docker daemon did not become ready" >&2
    exit 1
  fi
  sleep 2
done
docker run --rm hello-world

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

for attempt in 1 2 3; do
  if "${BROWSER}" --headless --no-sandbox --disable-gpu --dump-dom https://www.w3.org/ >/tmp/w3-dom.html 2>/tmp/w3-browser.err &&
    grep -Eiq 'W3C|World Wide Web Consortium' /tmp/w3-dom.html; then
    echo "Browser validation passed with ${BROWSER}"
    exit 0
  fi
  echo "Browser validation attempt ${attempt} failed" >&2
  cat /tmp/w3-browser.err >&2 || true
  sleep $((attempt * 5))
done

echo "Browser validation failed after 3 attempts" >&2
exit 1
