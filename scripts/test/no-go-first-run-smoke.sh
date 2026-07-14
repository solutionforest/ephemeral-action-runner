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

source_bundle=/etc/ssl/certs/ca-certificates.crt
[[ -r "$source_bundle" ]] || { echo "system CA bundle unavailable" >&2; exit 1; }
bundle="$temporary/one-root.pem"
awk '/-----BEGIN CERTIFICATE-----/{copy=1} copy{print} /-----END CERTIFICATE-----/{exit}' "$source_bundle" >"$bundle"

export PATH="$temporary/bin:$PATH"
export HOME="$temporary/home"
export XDG_CACHE_HOME="$temporary/cache"
export EPAR_HOST_TRUST_BUNDLE="$bundle"
export FAKE_PROJECT="$project"
export FAKE_DOCKER_LOG="$temporary/docker.log"

(cd "$project" && scripts/run-with-docker.sh start)
mapfile -t run_calls < <(grep ' <run>' "$FAKE_DOCKER_LOG")
[[ "${#run_calls[@]}" == 2 ]]
[[ "${run_calls[0]}" == *" <EPAR_HOST_TRUST_INIT_DEFERRED=1>"* ]]
[[ "${run_calls[0]}" == *" <EPAR_CONTROLLER_HOST_OS=linux>"* ]]
[[ "${run_calls[0]}" == *" <init>"* ]]
[[ "${run_calls[1]}" == *" <EPAR_HOST_TRUST_FEED=/run/epar-host-trust/current.json>"* ]]
[[ "${run_calls[1]}" == *":/run/epar-host-trust:ro>"* ]]
[[ "${run_calls[1]}" == *" <start>"* ]]

rm -rf "$project/.local" "$temporary/cache"
: >"$FAKE_DOCKER_LOG"
export FAKE_INIT_FAIL=1
set +e
(cd "$project" && scripts/run-with-docker.sh start)
status=$?
set -e
[[ "$status" == 23 ]] || { echo "failing implicit init exited $status, want 23" >&2; exit 1; }

echo "Unix no-Go first-run start lifecycle smoke passed"
