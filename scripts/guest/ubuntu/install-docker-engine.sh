#!/usr/bin/env bash
set -euo pipefail

UPSTREAM_DIR="${1:-/opt/epar/upstream/runner-images}"
ARCH="$(dpkg --print-architecture)"

export DEBIAN_FRONTEND=noninteractive
export NEEDRESTART_MODE=l
export NEEDRESTART_SUSPEND=1
APT_LOCK_TIMEOUT="${EPAR_APT_LOCK_TIMEOUT:-600}"
bash /opt/epar/wait-apt-ready.sh
apt-get -o "DPkg::Lock::Timeout=${APT_LOCK_TIMEOUT}" update
apt-get -o "DPkg::Lock::Timeout=${APT_LOCK_TIMEOUT}" install -y --no-install-recommends ca-certificates curl git gnupg jq lsb-release sudo tar unzip wget

install -d /opt/epar
cat >/usr/local/bin/apt-get <<'SH'
#!/usr/bin/env bash
set -euo pipefail
timeout="${EPAR_APT_LOCK_TIMEOUT:-600}"
if [[ "${1:-}" == "install" ]]; then
  shift
  exec /usr/bin/apt-get -o "DPkg::Lock::Timeout=${timeout}" install -y "$@"
fi
exec /usr/bin/apt-get -o "DPkg::Lock::Timeout=${timeout}" "$@"
SH
chmod +x /usr/local/bin/apt-get
trap 'rm -f /usr/local/bin/apt-get /usr/local/bin/docker /usr/local/bin/invoke_tests' EXIT

cat >/usr/local/bin/invoke_tests <<'SH'
#!/usr/bin/env bash
echo "epar: skipping upstream invoke_tests $*"
SH
chmod +x /usr/local/bin/invoke_tests

export HELPER_SCRIPTS="${UPSTREAM_DIR}/images/ubuntu/scripts/helpers"
export INSTALLER_SCRIPT_FOLDER="/opt/epar"
export IMAGE_OS="${IMAGE_OS:-ubuntu24}"
export IMAGE_VERSION="${IMAGE_VERSION:-epar}"

if [[ ! -d "${HELPER_SCRIPTS}" ]]; then
  echo "Missing runner-images helper scripts at ${HELPER_SCRIPTS}" >&2
  exit 1
fi

if [[ "${ARCH}" == "arm64" ]]; then
  if [[ -f "${UPSTREAM_DIR}/images/ubuntu/toolsets/toolset-2404-arm64.json" ]]; then
    cp "${UPSTREAM_DIR}/images/ubuntu/toolsets/toolset-2404-arm64.json" /opt/epar/toolset.json
  else
    cat >/opt/epar/toolset.json <<'JSON'
{
  "docker": {
    "components": [
      {"package": "containerd.io", "version": "latest"},
      {"package": "docker-ce-cli", "version": "latest"},
      {"package": "docker-ce", "version": "latest"}
    ],
    "plugins": [
      {"plugin": "buildx", "version": "latest", "asset": "linux-arm64"},
      {"plugin": "compose", "version": "latest", "asset": "linux-aarch64"}
    ]
  }
}
JSON
  fi
else
  cp "${UPSTREAM_DIR}/images/ubuntu/toolsets/toolset-2404.json" /opt/epar/toolset.json
fi
export TOOLSET_JSON="/opt/epar/toolset.json"

if [[ "${EPAR_SKIP_UPSTREAM_DOCKER_IMAGE_CACHE:-true}" == "true" ]]; then
  cat >/usr/local/bin/docker <<'SH'
#!/usr/bin/env bash
set -euo pipefail
if [[ "${1:-}" == "pull" ]]; then
  case "${2:-}" in
    ghcr.io/dependabot/*|ghcr.io/github/gh-aw-*|ghcr.io/github/github-mcp-server:*)
      echo "epar: skipping upstream Docker image cache pull $2"
      exit 0
      ;;
  esac
fi
exec /usr/bin/docker "$@"
SH
  chmod +x /usr/local/bin/docker
fi

bash "${UPSTREAM_DIR}/images/ubuntu/scripts/build/install-docker.sh"
rm -f /usr/local/bin/docker

usermod -aG docker admin 2>/dev/null || true
usermod -aG docker runner 2>/dev/null || true

install -d /opt/epar/features
touch /opt/epar/features/docker-engine
if [[ "${EPAR_CONTAINER_IMAGE_BUILD:-false}" != "true" ]]; then
  systemctl enable containerd.service docker.service
  systemctl enable docker.socket >/dev/null 2>&1 || true
  systemctl restart containerd.service
  systemctl restart docker.service
fi
bash /opt/epar/validate-docker-engine.sh

