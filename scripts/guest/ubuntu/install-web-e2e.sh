#!/usr/bin/env bash
set -euo pipefail

UPSTREAM_DIR="${UPSTREAM_DIR:-/opt/epar/upstream/runner-images}"
ARCH="$(dpkg --print-architecture)"

export DEBIAN_FRONTEND=noninteractive
export NEEDRESTART_MODE=l
export NEEDRESTART_SUSPEND=1
export HELPER_SCRIPTS="${UPSTREAM_DIR}/images/ubuntu/scripts/helpers"
export INSTALLER_SCRIPT_FOLDER="/opt/epar"
export IMAGE_OS="${IMAGE_OS:-ubuntu24}"
export IMAGE_VERSION="${IMAGE_VERSION:-epar}"
export TOOLSET_JSON="${TOOLSET_JSON:-/opt/epar/toolset.json}"

bash /opt/epar/install-docker-browser.sh "${UPSTREAM_DIR}"

bash /opt/epar/wait-apt-ready.sh
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
cat >/usr/local/bin/invoke_tests <<'SH'
#!/usr/bin/env bash
echo "epar: skipping upstream invoke_tests $*"
SH
chmod +x /usr/local/bin/invoke_tests
trap 'rm -f /usr/local/bin/apt-get /usr/local/bin/invoke_tests' EXIT

if [[ ! -f "${TOOLSET_JSON}" ]]; then
  install -d "$(dirname "${TOOLSET_JSON}")"
  if [[ "${ARCH}" == "arm64" && -f "${UPSTREAM_DIR}/images/ubuntu/toolsets/toolset-2404-arm64.json" ]]; then
    cp "${UPSTREAM_DIR}/images/ubuntu/toolsets/toolset-2404-arm64.json" "${TOOLSET_JSON}"
  elif [[ -f "${UPSTREAM_DIR}/images/ubuntu/toolsets/toolset-2404.json" ]]; then
    cp "${UPSTREAM_DIR}/images/ubuntu/toolsets/toolset-2404.json" "${TOOLSET_JSON}"
  else
    echo "Missing Ubuntu 24.04 runner-images toolset JSON" >&2
    exit 1
  fi
fi

apt-get update
apt-get install -y --no-install-recommends mysql-client rsync zip

required_node_major="$(jq -r '.node.default // empty' "${TOOLSET_JSON}")"
node_version=""
node_major=""
if command -v node >/dev/null 2>&1 && command -v npm >/dev/null 2>&1; then
  node_version="$(node --version 2>/dev/null || true)"
  node_major="${node_version#v}"
  node_major="${node_major%%.*}"
fi

if [[ "${required_node_major}" =~ ^[0-9]+$ ]] && [[ "${node_major}" =~ ^[0-9]+$ ]] && (( node_major >= required_node_major )); then
  echo "EPAR: using Node.js/npm from the base image (Node ${node_version}; required major ${required_node_major})."
  npm --version
else
  if [[ ! "${required_node_major}" =~ ^[0-9]+$ ]]; then
    echo "EPAR: pinned toolset has no usable Node.js default; installing Node.js/npm from the pinned upstream installer."
  elif [[ ! "${node_major}" =~ ^[0-9]+$ ]]; then
    echo "EPAR: base image Node.js/npm is missing or has an unusable version (${node_version:-unavailable}); installing required major ${required_node_major}."
  else
    echo "EPAR: base image Node.js major ${node_major} is below required major ${required_node_major}; installing from the pinned upstream installer."
  fi
  bash "${UPSTREAM_DIR}/images/ubuntu/scripts/build/install-nodejs.sh"
fi

install -d /opt/epar/features
touch /opt/epar/features/web-e2e
bash /opt/epar/validate-web-e2e.sh
