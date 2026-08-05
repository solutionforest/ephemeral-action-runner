#!/usr/bin/env bash
set -euo pipefail

source_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
helper="${source_root}/scripts/run-with-docker.sh"
temporary="$(mktemp -d)"
cleanup() { rm -rf -- "$temporary"; }
trap cleanup EXIT INT TERM

grep -Fq 'EPAR_NATIVE_CONTROLLER_BACKEND=docker' "$helper" || { echo 'no-Go helper does not select the Docker native-controller compiler backend' >&2; exit 1; }
if grep -Eq 'go run[[:space:]]+\./cmd/ephemeral-action-runner' "$helper"; then
  echo 'no-Go helper still runs the controller through go run' >&2
  exit 1
fi

cp "$helper" "$temporary/run-with-docker.sh"
chmod +x "$temporary/run-with-docker.sh"
mkdir -p "$temporary/bin"
cat >"$temporary/bin/uname" <<'SCRIPT'
#!/usr/bin/env bash
if [[ "${1:-}" == -s ]]; then printf '%s\n' Linux; else printf '%s\n' x86_64; fi
SCRIPT
chmod +x "$temporary/bin/uname"
set +e
legacy_output="$(PATH="$temporary/bin:$PATH" EPAR_LEGACY_CONTROLLER_IN_DOCKER=1 "$temporary/run-with-docker.sh" version 2>&1)"
legacy_status=$?
set -e
[[ "$legacy_status" == 1 ]] || { echo "removed legacy controller mode exited $legacy_status, want 1" >&2; exit 1; }
[[ "$legacy_output" == *'EPAR_LEGACY_CONTROLLER_IN_DOCKER=1 is no longer supported'* ]] || { echo 'removed legacy controller mode did not explain the replacement path' >&2; exit 1; }

echo 'Unix no-Go native-controller dispatch contract passed'
