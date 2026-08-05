#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
EPAR_HOST_TRUST_HELPER="${repo_root}/scripts/host-trust/host-trust-feed.sh"
source "${repo_root}/scripts/host-trust/wrapper-lib.sh"
go_image="${GO_DOCKER_IMAGE:-golang:latest}"
dev_image="${EPAR_DEV_IMAGE:-epar-dev-toolchain}"
native_cache_keep_previous=5
native_cache_max_bytes=$((256 * 1024 * 1024))
native_cache_grace_seconds=$((7 * 24 * 60 * 60))
abandoned_build_grace_seconds=$((24 * 60 * 60))
bootstrap_minimum_free_bytes="${EPAR_BOOTSTRAP_MIN_FREE_BYTES:-$((1 * 1024 * 1024 * 1024))}"
go_cache_limit_bytes="${EPAR_GO_CACHE_LIMIT_BYTES:-$((10 * 1024 * 1024 * 1024))}"

if command -v shasum >/dev/null 2>&1; then
  epar_sha256() { shasum -a 256 "$1" | awk '{print $1}'; }
  epar_sha256_stream() { shasum -a 256 | awk '{print $1}'; }
elif command -v sha256sum >/dev/null 2>&1; then
  epar_sha256() { sha256sum "$1" | awk '{print $1}'; }
  epar_sha256_stream() { sha256sum | awk '{print $1}'; }
else
  echo "shasum or sha256sum is required to build the native EPAR controller identity." >&2
  exit 1
fi
project_id="$(printf '%s' "$repo_root" | epar_sha256_stream | cut -c1-12)"
gomod_volume="${EPAR_GOMOD_VOLUME:-epar-${project_id}-gomod}"
gocache_volume="${EPAR_GOCACHE_VOLUME:-epar-${project_id}-gocache}"
manage_go_cache=0
if [[ -z "${EPAR_GOMOD_VOLUME:-}" && -z "${EPAR_GOCACHE_VOLUME:-}" ]]; then manage_go_cache=1; fi

epar_ensure_go_cache_volume() {
  local volume="$1"
  local role="$2"
  local expected="${project_id}|${role}|1|${repo_root}"
  local actual=""
  if actual="$(docker volume inspect --format '{{ index .Labels "io.solutionforest.epar.project" }}|{{ index .Labels "io.solutionforest.epar.cache" }}|{{ index .Labels "io.solutionforest.epar.schema" }}|{{ index .Labels "io.solutionforest.epar.root" }}' "$volume" 2>/dev/null)"; then
    [[ "$actual" == "$expected" ]] || { echo "refusing Go cache volume ${volume}: EPAR ownership labels do not match this project" >&2; return 1; }
    return 0
  fi
  docker volume create \
    --label "io.solutionforest.epar.project=${project_id}" \
    --label "io.solutionforest.epar.cache=${role}" \
    --label 'io.solutionforest.epar.schema=1' \
    --label "io.solutionforest.epar.root=${repo_root}" \
    "$volume" >/dev/null
  actual="$(docker volume inspect --format '{{ index .Labels "io.solutionforest.epar.project" }}|{{ index .Labels "io.solutionforest.epar.cache" }}|{{ index .Labels "io.solutionforest.epar.schema" }}|{{ index .Labels "io.solutionforest.epar.root" }}' "$volume")"
  [[ "$actual" == "$expected" ]] || { echo "refusing Go cache volume ${volume}: post-create ownership labels do not match this project" >&2; return 1; }
}

epar_enforce_go_cache_limit() {
  local active gc_name
  active="$(
    {
      docker ps -q --filter "volume=${gomod_volume}"
      docker ps -q --filter "volume=${gocache_volume}"
    } | sort -u | sed '/^$/d'
  )"
  if [[ -n "$active" ]]; then
    echo "warning: EPAR Go cache limit check skipped because an exact cache volume is active" >&2
    return 0
  fi
  gc_name="epar-${project_id}-go-cache-gc"
  if [[ -n "$(docker ps -aq --filter "name=^/${gc_name}$")" ]]; then
    echo "warning: EPAR Go cache limit check skipped because ${gc_name} already exists" >&2
    return 0
  fi
  docker run --rm \
    --name "$gc_name" \
    -e "EPAR_GO_CACHE_LIMIT_BYTES=${go_cache_limit_bytes}" \
    -v "${gomod_volume}:/go/pkg/mod" \
    -v "${gocache_volume}:/root/.cache/go-build" \
    "$dev_image" \
    sh -ceu 'mod_kib="$(du -sk /go/pkg/mod | awk "{print \$1}")"; build_kib="$(du -sk /root/.cache/go-build | awk "{print \$1}")"; used_bytes="$(((mod_kib + build_kib) * 1024))"; if [ "$used_bytes" -gt "$EPAR_GO_CACHE_LIMIT_BYTES" ]; then go clean -cache -modcache; fi'
}

epar_write_bootstrap_acquisition_journal() {
  local phase="$1"
  local previous_id="${2:-}"
  local resolved_go_id="${3:-}"
  local resolved_dev_id="${4:-}"
  local previous_dev_id="${5:-}"
  local journal_directory="${repo_root}/.local/storage/bootstrap"
  local journal_path="${journal_directory}/native-controller-acquisition.json"
  local temporary_path
  epar_bootstrap_json_escape() {
    local value="$1"
    value="${value//\\/\\\\}"
    value="${value//\"/\\\"}"
    value="${value//$'\n'/\\n}"
    value="${value//$'\r'/\\r}"
    value="${value//$'\t'/\\t}"
    printf '%s' "$value"
  }
  mkdir -p "$journal_directory"
  temporary_path="$(mktemp "${journal_directory}/.native-controller-acquisition.XXXXXX")"
  printf '{"schemaVersion":1,"projectID":"%s","projectRoot":"%s","phase":"%s","goImage":"%s","devImage":"%s","previousGoImageID":"%s","previousDevImageID":"%s","resolvedGoImageID":"%s","resolvedDevImageID":"%s","updatedAtUnix":%s}\n' \
    "$(epar_bootstrap_json_escape "$project_id")" "$(epar_bootstrap_json_escape "$repo_root")" "$(epar_bootstrap_json_escape "$phase")" "$(epar_bootstrap_json_escape "$go_image")" "$(epar_bootstrap_json_escape "$dev_image")" "$(epar_bootstrap_json_escape "$previous_id")" "$(epar_bootstrap_json_escape "$previous_dev_id")" "$(epar_bootstrap_json_escape "$resolved_go_id")" "$(epar_bootstrap_json_escape "$resolved_dev_id")" "$(date +%s)" >"$temporary_path"
  mv -f -- "$temporary_path" "$journal_path"
}

epar_docker_image_id() {
  local reference="$1"
  local image_id
  image_id="$(docker image inspect --format '{{.Id}}' "$reference" 2>/dev/null || true)"
  if [[ -z "$image_id" ]]; then return 0; fi
  [[ "$image_id" =~ ^sha256:[0-9a-f]{64}$ ]] || { echo "Docker returned an invalid immutable image ID for ${reference}" >&2; return 1; }
  printf '%s\n' "$image_id"
}

epar_resolve_go_toolchain_image() {
  local previous_id resolved_id
  previous_id="$(epar_docker_image_id "$go_image")"
  epar_write_bootstrap_acquisition_journal pulling-go-toolchain "$previous_id" '' '' "$previous_dev_image_id"
  docker pull "$go_image" >&2
  resolved_id="$(epar_docker_image_id "$go_image")"
  [[ -n "$resolved_id" ]] || { echo "could not resolve the immutable Docker image ID for ${go_image} after pull" >&2; return 1; }
  epar_write_bootstrap_acquisition_journal go-toolchain-resolved "$previous_id" "$resolved_id" '' "$previous_dev_image_id"
  EPAR_GO_PREVIOUS_IMAGE_ID="$previous_id"
  EPAR_GO_RESOLVED_IMAGE_ID="$resolved_id"
}

epar_prepare_bootstrap_build_trust() {
  local config_path feed_path config_id bundle_directory validator_output host_os
  config_path="$(epar_host_trust_config_path "$repo_root" "$@")"
  feed_path="$("$EPAR_HOST_TRUST_HELPER" sync --project-root "$repo_root" --config "$config_path" --purpose build)"
  [[ -n "$feed_path" && -f "$feed_path" && ! -L "$feed_path" ]] || { echo "the host trust publisher did not return a regular build feed" >&2; return 1; }
  config_id="$(printf '%s' "$config_path" | epar_sha256_stream | cut -c1-32)"
  bundle_directory="${repo_root}/.local/storage/bootstrap-trust/${config_id}"
  mkdir -p "$bundle_directory"
  [[ -d "$bundle_directory" && ! -L "$bundle_directory" ]] || { echo "bootstrap build trust directory must be a regular directory: ${bundle_directory}" >&2; return 1; }
  bootstrap_trust_bundle="${bundle_directory}/ca.pem"
  if [[ -e "$bootstrap_trust_bundle" && (! -f "$bootstrap_trust_bundle" || -L "$bootstrap_trust_bundle") ]]; then
    echo "bootstrap build trust output must be a regular non-symlink file: ${bootstrap_trust_bundle}" >&2
    return 1
  fi
  host_os="$(epar_host_trust_host_os)"
  validator_output="$(
    docker run --rm \
      --network none \
      -e GO111MODULE=off \
      -e GOTOOLCHAIN=local \
      -v "${repo_root}/scripts/bootstrap-trust:/bootstrap:ro" \
      -v "${feed_path}:/feed/current.json:ro" \
      -v "${bundle_directory}:/out" \
      "$dev_image" \
      /usr/local/go/bin/go run /bootstrap/main.go --feed /feed/current.json --output /out/ca.pem --expected-host-os "$host_os"
  )"
  [[ -s "$bootstrap_trust_bundle" && ! -L "$bootstrap_trust_bundle" ]] || { echo "bootstrap build trust validator did not produce a regular nonempty bundle" >&2; return 1; }
  bootstrap_trust_summary="$(printf '%s\n' "$validator_output" | sed -n '$p')"
}

epar_tls_failure_host() {
  local transcript="$1"
  grep -Eqi 'x509: certificate signed by unknown authority|certificate verify failed|unable to (get local issuer certificate|verify the first certificate)' "$transcript" || return 0
  sed -nE 's#.*https://([A-Za-z0-9.-]+)([:/"].*)?#\1#p' "$transcript" | sed -n '1p' | tr '[:upper:]' '[:lower:]'
}

epar_report_tls_failure() {
  local transcript="$1"
  local log_path="$2"
  local host_name diagnostic_output subject issuer fingerprint not_before not_after
  host_name="$(epar_tls_failure_host "$transcript")"
  [[ -n "$host_name" ]] || return 0
  diagnostic_output="$(
    docker run --rm \
      -e "EPAR_TLS_DIAGNOSTIC_HOST=${host_name}" \
      "$dev_image" \
      sh -c '
        set -u
        raw="$(mktemp)"
        leaf="$(mktemp)"
        cleanup() { rm -f -- "$raw" "$leaf"; }
        trap cleanup EXIT
        openssl s_client -connect "${EPAR_TLS_DIAGNOSTIC_HOST}:443" -servername "${EPAR_TLS_DIAGNOSTIC_HOST}" -showcerts </dev/null >"$raw" 2>&1 || true
        awk '"'"'/-----BEGIN CERTIFICATE-----/{capture=1} capture{print} /-----END CERTIFICATE-----/{exit}'"'"' "$raw" >"$leaf"
        grep -E "verify error|Verify return code" "$raw" || true
        if [ -s "$leaf" ]; then
          openssl x509 -in "$leaf" -noout -subject -issuer -fingerprint -sha256 -dates
        fi
      ' 2>&1
  )" || true
  subject="$(printf '%s\n' "$diagnostic_output" | sed -n 's/^subject=//p' | sed -n '1p')"
  issuer="$(printf '%s\n' "$diagnostic_output" | sed -n 's/^issuer=//p' | sed -n '1p')"
  fingerprint="$(printf '%s\n' "$diagnostic_output" | sed -n 's/^sha256 Fingerprint=//p' | sed -n '1p' | tr -d ':')"
  not_before="$(printf '%s\n' "$diagnostic_output" | sed -n 's/^notBefore=//p' | sed -n '1p')"
  not_after="$(printf '%s\n' "$diagnostic_output" | sed -n 's/^notAfter=//p' | sed -n '1p')"
  {
    printf '\n%s\n' 'EPAR TLS certificate diagnostic'
    printf '  Requested host: %s:443\n' "$host_name"
    printf '  Toolchain image: %s\n' "$dev_image"
    if [[ -z "$subject" ]]; then
      printf '%s\n' '  Certificate inspection: unavailable; see the raw build error above.'
    else
      printf '%s\n' '  Certificate presented to the build container:'
      printf '    Subject: %s\n' "$subject"
      printf '    Issuer: %s\n' "$issuer"
      [[ -z "$fingerprint" ]] || printf '    SHA-256: %s\n' "$fingerprint"
      [[ -z "$not_before" ]] || printf '    Valid from: %s\n' "$not_before"
      [[ -z "$not_after" ]] || printf '    Valid until: %s\n' "$not_after"
      printf '%s\n' "$diagnostic_output" | grep -E 'verify error|Verify return code' | sed 's/^/    OpenSSL: /' || true
      printf '%s\n' '  Interpretation: the host network presented a certificate whose issuer is unavailable to the Linux bootstrap container.'
      printf '%s\n' '  Check the host system or user trust store for a root matching the issuer above.'
    fi
    printf '%s\n' '  TLS verification was not disabled, and EPAR did not retry the download insecurely.'
    printf '  Full native-controller build log: %s\n' "$log_path"
  } | tee -a "$log_path" >&2
}

epar_directory_mtime() {
  stat -c %Y "$1" 2>/dev/null || stat -f %m "$1"
}

epar_directory_bytes() {
  du -sk "$1" | awk '{print $1 * 1024}'
}

epar_native_controller_lease_active() {
  local directory="$1"
  local lease lease_host lease_pid lease_started now host
  now="$(date +%s)"
  host="$(hostname 2>/dev/null || true)"
  for lease in "${directory}"/lease-* "${directory}"/lease.*; do
    [[ -f "$lease" ]] || continue
    lease_host="$(sed -n 's/^host=//p' "$lease" | head -n 1)"
    lease_pid="$(sed -n 's/^pid=//p' "$lease" | head -n 1)"
    lease_started="$(sed -n 's/^startedAtUnix=//p' "$lease" | head -n 1)"
    [[ "$lease_started" =~ ^[0-9]+$ ]] || return 0
    if [[ "$lease_host" != "$host" ]]; then
      ((now - lease_started >= 30 * 24 * 60 * 60)) || return 0
      continue
    fi
    [[ "$lease_pid" =~ ^[1-9][0-9]*$ ]] || return 0
    kill -0 "$lease_pid" 2>/dev/null && return 0
  done
  return 1
}

epar_native_controller_build_lease_valid() {
  local lease="$1"
  local name="${lease##*/}"
  [[ -f "$lease" && ! -L "$lease" ]] || return 1
  [[ "$name" =~ ^lease-build-([1-9][0-9]*)\.[0-9A-Za-z]{6}$ ]] || return 1
  local name_pid="${BASH_REMATCH[1]}"
  [[ "$(wc -l <"$lease" | tr -d ' ')" == "4" ]] || return 1
  [[ "$(grep -c '^schemaVersion=' "$lease")" == "1" ]] || return 1
  grep -Fqx 'schemaVersion=1' "$lease" || return 1
  [[ "$(grep -c '^host=.' "$lease")" == "1" ]] || return 1
  [[ "$(grep -c '^pid=[1-9][0-9]*$' "$lease")" == "1" ]] || return 1
  [[ "$(grep -c '^startedAtUnix=[0-9][0-9]*$' "$lease")" == "1" ]] || return 1
  [[ "$(sed -n 's/^pid=//p' "$lease")" == "$name_pid" ]] || return 1
}

epar_prune_native_controller_cache() {
  local cache_root="$1"
  local current_cache_key="$2"
  local remove_current="${3:-0}"
  local now path name mtime bytes manifest unexpected lease valid_build_leases executable
  local retained_count=0
  local retained_bytes=0
  local within_grace=0
  [[ "$current_cache_key" =~ ^[0-9a-f]{64}$ ]] || return 1
  [[ -d "$cache_root" ]] || return 0
  now="$(date +%s)"

  for path in "${cache_root}"/.build-* "${cache_root}"/.build.*; do
    [[ -d "$path" ]] || continue
    [[ ! -L "$path" ]] || continue
    name="${path##*/}"
    [[ "$name" =~ ^\.build[-.][0-9A-Za-z]+$ ]] || continue
    valid_build_leases=0
    for lease in "${path}"/lease-build-*; do
      epar_native_controller_build_lease_valid "$lease" || continue
      valid_build_leases=$((valid_build_leases + 1))
    done
    ((valid_build_leases == 1)) || continue
    epar_native_controller_lease_active "$path" && continue
    mtime="$(epar_directory_mtime "$path")" || continue
    if ((now - mtime >= abandoned_build_grace_seconds)); then
      rm -rf -- "${cache_root:?}/${name}"
    fi
  done

  retention_inventory_file="$(mktemp "${cache_root}/.retention.XXXXXX")"
  for path in "${cache_root}"/*; do
    [[ -d "$path" ]] || continue
    [[ ! -L "$path" ]] || continue
    name="${path##*/}"
    [[ "$name" =~ ^[0-9a-f]{64}$ ]] || continue
    [[ "$name" != "$current_cache_key" || "$remove_current" == 1 ]] || continue
    epar_native_controller_lease_active "$path" && continue
    manifest="${path}/controller-cache.manifest"
    [[ -f "$manifest" && ! -L "$manifest" ]] || continue
    grep -Fqx 'schemaVersion=1' "$manifest" || continue
    grep -Fqx "cacheKey=${name}" "$manifest" || continue
    [[ "$(grep -c '^executable=' "$manifest")" == "1" ]] || continue
    executable="$(sed -n 's/^executable=//p' "$manifest")"
    [[ "$executable" == ephemeral-action-runner || "$executable" == ephemeral-action-runner.exe ]] || continue
    [[ -f "${path}/${executable}" && ! -L "${path}/${executable}" ]] || continue
    unexpected="$(find "$path" -mindepth 1 -maxdepth 1 ! \( -type f \( -name "$executable" -o -name controller-cache.manifest -o -name 'lease-*' -o -name 'lease.*' \) \) -print -quit)"
    [[ -z "$unexpected" ]] || continue
    mtime="$(epar_directory_mtime "$path")" || continue
    bytes="$(epar_directory_bytes "$path")" || continue
    printf '%s:%s:%s\n' "$mtime" "$bytes" "$name" >>"$retention_inventory_file"
  done

  if [[ -d "${cache_root}/${current_cache_key}" ]]; then
    retained_bytes="$(epar_directory_bytes "${cache_root}/${current_cache_key}")"
  fi
  while IFS=: read -r mtime bytes name; do
    [[ "$mtime" =~ ^[0-9]+$ && "$bytes" =~ ^[0-9]+$ && "$name" =~ ^[0-9a-f]{64}$ ]] || continue
    within_grace=0
    ((now - mtime < native_cache_grace_seconds)) && within_grace=1
    if ((within_grace == 1 || (retained_count < native_cache_keep_previous && retained_bytes + bytes <= native_cache_max_bytes))); then
      retained_count=$((retained_count + 1))
      retained_bytes=$((retained_bytes + bytes))
      continue
    fi
    path="${cache_root}/${name}"
    [[ "${path%/*}" == "$cache_root" && "${path##*/}" =~ ^[0-9a-f]{64}$ ]] || return 1
    rm -rf -- "$path"
  done < <(sort -t: -k1,1nr -k3,3 "$retention_inventory_file")
  rm -f -- "$retention_inventory_file"
  retention_inventory_file=""
}

case "$(uname -s)/$(uname -m)" in
  Darwin/arm64) goos=darwin; goarch=arm64 ;;
  Linux/x86_64|Linux/amd64) goos=linux; goarch=amd64 ;;
  Linux/aarch64|Linux/arm64) goos=linux; goarch=arm64 ;;
  *) echo "unsupported native EPAR controller platform: $(uname -s)/$(uname -m)" >&2; exit 1 ;;
esac

native_backend="${EPAR_NATIVE_CONTROLLER_BACKEND:-docker}"
native_use_old="${EPAR_NATIVE_USE_OLD:-0}"
[[ "$native_backend" == local-go || "$native_backend" == docker ]] || { echo "invalid EPAR_NATIVE_CONTROLLER_BACKEND: ${native_backend}" >&2; exit 1; }
[[ "$native_use_old" == 0 || "$native_use_old" == 1 ]] || { echo "invalid EPAR_NATIVE_USE_OLD: ${native_use_old}" >&2; exit 1; }
controller_args=("$@")

cache_root="${repo_root}/.local/bin"
target="${goos}-${goarch}"
current_slot="${cache_root}/${target}"
old_slot="${cache_root}/${target}-old"
lock_directory="${cache_root}/.native-controller-${target}.lock"
native_executable=ephemeral-action-runner
receipt_name=controller.receipt
temporary_directory=""
lease_file=""
build_lease_file=""
retention_inventory_file=""
lock_lease_file=""

cleanup_build() {
  if [[ -n "$temporary_directory" && -d "$temporary_directory" && ! -L "$temporary_directory" ]]; then rm -rf -- "$temporary_directory"; fi
  if [[ -n "$lease_file" && -f "$lease_file" && ! -L "$lease_file" ]]; then rm -f -- "$lease_file"; fi
  if [[ -n "$build_lease_file" && -f "$build_lease_file" && ! -L "$build_lease_file" ]]; then rm -f -- "$build_lease_file"; fi
  if [[ -n "$retention_inventory_file" && -f "$retention_inventory_file" ]]; then rm -f -- "$retention_inventory_file"; fi
  if [[ -n "$lock_lease_file" && -f "$lock_lease_file" && ! -L "$lock_lease_file" ]]; then rm -f -- "$lock_lease_file"; fi
  if [[ -n "$lock_directory" && -d "$lock_directory" && ! -L "$lock_directory" ]]; then rmdir -- "$lock_directory" 2>/dev/null || true; fi
}
trap cleanup_build EXIT INT TERM

epar_reset_receipt() {
  receipt_schemaVersion= receipt_artifactKind= receipt_distribution= receipt_targetOS= receipt_targetArch= receipt_executable=
  receipt_sourceDigest= receipt_buildDigest= receipt_binaryDigest= receipt_sourceRevision= receipt_builder= receipt_toolchain= receipt_completedAtUtc=
  receipt_error=""
}

epar_read_receipt() {
  local path="$1" line key value seen='|' count=0
  epar_reset_receipt
  [[ -f "$path" && ! -L "$path" ]] || { receipt_error="receipt is missing or is not a regular file"; return 1; }
  while IFS= read -r line || [[ -n "$line" ]]; do
    [[ "$line" == *=* ]] || { receipt_error="receipt contains a malformed line"; return 1; }
    key="${line%%=*}"; value="${line#*=}"
    [[ -n "$key" && "$seen" != *"|${key}|"* ]] || { receipt_error="receipt contains duplicate or empty keys"; return 1; }
    seen+="${key}|"; count=$((count + 1))
    case "$key" in
      schemaVersion) receipt_schemaVersion="$value" ;;
      artifactKind) receipt_artifactKind="$value" ;;
      distribution) receipt_distribution="$value" ;;
      targetOS) receipt_targetOS="$value" ;;
      targetArch) receipt_targetArch="$value" ;;
      executable) receipt_executable="$value" ;;
      sourceDigest) receipt_sourceDigest="$value" ;;
      buildDigest) receipt_buildDigest="$value" ;;
      binaryDigest) receipt_binaryDigest="$value" ;;
      sourceRevision) receipt_sourceRevision="$value" ;;
      builder) receipt_builder="$value" ;;
      toolchain) receipt_toolchain="$value" ;;
      completedAtUtc) receipt_completedAtUtc="$value" ;;
      *) receipt_error="receipt contains an unknown key: ${key}"; return 1 ;;
    esac
  done <"$path"
  [[ "$count" == 13 && "$receipt_schemaVersion" == 3 && "$receipt_artifactKind" == native-controller && "$receipt_distribution" == source ]] || { receipt_error="receipt schema or artifact identity is unsupported"; return 1; }
  [[ "$receipt_targetOS" =~ ^(darwin|linux)$ && "$receipt_targetArch" =~ ^(amd64|arm64)$ && "$receipt_executable" == "$native_executable" ]] || { receipt_error="receipt target or executable is invalid"; return 1; }
  [[ "$receipt_sourceDigest" =~ ^sha256:[0-9a-f]{64}$ && "$receipt_buildDigest" =~ ^sha256:[0-9a-f]{64}$ && "$receipt_binaryDigest" =~ ^sha256:[0-9a-f]{64}$ ]] || { receipt_error="receipt digest is invalid"; return 1; }
  [[ -n "$receipt_sourceRevision" && ( "$receipt_builder" == local-go || "$receipt_builder" == docker ) && -n "$receipt_toolchain" ]] || { receipt_error="receipt provenance is incomplete"; return 1; }
  [[ "$receipt_completedAtUtc" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$ ]] || { receipt_error="receipt timestamp is invalid"; return 1; }
}

epar_slot_has_only_owned_entries() {
  local slot="$1" entry name
  for entry in "$slot"/* "$slot"/.[!.]* "$slot"/..?*; do
    [[ -e "$entry" || -L "$entry" ]] || continue
    name="${entry##*/}"
    case "$name" in
      "$native_executable"|"$receipt_name") [[ -f "$entry" && ! -L "$entry" ]] || return 1 ;;
      lease-native-[1-9][0-9]*.[0-9A-Za-z][0-9A-Za-z][0-9A-Za-z][0-9A-Za-z][0-9A-Za-z][0-9A-Za-z]) [[ -f "$entry" && ! -L "$entry" ]] || return 1 ;;
      *) return 1 ;;
    esac
  done
}

epar_read_owned_slot() {
  local slot="$1" expected_name="$2"
  [[ -d "$slot" && ! -L "$slot" && "$(dirname "$slot")" == "$cache_root" && "$(basename "$slot")" == "$expected_name" ]] || { receipt_error="slot is not an exact regular project-local directory"; return 1; }
  epar_slot_has_only_owned_entries "$slot" || { receipt_error="slot contains unknown, symlinked, or non-regular entries"; return 1; }
  epar_read_receipt "$slot/$receipt_name"
}

epar_source_digest() {
  local manifest
  manifest="$({
    printf '%s\n' 'schema=native-controller-source-v1'
    {
      find "${repo_root}/cmd" "${repo_root}/internal" -type f ! -name '*_test.go' -print
      printf '%s\n' "${repo_root}/go.mod" "${repo_root}/go.sum"
    } | LC_ALL=C sort -u | while IFS= read -r file; do
      [[ -f "$file" && ! -L "$file" ]] || { echo "missing or symlinked source input: $file" >&2; exit 1; }
      printf '%s\n%s\n' "${file#"${repo_root}/"}" "$(epar_sha256 "$file")"
    done
  })" || return 1
  printf 'sha256:%s\n' "$(printf '%s' "$manifest" | epar_sha256_stream)"
}

epar_recipe_digest() {
  local manifest
  manifest="$({
    printf '%s\n' 'schema=native-controller-recipe-v1'
    printf '%s\n' "${repo_root}/scripts/build-native-controller.sh" "${repo_root}/scripts/run-with-docker.sh" "${repo_root}/scripts/docker/dev.Dockerfile" "${repo_root}/scripts/bootstrap-trust/main.go" | while IFS= read -r file; do
      [[ -f "$file" && ! -L "$file" ]] || { echo "missing or symlinked build recipe input: $file" >&2; exit 1; }
      printf '%s\n%s\n' "${file#"${repo_root}/"}" "$(epar_sha256 "$file")"
    done
  })" || return 1
  printf 'sha256:%s\n' "$(printf '%s' "$manifest" | epar_sha256_stream)"
}

epar_build_digest() {
  local source_digest="$1" backend="$2" toolchain="$3" recipe_digest="$4"
  printf '%s\n' 'schema=native-controller-build-v1' "sourceDigest=${source_digest}" "target=${goos}/${goarch}" "builder=${backend}" "toolchain=${toolchain}" 'CGO_ENABLED=0' 'trimpath=true' 'ldflags=sourceRevision,sourceDigest,buildDigest' "recipeDigest=${recipe_digest}" | epar_sha256_stream | awk '{print "sha256:" $1}'
}

epar_source_revision_diagnostic() {
  local commit state
  if command -v git >/dev/null 2>&1; then
    commit="$(git -C "$repo_root" rev-parse --verify HEAD 2>/dev/null || true)"
    if [[ "$commit" =~ ^[0-9a-f]{40}$ ]]; then
      state=clean
      [[ -z "$(git -C "$repo_root" status --porcelain=v1 --untracked-files=all 2>/dev/null || true)" ]] || state=dirty
      printf 'git:%s:%s\n' "$commit" "$state"
      return 0
    fi
  fi
  printf '%s\n' "$source_digest"
}

epar_validate_slot() {
  local slot="$1" expected_name="$2" compare_source="$3" expected_source="$4" expected_build="$5" expected_backend="$6" expected_toolchain="$7" actual_binary
  epar_read_owned_slot "$slot" "$expected_name" || return 1
  [[ "$receipt_targetOS" == "$goos" && "$receipt_targetArch" == "$goarch" ]] || { receipt_error="receipt target is ${receipt_targetOS}/${receipt_targetArch}, not ${goos}/${goarch}"; return 1; }
  [[ -x "$slot/$native_executable" && ! -L "$slot/$native_executable" ]] || { receipt_error="controller executable is missing or not executable"; return 1; }
  actual_binary="sha256:$(epar_sha256 "$slot/$native_executable")"
  [[ "$actual_binary" == "$receipt_binaryDigest" ]] || { receipt_error="controller binary digest mismatch"; return 1; }
  if [[ "$compare_source" == 1 ]]; then
    [[ "$receipt_sourceDigest" == "$expected_source" ]] || { receipt_error="source digest mismatch (installed=${receipt_sourceDigest}, current=${expected_source})"; return 1; }
    [[ "$receipt_buildDigest" == "$expected_build" && "$receipt_builder" == "$expected_backend" && "$receipt_toolchain" == "$expected_toolchain" ]] || { receipt_error="build identity mismatch"; return 1; }
  fi
}

epar_acquire_stable_build_lock() {
  local deadline=$(( $(date +%s) + 120 )) valid candidate unexpected
  while ! mkdir "$lock_directory" 2>/dev/null; do
    if [[ -d "$lock_directory" && ! -L "$lock_directory" ]]; then
      valid=0
      for candidate in "$lock_directory"/lease-build-*; do epar_native_controller_build_lease_valid "$candidate" && valid=$((valid + 1)); done
      unexpected="$(find "$lock_directory" -mindepth 1 -maxdepth 1 ! \( -type f -name 'lease-build-*' \) -print -quit)"
      if ((valid == 1)) && [[ -z "$unexpected" ]] && ! epar_native_controller_lease_active "$lock_directory"; then rm -rf -- "$lock_directory"; continue; fi
    fi
    (( $(date +%s) < deadline )) || { echo 'another EPAR native-controller build is still in progress; wait for it to finish and retry.' >&2; return 1; }
    sleep 0.2
  done
  lock_lease_file="$(mktemp "${lock_directory}/lease-build-$$.XXXXXX")"
  printf '%s\n' 'schemaVersion=1' "host=$(hostname 2>/dev/null || true)" "pid=$$" "startedAtUnix=$(date +%s)" >"$lock_lease_file"
}

epar_release_stable_build_lock() {
  if [[ -n "$lock_lease_file" && -f "$lock_lease_file" && ! -L "$lock_lease_file" ]]; then rm -f -- "$lock_lease_file"; fi
  lock_lease_file=""
  if [[ -d "$lock_directory" && ! -L "$lock_directory" ]]; then rmdir -- "$lock_directory" 2>/dev/null || true; fi
}

epar_prepare_docker_toolchain() {
  local available_kib available_bytes previous_dev_image_id dev_image_id
  command -v docker >/dev/null 2>&1 || { echo 'docker command not found. Install Docker and make sure it is available on PATH.' >&2; return 1; }
  [[ "$bootstrap_minimum_free_bytes" =~ ^[1-9][0-9]*$ ]] || { echo 'EPAR_BOOTSTRAP_MIN_FREE_BYTES must be a positive integer byte count.' >&2; return 1; }
  [[ "$go_cache_limit_bytes" =~ ^[1-9][0-9]*$ ]] || { echo 'EPAR_GO_CACHE_LIMIT_BYTES must be a positive integer byte count.' >&2; return 1; }
  available_kib="$(df -Pk "$repo_root" | awk 'NR == 2 { print $4 }')"
  [[ "$available_kib" =~ ^[0-9]+$ ]] || { echo "cannot measure bootstrap storage for ${repo_root}" >&2; return 1; }
  available_bytes=$((available_kib * 1024))
  if ((available_bytes < bootstrap_minimum_free_bytes)) && [[ " $* " != *" --allow-insufficient-storage "* ]]; then
    echo "insufficient bootstrap storage for ${repo_root}: available=${available_bytes} required-reserve=${bootstrap_minimum_free_bytes}. Free space, inspect storage, or retry this invocation with --allow-insufficient-storage." >&2
    return 1
  fi
  if ((available_bytes < bootstrap_minimum_free_bytes)); then echo "WARNING: bootstrap storage is below the ${bootstrap_minimum_free_bytes}-byte reserve; continuing because --allow-insufficient-storage was explicitly supplied." >&2; fi
  previous_dev_image_id="$(epar_docker_image_id "$dev_image")"
  epar_resolve_go_toolchain_image
  docker build --quiet --provenance=false --build-arg "GO_IMAGE=${go_image}" -t "$dev_image" -f "${repo_root}/scripts/docker/dev.Dockerfile" "${repo_root}/scripts/docker" >/dev/null
  dev_image_id="$(epar_docker_image_id "$dev_image")"
  [[ "$dev_image_id" =~ ^sha256:[0-9a-f]{64}$ ]] || { echo "could not resolve the immutable Docker toolchain image ID for ${dev_image}" >&2; return 1; }
  epar_write_bootstrap_acquisition_journal toolchain-built "$EPAR_GO_PREVIOUS_IMAGE_ID" "$EPAR_GO_RESOLVED_IMAGE_ID" "$dev_image_id" "$previous_dev_image_id"
  if ((manage_go_cache == 1)); then epar_ensure_go_cache_volume "$gomod_volume" gomod; epar_ensure_go_cache_volume "$gocache_volume" gobuild; epar_enforce_go_cache_limit; fi
  docker_toolchain="$dev_image_id"
}

epar_build_candidate() {
  local source_digest="$1" recipe_digest="$2" toolchain build_digest build_stderr build_exit build_log_directory build_log_path
  temporary_directory="$(mktemp -d "${cache_root}/.build.XXXXXX")"
  build_lease_file="$(mktemp "${temporary_directory}/lease-build-$$.XXXXXX")"
  printf '%s\n' 'schemaVersion=1' "host=$(hostname 2>/dev/null || true)" "pid=$$" "startedAtUnix=$(date +%s)" >"$build_lease_file"
  build_log_directory="${repo_root}/work/logs"; build_log_path="${build_log_directory}/epar-native-controller-build.log"; mkdir -p "$build_log_directory"
  build_stderr="${temporary_directory}/native-controller-build.stderr"
  case "$native_backend" in
    local-go)
      native_go_bin="${EPAR_NATIVE_GO_BIN:-${EPAR_GO_BIN:-go}}"
      command -v "$native_go_bin" >/dev/null 2>&1 && "$native_go_bin" version >/dev/null 2>&1 || { echo "local Go is not runnable: ${native_go_bin}" >&2; return 1; }
      toolchain="$("$native_go_bin" version)"
      build_digest="$(epar_build_digest "$source_digest" local-go "$toolchain" "$recipe_digest")"
      printf '%s\n' "EPAR native-controller build started at $(date -u +%Y-%m-%dT%H:%M:%SZ)" "Builder: local-go" "Toolchain: ${toolchain}" "Target: ${goos}/${goarch}" '' >"$build_log_path"
      set +e
      CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" "$native_go_bin" build -trimpath -ldflags "-X main.sourceRevision=${source_digest} -X main.sourceDigest=${source_digest} -X main.buildDigest=${build_digest}" -o "${temporary_directory}/${native_executable}" ./cmd/ephemeral-action-runner 2>"$build_stderr"
      build_exit=$?
      set -e
      ;;
    docker)
      epar_prepare_docker_toolchain "${controller_args[@]}" || return 1
      toolchain="$docker_toolchain"
      build_digest="$(epar_build_digest "$source_digest" docker "$toolchain" "$recipe_digest")"
      epar_prepare_bootstrap_build_trust "${controller_args[@]}"
      printf '%s\n' "EPAR native-controller build started at $(date -u +%Y-%m-%dT%H:%M:%SZ)" 'Builder: docker' "Toolchain: ${toolchain}" "Target: ${goos}/${goarch}" "Bootstrap build trust: ${bootstrap_trust_summary}" '' >"$build_log_path"
      set +e
      docker run --rm -e CGO_ENABLED=0 -e "GOOS=${goos}" -e "GOARCH=${goarch}" -e GOTOOLCHAIN=local -e SSL_CERT_FILE=/run/epar-bootstrap-ca.pem -v "${repo_root}:/src:ro" -v "${temporary_directory}:/out" -v "${gomod_volume}:/go/pkg/mod" -v "${gocache_volume}:/root/.cache/go-build" -v "${bootstrap_trust_bundle}:/run/epar-bootstrap-ca.pem:ro" -w /src "$dev_image" go build -trimpath -ldflags "-X main.sourceRevision=${source_digest} -X main.sourceDigest=${source_digest} -X main.buildDigest=${build_digest}" -o "/out/${native_executable}" ./cmd/ephemeral-action-runner 2>"$build_stderr"
      build_exit=$?
      ;;
  esac
  [[ -s "$build_stderr" ]] && cat "$build_stderr" >>"$build_log_path"
  if ((build_exit != 0)); then
    [[ -s "$build_stderr" ]] && cat "$build_stderr" >&2
    [[ "$native_backend" != docker ]] || epar_report_tls_failure "$build_stderr" "$build_log_path"
    return "$build_exit"
  fi
  rm -f -- "$build_stderr" "$build_lease_file"; build_lease_file=""
  [[ -f "${temporary_directory}/${native_executable}" && ! -L "${temporary_directory}/${native_executable}" ]] || { echo 'native EPAR build did not produce the expected binary' >&2; return 1; }
  chmod 0755 "${temporary_directory}/${native_executable}"
  printf '%s\n' 'schemaVersion=3' 'artifactKind=native-controller' 'distribution=source' "targetOS=${goos}" "targetArch=${goarch}" "executable=${native_executable}" "sourceDigest=${source_digest}" "buildDigest=${build_digest}" "binaryDigest=sha256:$(epar_sha256 "${temporary_directory}/${native_executable}")" "sourceRevision=$(epar_source_revision_diagnostic)" "builder=${native_backend}" "toolchain=${toolchain}" "completedAtUtc=$(date -u +%Y-%m-%dT%H:%M:%SZ)" >"${temporary_directory}/${receipt_name}"
  epar_slot_has_only_owned_entries "$temporary_directory" && epar_read_receipt "${temporary_directory}/${receipt_name}" && [[ "$receipt_sourceDigest" == "$source_digest" && "$receipt_buildDigest" == "$build_digest" ]] || { echo 'native EPAR candidate receipt validation failed' >&2; return 1; }
  expected_toolchain="$toolchain"
  expected_build_digest="$build_digest"
}

epar_rotate_candidate() {
  if [[ -e "$current_slot" || -L "$current_slot" ]]; then
    epar_read_owned_slot "$current_slot" "$target" || { echo "refusing to replace current native-controller slot: ${receipt_error}" >&2; return 1; }
    epar_native_controller_lease_active "$current_slot" && { echo 'a current native EPAR controller is running; stop it before rebuilding.' >&2; return 1; }
  fi
  if [[ -e "$old_slot" || -L "$old_slot" ]]; then
    epar_read_owned_slot "$old_slot" "${target}-old" || { echo "refusing to remove old native-controller slot: ${receipt_error}" >&2; return 1; }
    epar_native_controller_lease_active "$old_slot" && { echo 'an old native EPAR controller is running; stop it before rebuilding.' >&2; return 1; }
    rm -rf -- "$old_slot"
  fi
  if [[ -e "$current_slot" ]]; then mv -- "$current_slot" "$old_slot"; fi
  if ! mv -- "$temporary_directory" "$current_slot"; then
    if [[ -d "$old_slot" && ! -e "$current_slot" ]]; then mv -- "$old_slot" "$current_slot" || true; fi
    echo 'native-controller promotion failed; restored the previous current slot.' >&2
    return 1
  fi
  temporary_directory=""
}

epar_enforce_configured_go_cache_limit() {
  [[ "$native_backend" == docker && "$manage_go_cache" == 1 ]] || return 0
  go_cache_limit_bytes="$("${current_slot}/${native_executable}" storage effective-go-cache-limit --project-root "$repo_root")"
  [[ "$go_cache_limit_bytes" =~ ^[1-9][0-9]*$ ]] || { echo 'EPAR returned an invalid configured Go cache limit' >&2; return 1; }
  epar_enforce_go_cache_limit
}

epar_launch_slot() {
  local slot="$1" label="$2" controller_command status=0
  shift 2
  epar_acquire_stable_build_lock || return $?
  if [[ "$label" == old ]]; then
    if ! epar_validate_slot "$slot" "${target}-old" 0 '' '' '' ''; then
      epar_release_stable_build_lock
      echo "selected old native-controller slot became invalid before launch: ${receipt_error}" >&2
      return 1
    fi
  elif ! epar_validate_slot "$slot" "$target" 1 "$source_digest" "$expected_build_digest" "$native_backend" "$expected_toolchain"; then
    epar_release_stable_build_lock
    echo "selected current native-controller slot became invalid before launch: ${receipt_error}" >&2
    return 1
  fi
  lease_file="$(mktemp "${slot}/lease-native-$$.XXXXXX")"
  printf '%s\n' 'schemaVersion=1' "host=$(hostname 2>/dev/null || true)" "pid=$$" "startedAtUnix=$(date +%s)" >"$lease_file"
  # Promotion uses this same exclusive gate. Release it only after the runtime
  # lease is complete, so rotation can never pass its lease check in the gap
  # between launch validation and lease publication.
  epar_release_stable_build_lock
  export EPAR_NATIVE_CONTROLLER=1 EPAR_CONTROLLER_SLOT="$label" DOCKER_CLI_HINTS="${DOCKER_CLI_HINTS:-false}" EPAR_HOST_NAME="${EPAR_HOST_NAME:-$(hostname 2>/dev/null || true)}"
  unset EPAR_BUILD_TRUST_FEED EPAR_HOST_TRUST_FEED EPAR_CONTROLLER_HOST_OS EPAR_HOST_TRUST_INIT_DEFERRED
  if [[ "$label" == current ]]; then epar_enforce_configured_go_cache_limit; fi
  controller_command="${1:-start}"
  if [[ "$controller_command" == init ]]; then epar_host_trust_prepare "$repo_root" "$controller_command" "$@"; fi
  "$slot/$native_executable" "$@" || status=$?
  if [[ "$status" == 0 && "$controller_command" == init ]]; then epar_host_trust_post_init "$repo_root" || status=$?; fi
  epar_host_trust_cleanup
  rm -f -- "$lease_file"; lease_file=""
  return "$status"
}

mkdir -p "$cache_root"
if [[ "$native_use_old" == 1 ]]; then
  epar_validate_slot "$old_slot" "${target}-old" 0 '' '' '' '' || { echo "old native-controller slot is unavailable or invalid: ${receipt_error}" >&2; exit 1; }
  echo "WARNING: executing recovery controller slot ${target}-old; it may use an older configuration contract." >&2
  epar_launch_slot "$old_slot" old "$@"
  exit $?
fi

# Retire only old hash-directory revisions whose manifest and inactive lease
# prove ownership. Current and old platform slots are deliberately excluded.
native_cache_keep_previous=0; native_cache_max_bytes=1; native_cache_grace_seconds=0
if ! epar_prune_native_controller_cache "$cache_root" "$(printf '%064d' 0)" 1; then echo 'warning: native-controller legacy revision cleanup skipped after an error' >&2; fi

source_digest="$(epar_source_digest)" || { echo 'cannot calculate the native-controller source digest' >&2; exit 1; }
recipe_digest="$(epar_recipe_digest)" || { echo 'cannot calculate the native-controller build recipe digest' >&2; exit 1; }
expected_toolchain=""; expected_build_digest=""
case "$native_backend" in
  local-go)
    native_go_bin="${EPAR_NATIVE_GO_BIN:-${EPAR_GO_BIN:-go}}"
    command -v "$native_go_bin" >/dev/null 2>&1 && "$native_go_bin" version >/dev/null 2>&1 || { echo "local Go is not runnable: ${native_go_bin}" >&2; exit 1; }
    expected_toolchain="$("$native_go_bin" version)"
    expected_build_digest="$(epar_build_digest "$source_digest" local-go "$expected_toolchain" "$recipe_digest")"
    ;;
  docker)
    if [[ -e "$current_slot" && ! -L "$current_slot" ]] && epar_read_owned_slot "$current_slot" "$target" && [[ "$receipt_builder" == docker ]]; then
      expected_toolchain="$receipt_toolchain"
      local_docker_toolchain="$(epar_docker_image_id "$dev_image")"
      [[ -z "$local_docker_toolchain" ]] || expected_toolchain="$local_docker_toolchain"
      expected_build_digest="$(epar_build_digest "$source_digest" docker "$expected_toolchain" "$recipe_digest")"
    fi
    ;;
esac

if [[ -n "$expected_build_digest" ]] && epar_validate_slot "$current_slot" "$target" 1 "$source_digest" "$expected_build_digest" "$native_backend" "$expected_toolchain"; then
  epar_launch_slot "$current_slot" current "$@"
  exit $?
fi
if [[ -e "$current_slot" || -L "$current_slot" ]]; then echo "Native controller rebuild required: ${receipt_error:-installed slot has no verifiable build identity}." >&2; else echo 'Native controller build required: no current project-local slot exists.' >&2; fi

epar_acquire_stable_build_lock
if [[ "$native_backend" == local-go ]]; then
  expected_toolchain="$("$native_go_bin" version)"; expected_build_digest="$(epar_build_digest "$source_digest" local-go "$expected_toolchain" "$recipe_digest")"
elif [[ -e "$current_slot" && ! -L "$current_slot" ]] && epar_read_owned_slot "$current_slot" "$target" && [[ "$receipt_builder" == docker ]]; then
  expected_toolchain="$receipt_toolchain"
  local_docker_toolchain="$(epar_docker_image_id "$dev_image")"
  [[ -z "$local_docker_toolchain" ]] || expected_toolchain="$local_docker_toolchain"
  expected_build_digest="$(epar_build_digest "$source_digest" docker "$expected_toolchain" "$recipe_digest")"
fi
if [[ -n "$expected_build_digest" ]] && epar_validate_slot "$current_slot" "$target" 1 "$source_digest" "$expected_build_digest" "$native_backend" "$expected_toolchain"; then
  epar_release_stable_build_lock
  epar_launch_slot "$current_slot" current "$@"
  exit $?
fi
epar_build_candidate "$source_digest" "$recipe_digest" "$@"
epar_rotate_candidate
epar_release_stable_build_lock
epar_launch_slot "$current_slot" current "$@"
