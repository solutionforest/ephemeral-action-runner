#!/bin/bash
set -euo pipefail
label=org.solutionforest.epar.runner
plist=/Library/LaunchDaemons/org.solutionforest.epar.runner.plist
if launchctl print "system/$label" >/dev/null 2>&1; then
  echo 'EPAR runner service already exists; refusing a second listener' >&2
  exit 1
fi
cat > "$plist" <<'PLIST'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Label</key><string>org.solutionforest.epar.runner</string>
<key>ProgramArguments</key><array><string>/bin/bash</string><string>/opt/epar/start-runner-with-env.sh</string></array>
<key>UserName</key><string>eparrunner</string>
<key>GroupName</key><string>staff</string>
<key>WorkingDirectory</key><string>/opt/actions-runner</string>
<key>RunAtLoad</key><true/>
<key>KeepAlive</key><false/>
<key>StandardOutPath</key><string>/var/log/actions-runner/run.log</string>
<key>StandardErrorPath</key><string>/var/log/actions-runner/run.log</string>
</dict></plist>
PLIST
chown root:wheel "$plist"
chmod 0644 "$plist"
plutil -lint "$plist"
launchctl bootstrap system "$plist"
for ((attempt=0; attempt<30; attempt++)); do
  pid="$(launchctl print "system/$label" | awk '/^[[:space:]]*pid = / {print $3; exit}')"
  if [[ "$pid" =~ ^[1-9][0-9]*$ ]] && kill -0 "$pid"; then
    printf '%s\n' "$pid" > /var/run/actions-runner.pid
    LC_ALL=C ps -p "$pid" -o lstart= > /var/run/actions-runner.pid.start
    bash /opt/epar/check-runner.sh
    exit 0
  fi
  sleep 1
done
tail -n 80 /var/log/actions-runner/run.log >&2 || true
echo 'macOS runner service failed to start' >&2
exit 1
