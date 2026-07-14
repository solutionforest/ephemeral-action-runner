#!/usr/bin/env bash
set -euo pipefail

source_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
temporary="$(mktemp -d)"
cleanup() { rm -rf "$temporary"; }
trap cleanup EXIT INT TERM

project="$temporary/project"
mkdir -p "$project/scripts/host-trust" "$project/scripts/docker" "$temporary/bin" "$temporary/home"
cp "$source_root/scripts/run-with-docker.sh" "$project/scripts/"
cp "$source_root/scripts/host-trust/wrapper-lib.sh" "$source_root/scripts/host-trust/host-trust-feed.sh" "$source_root/scripts/host-trust/macos-trust-settings.js" "$project/scripts/host-trust/"
: >"$project/scripts/docker/dev.Dockerfile"

cat >"$temporary/bin/docker" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
printf 'CALL' >>"$FAKE_DOCKER_LOG"
printf ' <%s>' "$@" >>"$FAKE_DOCKER_LOG"
printf '\n' >>"$FAKE_DOCKER_LOG"
[[ "${1:-}" == build ]] && exit 0
if [[ " $* " == *" go run ./cmd/ephemeral-action-runner init "* ]]; then
  [[ "${FAKE_INIT_FAIL:-0}" == 1 ]] && exit 23
  mkdir -p "$FAKE_PROJECT/.local"
  cat >"$FAKE_PROJECT/.local/config.yml" <<'YAML'
provider:
  type: docker-dind
runner:
  ephemeral: true
image:
  hostTrustMode: overlay
  hostTrustScopes: [system]
YAML
fi
exit 0
SH
chmod +x "$temporary/bin/docker" "$project/scripts/run-with-docker.sh" "$project/scripts/host-trust/host-trust-feed.sh"

host_os="$(uname -s)"
case "$host_os" in
  Linux)
    source_bundle=/etc/ssl/certs/ca-certificates.crt
    [[ -r "$source_bundle" ]] || { echo "system CA bundle unavailable" >&2; exit 1; }
    bundle="$temporary/one-root.pem"
    awk '/-----BEGIN CERTIFICATE-----/{copy=1} copy{print} /-----END CERTIFICATE-----/{exit}' "$source_bundle" >"$bundle"
    expected_controller_os=linux
    ;;
  Darwin)
    : "${HOME:?HOME must be set to read the macOS system trust store}"
    expected_controller_os=darwin
    ;;
  *)
    echo "Unix no-Go first-run smoke supports Linux and macOS only" >&2
    exit 1
    ;;
esac

export PATH="$temporary/bin:$PATH"
export XDG_CACHE_HOME="$temporary/cache"
if [[ "$host_os" == Linux ]]; then
  export HOME="$temporary/home"
  export EPAR_HOST_TRUST_BUNDLE="$bundle"
fi
export FAKE_PROJECT="$project"
export FAKE_DOCKER_LOG="$temporary/docker.log"

(cd "$project" && scripts/run-with-docker.sh start)
[[ "$(grep -c ' <run>' "$FAKE_DOCKER_LOG")" == 2 ]]
first_run_call="$(grep ' <run>' "$FAKE_DOCKER_LOG" | sed -n '1p')"
second_run_call="$(grep ' <run>' "$FAKE_DOCKER_LOG" | sed -n '2p')"
[[ "$first_run_call" == *" <EPAR_HOST_TRUST_INIT_DEFERRED=1>"* ]]
[[ "$first_run_call" == *" <EPAR_CONTROLLER_HOST_OS=$expected_controller_os>"* ]]
[[ "$first_run_call" == *" <init>"* ]]
[[ "$second_run_call" == *" <EPAR_HOST_TRUST_FEED=/run/epar-host-trust/current.json>"* ]]
[[ "$second_run_call" == *":/run/epar-host-trust:ro>"* ]]
[[ "$second_run_call" == *" <start>"* ]]

rm -rf "$project/.local" "$temporary/cache"
: >"$FAKE_DOCKER_LOG"
export FAKE_INIT_FAIL=1
set +e
(cd "$project" && scripts/run-with-docker.sh start)
status=$?
set -e
[[ "$status" == 23 ]] || { echo "failing implicit init exited $status, want 23" >&2; exit 1; }

echo "Unix no-Go first-run start lifecycle smoke passed"
