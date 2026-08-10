#!/usr/bin/env bash
set -euo pipefail

source_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
builder="${source_root}/scripts/build-native-controller.sh"
grep -q -- '--provenance=false' "$builder" || { echo 'native controller toolchain build must disable nondeterministic default provenance' >&2; exit 1; }
eval "$(sed -n '/^epar_tls_failure_host()/,/^case "$(uname -s)\/$(uname -m)"/p' "$builder" | sed '$d')"

native_cache_keep_previous=2
native_cache_max_bytes=$((1024 * 1024))
native_cache_grace_seconds=0
abandoned_build_grace_seconds=0
retention_inventory_file=""

temporary="$(mktemp -d)"
cleanup() {
  rm -rf -- "$temporary"
}
trap cleanup EXIT INT TERM

tls_transcript="${temporary}/tls-error.log"
printf '%s\n' 'module: Get "https://proxy.golang.org/example/@v/v1.0.0.zip": tls: failed to verify certificate: x509: certificate signed by unknown authority' >"$tls_transcript"
[[ "$(epar_tls_failure_host "$tls_transcript")" == 'proxy.golang.org' ]] || { echo 'native-controller TLS failure host was not extracted' >&2; exit 1; }
printf '%s\n' 'ordinary build failure' >"$tls_transcript"
[[ -z "$(epar_tls_failure_host "$tls_transcript")" ]] || { echo 'ordinary native-controller failure was misclassified as TLS' >&2; exit 1; }

repeat_character() {
  printf '%64s' '' | tr ' ' "$1"
}

write_manifested_revision() {
  local root="$1"
  local key="$2"
  local directory="${root}/${key}"
  mkdir -p "$directory"
  printf '%s' "$key" >"${directory}/ephemeral-action-runner"
  printf '%s\n' \
    'schemaVersion=1' \
    "cacheKey=${key}" \
    'executable=ephemeral-action-runner' >"${directory}/controller-cache.manifest"
}

cache_root="${temporary}/bin"
mkdir -p "$cache_root"
keys=()
for character in 0 1 2 3 4 5 6; do
  keys+=("$(repeat_character "$character")")
done
for index in 0 1 2 3 4; do
  write_manifested_revision "$cache_root" "${keys[$index]}"
  touch -t "20200101010${index}" "${cache_root}/${keys[$index]}"
done

printf '%s\n' \
  'schemaVersion=1' \
  "host=$(hostname 2>/dev/null || true)" \
  "pid=$$" \
  'startedAtUnix=1' >"${cache_root}/${keys[4]}/lease-active"

mkdir -p "${cache_root}/${keys[5]}"
printf '%s' "${keys[5]}" >"${cache_root}/${keys[5]}/ephemeral-action-runner"
mkdir -p "${cache_root}/${keys[6]}"
printf '%s' "${keys[6]}" >"${cache_root}/${keys[6]}/ephemeral-action-runner"
printf '%s\n' \
  'schemaVersion=1' \
  "cacheKey=${keys[5]}" \
  'executable=ephemeral-action-runner' >"${cache_root}/${keys[6]}/controller-cache.manifest"

stale_build="${cache_root}/.build-stale"
mkdir -p "$stale_build"
printf '%s\n' \
  'schemaVersion=1' \
  "host=$(hostname 2>/dev/null || true)" \
  'pid=2147483646' \
  'startedAtUnix=1' >"${stale_build}/lease-build-2147483646.ABC123"
touch -t 202001010100 "$stale_build"

active_build="${cache_root}/.build-active"
mkdir -p "$active_build"
printf '%s\n' \
  'schemaVersion=1' \
  "host=$(hostname 2>/dev/null || true)" \
  "pid=$$" \
  'startedAtUnix=1' >"${active_build}/lease-build-$$.DEF456"
touch -t 202001010100 "$active_build"

unmarked_build="${cache_root}/.build-unmarked"
mkdir -p "$unmarked_build"
touch -t 202001010100 "$unmarked_build"

malformed_build="${cache_root}/.build-malformed"
mkdir -p "$malformed_build"
printf '%s\n' \
  "host=$(hostname 2>/dev/null || true)" \
  'pid=2147483646' \
  'startedAtUnix=1' >"${malformed_build}/lease-build-2147483646.GHI789"
touch -t 202001010100 "$malformed_build"

foreign_build="${cache_root}/.build-foreign"
mkdir -p "$foreign_build"
printf '%s\n' \
  'schemaVersion=1' \
  "host=foreign-$(hostname 2>/dev/null || true)" \
  'pid=2147483646' \
  "startedAtUnix=$(date +%s)" >"${foreign_build}/lease-build-2147483646.JKL012"
touch -t 202001010100 "$foreign_build"

symlink_key="$(repeat_character 8)"
symlink_created=0
if ln -s "${cache_root}/${keys[0]}" "${cache_root}/${symlink_key}" 2>/dev/null && [[ -L "${cache_root}/${symlink_key}" ]]; then
  symlink_created=1
fi

epar_prune_native_controller_cache "$cache_root" "${keys[0]}"

expected=("${keys[0]}" "${keys[2]}" "${keys[3]}" "${keys[4]}" "${keys[5]}" "${keys[6]}")
for key in "${expected[@]}"; do
  [[ -d "${cache_root}/${key}" ]] || { echo "retention removed protected revision ${key}" >&2; exit 1; }
done
[[ ! -e "${cache_root}/${keys[1]}" ]] || { echo "retention kept the expired excess revision" >&2; exit 1; }
[[ ! -e "$stale_build" ]] || { echo "retention kept an inactive leased build" >&2; exit 1; }
[[ -d "$active_build" ]] || { echo "retention removed an active build" >&2; exit 1; }
[[ -d "$unmarked_build" ]] || { echo "retention removed a markerless build" >&2; exit 1; }
[[ -d "$malformed_build" ]] || { echo "retention treated a malformed build lease as ownership evidence" >&2; exit 1; }
[[ -d "$foreign_build" ]] || { echo "retention removed a recent foreign-host build lease" >&2; exit 1; }
if ((symlink_created == 1)); then
  [[ -L "${cache_root}/${symlink_key}" ]] || { echo "retention followed or removed a cache symlink" >&2; exit 1; }
fi

policy_root="${temporary}/policy"
mkdir -p "$policy_root"
policy_current="$(repeat_character a)"
policy_expired="$(repeat_character b)"
policy_grace="$(repeat_character c)"
write_manifested_revision "$policy_root" "$policy_current"
write_manifested_revision "$policy_root" "$policy_expired"
write_manifested_revision "$policy_root" "$policy_grace"
printf '%1024s' '' >"${policy_root}/${policy_expired}/ephemeral-action-runner"
printf '%1024s' '' >"${policy_root}/${policy_grace}/ephemeral-action-runner"
touch -t 202001010100 "${policy_root}/${policy_expired}"
native_cache_keep_previous=5
native_cache_max_bytes=512
native_cache_grace_seconds=$((7 * 24 * 60 * 60))
epar_prune_native_controller_cache "$policy_root" "$policy_current"
[[ ! -e "${policy_root}/${policy_expired}" ]] || { echo "retention kept an expired revision beyond the byte budget" >&2; exit 1; }
[[ -d "${policy_root}/${policy_grace}" ]] || { echo "retention removed a grace-protected revision beyond the byte budget" >&2; exit 1; }

builder_source="$(cat "$builder")"
for required in 'golang:latest' 'controller.receipt' 'schemaVersion=3' 'sourceDigest' 'buildDigest' 'binaryDigest' 'lease-native-' 'epar_write_bootstrap_acquisition_journal' 'epar_resolve_go_toolchain_image' 'previousDevImageID' 'previous_dev_image_id' 'epar-native-controller-build.log' 'epar_report_tls_failure' 'TLS verification was not disabled' 'epar_prepare_bootstrap_build_trust' '--network none' 'GO111MODULE=off' 'GOTOOLCHAIN=local' 'SSL_CERT_FILE=/run/epar-bootstrap-ca.pem' 'scripts/bootstrap-trust' ':/run/epar-bootstrap-ca.pem:ro'; do
  [[ "$builder_source" == *"$required"* ]] || { echo "stable native-controller wrapper contract is missing: ${required}" >&2; exit 1; }
done
launch_source="$(sed -n '/^epar_launch_slot()/,/^}/p' "$builder")"
launch_lock_line="$(printf '%s\n' "$launch_source" | grep -n 'epar_acquire_stable_build_lock' | head -n 1 | cut -d: -f1)"
launch_lease_line="$(printf '%s\n' "$launch_source" | grep -n 'lease_file=.*mktemp' | head -n 1 | cut -d: -f1)"
launch_unlock_line="$(printf '%s\n' "$launch_source" | grep -n 'epar_release_stable_build_lock' | tail -n 1 | cut -d: -f1)"
[[ "$launch_lock_line" =~ ^[0-9]+$ && "$launch_lease_line" =~ ^[0-9]+$ && "$launch_unlock_line" =~ ^[0-9]+$ && "$launch_lock_line" -lt "$launch_lease_line" && "$launch_lease_line" -lt "$launch_unlock_line" ]] || { echo 'Unix launch must hold the promotion lock through runtime lease publication' >&2; exit 1; }

native_smoke_root="${temporary}/native-runtime-smoke"
native_smoke_project="${native_smoke_root}/project"
native_smoke_bin="${native_smoke_root}/bin"
mkdir -p "${native_smoke_project}/scripts/host-trust" "${native_smoke_project}/scripts/docker" "${native_smoke_project}/scripts/bootstrap-trust" "${native_smoke_project}/cmd" "${native_smoke_project}/internal" "$native_smoke_bin"
cp "$builder" "${native_smoke_project}/scripts/build-native-controller.sh"
cp "${source_root}/scripts/run-with-docker.sh" "${native_smoke_project}/scripts/run-with-docker.sh"
cp "${source_root}/scripts/bootstrap-trust/main.go" "${native_smoke_project}/scripts/bootstrap-trust/main.go"
: >"${native_smoke_project}/scripts/docker/dev.Dockerfile"
: >"${native_smoke_project}/go.mod"
: >"${native_smoke_project}/go.sum"
cat >"${native_smoke_project}/scripts/host-trust/wrapper-lib.sh" <<'SH'
#!/usr/bin/env bash
EPAR_HOST_TRUST_POST_INIT_CONFIG=""
EPAR_BUILD_TRUST_FEED_DIR=""
EPAR_RUNNER_TRUST_FEED_DIR=""
epar_host_trust_config_path() { printf '%s/.local/config.yml\n' "$1"; }
epar_host_trust_prepare() { EPAR_HOST_TRUST_POST_INIT_CONFIG="$(epar_host_trust_config_path "$1")"; }
epar_host_trust_post_init() { [[ -n "$EPAR_HOST_TRUST_POST_INIT_CONFIG" ]] && "$EPAR_HOST_TRUST_HELPER" sync --project-root "$1" --config "$EPAR_HOST_TRUST_POST_INIT_CONFIG" >/dev/null; }
epar_host_trust_cleanup() { :; }
epar_host_trust_host_os() { printf '%s\n' linux; }
SH
cat >"${native_smoke_project}/scripts/host-trust/host-trust-feed.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
case "${1:-}" in
  sync)
    if [[ " $* " == *" --purpose build "* ]]; then
      printf 'bootstrap\n' >>"$FAKE_HELPER_LOG"
      printf '{}\n' >"$FAKE_BOOTSTRAP_FEED"
      printf '%s\n' "$FAKE_BOOTSTRAP_FEED"
    else
      printf 'post-init\n' >>"$FAKE_HELPER_LOG"
    fi
    ;;
  watch)
    trap 'exit 0' INT TERM
    while :; do sleep 1; done
    ;;
  *)
    echo "unexpected host-trust helper command: $*" >&2
    exit 1
    ;;
esac
SH
cat >"${native_smoke_bin}/docker" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
printf 'CALL' >>"$FAKE_DOCKER_LOG"
printf ' <%s>' "$@" >>"$FAKE_DOCKER_LOG"
printf '\n' >>"$FAKE_DOCKER_LOG"
case "${1:-}" in
  image)
    printf 'sha256:%064d\n' 0
    ;;
  pull|build)
    ;;
  run)
    output_directory=""
    for argument in "$@"; do
      case "$argument" in
        *:/out) output_directory="${argument%:/out}" ;;
      esac
    done
    if [[ " $* " == *' /bootstrap/main.go '* ]]; then
      printf 'bootstrap trust\n'
      printf 'fake bootstrap trust\n' >"${output_directory}/ca.pem"
    elif [[ " $* " == *' go build '* ]]; then
      cat >"${output_directory}/ephemeral-action-runner" <<'NATIVE'
#!/usr/bin/env bash
set -euo pipefail
printf 'runtime build=<%s> runner=<%s> os=<%s> deferred=<%s> args=<%s>\n' "${EPAR_BUILD_TRUST_FEED:-}" "${EPAR_HOST_TRUST_FEED:-}" "${EPAR_CONTROLLER_HOST_OS:-}" "${EPAR_HOST_TRUST_INIT_DEFERRED:-}" "$*" >>"${FAKE_NATIVE_LOG:?}"
if [[ "${1:-}" == init ]]; then
  mkdir -p .local
  printf '%s\n' 'image:' '  hostTrustMode: overlay' '  hostTrustScopes: [system, user]' >.local/config.yml
fi
NATIVE
      chmod +x "${output_directory}/ephemeral-action-runner"
    fi
    ;;
  *)
    echo "unexpected fake Docker command: $*" >&2
    exit 1
    ;;
esac
SH
cat >"${native_smoke_bin}/uname" <<'SH'
#!/usr/bin/env bash
if [[ "${1:-}" == -s ]]; then printf '%s\n' Linux; else printf '%s\n' x86_64; fi
SH
chmod +x "${native_smoke_project}/scripts/build-native-controller.sh" "${native_smoke_project}/scripts/run-with-docker.sh" "${native_smoke_project}/scripts/host-trust/host-trust-feed.sh" "${native_smoke_bin}/docker" "${native_smoke_bin}/uname"

native_smoke_env=(
  "PATH=${native_smoke_bin}:$PATH"
  "EPAR_GOMOD_VOLUME=native-smoke-gomod"
  "EPAR_GOCACHE_VOLUME=native-smoke-gocache"
  'EPAR_BOOTSTRAP_MIN_FREE_BYTES=1'
  "FAKE_HELPER_LOG=${native_smoke_root}/helper.log"
  "FAKE_BOOTSTRAP_FEED=${native_smoke_root}/bootstrap-feed.json"
  "FAKE_DOCKER_LOG=${native_smoke_root}/docker.log"
  "FAKE_NATIVE_LOG=${native_smoke_root}/native.log"
  'EPAR_BUILD_TRUST_FEED=stale-build'
  'EPAR_HOST_TRUST_FEED=stale-runner'
  'EPAR_CONTROLLER_HOST_OS=darwin'
  'EPAR_HOST_TRUST_INIT_DEFERRED=1'
)
(cd "$native_smoke_project" && env "${native_smoke_env[@]}" scripts/build-native-controller.sh start)
grep -Fxq 'runtime build=<> runner=<> os=<> deferred=<> args=<start>' "${native_smoke_root}/native.log"
grep -Fxq 'bootstrap' "${native_smoke_root}/helper.log"
[[ "$(wc -l <"${native_smoke_root}/helper.log" | tr -d ' ')" == 1 ]] || { echo 'ordinary cached-native start unexpectedly used a runtime trust bridge' >&2; exit 1; }
grep -Fq ':/feed/current.json:ro>' "${native_smoke_root}/docker.log"
grep -Fq ' <SSL_CERT_FILE=/run/epar-bootstrap-ca.pem>' "${native_smoke_root}/docker.log"

[[ "$(grep -c '^CALL <pull>' "${native_smoke_root}/docker.log")" == 1 ]] || { echo 'first Docker controller build did not pull exactly once' >&2; exit 1; }
[[ "$(grep -c '^CALL <build>' "${native_smoke_root}/docker.log")" == 1 ]] || { echo 'first Docker controller build did not build the toolchain exactly once' >&2; exit 1; }

: >"${native_smoke_root}/helper.log"
(cd "$native_smoke_project" && env "${native_smoke_env[@]}" scripts/build-native-controller.sh init)
grep -Fxq 'runtime build=<> runner=<> os=<> deferred=<> args=<init>' "${native_smoke_root}/native.log"
grep -Fxq 'post-init' "${native_smoke_root}/helper.log"
[[ "$(grep -c '^CALL <pull>' "${native_smoke_root}/docker.log")" == 1 ]] || { echo 'reusable Docker receipt unexpectedly pulled a toolchain' >&2; exit 1; }
[[ "$(grep -c '^CALL <build>' "${native_smoke_root}/docker.log")" == 1 ]] || { echo 'reusable Docker receipt unexpectedly rebuilt a toolchain' >&2; exit 1; }

current_slot="${native_smoke_project}/.local/bin/linux-amd64"
current_receipt="${current_slot}/controller.receipt"
before_blocked_digest="$(sed -n 's/^buildDigest=//p' "$current_receipt")"
printf '%s\n' changed-source >"${native_smoke_project}/internal/changed.txt"
active_lease="${current_slot}/lease-native-$$.ABC123"
printf '%s\n' 'schemaVersion=1' "host=$(hostname 2>/dev/null || true)" "pid=$$" "startedAtUnix=$(date +%s)" >"$active_lease"
set +e
blocked_output="$(cd "$native_smoke_project" && env "${native_smoke_env[@]}" scripts/build-native-controller.sh start 2>&1)"
blocked_status=$?
set -e
after_blocked_digest="$(sed -n 's/^buildDigest=//p' "$current_receipt")"
rm -f -- "$active_lease"
[[ "$blocked_status" != 0 && "$blocked_output" == *'controller is running; stop it before rebuilding'* && "$after_blocked_digest" == "$before_blocked_digest" ]] || { echo 'active Unix controller lease did not block source-triggered rotation' >&2; exit 1; }

echo "Unix native-controller cache retention contract passed"
