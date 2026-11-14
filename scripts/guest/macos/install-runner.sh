#!/bin/bash
set -euo pipefail
version="${1:?exact runner version required}"
package="${2:?verified runner archive required}"
digest="${3:?SHA-256 digest required}"
[[ "$digest" =~ ^sha256:[0-9a-f]{64}$ ]]
test "$(uname -s)/$(uname -m)" = Darwin/arm64
echo "${digest#sha256:}  $package" | shasum -a 256 -c -
tar -xzf "$package" -C /opt/actions-runner
chown -R eparrunner:staff /opt/actions-runner
installed="$(sudo -u eparrunner -H /opt/actions-runner/bin/Runner.Listener --version | tr -d '\r' | tail -n 1)"
test "$installed" = "$version"
rm -f "$package"
