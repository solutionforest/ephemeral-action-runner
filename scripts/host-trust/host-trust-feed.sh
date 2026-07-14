#!/usr/bin/env bash
set -euo pipefail

# Publish a read-only, content-addressed host trust feed for an EPAR
# controller. This script runs on the real host; it is deliberately never run
# inside an EPAR runner container.

usage() {
  cat >&2 <<'EOF'
Usage: host-trust-feed.sh sync|watch --project-root <path> --config <path> [--interval <seconds>]

The config must opt in with:
  image:
    hostTrustMode: overlay
EOF
}

command_name="${1:-}"
shift || true
project_root=""
config_path=""
interval=10
script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"

while (($#)); do
  case "$1" in
    --project-root) project_root="${2:?missing value for --project-root}"; shift 2 ;;
    --config) config_path="${2:?missing value for --config}"; shift 2 ;;
    --interval) interval="${2:?missing value for --interval}"; shift 2 ;;
    *) usage; exit 2 ;;
  esac
done

if [[ "$command_name" != "sync" && "$command_name" != "watch" ]] || [[ -z "$project_root" || -z "$config_path" ]]; then
  usage
  exit 2
fi
if [[ ! "$interval" =~ ^[1-9][0-9]*$ ]]; then
  echo "host trust interval must be a positive integer" >&2
  exit 2
fi

project_root="$(cd "$project_root" && pwd -P)"
if [[ "$config_path" != /* ]]; then
  config_path="$project_root/$config_path"
fi
config_path="$(cd "$(dirname "$config_path")" && pwd -P)/$(basename "$config_path")"
if command -v realpath >/dev/null 2>&1; then
  config_path="$(realpath "$config_path")"
fi

sha256_text() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum | awk '{print $1}'
  else
    shasum -a 256 | awk '{print $1}'
  fi
}

config_values() {
  # Supported deliberately-small YAML subset: EPAR's own parser is flat in
  # each section and supports inline or block lists. Emit mode followed by
  # one scope per line.
  awk '
    function trim(s) { sub(/^[[:space:]]+/, "", s); sub(/[[:space:]]+$/, "", s); return s }
    function unquote(s, first, last) {
      s=trim(s); first=substr(s,1,1); last=substr(s,length(s),1)
      if ((first=="\"" && last=="\"") || (first=="\047" && last=="\047")) return substr(s,2,length(s)-2)
      return s
    }
    function uncomment(s) { sub(/[[:space:]]+#.*/, "", s); return s }
    function emit_inline(s, n, a, i) {
      s=trim(s); sub(/^\[/, "", s); sub(/\]$/, "", s); n=split(s,a,",");
      for (i=1;i<=n;i++) { a[i]=unquote(a[i]); if (a[i]!="") print "scope=" a[i] }
    }
    {
      raw=$0; line=trim(uncomment(raw)); if (line=="") next
      if (raw ~ /^[^[:space:]#]/) { in_image=(line=="image:"); list=0; next }
      if (!in_image) next
      if (line ~ /^hostTrustMode[[:space:]]*:/) { sub(/^[^:]*:/, "", line); print "mode=" unquote(line); list=0; next }
      if (line ~ /^hostTrustScopes[[:space:]]*:/) { sub(/^[^:]*:/, "", line); line=trim(line); if (line=="") { list=1 } else { list=0; emit_inline(line) }; next }
      if (list && line ~ /^-[[:space:]]*/) { sub(/^-[[:space:]]*/, "", line); print "scope=" unquote(line); next }
      list=0
    }
  ' "$config_path"
}

if [[ ! -f "$config_path" ]]; then
  # The first `start` can create config interactively. Treat missing config as
  # disabled: native controller code will re-evaluate after init.
  exit 0
fi

mode=""
scopes=()
while IFS= read -r value; do
  case "$value" in
    mode=*) mode="$(printf '%s' "${value#mode=}" | tr '[:upper:]' '[:lower:]')" ;;
    scope=*) scopes+=("$(printf '%s' "${value#scope=}" | tr '[:upper:]' '[:lower:]')") ;;
  esac
done < <(config_values)
if [[ "$mode" != "overlay" ]]; then
  exit 0
fi

host_os="$(uname -s)"
case "$host_os" in
  Linux) cache_root="${XDG_CACHE_HOME:-$HOME/.cache}/ephemeral-action-runner/host-trust" ;;
  Darwin) cache_root="${XDG_CACHE_HOME:-$HOME/Library/Caches}/ephemeral-action-runner/host-trust" ;;
  *) echo "host-trust-feed.sh supports Linux and macOS only; use host-trust-feed.ps1 on Windows" >&2; exit 1 ;;
esac

if ((${#scopes[@]} == 0)); then
  scopes=(system)
fi
for scope in "${scopes[@]}"; do
  case "$scope" in
    system|user) ;;
    *) echo "unsupported hostTrustScopes value on ${host_os}: $scope" >&2; exit 1 ;;
  esac
  if [[ "$host_os" == "Linux" && "$scope" != "system" ]]; then
    echo "Linux host trust overlay supports hostTrustScopes: [system] only" >&2
    exit 1
  fi
done

config_id="$(printf '%s' "$config_path" | sha256_text | cut -c1-32)"
feed_root="$cache_root/$config_id"
lock_dir="$feed_root.lock"

cleanup_lock() {
  rm -f "$lock_dir/pid" 2>/dev/null || true
  rmdir "$lock_dir" 2>/dev/null || true
}
acquire_lock() {
  mkdir -p "$cache_root"
  if mkdir "$lock_dir" 2>/dev/null; then
    printf '%s\n' "$$" >"$lock_dir/pid"
    trap cleanup_lock EXIT
    trap 'exit 0' INT TERM
    return 0
  fi
  local owner=""
  owner="$(cat "$lock_dir/pid" 2>/dev/null || true)"
  if [[ "$owner" =~ ^[1-9][0-9]*$ ]] && ! kill -0 "$owner" 2>/dev/null; then
    rm -f "$lock_dir/pid"
    rmdir "$lock_dir"
    mkdir "$lock_dir"
    printf '%s\n' "$$" >"$lock_dir/pid"
    trap cleanup_lock EXIT
    trap 'exit 0' INT TERM
    return 0
  fi
  echo "host trust watcher already owns config feed $config_id" >&2
  return 1
}

write_pem_blocks() {
  local input="$1" output_dir="$2"
  awk -v out="$output_dir" '
    /-----BEGIN CERTIFICATE-----/ { n++; f=sprintf("%s/raw-%06d.pem",out,n); writing=1 }
    writing { print > f }
    /-----END CERTIFICATE-----/ { close(f); writing=0 }
  ' "$input"
}

write_strict_pem_blocks() {
  local input="$1" output_dir="$2" prefix="$3"
  awk -v out="$output_dir" -v prefix="$prefix" '
    $0 == "-----BEGIN CERTIFICATE-----" {
      if (writing) invalid=1
      n++
      f=sprintf("%s/%s-%06d.pem",out,prefix,n)
      writing=1
      print > f
      next
    }
    $0 == "-----END CERTIFICATE-----" {
      if (!writing) { invalid=1; next }
      print > f
      close(f)
      writing=0
      next
    }
    writing { print > f; next }
    $0 !~ /^[[:space:]]*$/ { invalid=1 }
    END { if (writing || invalid || n == 0) exit 1 }
  ' "$input"
}

collect_certificates() {
  local raw_dir="$1" tmp=""
  mkdir -p "$raw_dir"
  case "$host_os" in
    Linux)
      local bundle="${EPAR_HOST_TRUST_BUNDLE:-}"
      if [[ -z "$bundle" && -f /etc/debian_version && -r /etc/ssl/certs/ca-certificates.crt ]]; then
        bundle=/etc/ssl/certs/ca-certificates.crt
      fi
      if [[ -z "$bundle" ]] && command -v trust >/dev/null 2>&1; then
        bundle="$work/p11-kit-bundle.pem"
        trust extract --filter=ca-anchors --purpose=server-auth --format=pem-bundle --overwrite "$bundle" || return 1
      fi
      if [[ -z "$bundle" ]]; then
        for candidate in /etc/ssl/certs/ca-certificates.crt /etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem /etc/pki/tls/certs/ca-bundle.crt /var/lib/ca-certificates/ca-bundle.pem /etc/ssl/ca-bundle.pem; do
          [[ -r "$candidate" ]] && { bundle="$candidate"; break; }
        done
      fi
      [[ -n "$bundle" && -r "$bundle" ]] || { echo "Linux system CA bundle is unavailable (set EPAR_HOST_TRUST_BUNDLE to override)" >&2; return 1; }
      write_pem_blocks "$bundle" "$raw_dir"
      ;;
    Darwin)
      local mac_dir="$work/macos" candidates="$work/macos/candidates"
      local allowed="$mac_dir/allowed.txt" denied="$mac_dir/denied.txt" candidate_index="$mac_dir/candidates.txt"
      mkdir -p "$candidates"
      : >"$allowed"; : >"$denied"; : >"$candidate_index"
      collect_mac_keychain_certificates() {
        local path="$1" required="$2" prefix="$3"
        local output="$mac_dir/$prefix.pem"
        if ! security find-certificate -a -p "$path" >"$output"; then
          echo "macOS keychain collection failed: $path" >&2
          return 1
        fi
        if ! grep -q '[^[:space:]]' "$output"; then
          if [[ "$required" == required ]]; then
            echo "macOS required keychain returned no certificates: $path" >&2
            return 1
          fi
          return 0
        fi
        if ! write_strict_pem_blocks "$output" "$candidates" "$prefix"; then
          echo "macOS keychain emitted malformed nonempty certificate output: $path" >&2
          return 1
        fi
      }
      local user_trust_enabled=0 user_trust_state=""
      if printf '%s\n' "${scopes[@]}" | grep -qx user; then
        user_trust_state="$(security user-trust-settings-enable)" || return 1
        case "$(printf '%s' "$user_trust_state" | tr '[:upper:]' '[:lower:]')" in
          'user-level trust settings are enabled') user_trust_enabled=1 ;;
          'user-level trust settings are disabled') user_trust_enabled=0 ;;
          *) echo "unrecognized macOS user trust settings state: $user_trust_state" >&2; return 1 ;;
        esac
      fi
      if printf '%s\n' "${scopes[@]}" | grep -qx system; then
        collect_mac_keychain_certificates /System/Library/Keychains/SystemRootCertificates.keychain required system
        collect_mac_keychain_certificates /Library/Keychains/System.keychain optional admin
      fi
      if [[ "$user_trust_enabled" == 1 ]]; then
        local user_keychain found_user_keychain=0 user_keychain_index=0
        while IFS= read -r user_keychain; do
          user_keychain="${user_keychain#\"}"; user_keychain="${user_keychain%\"}"
          [[ -n "$user_keychain" ]] || continue
          user_keychain_index=$((user_keychain_index + 1))
          collect_mac_keychain_certificates "$user_keychain" optional "user-$(printf '%06d' "$user_keychain_index")"
          found_user_keychain=1
        done < <(security list-keychains -d user | sed 's/^[[:space:]]*//')
        [[ "$found_user_keychain" == 1 ]] || { echo "macOS user keychain search list is unavailable" >&2; return 1; }
      fi
      local candidate fingerprint
      shopt -s nullglob
      for candidate in "$candidates"/*.pem; do
        if ! fingerprint="$(openssl x509 -in "$candidate" -noout -fingerprint -sha1 | sed 's/^[^=]*=//;s/://g' | tr '[:lower:]' '[:upper:]')"; then
          echo "macOS keychain emitted an invalid certificate" >&2
          return 1
        fi
        printf '%s %s\n' "$fingerprint" "$candidate" >>"$candidate_index"
        if [[ "$(basename "$candidate")" == system-* ]]; then
          printf '%s\n' "$fingerprint" >>"$allowed"
        fi
      done
      collect_mac_domain_decisions() {
        local domain="$1" required="$2" plist="$mac_dir/$1.plist" json="$mac_dir/$1.json" decisions="$mac_dir/$1.decisions" decision hash export_output
        rm -f "$plist"
        local args=(trust-settings-export)
        if [[ "$domain" == system ]]; then args+=(-s); fi
        if [[ "$domain" == admin ]]; then args+=(-d); fi
        args+=("$plist")
        if ! export_output="$(security "${args[@]}" 2>&1)"; then
          if [[ "$required" != required && "$export_output" == *"SecTrustSettingsCreateExternalRepresentation: No Trust Settings were found."* ]]; then
            return 0
          fi
          [[ -n "$export_output" ]] && printf '%s\n' "$export_output" >&2
          echo "macOS $domain trust settings export failed" >&2
          return 1
        fi
        plutil -convert json -o "$json" "$plist"
        if ! osascript -l JavaScript "$script_dir/macos-trust-settings.js" "$json" >"$decisions"; then
          echo "macOS $domain trust settings parser failed" >&2
          return 1
        fi
        while read -r decision hash; do
          [[ -n "${decision:-}" ]] || continue
          if [[ "$decision" == deny ]]; then printf '%s\n' "$hash" >>"$denied"; else printf '%s\n' "$hash" >>"$allowed"; fi
        done <"$decisions"
      }
      if printf '%s\n' "${scopes[@]}" | grep -qx system; then
        collect_mac_domain_decisions system required
        collect_mac_domain_decisions admin optional
      fi
      if [[ "$user_trust_enabled" == 1 ]]; then collect_mac_domain_decisions user optional; fi
      sort -u -o "$allowed" "$allowed"; sort -u -o "$denied" "$denied"
      while read -r fingerprint candidate; do
        if grep -qx "$fingerprint" "$allowed" && ! grep -qx "$fingerprint" "$denied"; then
          cp "$candidate" "$raw_dir/$(basename "$candidate")"
        fi
      done <"$candidate_index"
      while read -r fingerprint; do
        grep -q "^${fingerprint} " "$candidate_index" || { echo "macOS trust settings reference missing certificate $fingerprint" >&2; return 1; }
      done <"$allowed"
      shopt -u nullglob
      ;;
  esac
}

publish_once() {
  local work raw cert generation generation_dir current_tmp count cert_file cert_hash snapshot scopes_json generated_at expires_at
  work="$(mktemp -d "$cache_root/.host-trust-work.XXXXXX")"
  trap 'rm -rf "$work"' RETURN
  raw="$work/raw"
  cert="$work/certs"
  mkdir -p "$cert"
  collect_certificates "$raw"
  shopt -s nullglob
  for cert_file in "$raw"/*.pem; do
    # Content-address each source certificate and reject malformed output from
    # host tools before publishing it to the controller feed.
    if ! openssl x509 -in "$cert_file" -noout >/dev/null 2>&1; then
      echo "host trust source emitted an invalid certificate" >&2
      return 1
    fi
    if ! openssl x509 -in "$cert_file" -noout -text | grep -q 'CA:TRUE'; then
      continue
    fi
    cert_hash="$(openssl x509 -in "$cert_file" -outform DER | sha256_text)"
    openssl x509 -in "$cert_file" -outform PEM >"$cert/$cert_hash.pem"
  done
  shopt -u nullglob
  count="$(find "$cert" -maxdepth 1 -type f -name '*.pem' | wc -l | tr -d ' ')"
  if [[ "$count" == "0" ]]; then
    echo "host trust overlay requires a nonempty host snapshot" >&2
    return 1
  fi
  generated_at="$(date -u +'%Y-%m-%dT%H:%M:%SZ')"
  if [[ "$host_os" == Darwin ]]; then expires_at="$(date -u -v+30S +'%Y-%m-%dT%H:%M:%SZ')"; else expires_at="$(date -u -d '+30 seconds' +'%Y-%m-%dT%H:%M:%SZ')"; fi
  scopes_json="$(printf '"%s",' "${scopes[@]}")"; scopes_json="[${scopes_json%,}]"
  snapshot="$work/snapshot.json"
  {
    printf '{\n  "schemaVersion": 1,\n  "hostOS": "%s",\n  "scopes": %s,\n  "generatedAt": "%s",\n  "expiresAt": "%s",\n  "certificates": [' "$([ "$host_os" = Linux ] && printf linux || printf darwin)" "$scopes_json" "$generated_at" "$expires_at"
    local first=1 pem_json
    for cert_file in "$cert"/*.pem; do
      cert_hash="$(basename "$cert_file" .pem)"
      pem_json="$(sed ':a;N;$!ba;s/\\/\\\\/g;s/"/\\"/g;s/\n/\\n/g' "$cert_file")"
      [[ $first == 1 ]] || printf ','
      printf '\n    {"sha256":"%s","pem":"%s"}' "$cert_hash" "$pem_json"
      first=0
    done
    printf '\n  ],\n  "distrustSHA256": []\n}\n'
  } >"$snapshot"
  generation="$({
    printf 'epar-hosttrust-feed-generation=1\n'
    printf 'hostOS=%s\n' "$([ "$host_os" = Linux ] && printf linux || printf darwin)"
    printf 'scope=%s\n' "${scopes[@]}" | sort
    find "$cert" -maxdepth 1 -type f -name '*.pem' -exec basename {} .pem \; | sort | sed 's/^/certificate=/'
  } | sha256_text)"
  generation_dir="$feed_root/generations/$generation"
  mkdir -p "$feed_root/generations"
  if [[ ! -d "$generation_dir" ]]; then
    local generation_tmp="$feed_root/generations/.$generation.$$"
    mkdir -p "$generation_tmp"
    cp "$snapshot" "$generation_tmp/snapshot.json"
    if ! mv "$generation_tmp" "$generation_dir" 2>/dev/null; then
      [[ -d "$generation_dir" ]] || return 1
      rm -rf "$generation_tmp"
    fi
  fi
  current_tmp="$feed_root/.current.$$.json"
  cp "$snapshot" "$current_tmp"
  mv -f "$current_tmp" "$feed_root/current.json"
  printf '%s\n' "$feed_root/current.json"
  rm -rf "$work"
  trap - RETURN
}

acquire_lock
case "$command_name" in
  sync) publish_once ;;
  watch)
    publish_once
    while :; do
      sleep "$interval"
      publish_once || echo "host trust snapshot refresh failed; retaining the last published generation" >&2
    done
    ;;
esac
