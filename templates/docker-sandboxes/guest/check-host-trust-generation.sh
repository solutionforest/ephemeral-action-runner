#!/usr/bin/env bash
set -euo pipefail

if [[ "$#" -eq 0 ]]; then
  marker="/opt/epar/host-trust-generation.json"
  lease="/run/epar/host-trust-lease.json"
elif [[ "$#" -eq 2 ]]; then
  marker="$1"
  lease="$2"
else
  echo "EPAR host-trust gate: invalid invocation" >&2
  exit 1
fi

if [[ ! -s "${marker}" ]]; then
  echo "EPAR host-trust gate: image generation marker is missing" >&2
  exit 1
fi
if [[ ! -s "${lease}" ]]; then
  echo "EPAR host-trust gate: controller lease is missing" >&2
  exit 1
fi
if [[ ! -x /usr/bin/python3 ]]; then
  echo "EPAR host-trust gate: python3 is required" >&2
  exit 1
fi

/usr/bin/env -i PATH=/usr/bin:/bin LANG=C.UTF-8 /usr/bin/python3 -I -S - "${marker}" "${lease}" <<'PY'
import datetime
import json
import sys

marker_path, lease_path = sys.argv[1:]

def read_json(path, label):
    try:
        with open(path, "r", encoding="utf-8") as handle:
            value = json.load(handle)
    except Exception as exc:
        raise SystemExit(f"EPAR host-trust gate: invalid {label}: {exc}")
    if not isinstance(value, dict):
        raise SystemExit(f"EPAR host-trust gate: {label} must be a JSON object")
    return value

marker = read_json(marker_path, "image marker")
lease = read_json(lease_path, "controller lease")

for key in ("generation", "hostOS", "mode", "scopes"):
    if marker.get(key) != lease.get(key):
        raise SystemExit(
            f"EPAR host-trust gate: {key} mismatch "
            f"(image={marker.get(key)!r}, lease={lease.get(key)!r})"
        )

if marker.get("mode") not in ("overlay", "disabled") or not marker.get("generation"):
    raise SystemExit("EPAR host-trust gate: invalid image trust policy")
if marker.get("mode") == "disabled" and marker.get("scopes") != []:
    raise SystemExit("EPAR host-trust gate: disabled trust mode must not carry scopes")

expires = lease.get("expiresAt")
if not isinstance(expires, str) or not expires:
    raise SystemExit("EPAR host-trust gate: lease expiry is missing")
try:
    expires_at = datetime.datetime.fromisoformat(expires.replace("Z", "+00:00"))
except ValueError as exc:
    raise SystemExit(f"EPAR host-trust gate: invalid lease expiry: {exc}")
if expires_at.tzinfo is None:
    raise SystemExit("EPAR host-trust gate: lease expiry must include a timezone")
now = datetime.datetime.now(datetime.timezone.utc)
if expires_at <= now:
    raise SystemExit(
        "EPAR host-trust gate: lease expired at "
        + expires_at.astimezone(datetime.timezone.utc).isoformat()
    )

print("EPAR host-trust gate: generation and lease are current")
PY
