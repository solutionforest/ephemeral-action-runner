# Running EPAR Without Installing Go

The standard path is to download GitHub's automatic **Source code (zip)** or **Source code (tar.gz)** from the [EPAR Releases page](https://github.com/solutionforest/ephemeral-action-runner/releases) and run `./start` from the extracted source folder. The wrapper uses local Go when it works; if you do not want Go installed on the host, it uses EPAR's Docker-based native-controller builder instead. Docker remains required for that build toolchain.

## Run With Docker

Run `./start` at the source folder root on macOS, Linux, WSL, or Git Bash, or `./start.ps1` / `start.cmd` in native Windows PowerShell or cmd. The wrapper uses local Go when it is installed and runnable; otherwise a containerized Go toolchain cross-compiles a CGO-disabled host-native binary at `.local/bin/ephemeral-action-runner` (or `.exe` on Windows). Its adjacent `ephemeral-action-runner.manifest` records the deterministic source, platform, and toolchain fingerprint:

```bash
./start --config .local/config.yml --instances 2
```

```powershell
.\start.ps1 --config .local\config.yml --instances 2
```

Under the hood, the wrapper calls `scripts/run-with-docker.sh` or `scripts/run-with-docker.ps1`, builds the small toolchain image from `scripts/docker/dev.Dockerfile`, compiles EPAR with `CGO_ENABLED=0`, and runs the cached host-native binary. This stays source-based and does not download a separately packaged EPAR executable. Native execution is mandatory for Docker Sandboxes because its management endpoint and host security state are not exposed to the build container.

### Host trust

Before compiling the native controller, the wrapper publishes a short-lived system-trust feed from the real Windows, macOS, or Linux host, validates its freshness, certificate hashes, CA constraints, and distrust entries in an offline container, and mounts the resulting bundle read-only into the Go toolchain container. This operational trust is automatic even when `image.hostTrustMode` is disabled and is not copied into runners. After a native controller is available, it reads the host trust stores directly; only the legacy containerized-controller path uses the separate short-lived `EPAR_BUILD_TRUST_FEED` and optional `EPAR_HOST_TRUST_FEED` bridge. If bootstrap TLS still fails, the wrapper preserves `work/logs/epar-native-controller-build.log` and prints a certificate diagnostic without disabling verification or retrying insecurely.

The explicit legacy path `EPAR_LEGACY_CONTROLLER_IN_DOCKER=1` remains available only for compatible providers. That path uses the existing short-lived host-trust bridge and rejects `provider.type: docker-sandboxes`; it is not an automatic fallback.

On a first `start` with no config, the native controller performs interactive initialization with the real host identity. A failed host-root collection stops initialization rather than using roots from the toolchain container.

On Windows the helper reads local-machine and current-user root stores and excludes Windows-disallowed certificates. On macOS it evaluates the system, administrator, and selected user's native trust settings for TLS server use, with explicit deny taking precedence. On Linux it reads the distribution-generated system CA bundle; set `EPAR_HOST_TRUST_BUNDLE` to a readable generated PEM bundle when the host uses an unsupported layout.

Do not replace the official wrapper with a bare `docker run` for Docker Sandboxes. EPAR rejects the legacy controller-in-Docker path for that provider.

You can run the Docker wrapper directly instead of through `./start`:

```bash
scripts/run-with-docker.sh version
scripts/run-with-docker.sh start --config .local/config.yml
```

Set `EPAR_USE_DOCKER_RUN=1` to force `./start` to use the containerized compiler even when Go is installed, or `=0` to force local `go run` and error instead of falling back. Docker volumes cache Go modules and build output, while unchanged source reuses the one native binary. A changed source or toolchain rebuilds it atomically. If an existing EPAR process is still using the prior binary, stop that process before retrying; the wrapper does not retain another historical binary as a fallback.

Before containerized compilation, the wrapper requires 1 GiB free on the source-folder filesystem so bootstrap does not begin when the host is already critically constrained. `EPAR_BOOTSTRAP_MIN_FREE_BYTES` may raise this reserve for managed installations.

The wrapper passes the real host name to the native controller as `EPAR_HOST_NAME` so first-run defaults and generated host labels describe the machine running EPAR. Set `EPAR_HOST_NAME` before launching EPAR to override that identity.

The wrapper also defaults `DOCKER_CLI_HINTS=false` for its Docker calls. This suppresses Docker Desktop hint text that can otherwise appear after a normal Ctrl-C shutdown. Set `DOCKER_CLI_HINTS=true` before launching EPAR if you want Docker CLI hints during wrapper runs.

### Linux file ownership

The compiler container mounts the source read-only and writes only the temporary native binary output plus named Go caches. The controller itself runs as the invoking host user, so its `.local/` and `work/` state is not created by a root controller process.

### Windows: WSL versus native PowerShell

- From WSL2 or Git Bash, use `./start`. It behaves like the Linux case and needs Docker Desktop's WSL2 integration when run from WSL2.
- From native PowerShell or cmd, use `./start.ps1` or `start.cmd`, which use `scripts/run-with-docker.ps1` instead of the Bash script.

`start.ps1` and `scripts/run-with-docker.ps1` are less exercised than the Bash/macOS path. If you hit an issue, check whether Docker Desktop file sharing is enabled for the drive that holds the source folder.

## macOS Login Item Startup

The `.command` login item described in [macOS Startup](macos-startup.md) delegates to `./start`, so it gets the same automatic fallback. See that document's [No Go Install](macos-startup.md#no-go-install) section for the `EPAR_USE_DOCKER_RUN` override.
