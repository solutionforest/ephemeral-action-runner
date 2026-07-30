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
for required in 'golang:latest' 'ephemeral-action-runner.manifest' 'schemaVersion=2' 'lease-native-' 'epar_write_bootstrap_acquisition_journal' 'epar_resolve_go_toolchain_image' 'previousDevImageID' 'previous_dev_image_id' 'epar-native-controller-build.log' 'epar_report_tls_failure' 'TLS verification was not disabled' 'epar_prepare_bootstrap_build_trust' '--network none' 'GO111MODULE=off' 'GOTOOLCHAIN=local' 'SSL_CERT_FILE=/run/epar-bootstrap-ca.pem' 'scripts/bootstrap-trust' ':/run/epar-bootstrap-ca.pem:ro'; do
  [[ "$builder_source" == *"$required"* ]] || { echo "stable native-controller wrapper contract is missing: ${required}" >&2; exit 1; }
done

echo "Unix native-controller cache retention contract passed"
