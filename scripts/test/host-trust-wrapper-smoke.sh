#!/usr/bin/env bash
set -euo pipefail

project_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
helper="$project_root/scripts/host-trust/host-trust-feed.sh"
temporary="$(mktemp -d)"
watch_pid=""
cleanup() {
  if [[ -n "$watch_pid" ]]; then
    kill "$watch_pid" 2>/dev/null || true
    wait "$watch_pid" 2>/dev/null || true
  fi
  rm -rf "$temporary"
}
trap cleanup EXIT INT TERM

export HOME="$temporary/home"
export XDG_CACHE_HOME="$temporary/cache"
mkdir -p "$HOME"
host_os="$(uname -s)"
if [[ "$host_os" == Linux && -z "${EPAR_HOST_TRUST_BUNDLE:-}" ]]; then
  source_bundle=/etc/ssl/certs/ca-certificates.crt
  [[ -r "$source_bundle" ]] || { echo "system CA bundle unavailable for wrapper smoke" >&2; exit 1; }
  EPAR_HOST_TRUST_BUNDLE="$temporary/one-system-root.pem"
  awk '/-----BEGIN CERTIFICATE-----/{copy=1} copy{print} /-----END CERTIFICATE-----/{exit}' "$source_bundle" >"$EPAR_HOST_TRUST_BUNDLE"
  export EPAR_HOST_TRUST_BUNDLE
fi
config="$temporary/config.yml"
if [[ "$host_os" == Darwin ]]; then scopes='[system, user]'; else scopes='[system]'; fi
cat >"$config" <<YAML
image:
  hostTrustMode: overlay
  hostTrustScopes: $scopes
YAML

if [[ "$host_os" == Darwin ]]; then
  fingerprint="AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
  valid_json="$temporary/macos-valid.json"
  printf '{"trustVersion":1,"trustList":{"%s":{"trustSettings":[]}}}\n' "$fingerprint" >"$valid_json"
  [[ "$(osascript -l JavaScript "$project_root/scripts/host-trust/macos-trust-settings.js" "$valid_json")" == "allow $fingerprint" ]]
  printf '{"trustVersion":1,"trustList":{"%s":{"trustSettings":[{"kSecTrustSettingsPolicy":"opaque","kSecTrustSettingsPolicyName":"sslServer","kSecTrustSettingsKeyUsage":8,"kSecTrustSettingsResult":2}]}}}\n' "$fingerprint" >"$valid_json"
  [[ "$(osascript -l JavaScript "$project_root/scripts/host-trust/macos-trust-settings.js" "$valid_json")" == "allow $fingerprint" ]]
  printf '{"trustVersion":1,"trustList":{"%s":{"trustSettings":[{"kSecTrustSettingsKeyUsage":9,"kSecTrustSettingsResult":1}]}}}\n' "$fingerprint" >"$valid_json"
  [[ -z "$(osascript -l JavaScript "$project_root/scripts/host-trust/macos-trust-settings.js" "$valid_json")" ]]
  for invalid in \
    '{"trustVersion":1,"trustList":null}' \
    "{\"trustVersion\":1,\"trustList\":{\"$fingerprint\":{}}}" \
    "{\"trustVersion\":1,\"trustList\":{\"$fingerprint\":{\"trustSettings\":[null]}}}" \
    "{\"trustVersion\":1,\"trustList\":{\"$fingerprint\":{\"trustSettings\":[5]}}}"; do
    invalid_json="$temporary/macos-invalid-$RANDOM.json"
    printf '%s\n' "$invalid" >"$invalid_json"
    if osascript -l JavaScript "$project_root/scripts/host-trust/macos-trust-settings.js" "$invalid_json" >/dev/null 2>&1; then
      echo "malformed macOS trust settings fixture was accepted: $invalid" >&2
      exit 1
    fi
  done
fi

first="$($helper sync --project-root "$project_root" --config "$config")"
second="$($helper sync --project-root "$project_root" --config "$config")"
[[ "$first" == "$second" && -s "$first" ]]

if [[ "$host_os" == Darwin ]]; then
  fake_osascript="$temporary/fake-osascript"
  mkdir -p "$fake_osascript"
  printf '#!/usr/bin/env bash\nexit 17\n' >"$fake_osascript/osascript"
  chmod +x "$fake_osascript/osascript"
  if PATH="$fake_osascript:$PATH" "$helper" sync --project-root "$project_root" --config "$config" >/dev/null 2>&1; then
    echo "macOS parser process failure was ignored" >&2
    exit 1
  fi
  fake_security="$temporary/fake-security"
  mkdir -p "$fake_security"
  cat >"$fake_security/security" <<'SH'
#!/usr/bin/env bash
if [[ " $* " == *" trust-settings-export "* && " $* " == *" -d "* ]]; then
  echo "transient admin trust export failure" >&2
  exit 19
fi
exec /usr/bin/security "$@"
SH
  chmod +x "$fake_security/security"
  if PATH="$fake_security:$PATH" "$helper" sync --project-root "$project_root" --config "$config" >/dev/null 2>&1; then
    echo "macOS optional-domain export failure was treated as absence" >&2
    exit 1
  fi
fi

EPAR_HOST_TRUST_HELPER="$helper"
# shellcheck disable=SC1091
. "$project_root/scripts/host-trust/wrapper-lib.sh"
nested_root="$temporary/nested-project"
mkdir -p "$nested_root/.local"
[[ "$(epar_host_trust_config_path "$temporary" start --project-root nested-project)" == "$nested_root/.local/config.yml" ]]
[[ "$(epar_host_trust_config_path "$temporary" start --project-root=nested-project --config custom.yml)" == "$nested_root/custom.yml" ]]
epar_host_trust_prepare "$project_root" pool pool status --config "$config"
[[ -z "$EPAR_HOST_TRUST_WATCH_PID" ]]

epar_host_trust_prepare "$project_root" pool pool up --config "$config"
watch_pid="$EPAR_HOST_TRUST_WATCH_PID"
[[ -n "$watch_pid" ]] && kill -0 "$watch_pid"
if "$helper" sync --project-root "$project_root" --config "$config" >/dev/null 2>&1; then
  echo "second controller unexpectedly acquired the live wrapper lock" >&2
  exit 1
fi
epar_host_trust_cleanup
watch_pid=""

current="$($helper sync --project-root "$project_root" --config "$config")"
lock_dir="$(dirname "$current").lock"
mkdir "$lock_dir"
printf '%s\n' 2147483647 >"$lock_dir/pid"
current="$($helper sync --project-root "$project_root" --config "$config")"
[[ -s "$current" && ! -e "$lock_dir" ]]

default_config="$temporary/default-scopes.yml"
cat >"$default_config" <<'YAML'
image:
  hostTrustMode: overlay
YAML
default_current="$($helper sync --project-root "$project_root" --config "$default_config")"
grep -Eq '"scopes"[[:space:]]*:[[:space:]]*\[[[:space:]]*"system"[[:space:]]*\]' "$default_current"

quoted_config="$temporary/quoted-values.yml"
cat >"$quoted_config" <<'YAML'
image:
  hostTrustMode: "overlay"
  hostTrustScopes:
    - "system"
YAML
quoted_current="$($helper sync --project-root "$project_root" --config "$quoted_config")"
grep -Eq '"scopes"[[:space:]]*:[[:space:]]*\[[[:space:]]*"system"[[:space:]]*\]' "$quoted_current"

mixed_case_config="$temporary/mixed-case.yml"
cat >"$mixed_case_config" <<'YAML'
image:
  hostTrustMode: Overlay
  hostTrustScopes: [System]
YAML
mixed_case_current="$($helper sync --project-root "$project_root" --config "$mixed_case_config")"
grep -Eq '"scopes"[[:space:]]*:[[:space:]]*\[[[:space:]]*"system"[[:space:]]*\]' "$mixed_case_current"

echo "Unix host-trust wrapper lifecycle smoke passed"
