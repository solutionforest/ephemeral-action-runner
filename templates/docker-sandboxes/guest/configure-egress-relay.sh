#!/usr/bin/env bash
set -euo pipefail
umask 077

if [[ "$(id -u)" != "0" ]]; then
  echo "configure-egress-relay.sh must run as root" >&2
  exit 1
fi

config_dir="/run/epar"
config_path="${config_dir}/egress-relay.json"
daemon_config="/etc/docker/daemon.json"
bridge_proxy="http://127.0.0.1:3129"
daemon_no_proxy="localhost,127.0.0.1,::1,host.docker.internal,gateway.docker.internal,kubernetes.docker.internal,host.containers.internal,10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,169.254.0.0/16,.local"
dockerd_log="/var/log/docker/epar-dockerd.log"
daemon_backup="${config_dir}/docker-daemon.pre-relay.json"
relay_ca_source="${config_dir}/egress-relay-ca.crt"
relay_ca_key="${config_dir}/egress-relay-ca.key"
relay_ca_trust="/usr/local/share/ca-certificates/epar/epar-egress-relay.crt"
daemon_mutated=false
dockerd_restart_attempted=false
started_dockerd_pid=""
relay_ca_changed=false
relay_operation="activation"
relay_stage="bootstrap"
cleanup_sensitive_staging() {
  rm -f "${config_path}.input" "${config_path}.tmp" "${config_path}.new" "${daemon_config}.tmp" "${daemon_config}.new" "${daemon_config}.rollback.new"
}

start_private_dockerd() {
  install -d -m 0755 -o root -g root /var/log/docker
  env -i HOME=/root PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin LANG=C.UTF-8 GODEBUG=tlsmlkem=0,tlssecpmlkem=0 \
    nohup /usr/bin/dockerd --config-file "${daemon_config}" >>"${dockerd_log}" 2>&1 </dev/null &
  started_dockerd_pid="$!"
  for attempt in $(seq 1 120); do
    if ! kill -0 "${started_dockerd_pid}" >/dev/null 2>&1; then
      return 1
    fi
    if docker info >/dev/null 2>&1; then
      return 0
    fi
    if [[ "${attempt}" == "120" ]]; then
      return 1
    fi
    sleep 1
  done
  return 1
}

remove_relay_ca_trust() {
  if [[ ! -e "${relay_ca_trust}" ]]; then
    return 0
  fi
  [[ -f "${relay_ca_trust}" && ! -L "${relay_ca_trust}" ]] || return 1
  cmp -s "${relay_ca_source}" "${relay_ca_trust}" || return 1
  rm -f "${relay_ca_trust}"
  update-ca-certificates >/dev/null
}

rollback_daemon() {
  if [[ "${daemon_mutated}" != "true" || ! -f "${daemon_backup}" ]]; then
    return 0
  fi
  echo "EPAR host-trust relay: restoring the private Docker daemon after failed activation" >&2
  install -m 0644 -o root -g root "${daemon_backup}" "${daemon_config}.rollback.new" || return 1
  mv -f "${daemon_config}.rollback.new" "${daemon_config}" || return 1
  if [[ "${dockerd_restart_attempted}" != "true" ]]; then
    return 0
  fi
  mapfile -t rollback_pids < <(pgrep -x dockerd || true)
  if [[ "${#rollback_pids[@]}" -gt "1" ]]; then
    return 1
  fi
  if [[ "${#rollback_pids[@]}" == "1" ]]; then
    rollback_pid="${rollback_pids[0]}"
    if [[ "$(readlink -f "/proc/${rollback_pid}/exe" 2>/dev/null || true)" != "/usr/bin/dockerd" ]]; then
      return 1
    fi
    kill -TERM "${rollback_pid}" || return 1
    for _ in $(seq 1 60); do
      if ! kill -0 "${rollback_pid}" >/dev/null 2>&1; then
        break
      fi
      sleep 1
    done
    if kill -0 "${rollback_pid}" >/dev/null 2>&1; then
      return 1
    fi
  fi
  start_private_dockerd
}

on_exit() {
  status="$?"
  trap - EXIT
  set +e
  if [[ "${status}" != "0" ]]; then
    failed_stage="${relay_stage}"
    echo "EPAR host-trust relay: ${relay_operation} failed at ${failed_stage} (exit=${status})" >&2
    relay_stage="rollback-daemon"
    if ! rollback_daemon; then
      echo "EPAR host-trust relay: private Docker daemon rollback failed" >&2
      status=1
    else
      rm -f "${daemon_backup}"
    fi
    relay_stage="rollback-relay-ca"
    if [[ "${relay_ca_changed}" == "true" ]] && ! remove_relay_ca_trust; then
      echo "EPAR host-trust relay: local TLS authority rollback failed" >&2
      status=1
    fi
  fi
  cleanup_sensitive_staging
  exit "${status}"
}
trap on_exit EXIT

relay_stage="bootstrap"
for command_name in cmp curl docker install jq pgrep python3 readlink stat update-ca-certificates; do
  command -v "${command_name}" >/dev/null 2>&1 || {
    echo "EPAR host-trust relay: required command ${command_name} is unavailable" >&2
    exit 1
  }
done
relay_stage="validate-daemon-config"
[[ -f "${daemon_config}" && ! -L "${daemon_config}" ]]
[[ "$(stat -c '%U:%G:%a' "${daemon_config}")" == "root:root:644" ]]
install -d -m 0755 -o root -g root "${config_dir}"
mode="${1:-activate}"
if [[ "$#" -gt "1" || ( "${mode}" != "activate" && "${mode}" != "--commit" && "${mode}" != "--rollback" ) ]]; then
  echo "EPAR host-trust relay: unsupported transaction operation" >&2
  exit 1
fi
case "${mode}" in
  activate) relay_operation="activation" ;;
  --commit) relay_operation="commit" ;;
  --rollback) relay_operation="rollback" ;;
esac
if [[ "${mode}" == "--commit" ]]; then
  relay_stage="commit"
  rm -f "${daemon_backup}"
  exit 0
fi
if [[ "${mode}" == "--rollback" ]]; then
  relay_stage="rollback-daemon"
  rm -f /run/epar/egress-relay-active "${config_path}"
  if [[ -f "${daemon_backup}" ]]; then
    if [[ -n "$(docker ps -aq)" ]]; then
      echo "EPAR host-trust relay: refusing daemon rollback after containers exist" >&2
      exit 1
    fi
    daemon_mutated=true
    dockerd_restart_attempted=true
    if ! rollback_daemon; then
      daemon_mutated=false
      echo "EPAR host-trust relay: private Docker daemon rollback failed" >&2
      exit 1
    fi
    daemon_mutated=false
    dockerd_restart_attempted=false
    rm -f "${daemon_backup}"
  fi
  remove_relay_ca_trust
  exit 0
fi
if [[ "$#" != "0" ]]; then
  echo "EPAR host-trust relay: activation does not accept arguments" >&2
  exit 1
fi
relay_stage="validate-guest-relay"
[[ -x /opt/epar/epar-egress-bridge ]]
[[ -s /opt/epar/trust/ca-bundle.pem && ! -L /opt/epar/trust/ca-bundle.pem ]]
[[ -s "${relay_ca_source}" && ! -L "${relay_ca_source}" ]]
[[ "$(stat -c '%U:%G:%a' "${relay_ca_source}")" == "root:root:444" ]]
[[ -s "${relay_ca_key}" && ! -L "${relay_ca_key}" ]]
[[ "$(stat -c '%U:%G:%a' "${relay_ca_key}")" == "root:root:600" ]]
[[ -f /run/epar/egress-bridge.pid && ! -L /run/epar/egress-bridge.pid ]]
[[ "$(stat -c '%U:%G:%a' /run/epar/egress-bridge.pid)" == "root:root:444" ]]
bridge_pid="$(cat /run/epar/egress-bridge.pid)"
[[ "${bridge_pid}" =~ ^[1-9][0-9]*$ ]]
[[ "$(readlink -f "/proc/${bridge_pid}/exe" 2>/dev/null || true)" == "/opt/epar/epar-egress-bridge" ]]
relay_stage="install-relay-ca"
install -d -m 0755 -o root -g root "$(dirname "${relay_ca_trust}")"
if [[ -e "${relay_ca_trust}" ]]; then
  [[ -f "${relay_ca_trust}" && ! -L "${relay_ca_trust}" ]]
  cmp -s "${relay_ca_source}" "${relay_ca_trust}"
else
  install -m 0444 -o root -g root "${relay_ca_source}" "${relay_ca_trust}"
  update-ca-certificates >/dev/null
  relay_ca_changed=true
fi
relay_stage="write-relay-config"
rm -f /run/epar/egress-relay-active
rm -f "${config_path}.input" "${config_path}.tmp" "${config_path}.new"
install -m 0600 -o root -g root /dev/null "${config_path}.input"
head -c 8193 >"${config_path}.input"
python3 -I -S - "${config_path}.input" "${config_path}.tmp" <<'PY'
import base64
import json
import os
import re
import sys

source, destination = sys.argv[1:]
with open(source, "rb") as handle:
    raw = handle.read(8193)
if not raw or len(raw) > 8192:
    raise SystemExit("EPAR host-trust relay: invalid configuration size")
try:
    def strict_object(pairs):
        value = {}
        for key, item in pairs:
            if key in value:
                raise ValueError("duplicate key")
            value[key] = item
        return value
    value = json.loads(raw, object_pairs_hook=strict_object)
except Exception:
    raise SystemExit("EPAR host-trust relay: invalid configuration")
if not isinstance(value, dict) or set(value) != {"schemaVersion", "relayAddress", "token"}:
    raise SystemExit("EPAR host-trust relay: unsupported configuration schema")
if value.get("schemaVersion") != 1:
    raise SystemExit("EPAR host-trust relay: unsupported configuration version")
address = value.get("relayAddress")
if not isinstance(address, str) or not re.fullmatch(r"host\.docker\.internal:[1-9][0-9]{0,4}", address):
    raise SystemExit("EPAR host-trust relay: invalid relay address")
port = int(address.rsplit(":", 1)[1])
if port > 65535:
    raise SystemExit("EPAR host-trust relay: invalid relay port")
token = value.get("token")
if not isinstance(token, str) or len(token) != 43:
    raise SystemExit("EPAR host-trust relay: invalid token")
try:
    decoded = base64.urlsafe_b64decode(token + "=")
except Exception:
    raise SystemExit("EPAR host-trust relay: invalid token")
if len(decoded) != 32 or base64.urlsafe_b64encode(decoded).decode().rstrip("=") != token:
    raise SystemExit("EPAR host-trust relay: invalid token")
with open(destination, "x", encoding="utf-8") as handle:
    json.dump(value, handle, separators=(",", ":"), sort_keys=True)
    handle.write("\n")
os.chmod(destination, 0o600)
PY
rm -f "${config_path}.input"
install -m 0600 -o root -g root "${config_path}.tmp" "${config_path}.new"
rm -f "${config_path}.tmp"
mv -f "${config_path}.new" "${config_path}"
relay_stage="publish-relay-config"
[[ "$(stat -c '%U:%G:%a' "${config_path}")" == "root:root:600" && ! -L "${config_path}" ]]

relay_stage="guest-bridge-health"
health_code="$(curl --silent --show-error --output /dev/null --write-out '%{http_code}' --max-time 15 "http://127.0.0.1:3129/health")"
if [[ "${health_code}" != "204" ]]; then
  echo "EPAR host-trust relay: authenticated guest bridge health check failed" >&2
  exit 1
fi

relay_stage="recover-daemon-transaction"
if [[ -e "${daemon_backup}" ]]; then
  if [[ -n "$(docker ps -aq)" ]]; then
    echo "EPAR host-trust relay: unfinished daemon transaction cannot be recovered after containers exist" >&2
    exit 1
  fi
  daemon_mutated=true
  dockerd_restart_attempted=true
  if ! rollback_daemon; then
    daemon_mutated=false
    echo "EPAR host-trust relay: unfinished daemon transaction rollback failed" >&2
    exit 1
  fi
  daemon_mutated=false
  dockerd_restart_attempted=false
  rm -f "${daemon_backup}"
fi

relay_stage="detect-private-dockerd"
daemon_already_configured=false
if docker info >/dev/null 2>&1 \
  && [[ -z "$(docker info --format '{{.HTTPProxy}}')" ]] \
  && [[ "$(docker info --format '{{.HTTPSProxy}}')" == "${bridge_proxy}" ]] \
  && [[ "$(docker info --format '{{.NoProxy}}')" == "${daemon_no_proxy}" ]] \
  && daemon_pid="$(pgrep -x dockerd)" \
  && [[ -n "${daemon_pid}" ]] \
  && tr '\0' '\n' <"/proc/${daemon_pid}/environ" | grep -Fx 'GODEBUG=tlsmlkem=0,tlssecpmlkem=0' >/dev/null \
  && jq -e --arg proxy "${bridge_proxy}" --arg no_proxy "${daemon_no_proxy}" '((keys - ["proxies", "registry-mirrors"]) | length) == 0 and .proxies == {"https-proxy": $proxy, "no-proxy": $no_proxy}' "${daemon_config}" >/dev/null; then
  daemon_already_configured=true
fi

relay_stage="configure-private-dockerd"
if [[ "${daemon_already_configured}" != "true" ]]; then
  install -m 0600 -o root -g root "${daemon_config}" "${daemon_backup}"
  jq --arg proxy "${bridge_proxy}" --arg no_proxy "${daemon_no_proxy}" '
    if type != "object" or ((keys - ["proxies", "registry-mirrors"]) | length) != 0 then
      error("unsupported Docker daemon configuration")
    else
      .proxies = {"https-proxy": $proxy, "no-proxy": $no_proxy}
    end
  ' "${daemon_config}" >"${daemon_config}.tmp"
  install -m 0644 -o root -g root "${daemon_config}.tmp" "${daemon_config}.new"
  rm -f "${daemon_config}.tmp"
  mv -f "${daemon_config}.new" "${daemon_config}"
  daemon_mutated=true
  mapfile -t dockerd_pids < <(pgrep -x dockerd || true)
  if [[ "${#dockerd_pids[@]}" != "1" ]]; then
    echo "EPAR host-trust relay: expected exactly one private dockerd before restart" >&2
    exit 1
  fi
  dockerd_pid="${dockerd_pids[0]}"
  if [[ "$(readlink -f "/proc/${dockerd_pid}/exe" 2>/dev/null || true)" != "/usr/bin/dockerd" ]]; then
    echo "EPAR host-trust relay: refusing to restart an unexpected dockerd executable" >&2
    exit 1
  fi
  if [[ -n "$(docker ps -aq)" ]]; then
    echo "EPAR host-trust relay: refusing to restart dockerd after containers exist" >&2
    exit 1
  fi
  relay_stage="restart-private-dockerd"
  dockerd_restart_attempted=true
  kill -TERM "${dockerd_pid}"
  for _ in $(seq 1 60); do
    if ! kill -0 "${dockerd_pid}" >/dev/null 2>&1; then
      break
    fi
    sleep 1
  done
  if kill -0 "${dockerd_pid}" >/dev/null 2>&1; then
    echo "EPAR host-trust relay: private dockerd did not stop cleanly" >&2
    exit 1
  fi

  if ! start_private_dockerd; then
    echo "EPAR host-trust relay: restarted private dockerd did not become ready; inspect ${dockerd_log}" >&2
    exit 1
  fi
fi

relay_stage="private-dockerd-contract"
mapfile -t dockerd_pids < <(pgrep -x dockerd || true)
[[ "${#dockerd_pids[@]}" == "1" ]]
[[ "$(readlink -f "/proc/${dockerd_pids[0]}/exe" 2>/dev/null || true)" == "/usr/bin/dockerd" ]]
tr '\0' '\n' <"/proc/${dockerd_pids[0]}/environ" | grep -Fx 'GODEBUG=tlsmlkem=0,tlssecpmlkem=0' >/dev/null
[[ -z "$(docker info --format '{{.HTTPProxy}}')" ]]
[[ "$(docker info --format '{{.HTTPSProxy}}')" == "${bridge_proxy}" ]]
[[ "$(docker info --format '{{.NoProxy}}')" == "${daemon_no_proxy}" ]]

relay_stage="registry-tls-proof"
registry_status="$(curl --silent --show-error --output /dev/null --write-out '%{http_code}' --max-time 30 --proxy "${bridge_proxy}" --noproxy '' --cacert "${relay_ca_trust}" https://registry-1.docker.io/v2/)"
if [[ "${registry_status}" != "401" ]]; then
  echo "EPAR host-trust relay: registry TLS proof returned HTTP ${registry_status}" >&2
  exit 1
fi

relay_stage="publish-active-marker"
install -m 0444 -o root -g root /dev/null /run/epar/egress-relay-active
echo "EPAR host-trust relay: authenticated host-trust transport is active"
