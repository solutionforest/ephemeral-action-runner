#!/usr/bin/env bash
set -euo pipefail

marker="${EPAR_HOST_TRUST_MARKER:-/opt/epar/host-trust-generation.json}"
lease="${EPAR_HOST_TRUST_LEASE:-/run/epar/host-trust-lease.json}"

if [[ ! -s "${marker}" ]]; then
  echo "EPAR host-trust gate: image generation marker is missing" >&2
  exit 1
fi
if [[ ! -s "${lease}" ]]; then
  echo "EPAR host-trust gate: controller lease is missing" >&2
  exit 1
fi
if ! command -v python3 >/dev/null 2>&1; then
  echo "EPAR host-trust gate: python3 is required" >&2
  exit 1
fi

python3 - "${marker}" "${lease}" <<'PY'
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

if marker.get("mode") != "overlay" or not marker.get("generation"):
    raise SystemExit("EPAR host-trust gate: invalid image trust policy")

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
