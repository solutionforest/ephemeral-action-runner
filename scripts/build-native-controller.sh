#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
EPAR_HOST_TRUST_HELPER="${repo_root}/scripts/host-trust/host-trust-feed.sh"
source "${repo_root}/scripts/host-trust/wrapper-lib.sh"
go_image="${GO_DOCKER_IMAGE:-golang:1.25}"
dev_image="${EPAR_DEV_IMAGE:-epar-dev-toolchain}"
native_cache_keep_previous=5
native_cache_max_bytes=$((256 * 1024 * 1024))
native_cache_grace_seconds=$((7 * 24 * 60 * 60))
abandoned_build_grace_seconds=$((24 * 60 * 60))
bootstrap_minimum_free_bytes="${EPAR_BOOTSTRAP_MIN_FREE_BYTES:-$((1 * 1024 * 1024 * 1024))}"
go_cache_limit_bytes="${EPAR_GO_CACHE_LIMIT_BYTES:-$((10 * 1024 * 1024 * 1024))}"

command -v docker >/dev/null 2>&1 || { echo "docker command not found. Install Docker Desktop, Docker Engine, or a compatible Docker host." >&2; exit 1; }
command -v shasum >/dev/null 2>&1 || { echo "shasum is required to build the native EPAR cache key." >&2; exit 1; }
[[ "$bootstrap_minimum_free_bytes" =~ ^[1-9][0-9]*$ ]] || { echo "EPAR_BOOTSTRAP_MIN_FREE_BYTES must be a positive integer byte count." >&2; exit 1; }
[[ "$go_cache_limit_bytes" =~ ^[1-9][0-9]*$ ]] || { echo "EPAR_GO_CACHE_LIMIT_BYTES must be a positive integer byte count." >&2; exit 1; }
project_id="$(printf '%s' "$repo_root" | shasum -a 256 | awk '{print substr($1,1,12)}')"
gomod_volume="${EPAR_GOMOD_VOLUME:-epar-${project_id}-gomod}"
gocache_volume="${EPAR_GOCACHE_VOLUME:-epar-${project_id}-gocache}"
manage_go_cache=0
if [[ -z "${EPAR_GOMOD_VOLUME:-}" && -z "${EPAR_GOCACHE_VOLUME:-}" ]]; then manage_go_cache=1; fi
bootstrap_available_kib="$(df -Pk "$repo_root" | awk 'NR == 2 { print $4 }')"
[[ "$bootstrap_available_kib" =~ ^[0-9]+$ ]] || { echo "cannot measure bootstrap storage for ${repo_root}" >&2; exit 1; }
bootstrap_available_bytes=$((bootstrap_available_kib * 1024))
if ((bootstrap_available_bytes < bootstrap_minimum_free_bytes)); then
  if [[ " $* " == *" --allow-insufficient-storage "* ]]; then
    echo "WARNING: bootstrap storage is below the ${bootstrap_minimum_free_bytes}-byte reserve; continuing because --allow-insufficient-storage was explicitly supplied." >&2
  else
    echo "insufficient bootstrap storage for ${repo_root}: available=${bootstrap_available_bytes} required-reserve=${bootstrap_minimum_free_bytes}. Free space, inspect storage, or retry this invocation with --allow-insufficient-storage." >&2
    exit 1
  fi
fi

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
    [[ "$name" != "$current_cache_key" ]] || continue
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

docker build --quiet \
  --build-arg "GO_IMAGE=${go_image}" \
  -t "$dev_image" \
  -f "${repo_root}/scripts/docker/dev.Dockerfile" \
  "${repo_root}/scripts/docker" >/dev/null
dev_image_id="$(docker image inspect --format '{{.Id}}' "$dev_image")"
[[ "$dev_image_id" =~ ^sha256:[0-9a-f]{64}$ ]] || { echo "could not resolve the immutable Docker toolchain image ID for ${dev_image}" >&2; exit 1; }
if ((manage_go_cache == 1)); then
  epar_ensure_go_cache_volume "$gomod_volume" gomod
  epar_ensure_go_cache_volume "$gocache_volume" gobuild
  epar_enforce_go_cache_limit
fi

git_commit=unknown
source_state=unknown
if command -v git >/dev/null 2>&1; then
  git_commit_candidate="$(git -C "$repo_root" rev-parse --verify HEAD 2>/dev/null || true)"
  if [[ "$git_commit_candidate" =~ ^[0-9a-f]{40}$ ]]; then
    git_commit="$git_commit_candidate"
    if git -C "$repo_root" status --porcelain=v1 --untracked-files=all >/dev/null 2>&1; then
      if [[ -z "$(git -C "$repo_root" status --porcelain=v1 --untracked-files=all)" ]]; then source_state=clean; else source_state=dirty; fi
    fi
  fi
fi

source_manifest="$(
  printf '%s\n%s\n%s\n%s\n' "${goos}/${goarch}" "$dev_image_id" "$git_commit" "$source_state"
  {
    find "${repo_root}/cmd" "${repo_root}/internal" -type f -name '*.go' -print
    find "${repo_root}/scripts/docker" -type f -print
    printf '%s\n' "${repo_root}/go.mod" "${repo_root}/go.sum" "${repo_root}/scripts/build-native-controller.sh"
  } | LC_ALL=C sort | while IFS= read -r file; do
    printf '%s\n' "${file#"${repo_root}/"}"
    shasum -a 256 "$file" | awk '{print $1}'
  done
)"
cache_key="$(printf '%s' "$source_manifest" | shasum -a 256 | awk '{print $1}')"
case "$source_state" in
  clean) controller_source_revision="sha256:${cache_key}" ;;
  dirty) controller_source_revision="dirty:sha256:${cache_key}" ;;
  *) controller_source_revision=unknown ;;
esac
cache_root="${repo_root}/.local/bin"
cache_directory="${cache_root}/${cache_key}"
binary="${cache_directory}/ephemeral-action-runner"

temporary_directory=""
lease_file=""
build_lease_file=""
retention_inventory_file=""
manifest_temporary=""
cleanup_build() {
  if [[ -n "$temporary_directory" && -d "$temporary_directory" ]]; then rm -rf -- "$temporary_directory"; fi
  if [[ -n "$lease_file" && -f "$lease_file" ]]; then rm -f -- "$lease_file"; fi
  if [[ -n "$build_lease_file" && -f "$build_lease_file" ]]; then rm -f -- "$build_lease_file"; fi
  if [[ -n "$retention_inventory_file" && -f "$retention_inventory_file" ]]; then rm -f -- "$retention_inventory_file"; fi
  if [[ -n "$manifest_temporary" && -f "$manifest_temporary" ]]; then rm -f -- "$manifest_temporary"; fi
}
trap cleanup_build EXIT INT TERM

if [[ ! -x "$binary" ]]; then
  mkdir -p "$cache_root"
  temporary_directory="$(mktemp -d "${cache_root}/.build.XXXXXX")"
  build_lease_file="$(mktemp "${temporary_directory}/lease-build-$$.XXXXXX")"
  printf '%s\n' \
    'schemaVersion=1' \
    "host=$(hostname 2>/dev/null || true)" \
    "pid=$$" \
    "startedAtUnix=$(date +%s)" >"$build_lease_file"
  docker run --rm \
    -e CGO_ENABLED=0 \
    -e "GOOS=${goos}" \
    -e "GOARCH=${goarch}" \
    -v "${repo_root}:/src:ro" \
    -v "${temporary_directory}:/out" \
    -v "${gomod_volume}:/go/pkg/mod" \
    -v "${gocache_volume}:/root/.cache/go-build" \
    -w /src \
    "$dev_image" \
    go build -trimpath -ldflags "-X main.sourceRevision=${controller_source_revision}" -o /out/ephemeral-action-runner ./cmd/ephemeral-action-runner
  [[ -f "${temporary_directory}/ephemeral-action-runner" ]] || { echo "native EPAR build did not produce the expected binary" >&2; exit 1; }
  chmod 0755 "${temporary_directory}/ephemeral-action-runner"
  if [[ ! -e "$cache_directory" ]]; then
    rm -f -- "$build_lease_file"
    build_lease_file=""
    mv -- "$temporary_directory" "$cache_directory"
    temporary_directory=""
  fi
fi
if ((manage_go_cache == 1)); then
  go_cache_limit_bytes="$("$binary" storage effective-go-cache-limit --project-root "$repo_root")"
  [[ "$go_cache_limit_bytes" =~ ^[1-9][0-9]*$ ]] || { echo "EPAR returned an invalid configured Go cache limit" >&2; exit 1; }
  epar_enforce_go_cache_limit
fi

manifest_temporary="$(mktemp "${cache_directory}/.manifest.XXXXXX")"
printf '%s\n' \
  'schemaVersion=1' \
  "cacheKey=${cache_key}" \
  'executable=ephemeral-action-runner' \
  "completedAtUnix=$(date +%s)" >"$manifest_temporary"
mv -f -- "$manifest_temporary" "${cache_directory}/controller-cache.manifest"
manifest_temporary=""

lease_file="$(mktemp "${cache_directory}/lease.$$.XXXXXX")"
printf '%s\n' \
  'schemaVersion=1' \
  "host=$(hostname 2>/dev/null || true)" \
  "pid=$$" \
  "startedAtUnix=$(date +%s)" >"$lease_file"
if ! epar_prune_native_controller_cache "$cache_root" "$cache_key"; then
  echo "warning: native-controller cache retention skipped after an error" >&2
fi

export EPAR_NATIVE_CONTROLLER=1
export EPAR_CONTROLLER_HOST_OS="$goos"
export DOCKER_CLI_HINTS="${DOCKER_CLI_HINTS:-false}"
export EPAR_HOST_NAME="${EPAR_HOST_NAME:-$(hostname 2>/dev/null || true)}"
controller_command="${1:-start}"
epar_host_trust_prepare "$repo_root" "$controller_command" "$@"
if [[ -n "${EPAR_BUILD_TRUST_FEED_DIR}" ]]; then export EPAR_BUILD_TRUST_FEED="${EPAR_BUILD_TRUST_FEED_DIR}/current.json"; fi
if [[ -n "${EPAR_RUNNER_TRUST_FEED_DIR}" ]]; then export EPAR_HOST_TRUST_FEED="${EPAR_RUNNER_TRUST_FEED_DIR}/current.json"; fi
status=0
"$binary" "$@" || status=$?
epar_host_trust_cleanup
cleanup_build
trap - EXIT INT TERM
exit "$status"
