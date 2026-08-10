#!/usr/bin/env bash
set -euo pipefail

printf 'schemaVersion\t1\n'
printf 'platform\t%s\n' "${EPAR_TEMPLATE_PLATFORM:?EPAR_TEMPLATE_PLATFORM is required}"
printf '\n[os-release]\n'
LC_ALL=C sort /etc/os-release
printf '\n[dpkg]\n'
dpkg-query --show --showformat='${binary:Package}\t${Version}\t${Architecture}\n' | LC_ALL=C sort
printf '\n[tools]\n'
for tool_name in bash docker dockerd git go java node npm python3; do
  tool_path="$(command -v "${tool_name}" 2>/dev/null || true)"
  if [[ -n "${tool_path}" ]]; then
    tool_version="$("${tool_path}" --version 2>&1 | head -n 1 || true)"
    printf '%s\t%s\t%s\n' "${tool_name}" "${tool_path}" "${tool_version}"
  else
    printf '%s\t<absent>\t\n' "${tool_name}"
  fi
done
printf 'actions-runner\t/opt/actions-runner/bin/Runner.Listener\t%s\n' "$(/opt/actions-runner/bin/Runner.Listener --version)"
printf 'tini\t/usr/local/bin/tini\t%s\n' "$(/usr/local/bin/tini --version 2>&1)"
printf '\n[architecture-emulation]\n'
python3 - <<'PY'
import json
from pathlib import Path

compatibility = json.loads(Path('/opt/epar/template-compatibility.json').read_text(encoding='utf-8'))
emulation = compatibility['architectureEmulation']
print(f"backend\t{emulation['backend']}")
print(f"policy\t{emulation['policy']}")
print(f"release\t{emulation['release']}")
print(f"source-index-digest\t{emulation['sourceIndexDigest']}")
print(f"source-manifest-digest\t{emulation['sourceManifestDigest']}")
print(f"qemu-version\t{emulation['qemuVersion']}")
PY
for asset_path in /opt/epar/emulation/binfmt /opt/epar/emulation/qemu-*; do
  printf 'asset\t%s\t%s\t%s\n' "$(basename "${asset_path}")" "$(stat -c '%s' "${asset_path}")" "$(sha256sum "${asset_path}" | cut -d' ' -f1)"
done
for license_path in /usr/share/licenses/epar-emulation/*.txt; do
  printf 'license\t%s\t%s\t%s\n' "$(basename "${license_path}")" "$(stat -c '%s' "${license_path}")" "$(sha256sum "${license_path}" | cut -d' ' -f1)"
done
