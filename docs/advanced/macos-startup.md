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
- delegates to `./start`, which runs EPAR with `go run ./cmd/ephemeral-action-runner` if Go is installed and working, or a containerized `go run` if not (see [No Go Install](#no-go-install));
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
export EPAR_USE_DOCKER_RUN="auto"
export EPAR_MIRROR_CONTAINER="epar-dockerhub-cache"
export EPAR_WAIT_FOR_DOCKER=1
export EPAR_DOCKER_WAIT_ATTEMPTS=120
```

Set `EPAR_WAIT_FOR_DOCKER=0` only when the selected provider does not need Docker at startup.

If you use the optional Docker registry mirror, create the mirror container separately. The startup script starts the container if it already exists, but it does not create or configure the mirror service.

## No Go Install

`start-epar.command` delegates to `./start` at the repo root. If `go` isn't on `PATH`, or the `go` found there doesn't actually run (stale/wrong-architecture installs happen — see below), `./start` runs EPAR straight from source with a containerized Go toolchain (`scripts/run-with-docker.sh`) instead of failing. No binary is built or left on disk either way. Docker is required in both cases.

To force this path even when Go is installed (for example, to avoid rebuilding via `go run` on every start), set in your local copy:

```bash
export EPAR_USE_DOCKER_RUN=1
```

Set `EPAR_USE_DOCKER_RUN=0` to force `go run` locally and error out instead of falling back. See [Running EPAR Without Installing Go](no-go-install.md) for details.

`./start` checks that `go version` actually runs, not just that a `go` binary exists on `PATH` — this catches a real failure mode: a stale Go install left over on `PATH` (e.g. an old Intel-only `/usr/local/go` from before an Apple Silicon migration) that segfaults instead of running. If you hit that, either remove the stale install or set `EPAR_GO_BIN`/`EPAR_USE_DOCKER_RUN` to bypass it.

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
    <string>/path/to/ephemeral-action-runner/work/state/launchd.out.log</string>

    <key>StandardErrorPath</key>
    <string>/path/to/ephemeral-action-runner/work/state/launchd.err.log</string>
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
