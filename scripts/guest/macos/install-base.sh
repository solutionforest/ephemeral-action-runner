#!/bin/bash
set -euo pipefail
test "$(uname -s)/$(uname -m)" = Darwin/arm64
# Keep an EPAR account separate from the source image's admin/runner aliases.
if ! id eparrunner >/dev/null 2>&1; then
  uid=502
  while dscl . -search /Users UniqueID "$uid" | grep -q .; do uid=$((uid + 1)); done
  dscl . -create /Users/eparrunner
  dscl . -create /Users/eparrunner UniqueID "$uid"
  dscl . -create /Users/eparrunner PrimaryGroupID 20
  dscl . -create /Users/eparrunner NFSHomeDirectory /Users/eparrunner
  dscl . -create /Users/eparrunner UserShell /bin/bash
  dscl . -create /Users/eparrunner RealName 'EPAR ephemeral runner'
  dscl . -create /Users/eparrunner IsHidden 1
fi
test "$(dscl . -read /Users/eparrunner NFSHomeDirectory | awk '{print $2}')" = /Users/eparrunner
test "$(id -u eparrunner)" -ne 0
install -d -o eparrunner -g staff /Users/eparrunner /opt/actions-runner /var/log/actions-runner
install -d -o root -g wheel /opt/epar
echo 'macOS ARM64 runner account prepared; workflow tools belong in image.customInstallScripts.'
