#!/usr/bin/env bash
set -euo pipefail

# Keep certificate subjects from Git for Windows' MSYS argument rewriting.
# Container command paths below use a double slash, which Linux accepts and
# MSYS leaves untouched; bind arguments still receive normal drive mapping.
export MSYS2_ARG_CONV_EXCL='/CN='

# Disposable Linux-container fixture for the Docker-DinD host-trust contract.
# It creates test-only CAs under a temporary directory and never reads or
# modifies the host's real trust store.

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
mkdir -p "$repo_root/work"
work="$(mktemp -d "$repo_root/work/host-trust-e2e.XXXXXX")"
docker_work="$work"
if [[ "$(uname -s)" == MINGW* ]] && command -v cygpath >/dev/null 2>&1; then
  docker_work="$(cygpath -w "$work")"
fi
id="epar-host-trust-e2e-$$"
network="$id"
image_g1="$id:g1"
image_g2="$id:g2"
server_name="${id}-server"

cleanup() {
  docker rm -f "$server_name" >/dev/null 2>&1 || true
  docker network rm "$network" >/dev/null 2>&1 || true
  docker image rm -f "$image_g1" "$image_g2" >/dev/null 2>&1 || true
  rm -rf "$work"
}
trap cleanup EXIT INT TERM

make_ca() {
  local name="$1"
  MSYS2_ARG_CONV_EXCL='/CN=' openssl req -x509 -newkey rsa:2048 -nodes -days 2 \
    -subj "/CN=EPAR fixture ${name}" \
    -addext 'basicConstraints=critical,CA:TRUE,pathlen:1' \
    -addext 'keyUsage=critical,keyCertSign,cRLSign' \
    -keyout "$work/${name}.key" -out "$work/${name}.crt" >/dev/null 2>&1
}

make_server() {
  local name="$1" ca="$2"
  MSYS2_ARG_CONV_EXCL='/CN=' openssl req -newkey rsa:2048 -nodes \
    -subj '/CN=tls-server' \
    -addext 'subjectAltName=DNS:tls-server' \
    -keyout "$work/${name}.key" -out "$work/${name}.csr" >/dev/null 2>&1
  openssl x509 -req -days 2 -in "$work/${name}.csr" \
    -CA "$work/${ca}.crt" -CAkey "$work/${ca}.key" -CAcreateserial \
    -copy_extensions copy -out "$work/${name}.crt" >/dev/null 2>&1
}

make_ca g1
make_ca g2
make_ca explicit
make_server server-g1 g1
make_server server-g2 g2
make_server server-explicit explicit

mkdir -p "$work/context/scripts"
cp "$repo_root/scripts/guest/ubuntu/apply-trusted-ca-runtime.sh" "$work/context/scripts/"
cp "$repo_root/scripts/guest/ubuntu/check-host-trust-generation.sh" "$work/context/scripts/"
cp "$work/g1.crt" "$work/g2.crt" "$work/explicit.crt" "$work/context/"

cat >"$work/context/Dockerfile" <<'DOCKERFILE'
FROM ubuntu:24.04
ARG HOST_ROOT
ARG GENERATION
RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      ca-certificates curl nodejs npm python3 python3-requests \
 && rm -rf /var/lib/apt/lists/*
RUN install -d -m 0755 /usr/local/share/ca-certificates/epar-host \
                         /usr/local/share/ca-certificates/epar /opt/epar
COPY ${HOST_ROOT}.crt /usr/local/share/ca-certificates/epar-host/host.crt
COPY explicit.crt /usr/local/share/ca-certificates/epar/explicit.crt
COPY scripts/apply-trusted-ca-runtime.sh /opt/epar/apply-trusted-ca-runtime.sh
COPY scripts/check-host-trust-generation.sh /opt/epar/check-host-trust-generation.sh
RUN update-ca-certificates \
 && printf '{"schemaVersion":1,"generation":"%s","hostOS":"linux","mode":"overlay","scopes":["system"]}\n' "$GENERATION" >/opt/epar/host-trust-generation.json \
 && chmod 0755 /opt/epar/*.sh
CMD ["sleep", "infinity"]
DOCKERFILE

docker build --quiet --build-arg HOST_ROOT=g1 --build-arg GENERATION=g1 -t "$image_g1" "$work/context" >/dev/null
docker build --quiet --build-arg HOST_ROOT=g2 --build-arg GENERATION=g2 -t "$image_g2" "$work/context" >/dev/null
docker network create "$network" >/dev/null

cat >"$work/https-server.py" <<'PY'
import http.server
import ssl
import sys

class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        payload = b"{}\n"
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def log_message(self, *_):
        pass

server = http.server.ThreadingHTTPServer(("0.0.0.0", 4443), Handler)
context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
context.load_cert_chain(sys.argv[1], sys.argv[2])
server.socket = context.wrap_socket(server.socket, server_side=True)
server.serve_forever()
PY

start_server() {
  local certificate="$1"
  docker rm -f "$server_name" >/dev/null 2>&1 || true
  docker run -d --name "$server_name" --network "$network" --network-alias tls-server \
    -v "$docker_work:/fixture:ro" "$image_g2" \
    python3 //fixture/https-server.py "//fixture/${certificate}.crt" "//fixture/${certificate}.key" >/dev/null
  for _ in $(seq 1 30); do
    docker exec "$server_name" python3 -c 'import socket; socket.create_connection(("127.0.0.1", 4443), 1).close()' 2>/dev/null && return 0
    sleep 1
  done
  echo "fixture TLS server did not become ready" >&2
  docker ps -a --filter "name=^/${server_name}$" >&2 || true
  docker logs "$server_name" >&2 || true
  return 1
}

assert_clients() {
  local image="$1"
  docker run --rm --network "$network" "$image" bash -euc '
    source /opt/epar/apply-trusted-ca-runtime.sh
    curl -fsS https://tls-server:4443/ >/dev/null
    node -e '\''require("https").get("https://tls-server:4443/", r => { if (r.statusCode !== 200) process.exit(1); r.resume() }).on("error", e => { console.error(e); process.exit(1) })'\''
    npm ping --registry=https://tls-server:4443/ >/dev/null
    python3 -c '\''import requests; r=requests.get("https://tls-server:4443/", timeout=5); r.raise_for_status()'\''
  '
}

write_lease() {
  local generation="$1"
  local expires
  expires="$(date -u -d '+60 seconds' +'%Y-%m-%dT%H:%M:%SZ')"
  printf '{"schemaVersion":1,"generation":"%s","hostOS":"linux","mode":"overlay","scopes":["system"],"expiresAt":"%s"}\n' \
    "$generation" "$expires" >"$work/lease.json"
}

start_server server-g1
assert_clients "$image_g1"

# G1 must fail closed when a G2 lease races assignment to the stale runner.
write_lease g2
if docker run --rm -v "$docker_work/lease.json:/run/epar/host-trust-lease.json:ro" "$image_g1" \
  //opt/epar/check-host-trust-generation.sh >/dev/null 2>&1; then
  echo "stale G1 image accepted the G2 controller lease" >&2
  exit 1
fi

start_server server-g2
write_lease g2
docker run --rm -v "$docker_work/lease.json:/run/epar/host-trust-lease.json:ro" "$image_g2" \
  //opt/epar/check-host-trust-generation.sh >/dev/null
assert_clients "$image_g2"

# Removing G1 from the host overlay removes that trust unless another source
# (Ubuntu itself or the explicit EPAR directory) still supplies it.
start_server server-g1
if docker run --rm --network "$network" "$image_g2" curl -fsS https://tls-server:4443/ >/dev/null 2>&1; then
  echo "G2 image unexpectedly retained the removed G1 fixture root" >&2
  exit 1
fi

# The explicitly configured root is unioned independently and survives host
# generation rotation/removal.
start_server server-explicit
assert_clients "$image_g2"

echo "host-trust Docker E2E passed: G1 clients, stale hook, G2 rotation, host-root removal, explicit-root retention"
