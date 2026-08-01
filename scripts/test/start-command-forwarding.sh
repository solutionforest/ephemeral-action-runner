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

assert_forwarded() {
  local description="$1"
  shift
  local expected_file="$test_root/expected-arguments"
  printf '%s\n' "$@" >"$expected_file"
  if ! cmp -s "$expected_file" "$EPAR_START_FORWARD_LOG"; then
    echo "$description forwarding mismatch:" >&2
    diff -u "$expected_file" "$EPAR_START_FORWARD_LOG" >&2 || true
    exit 1
  fi
}

"$repo_root/start"
assert_forwarded "default start" run ./cmd/ephemeral-action-runner start

"$repo_root/start" --config .local/custom-config.yml --instances 2
assert_forwarded "start flag" run ./cmd/ephemeral-action-runner start --config .local/custom-config.yml --instances 2

"$repo_root/start" --config ".local/config with spaces.yml" --label 'value "with quotes"'
assert_forwarded "quoted start" run ./cmd/ephemeral-action-runner start --config ".local/config with spaces.yml" --label 'value "with quotes"'

"$repo_root/start" storage prune --provider docker-sandboxes
assert_forwarded "explicit command" run ./cmd/ephemeral-action-runner storage prune --provider docker-sandboxes

"$repo_root/start" image update --config .local/custom-config.yml
assert_forwarded "image update" run ./cmd/ephemeral-action-runner image update --config .local/custom-config.yml

"$repo_root/start" version
assert_forwarded "version" run ./cmd/ephemeral-action-runner version

printf 'start command-forwarding smoke passed\n'
