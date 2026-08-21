#!/usr/bin/env bash
set -euo pipefail

source_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
[[ -x "$source_root/scripts/build-native-controller.sh" ]] || { echo 'scripts/build-native-controller.sh must be tracked as executable because the start wrappers execute it directly' >&2; exit 1; }
test_root="$(mktemp -d)"
cleanup() { rm -rf -- "$test_root"; }
trap cleanup EXIT INT TERM

project="$test_root/project"
fake_bin="$test_root/bin"
mkdir -p "$project/scripts/host-trust" "$project/scripts/docker" "$project/scripts/bootstrap-trust" "$fake_bin"
cp "$source_root/start" "$project/"
cp "$source_root/scripts/build-native-controller.sh" "$source_root/scripts/run-with-docker.sh" "$project/scripts/"
cp "$source_root/scripts/host-trust/wrapper-lib.sh" "$project/scripts/host-trust/"
cp "$source_root/scripts/docker/dev.Dockerfile" "$project/scripts/docker/"
cp "$source_root/scripts/bootstrap-trust/main.go" "$project/scripts/bootstrap-trust/"
cp "$source_root/go.mod" "$source_root/go.sum" "$project/"
cp -R "$source_root/cmd" "$source_root/internal" "$project/"
chmod +x "$project/start" "$project/scripts/build-native-controller.sh" "$project/scripts/run-with-docker.sh"
grep -Fq 'Go is not installed or runnable on this machine' "$project/start" || { echo 'start wrapper does not explain the no-Go Docker fallback' >&2; exit 1; }
grep -Fq 'if the project-local controller needs to be rebuilt' "$project/start" || { echo 'start wrapper does not explain that Docker is conditional on a rebuild' >&2; exit 1; }

cat >"$fake_bin/go" <<'SCRIPT'
#!/usr/bin/env bash
set -euo pipefail
if [[ "${1:-}" == version ]]; then
  printf '%s\n' 'go version go1.99.0 epar-test'
  exit 0
fi
[[ "${1:-}" == build ]] || { echo "unexpected fake go command: $*" >&2; exit 1; }
printf 'BUILD' >>"${FAKE_GO_LOG:?}"
printf ' <%s>' "$@" >>"${FAKE_GO_LOG:?}"
printf '\n' >>"${FAKE_GO_LOG:?}"
output=""
while (($#)); do
  if [[ "$1" == -o ]]; then output="$2"; shift 2; continue; fi
  shift
done
[[ -n "$output" ]] || { echo 'fake go build received no -o path' >&2; exit 1; }
cat >"$output" <<'NATIVE'
#!/usr/bin/env bash
set -euo pipefail
printf 'CALL' >>"${FAKE_NATIVE_LOG:?}"
printf ' <%s>' "$@" >>"${FAKE_NATIVE_LOG:?}"
printf '\n' >>"${FAKE_NATIVE_LOG:?}"
NATIVE
chmod +x "$output"
SCRIPT
cat >"$fake_bin/docker" <<'SCRIPT'
#!/usr/bin/env bash
set -euo pipefail
printf 'DOCKER <%s>\n' "$*" >>"${FAKE_DOCKER_LOG:?}"
echo 'local Go path must not invoke Docker' >&2
exit 97
SCRIPT
cat >"$fake_bin/uname" <<'SCRIPT'
#!/usr/bin/env bash
if [[ "${1:-}" == -s ]]; then printf '%s\n' Linux; else printf '%s\n' x86_64; fi
SCRIPT
cat >"$fake_bin/shasum" <<'SCRIPT'
#!/usr/bin/env bash
set -euo pipefail
[[ "${1:-}" == -a && "${2:-}" == 256 ]] && shift 2
sha256sum "$@"
SCRIPT
chmod +x "$fake_bin/go" "$fake_bin/docker" "$fake_bin/uname" "$fake_bin/shasum"

export PATH="$fake_bin:$PATH"
export EPAR_GO_BIN=go
export EPAR_USE_DOCKER_RUN=0
export FAKE_GO_LOG="$test_root/go.log"
export FAKE_NATIVE_LOG="$test_root/native.log"
export FAKE_DOCKER_LOG="$test_root/docker.log"

assert_last_call() {
  local description="$1"
  shift
  local expected actual
  expected="CALL"
  for argument in "$@"; do expected+=" <${argument}>"; done
  actual="$(tail -n 1 "$FAKE_NATIVE_LOG")"
  [[ "$actual" == "$expected" ]] || { echo "$description forwarding mismatch: got $actual, want $expected" >&2; exit 1; }
}

(cd "$project" && ./start)
assert_last_call 'default start' start

(cd "$project" && ./start --config .local/custom-config.yml --instances 2)
assert_last_call 'start flag' start --config .local/custom-config.yml --instances 2

(cd "$project" && ./start --config .local/custom-config.yml --external-outage-retry=continuous)
assert_last_call 'external outage retry flag' start --config .local/custom-config.yml --external-outage-retry=continuous

(cd "$project" && ./start --config '.local/config with spaces.yml' --label 'value "with quotes"')
assert_last_call 'quoted start' start --config '.local/config with spaces.yml' --label 'value "with quotes"'

(cd "$project" && ./start storage prune --provider docker-sandboxes)
assert_last_call 'explicit command' storage prune --provider docker-sandboxes

(cd "$project" && ./start storage status --operation template-build --provider docker-sandboxes --config .local/custom-config.yml --project-root .)
assert_last_call 'storage operation' storage status --operation template-build --provider docker-sandboxes --config .local/custom-config.yml --project-root .

(cd "$project" && ./start image update --config .local/custom-config.yml)
assert_last_call 'image update' image update --config .local/custom-config.yml

(cd "$project" && ./start version)
assert_last_call 'version' version

[[ "$(wc -l <"$FAKE_GO_LOG" | tr -d ' ')" == 1 ]] || { echo 'unchanged source unexpectedly rebuilt the local controller' >&2; exit 1; }
grep -Fq ' <build>' "$FAKE_GO_LOG"
grep -Fq ' <-trimpath>' "$FAKE_GO_LOG"
grep -Fq 'main.sourceRevision=sha256:' "$FAKE_GO_LOG"
[[ ! -s "$FAKE_DOCKER_LOG" ]] || { echo 'local controller path invoked Docker' >&2; exit 1; }
[[ -x "$project/.local/bin/linux-amd64/ephemeral-action-runner" || -x "$project/.local/bin/darwin-arm64/ephemeral-action-runner" ]] || { echo 'project-local current controller slot was not created' >&2; exit 1; }

printf '\n' >>"$project/cmd/ephemeral-action-runner/version.go"
(cd "$project" && ./start version)
assert_last_call 'rebuilt current version' version
target=""
for candidate in linux-amd64 linux-arm64 darwin-arm64; do
  if [[ -d "$project/.local/bin/${candidate}-old" ]]; then target="$candidate"; break; fi
done
[[ -n "$target" ]] || { echo 'source mismatch did not retain an old controller slot' >&2; exit 1; }
(cd "$project" && ./start --use-old version)
assert_last_call 'explicit old version' version
[[ "$(wc -l <"$FAKE_GO_LOG" | tr -d ' ')" == 2 ]] || { echo '--use-old unexpectedly rebuilt a controller' >&2; exit 1; }
[[ ! -s "$FAKE_DOCKER_LOG" ]] || { echo '--use-old unexpectedly invoked Docker' >&2; exit 1; }
set +e
late_old_output="$(cd "$project" && ./start version --use-old 2>&1)"
late_old_status=$?
set -e
[[ "$late_old_status" == 2 && "$late_old_output" == *'must be the first argument'* ]] || { echo 'late --use-old did not fail as a wrapper-argument error' >&2; exit 1; }

if grep -Eq 'go run[[:space:]]+\./cmd/ephemeral-action-runner' "$project/start" "$project/scripts/build-native-controller.sh" "$project/scripts/run-with-docker.sh"; then
  echo 'normal wrapper/controller path still contains go run' >&2
  exit 1
fi

printf 'start command-forwarding smoke passed\n'
