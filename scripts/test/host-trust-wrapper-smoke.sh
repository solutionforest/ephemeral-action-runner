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

host_os="$(uname -s)"
export XDG_CACHE_HOME="$temporary/cache"
if [[ "$host_os" == Darwin ]]; then
  : "${HOME:?HOME must be set to read the macOS user keychain search list}"
else
  export HOME="$temporary/home"
  mkdir -p "$HOME"
fi
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
  valid_plist="$temporary/macos-valid.plist"
  cat >"$valid_plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict><key>trustVersion</key><integer>1</integer><key>trustList</key><dict><key>$fingerprint</key><dict><key>trustSettings</key><array/></dict></dict></dict></plist>
PLIST
  [[ "$(osascript -l JavaScript "$project_root/scripts/host-trust/macos-trust-settings.js" "$valid_plist")" == "allow $fingerprint" ]]
  cat >"$valid_plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict><key>trustVersion</key><integer>1</integer><key>trustList</key><dict><key>$fingerprint</key><dict><key>trustSettings</key><array><dict><key>kSecTrustSettingsPolicy</key><data>
  AQID
</data><key>kSecTrustSettingsPolicyName</key><string>sslServer</string><key>kSecTrustSettingsResult</key><integer>2</integer></dict></array></dict></dict></dict></plist>
PLIST
  [[ "$(osascript -l JavaScript "$project_root/scripts/host-trust/macos-trust-settings.js" "$valid_plist")" == "allow $fingerprint" ]]
  cat >"$valid_plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict><key>trustVersion</key><integer>1</integer><key>trustList</key><dict><key>$fingerprint</key><dict><key>trustSettings</key><array><dict><key>kSecTrustSettingsPolicy</key><data/><key>kSecTrustSettingsPolicyName</key><string>sslServer</string><key>kSecTrustSettingsResult</key><integer>2</integer></dict></array></dict></dict></dict></plist>
PLIST
  [[ "$(osascript -l JavaScript "$project_root/scripts/host-trust/macos-trust-settings.js" "$valid_plist")" == "allow $fingerprint" ]]
  cat >"$valid_plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict><key>trustVersion</key><integer>1</integer><key>trustList</key><dict><key>$fingerprint</key><dict><key>issuerName</key><data>AQID</data><key>serialNumber</key><data>BAUG</data></dict></dict></dict></plist>
PLIST
  [[ "$(osascript -l JavaScript "$project_root/scripts/host-trust/macos-trust-settings.js" "$valid_plist")" == "allow $fingerprint" ]]
  cat >"$valid_plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict><key>trustVersion</key><integer>1</integer><key>trustList</key><dict><key>$fingerprint</key><dict><key>trustSettings</key><array><dict><key>kSecTrustSettingsPolicy</key><data>AQID</data><key>kSecTrustSettingsPolicyName</key><string>sslServer</string><key>kSecTrustSettingsKeyUsage</key><integer>8</integer><key>kSecTrustSettingsResult</key><integer>2</integer></dict></array></dict></dict></dict></plist>
PLIST
  [[ "$(osascript -l JavaScript "$project_root/scripts/host-trust/macos-trust-settings.js" "$valid_plist")" == "allow $fingerprint" ]]
  for constrained_setting in \
    '<key>kSecTrustSettingsPolicy</key><data>AQID</data><key>kSecTrustSettingsResult</key><integer>2</integer>' \
    '<key>kSecTrustSettingsPolicy</key><string>not-data</string><key>kSecTrustSettingsPolicyName</key><string>sslServer</string><key>kSecTrustSettingsResult</key><integer>2</integer>' \
    '<key>kSecTrustSettingsAllowedError</key><integer>-1</integer><key>kSecTrustSettingsPolicy</key><data>AQID</data><key>kSecTrustSettingsPolicyName</key><string>sslServer</string><key>kSecTrustSettingsResult</key><integer>2</integer>' \
    '<key>kSecTrustSettingsPolicyString</key><string>restricted.example</string><key>kSecTrustSettingsPolicy</key><data>AQID</data><key>kSecTrustSettingsPolicyName</key><string>sslServer</string><key>kSecTrustSettingsResult</key><integer>2</integer>'; do
    cat >"$valid_plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict><key>trustVersion</key><integer>1</integer><key>trustList</key><dict><key>$fingerprint</key><dict><key>trustSettings</key><array><dict>$constrained_setting</dict></array></dict></dict></dict></plist>
PLIST
    [[ -z "$(osascript -l JavaScript "$project_root/scripts/host-trust/macos-trust-settings.js" "$valid_plist")" ]]
  done
  cat >"$valid_plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict><key>trustVersion</key><integer>1</integer><key>trustList</key><dict><key>$fingerprint</key><dict><key>trustSettings</key><array><dict><key>kSecTrustSettingsResult</key><integer>1</integer></dict><dict><key>kSecTrustSettingsPolicyString</key><string>restricted.example</string><key>kSecTrustSettingsResult</key><integer>3</integer></dict></array></dict></dict></dict></plist>
PLIST
  [[ "$(osascript -l JavaScript "$project_root/scripts/host-trust/macos-trust-settings.js" "$valid_plist")" == "deny $fingerprint" ]]
  for invalid in \
    '<plist version="1.0"><dict>' \
    '<plist version="1.0"><dict><key>trustVersion</key><integer>1</integer><key>trustList</key><string>invalid</string></dict></plist>' \
    "<plist version=\"1.0\"><dict><key>trustVersion</key><integer>1</integer><key>trustList</key><dict><key>$fingerprint</key><date>2026-07-14T00:00:00Z</date></dict></dict></plist>" \
    "<plist version=\"1.0\"><dict><key>trustVersion</key><integer>1</integer><key>trustList</key><dict><key>$fingerprint</key><dict><key>trustSettings</key><dict/></dict></dict></dict></plist>" \
    "<plist version=\"1.0\"><dict><key>trustVersion</key><integer>1</integer><key>trustList</key><dict><key>$fingerprint</key><dict><key>trustSettings</key><string>invalid</string></dict></dict></dict></plist>" \
    "<plist version=\"1.0\"><dict><key>trustVersion</key><integer>1</integer><key>trustList</key><dict><key>$fingerprint</key><dict><key>trustSettings</key><data>AQID</data></dict></dict></dict></plist>" \
    "<plist version=\"1.0\"><dict><key>trustVersion</key><integer>1</integer><key>trustList</key><dict><key>$fingerprint</key><dict><key>trustSettings</key><array><date>2026-07-14T00:00:00Z</date></array></dict></dict></dict></plist>" \
    "<plist version=\"1.0\"><dict><key>trustVersion</key><integer>1</integer><key>trustList</key><dict><key>$fingerprint</key><dict><key>trustSettings</key><array><string>invalid</string></array></dict></dict></dict></plist>" \
    "<plist version=\"1.0\"><dict><key>trustVersion</key><integer>1</integer><key>trustList</key><dict><key>$fingerprint</key><dict><key>trustSettings</key><array><dict><key>kSecTrustSettingsPolicy</key><data>A===</data></dict></array></dict></dict></dict></plist>" \
    "<plist version=\"1.0\"><dict><key>trustVersion</key><integer>1</integer><key>trustList</key><dict><key>$fingerprint</key><dict><key>trustSettings</key><array><dict><key>kSecTrustSettingsPolicy</key><data encoding=\"base64\">AQID</data></dict></array></dict></dict></dict></plist>" \
    "<plist version=\"1.0\"><dict><key>trustVersion</key><integer>1</integer><key>trustList</key><dict><key>$fingerprint</key><dict><key>trustSettings</key><array><dict><key>kSecTrustSettingsPolicy</key><data>%%%not-base64%%%</data></dict></array></dict></dict></dict></plist>"; do
    invalid_plist="$temporary/macos-invalid-$RANDOM.plist"
    printf '%s\n' "$invalid" >"$invalid_plist"
    if osascript -l JavaScript "$project_root/scripts/host-trust/macos-trust-settings.js" "$invalid_plist" >/dev/null 2>&1; then
      echo "malformed macOS trust settings plist fixture was accepted: $invalid" >&2
      exit 1
    fi
  done
fi

first="$($helper sync --project-root "$project_root" --config "$config")"
second="$($helper sync --project-root "$project_root" --config "$config")"
[[ "$first" == "$second" && -s "$first" ]]
grep -Fq '"pem":"-----BEGIN CERTIFICATE-----\n' "$first"
grep -Fq '\n-----END CERTIFICATE-----"' "$first"

if [[ "$host_os" == Darwin ]]; then
  system_config="$temporary/system-only.yml"
  cat >"$system_config" <<'YAML'
image:
  hostTrustMode: overlay
  hostTrustScopes: [system]
YAML
  fake_optional_keychains="$temporary/fake-optional-keychains"
  mkdir -p "$fake_optional_keychains"
  cat >"$fake_optional_keychains/security" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
args=" $* "
if [[ "$args" == *" user-trust-settings-enable "* ]]; then
  echo 'User-level Trust Settings are Enabled'
  exit 0
fi
if [[ "$args" == *" list-keychains -d user "* ]]; then
  echo '"/tmp/epar-empty-user.keychain-db"'
  exit 0
fi
if [[ "$args" == *" find-certificate "* ]] && { [[ "$args" == *" -p /Library/Keychains/System.keychain "* ]] || [[ "$args" == *" -p /tmp/epar-empty-user.keychain-db "* ]]; }; then
  exit 0
fi
if [[ "$args" == *" trust-settings-export "* ]]; then
  if [[ "$args" == *" -s "* ]]; then
    output="${!#}"
    cat >"$output" <<'PLIST'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict><key>trustVersion</key><integer>1</integer><key>trustList</key><dict/></dict></plist>
PLIST
    exit 0
  fi
  echo 'SecTrustSettingsCreateExternalRepresentation: No Trust Settings were found.' >&2
  exit 1
fi
exec /usr/bin/security "$@"
SH
  chmod +x "$fake_optional_keychains/security"
  optional_feed="$(PATH="$fake_optional_keychains:$PATH" "$helper" sync --project-root "$project_root" --config "$config")"
  [[ -s "$optional_feed" ]] || { echo "macOS baseline roots were not retained with empty admin/user keychains and empty explicit trust settings" >&2; exit 1; }

  for required_output in empty malformed; do
    fake_required_keychain="$temporary/fake-required-keychain-$required_output"
    mkdir -p "$fake_required_keychain"
    cat >"$fake_required_keychain/security" <<SH
#!/usr/bin/env bash
set -euo pipefail
if [[ " \$* " == *" find-certificate -a -p /System/Library/Keychains/SystemRootCertificates.keychain "* ]]; then
  [[ "$required_output" == malformed ]] && echo 'not a certificate'
  exit 0
fi
exec /usr/bin/security "\$@"
SH
    chmod +x "$fake_required_keychain/security"
    if PATH="$fake_required_keychain:$PATH" "$helper" sync --project-root "$project_root" --config "$system_config" >/dev/null 2>&1; then
      echo "macOS required SystemRootCertificates $required_output output was accepted" >&2
      exit 1
    fi
  done

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

missing_config="$temporary/missing-project/.local/config.yml"
missing_output="$("$helper" sync --project-root "$project_root" --config "$missing_config")"
[[ -z "$missing_output" ]] || { echo "missing config unexpectedly produced a host-trust feed" >&2; exit 1; }

native_project="$temporary/native-go-project"
fake_go="$temporary/fake-go"
fake_go_log="$temporary/fake-go.log"
mkdir -p "$native_project/scripts/host-trust"
cp "$project_root/start" "$native_project/start"
cp "$project_root/scripts/host-trust/wrapper-lib.sh" "$project_root/scripts/host-trust/host-trust-feed.sh" "$native_project/scripts/host-trust/"
cat >"$fake_go" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
if [[ "${1:-}" == version ]]; then
  echo 'go version go1.25.0 test/arch'
  exit 0
fi
printf '%s\n' "$*" >>"$FAKE_GO_LOG"
SH
chmod +x "$fake_go"
(cd "$native_project" && EPAR_GO_BIN="$fake_go" FAKE_GO_LOG="$fake_go_log" ./start)
grep -Fxq 'run ./cmd/ephemeral-action-runner start' "$fake_go_log"

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
