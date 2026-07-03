#!/usr/bin/env bash
set -euo pipefail

UPSTREAM_DIR="${1:-/opt/epar/upstream/runner-images}"
ARCH="$(dpkg --print-architecture)"

export DEBIAN_FRONTEND=noninteractive

if [[ "${ARCH}" == "amd64" ]]; then
  export HELPER_SCRIPTS="${UPSTREAM_DIR}/images/ubuntu/scripts/helpers"
  export INSTALLER_SCRIPT_FOLDER="/opt/epar"
  export IMAGE_OS="${IMAGE_OS:-ubuntu24}"
  export IMAGE_VERSION="${IMAGE_VERSION:-epar}"
  if [[ ! -f /opt/epar/toolset.json ]]; then
    cp "${UPSTREAM_DIR}/images/ubuntu/toolsets/toolset-2404.json" /opt/epar/toolset.json
  fi
  bash "${UPSTREAM_DIR}/images/ubuntu/scripts/build/install-google-chrome.sh"
  exit 0
fi

echo "Installing ARM64 Chromium-compatible browser"
apt-get update
apt-get install -y --no-install-recommends nodejs npm

install -d /opt/epar/browser /opt/epar/ms-playwright
cd /opt/epar/browser
if [[ ! -f package.json ]]; then
  npm init -y
fi
npm install playwright
PLAYWRIGHT_BROWSERS_PATH=/opt/epar/ms-playwright ./node_modules/.bin/playwright install chromium --with-deps

cat >/opt/epar/browser/dump-dom.js <<'JS'
const { chromium } = require('/opt/epar/browser/node_modules/playwright');

const url = process.argv[2];
if (!url) {
  console.error('usage: chromium --headless --dump-dom <url>');
  process.exit(2);
}

(async () => {
  const browser = await chromium.launch({ headless: true, args: ['--no-sandbox'] });
  try {
    const page = await browser.newPage();
    await page.goto(url, { waitUntil: 'domcontentloaded', timeout: 60000 });
    console.log(await page.content());
  } finally {
    await browser.close();
  }
})().catch((err) => {
  console.error(err);
  process.exit(1);
});
JS

cat >/usr/local/bin/epar-browser <<'SH'
#!/usr/bin/env bash
set -euo pipefail
url=""
for arg in "$@"; do
  case "$arg" in
    http://*|https://*|file://*) url="$arg" ;;
  esac
done
if [[ -z "$url" ]]; then
  url="${@: -1}"
fi
exec env PLAYWRIGHT_BROWSERS_PATH=/opt/epar/ms-playwright node /opt/epar/browser/dump-dom.js "$url"
SH
chmod +x /usr/local/bin/epar-browser
ln -sf /usr/local/bin/epar-browser /usr/local/bin/chromium
ln -sf /usr/local/bin/epar-browser /usr/local/bin/chromium-browser
