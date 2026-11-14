#!/bin/bash
set -euo pipefail
sudo -u eparrunner -H /bin/bash -c '
  set -euo pipefail
  source /opt/epar/runner.env
  xcodebuild -version
  xcrun --sdk iphoneos --show-sdk-version
  xcrun --sdk iphonesimulator --show-sdk-version
  java -version
  test -f "$ANDROID_HOME/platforms/android-35/android.jar"
  "$ANDROID_HOME/build-tools/35.0.0/aapt" version
  "$ANDROID_HOME/build-tools/35.0.0/apksigner" version
  test -w "$ANDROID_HOME"
'
