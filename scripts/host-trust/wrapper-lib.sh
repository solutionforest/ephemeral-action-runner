#!/usr/bin/env bash

# Shared host-trust bridge functions for host-side EPAR launchers. Callers must
# set EPAR_HOST_TRUST_HELPER to the real-host helper script before sourcing.

EPAR_HOST_TRUST_FEED_DIR=""
EPAR_HOST_TRUST_WATCH_PID=""
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
  EPAR_HOST_TRUST_WATCH_PID=""
  EPAR_HOST_TRUST_POST_INIT_CONFIG=""
  local config_path feed_path watcher_log subcommand=""
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
    image) [[ "$subcommand" == build ]] || return 0 ;;
    pool) [[ "$subcommand" == up || "$subcommand" == verify ]] || return 0 ;;
    *) return 0 ;;
  esac
  feed_path="$("$EPAR_HOST_TRUST_HELPER" sync --project-root "$project_root" --config "$config_path")" || return $?
  [[ -n "$feed_path" ]] || return 0
  EPAR_HOST_TRUST_FEED_DIR="$(dirname "$feed_path")"
  watcher_log="${EPAR_HOST_TRUST_FEED_DIR}/watcher.log"
  "$EPAR_HOST_TRUST_HELPER" watch --project-root "$project_root" --config "$config_path" --interval 10 >>"$watcher_log" 2>&1 &
  EPAR_HOST_TRUST_WATCH_PID="$!"
  # Fail closed when the singleton watcher rejects the lock or exits before
  # the controller receives its first feed generation.
  sleep 0.1
  if ! kill -0 "$EPAR_HOST_TRUST_WATCH_PID" 2>/dev/null; then
    wait "$EPAR_HOST_TRUST_WATCH_PID" || true
    EPAR_HOST_TRUST_WATCH_PID=""
    echo "host trust watcher failed to start; see $watcher_log" >&2
    return 1
  fi
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
  if [[ -n "${EPAR_HOST_TRUST_WATCH_PID:-}" ]]; then
    kill "$EPAR_HOST_TRUST_WATCH_PID" 2>/dev/null || true
    wait "$EPAR_HOST_TRUST_WATCH_PID" 2>/dev/null || true
    EPAR_HOST_TRUST_WATCH_PID=""
  fi
}
