# macOS Startup

EPAR's `start` command is a foreground supervisor. On a personal Mac, you can start it after login with either a `.command` file in macOS Open at Login or a user `launchd` LaunchAgent.

The `.command` approach is the simplest option. It opens a Terminal window, and closing that window stops EPAR. Use `launchd` only when you want a quieter background service.

## Open At Login With A .command File

From the source folder, copy the example startup script into ignored local state:

```bash
cd /path/to/ephemeral-action-runner
mkdir -p .local
cp examples/macos/start-epar.command .local/start-epar.command
chmod +x .local/start-epar.command
```

Double-click `.local/start-epar.command` in Finder or run it from Terminal:

```bash
.local/start-epar.command
```

The script:

- finds the EPAR source folder, such as when the script lives at `.local/start-epar.command`;
- runs EPAR with `go run ./cmd/ephemeral-action-runner`;
- uses `.local/config.yml` by default;
- waits for Docker to become ready before starting EPAR;
- starts an existing `epar-dockerhub-cache` mirror container if one exists;
- runs the `start` flow.

If `.local/config.yml` does not exist and the script is running in a Terminal window, EPAR's normal first-run setup can create it. For `launchd`, create the config first by running EPAR manually once.

To start it automatically after login, open macOS System Settings, go to **General**, then **Login Items & Extensions**, then add `.local/start-epar.command` under **Open at Login**.

This starts only after the user logs in. It is not a boot-time system daemon.

## Customization

You can edit the copied `.local/start-epar.command` file or set environment variables near the top of your local copy:

```bash
export EPAR_ROOT="/path/to/ephemeral-action-runner"
export EPAR_CONFIG="${EPAR_ROOT}/.local/config.yml"
export EPAR_GO_BIN="/usr/local/go/bin/go"
export EPAR_MIRROR_CONTAINER="epar-dockerhub-cache"
export EPAR_WAIT_FOR_DOCKER=1
export EPAR_DOCKER_WAIT_ATTEMPTS=120
```

Set `EPAR_WAIT_FOR_DOCKER=0` only when the selected provider does not need Docker at startup.

If you use the optional Docker registry mirror, create the mirror container separately. The startup script starts the container if it already exists, but it does not create or configure the mirror service.

## launchd Alternative

For a background service without a Terminal window, create `.local/config.yml` first, then create a user LaunchAgent that runs the same `.local/start-epar.command` script.

Example `~/Library/LaunchAgents/com.example.epar.plist`:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
  "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
  <dict>
    <key>Label</key>
    <string>com.example.epar</string>

    <key>ProgramArguments</key>
    <array>
      <string>/path/to/ephemeral-action-runner/.local/start-epar.command</string>
    </array>

    <key>RunAtLoad</key>
    <true/>

    <key>KeepAlive</key>
    <true/>

    <key>WorkingDirectory</key>
    <string>/path/to/ephemeral-action-runner</string>

    <key>StandardOutPath</key>
    <string>/path/to/ephemeral-action-runner/work/logs/launchd.out.log</string>

    <key>StandardErrorPath</key>
    <string>/path/to/ephemeral-action-runner/work/logs/launchd.err.log</string>
  </dict>
</plist>
```

Load it:

```bash
launchctl bootstrap "gui/$(id -u)" ~/Library/LaunchAgents/com.example.epar.plist
launchctl enable "gui/$(id -u)/com.example.epar"
launchctl kickstart -k "gui/$(id -u)/com.example.epar"
```

Stop and remove it:

```bash
launchctl bootout "gui/$(id -u)" ~/Library/LaunchAgents/com.example.epar.plist
```

## Operational Notes

- `start` cleans up prefixed instances when it exits. Use `--keep-on-exit` only for debugging.
- The first run can take a while because `start` may build or refresh the configured image before starting runners.
- If Docker Desktop, OrbStack, or Docker Engine cannot start, the script exits before EPAR starts.
- For Docker-DinD, the host Docker runtime must support privileged containers.
- For Tart-only pools that do not use host Docker or a local registry mirror, disable the Docker wait in your local copy.
