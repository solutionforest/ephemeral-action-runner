# Running EPAR Without Installing Go

The standard path is to download GitHub's automatic **Source code (zip)** or **Source code (tar.gz)** from the [EPAR Releases page](https://github.com/solutionforest/ephemeral-action-runner/releases) and run `go run ./cmd/ephemeral-action-runner` from the extracted source folder. If you do not want Go installed on the host, use EPAR's Docker-based source runner instead. The default Docker-DinD provider still needs Docker.

## Run With Docker

Run `./start` at the source folder root on macOS, Linux, WSL, or Git Bash, or `./start.ps1` / `start.cmd` in native Windows PowerShell or cmd. The wrapper uses local Go when it is installed and runnable; otherwise it runs EPAR from the checked-out source with a containerized Go toolchain:

```bash
./start --config .local/config.yml --instances 2
```

```powershell
.\start.ps1 --config .local\config.yml --instances 2
```

Under the hood, the wrapper calls `scripts/run-with-docker.sh` (or `scripts/run-with-docker.ps1` on Windows), builds a small local image with the Go toolchain and Docker CLI from `scripts/docker/dev.Dockerfile`, and runs `go run ./cmd/ephemeral-action-runner ...`. This stays source-based; it does not download or use a separately packaged EPAR executable. The Docker CLI in the toolchain image lets EPAR's own Docker operations reach the host daemon through the mounted socket.

### Host trust bridge

When the selected config enables `image.hostTrustMode: overlay`, the controller must inherit trust from the real Windows, macOS, or Linux host, not from the temporary Linux Go-toolchain container. The official wrappers handle this boundary automatically:

1. A native host helper reads the configured host trust scopes.
2. It rejects an empty collection and publishes a fresh, content-addressed snapshot in the host user's cache.
3. A watcher refreshes the snapshot every 10 seconds.
4. The wrapper bind-mounts only that feed read-only at `/run/epar-host-trust` and identifies the real controller host OS.
5. The containerized controller validates certificate hashes, scopes, host OS, and the 30-second feed expiry before using it.

On a first `start` with no config, the wrapper performs this as two phases: it runs interactive initialization with the real host OS identity, validates and publishes the selected host roots, then starts the controller again with the new feed mounted. A failed collection leaves the generated config disabled and does not start a controller with the toolchain container's trust store.

On Windows the helper reads local-machine and current-user root stores and excludes Windows-disallowed certificates. On macOS it evaluates the system, administrator, and selected user's native trust settings for TLS server use, with explicit deny taking precedence. On Linux it reads the distribution-generated system CA bundle; set `EPAR_HOST_TRUST_BUNDLE` to a readable generated PEM bundle when the host uses an unsupported layout.

The watcher and controller are tied to the wrapper lifecycle. If the host helper stops, its feed expires and EPAR stops authorizing stale idle runners. The feed is never mounted into the disposable runner containers.

Do not replace the official wrapper with a bare `docker run` when overlay mode is enabled. A containerized controller without `EPAR_CONTROLLER_HOST_OS`, a fresh `EPAR_HOST_TRUST_FEED`, and the corresponding read-only mount fails closed; it does not fall back to the toolchain container's CA store.

You can run the Docker wrapper directly instead of through `./start`:

```bash
scripts/run-with-docker.sh version
scripts/run-with-docker.sh start --config .local/config.yml
```

Set `EPAR_USE_DOCKER_RUN=1` to force `./start` down this path even when Go is installed, or `=0` to force local `go run` and error instead of falling back. A Docker volume caches Go modules and build output across runs, so repeat starts are fast.

The Docker wrapper passes the real host name into the toolchain container as `EPAR_HOST_NAME` so first-run defaults and generated host labels describe the machine running EPAR, not the temporary Go container. Set `EPAR_HOST_NAME` before launching EPAR to override that identity.

The wrapper also defaults `DOCKER_CLI_HINTS=false` for its Docker calls. This suppresses Docker Desktop hint text that can otherwise appear after a normal Ctrl-C shutdown. Set `DOCKER_CLI_HINTS=true` before launching EPAR if you want Docker CLI hints during wrapper runs.

### Linux file ownership

The container runs as root because the Go toolchain image requires it to write module and build-cache directories. On Docker Desktop for macOS, bind-mounted files remain owned by your normal user. On native Linux hosts, `scripts/run-with-docker.sh` returns ownership of `.local/` and `work/` to the invoking user after each run; do not rely on the same behavior for other repository directories.

### Windows: WSL versus native PowerShell

- From WSL2 or Git Bash, use `./start`. It behaves like the Linux case and needs Docker Desktop's WSL2 integration when run from WSL2.
- From native PowerShell or cmd, use `./start.ps1` or `start.cmd`, which use `scripts/run-with-docker.ps1` instead of the Bash script.

`start.ps1` and `scripts/run-with-docker.ps1` are less exercised than the Bash/macOS path. If you hit an issue, check whether Docker Desktop file sharing is enabled for the drive that holds the source folder.

## macOS Login Item Startup

The `.command` login item described in [macOS Startup](macos-startup.md) delegates to `./start`, so it gets the same automatic fallback. See that document's [No Go Install](macos-startup.md#no-go-install) section for the `EPAR_USE_DOCKER_RUN` override.
