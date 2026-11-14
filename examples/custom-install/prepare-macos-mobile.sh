#!/bin/bash
set -euo pipefail
test "$(uname -s)/$(uname -m)" = Darwin/arm64
# The default Cirrus Xcode image already contains these large toolchains.
# Put a private clone of its SDK at a path independent of the admin account.
jdk=/opt/homebrew/opt/openjdk@17/libexec/openjdk.jdk/Contents/Home
test -x "$jdk/bin/java"
if [[ ! -d /opt/epar/android-sdk ]]; then
  test -d /Users/admin/android-sdk
  cp -cR /Users/admin/android-sdk /opt/epar/android-sdk
fi
chown -R eparrunner:staff /opt/epar/android-sdk
cat > /opt/epar/runner.env <<EOF
export JAVA_HOME=$jdk
export ANDROID_HOME=/opt/epar/android-sdk
export ANDROID_SDK_ROOT=/opt/epar/android-sdk
export PATH=$jdk/bin:/opt/epar/android-sdk/cmdline-tools/latest/bin:/opt/epar/android-sdk/platform-tools:\$PATH
EOF
chmod 0644 /opt/epar/runner.env
# Xcode tags can be rebuilt with newer Android SDKs. Install the declared
# build inputs once in the reusable image instead of relying on tag contents.
source /opt/epar/runner.env
if [[ ! -f "$ANDROID_HOME/platforms/android-35/android.jar" || ! -d "$ANDROID_HOME/build-tools/35.0.0" ]]; then
  sudo -u eparrunner -H env JAVA_HOME="$JAVA_HOME" PATH="$PATH" ANDROID_HOME="$ANDROID_HOME" \
    "$ANDROID_HOME/cmdline-tools/latest/bin/sdkmanager" --sdk_root="$ANDROID_HOME" \
    'platforms;android-35' 'build-tools;35.0.0'
fi
install -d /opt/epar/features
touch /opt/epar/features/mobile
bash /opt/epar/validate-mobile.sh
