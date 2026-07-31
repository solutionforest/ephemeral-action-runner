#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
test_root="$(mktemp -d)"
trap 'rm -rf "$test_root"' EXIT

test_uname="$(uname -s)"
case "$test_uname" in
  MINGW*|MSYS*|CYGWIN*) test_uname=Linux ;;
esac
cat >"$test_root/uname" <<'SCRIPT'
#!/usr/bin/env bash
printf '%s\n' "$EPAR_TEST_UNAME"
SCRIPT
cat >"$test_root/go" <<'SCRIPT'
#!/usr/bin/env bash
if [[ "${1:-}" == "version" ]]; then
  exit 0
fi
printf '%s\n' "$@" >"$EPAR_START_FORWARD_LOG"
SCRIPT
chmod +x "$test_root/uname" "$test_root/go"

export PATH="$test_root:$PATH"
export EPAR_TEST_UNAME="$test_uname"
export EPAR_GO_BIN=go
export EPAR_USE_DOCKER_RUN=0
export EPAR_START_FORWARD_LOG="$test_root/arguments"
export EPAR_CONFIG="$test_root/config.yml"
printf 'image:\n  hostTrustMode: disabled\n' >"$EPAR_CONFIG"

"$repo_root/start"
actual="$(tr '\n' ' ' <"$EPAR_START_FORWARD_LOG")"
expected="run ./cmd/ephemeral-action-runner start "
if [[ "$actual" != "$expected" ]]; then
  echo "default start forwarding mismatch: got '$actual', want '$expected'" >&2
  exit 1
fi

"$repo_root/start" --config .local/custom-config.yml --instances 2
actual="$(tr '\n' ' ' <"$EPAR_START_FORWARD_LOG")"
expected="run ./cmd/ephemeral-action-runner start --config .local/custom-config.yml --instances 2 "
if [[ "$actual" != "$expected" ]]; then
  echo "start flag forwarding mismatch: got '$actual', want '$expected'" >&2
  exit 1
fi

"$repo_root/start" storage prune --provider docker-sandboxes
actual="$(tr '\n' ' ' <"$EPAR_START_FORWARD_LOG")"
expected="run ./cmd/ephemeral-action-runner storage prune --provider docker-sandboxes "
if [[ "$actual" != "$expected" ]]; then
  echo "explicit command forwarding mismatch: got '$actual', want '$expected'" >&2
  exit 1
fi

"$repo_root/start" image update --config .local/custom-config.yml
actual="$(tr '\n' ' ' <"$EPAR_START_FORWARD_LOG")"
expected="run ./cmd/ephemeral-action-runner image update --config .local/custom-config.yml "
if [[ "$actual" != "$expected" ]]; then
  echo "image update forwarding mismatch: got '$actual', want '$expected'" >&2
  exit 1
fi

"$repo_root/start" version
actual="$(tr '\n' ' ' <"$EPAR_START_FORWARD_LOG")"
expected="run ./cmd/ephemeral-action-runner version "
if [[ "$actual" != "$expected" ]]; then
  echo "version forwarding mismatch: got '$actual', want '$expected'" >&2
  exit 1
fi

printf 'start command-forwarding smoke passed\n'
