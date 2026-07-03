#!/usr/bin/env bash
set -euo pipefail

systemctl stop docker.service docker.socket containerd.service >/dev/null 2>&1 || true
systemctl reset-failed docker.service docker.socket containerd.service >/dev/null 2>&1 || true

# Optional Docker-enabled images validate Docker before cloning. Docker records
# the default bridge network in local-kv.db; clearing it lets each clone recreate
# a clean docker0 bridge on first boot.
rm -f /var/lib/docker/network/files/local-kv.db

rm -rf /tmp/epar-* /tmp/w3-dom.html /tmp/w3-browser.err /tmp/docker-info.out /tmp/docker-info.err
sync
