#!/usr/bin/env bash

# Shared host-trust bridge functions for host-side EPAR launchers. Callers must
# set EPAR_HOST_TRUST_HELPER to the real-host helper script before sourcing.

EPAR_HOST_TRUST_FEED_DIR=""
EPAR_BUILD_TRUST_FEED_DIR=""
EPAR_RUNNER_TRUST_FEED_DIR=""
EPAR_HOST_TRUST_WATCH_PID=""
EPAR_TRUST_WATCH_PIDS=()
EPAR_HOST_TRUST_POST_INIT_CONFIG=""

epar_host_trust_config_path() {
  local project_root="$1"
  shift
  local effective_root="$project_root" config_path="${EPAR_CONFIG:-}" arg
  local -a arguments=("$@")
  local index
  for ((index=0; index<${#arguments[@]}; index++)); do
    arg="${arguments[$index]}"
    case "$arg" in
      --project-root)
        ((index + 1 < ${#arguments[@]})) || { echo "$arg requires a value" >&2; return 1; }
        effective_root="${arguments[++index]}"
        ;;
      --project-root=*) effective_root="${arg#--project-root=}" ;;
    esac
  done
  if [[ "$effective_root" != /* ]]; then effective_root="$project_root/$effective_root"; fi
  effective_root="$(cd "$effective_root" && pwd -P)"
  while (($#)); do
    arg="$1"
    case "$arg" in
      --config) config_path="${2:-}"; shift 2; continue ;;
      --config=*) config_path="${arg#--config=}" ;;
    esac
    shift
  done
  if [[ -z "$config_path" ]]; then
    config_path="$effective_root/.local/config.yml"
  elif [[ "$config_path" != /* ]]; then
    config_path="$effective_root/$config_path"
  fi
  printf '%s\n' "$config_path"
}

epar_host_trust_host_os() {
  case "$(uname -s)" in
    Linux) printf '%s\n' linux ;;
    Darwin) printf '%s\n' darwin ;;
    *) printf '%s\n' unknown ;;
  esac
}

epar_host_trust_feed_string_field() {
  local feed_path="$1" field="$2"
  sed -n "s/^[[:space:]]*\"${field}\":[[:space:]]*\"\([^\"]*\)\",\{0,1\}[[:space:]]*$/\1/p" "$feed_path" | head -n 1
}

epar_host_trust_timestamp_epoch() {
  local value="$1"
  case "$(uname -s)" in
    Linux) date -u -d "$value" +%s 2>/dev/null ;;
    Darwin) date -j -u -f '%Y-%m-%dT%H:%M:%SZ' "$value" '+%s' 2>/dev/null ;;
    *) return 1 ;;
  esac
}

epar_host_trust_current_feed_valid() {
  local feed_path="$1" expected_host generated_at expires_at generated_epoch expires_epoch now_epoch
  [[ -s "$feed_path" ]] || return 1
  expected_host="$(epar_host_trust_host_os)"
  grep -Eq '^[[:space:]]*"schemaVersion":[[:space:]]*1,[[:space:]]*$' "$feed_path" || return 1
  grep -Eq "^[[:space:]]*\"hostOS\":[[:space:]]*\"${expected_host}\",[[:space:]]*$" "$feed_path" || return 1
  if [[ "$expected_host" == linux ]]; then
    grep -Eq '^[[:space:]]*"scopes":[[:space:]]*\["system"\],[[:space:]]*$' "$feed_path" || return 1
  else
    grep -Eq '^[[:space:]]*"scopes":[[:space:]]*\["(system|user)"(,"(system|user)")*\],[[:space:]]*$' "$feed_path" || return 1
  fi
  grep -Eq '"sha256":"[0-9a-f]{64}","pem":"-----BEGIN CERTIFICATE-----\\n' "$feed_path" || return 1
  grep -Eq '^[[:space:]]*"distrustSHA256":[[:space:]]*\[\][[:space:]]*$' "$feed_path" || return 1
  [[ "$(tail -n 1 "$feed_path")" == '}' ]] || return 1
  generated_at="$(epar_host_trust_feed_string_field "$feed_path" generatedAt)"
  expires_at="$(epar_host_trust_feed_string_field "$feed_path" expiresAt)"
  [[ -n "$generated_at" && -n "$expires_at" ]] || return 1
  generated_epoch="$(epar_host_trust_timestamp_epoch "$generated_at")" || return 1
  expires_epoch="$(epar_host_trust_timestamp_epoch "$expires_at")" || return 1
  now_epoch="$(date -u +%s)"
  ((expires_epoch > generated_epoch && generated_epoch <= now_epoch + 5 && now_epoch - generated_epoch <= 30 && now_epoch <= expires_epoch))
}

epar_host_trust_wait_for_watcher() {
  local watcher_pid="$1" feed_dir="$2" purpose="$3" watcher_log="$4" timeout_seconds="${5:-5}"
  local deadline=$((SECONDS + timeout_seconds)) owner="" ready_owner="" feed_state
  while :; do
    if ! kill -0 "$watcher_pid" 2>/dev/null; then
      local exit_status=0
      wait "$watcher_pid" 2>/dev/null || exit_status=$?
      echo "$purpose trust watcher exited during startup with status $exit_status before owning its lock, publishing its ready marker, and publishing a valid current.json; see $watcher_log" >&2
      return 1
    fi
    owner="$(cat "${feed_dir}.lock/pid" 2>/dev/null || true)"
    ready_owner="$(cat "${feed_dir}.lock/ready" 2>/dev/null || true)"
    if [[ "$owner" == "$watcher_pid" && "$ready_owner" == "$watcher_pid" ]] && epar_host_trust_current_feed_valid "$feed_dir/current.json"; then
      return 0
    fi
    if ((SECONDS >= deadline)); then
      if [[ ! -f "$feed_dir/current.json" ]]; then feed_state=missing; elif epar_host_trust_current_feed_valid "$feed_dir/current.json"; then feed_state=valid; else feed_state='invalid or stale'; fi
      [[ -n "$owner" ]] || owner='missing or invalid'
      [[ -n "$ready_owner" ]] || ready_owner='missing or invalid'
      echo "$purpose trust watcher did not become ready within ${timeout_seconds}s: expected PID $watcher_pid, observed lock owner $owner and ready marker $ready_owner; current.json is $feed_state; see $watcher_log" >&2
      return 1
    fi
    sleep 0.05
  done
}

epar_host_trust_init_arguments() {
  local -a source=("$@") result=(init)
  local index argument
  for ((index=1; index<${#source[@]}; index++)); do
    argument="${source[$index]}"
    case "$argument" in
      --config|--project-root)
        ((index + 1 < ${#source[@]})) || { echo "$argument requires a value" >&2; return 1; }
        result+=("$argument" "${source[++index]}")
        ;;
      --config=*|--project-root=*) result+=("$argument") ;;
    esac
  done
  if ! printf '%s\n' "${result[@]}" | grep -Eq '^--config(=|$)' && [[ -n "${EPAR_CONFIG:-}" ]]; then
    result+=(--config "$EPAR_CONFIG")
  fi
  printf '%s\0' "${result[@]}"
}

epar_host_trust_prepare() {
  local project_root="$1" command="$2"
  shift 2
  EPAR_HOST_TRUST_FEED_DIR=""
  EPAR_BUILD_TRUST_FEED_DIR=""
  EPAR_RUNNER_TRUST_FEED_DIR=""
  EPAR_HOST_TRUST_WATCH_PID=""
  EPAR_TRUST_WATCH_PIDS=()
  EPAR_HOST_TRUST_POST_INIT_CONFIG=""
  local config_path feed_path feed_dir watcher_log subcommand="" purpose watcher_pid sync_status
  if (($# >= 2)); then subcommand="$2"; fi
  config_path="$(epar_host_trust_config_path "$project_root" "$@")"
  case "$command" in
    init)
      # A newly written config cannot be examined before init. The caller runs
      # the one-shot preflight after a successful init.
      EPAR_HOST_TRUST_POST_INIT_CONFIG="$config_path"
      return 0
      ;;
    start) ;;
    image) [[ "$subcommand" == build || "$subcommand" == update ]] || return 0 ;;
    pool) [[ "$subcommand" == up || "$subcommand" == verify ]] || return 0 ;;
    *) return 0 ;;
  esac
  for purpose in build runner; do
    feed_path="$("$EPAR_HOST_TRUST_HELPER" sync --project-root "$project_root" --config "$config_path" --purpose "$purpose")" || {
      sync_status=$?
      epar_host_trust_cleanup
      return "$sync_status"
    }
    [[ -n "$feed_path" ]] || continue
    feed_dir="$(dirname "$feed_path")"
    if [[ "$purpose" == build ]]; then EPAR_BUILD_TRUST_FEED_DIR="$feed_dir"; else EPAR_RUNNER_TRUST_FEED_DIR="$feed_dir"; fi
    watcher_log="${feed_dir}/watcher.log"
    "$EPAR_HOST_TRUST_HELPER" watch --project-root "$project_root" --config "$config_path" --purpose "$purpose" --interval 10 >>"$watcher_log" 2>&1 &
    watcher_pid="$!"
    EPAR_TRUST_WATCH_PIDS+=("$watcher_pid")
    if ! epar_host_trust_wait_for_watcher "$watcher_pid" "$feed_dir" "$purpose" "$watcher_log"; then
      epar_host_trust_cleanup
      return 1
    fi
  done
  EPAR_HOST_TRUST_FEED_DIR="$EPAR_RUNNER_TRUST_FEED_DIR"
  if ((${#EPAR_TRUST_WATCH_PIDS[@]} > 0)); then EPAR_HOST_TRUST_WATCH_PID="${EPAR_TRUST_WATCH_PIDS[0]}"; fi
}

epar_host_trust_post_init() {
  local project_root="$1"
  [[ -n "$EPAR_HOST_TRUST_POST_INIT_CONFIG" ]] || return 0
  if "$EPAR_HOST_TRUST_HELPER" sync --project-root "$project_root" --config "$EPAR_HOST_TRUST_POST_INIT_CONFIG" >/dev/null; then
    return 0
  fi
  local temporary="${EPAR_HOST_TRUST_POST_INIT_CONFIG}.host-trust-disabled.$$"
  awk '
    /^[[:space:]]*hostTrustMode:[[:space:]]*overlay[[:space:]]*($|#)/ {
      sub(/hostTrustMode:[[:space:]]*overlay/, "hostTrustMode: disabled")
    }
    { print }
  ' "$EPAR_HOST_TRUST_POST_INIT_CONFIG" >"$temporary"
  mv -f "$temporary" "$EPAR_HOST_TRUST_POST_INIT_CONFIG"
  echo "host trust preflight failed; the generated config was left with image.hostTrustMode: disabled" >&2
  return 1
}

epar_host_trust_cleanup() {
  local watcher_pid
  for watcher_pid in "${EPAR_TRUST_WATCH_PIDS[@]:-}"; do
    [[ -n "$watcher_pid" ]] || continue
    kill "$watcher_pid" 2>/dev/null || true
    wait "$watcher_pid" 2>/dev/null || true
  done
  EPAR_TRUST_WATCH_PIDS=()
  EPAR_HOST_TRUST_WATCH_PID=""
}
