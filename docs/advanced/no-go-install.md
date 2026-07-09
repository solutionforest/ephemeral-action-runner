# Running EPAR Without Installing Go

The normal beta path (`go run ./cmd/ephemeral-action-runner`, see [Usage](../usage.md)) needs Go 1.22+ on the host. If you don't want to install Go, use one of the two paths below instead. Both still need Docker for the default Docker-DinD provider.

| Path | What you get | Best for |
| --- | --- | --- |
| A: Download a release archive | A prebuilt binary from GitHub Releases | Quick trial, no Docker-in-the-loop |
| B: Run with Docker (recommended) | Nothing persisted — source runs fresh each time via `go run` in a container | Default choice: matches the project's own source-first path, no unsigned binary ever exists on disk |

## Path B: Run With Docker

Run `./start` at the repo root (macOS, Linux, WSL, Git Bash) or `.\start.ps1` / `start.cmd` on native Windows PowerShell/cmd. Either uses local Go if installed and actually runnable; otherwise it runs EPAR straight from source with a containerized Go toolchain:

```bash
./start --config .local/config.yml --instances 2
```

```powershell
.\start.ps1 --config .local\config.yml --instances 2
```

Under the hood this calls `scripts/run-with-docker.sh` (or `scripts/run-with-docker.ps1` on Windows), which builds a small local image (Go toolchain + Docker CLI, from `scripts/docker/dev.Dockerfile`) and runs:

```
docker run --rm -it \
  -v <repo>:/app -w /app \
  -v epar-gomod:/go/pkg/mod -v epar-gocache:/root/.cache/go-build \
  -v /var/run/docker.sock:/var/run/docker.sock \
  epar-dev-toolchain \
  go run ./cmd/ephemeral-action-runner ...
```

This is `go run` — the same thing the docs recommend when Go is installed locally — just executed inside a container. **No binary is built or written to disk.** The Docker CLI baked into that small image is what lets EPAR's own runtime Docker calls (image build, container create, etc.) reach your host's Docker daemon through the mounted socket; without it, `docker run golang:1.24 go run ...` fails with "Docker is required" even with the socket mounted, because the bare `golang` image has no `docker` client binary.

You can run the script directly instead of through `./start`:

```bash
scripts/run-with-docker.sh version
scripts/run-with-docker.sh start --config .local/config.yml
```

Set `EPAR_USE_DOCKER_RUN=1` to force `./start` down this path even when Go is installed, or `=0` to force `go run` locally and error out instead of falling back. A Docker volume caches Go modules and build output across runs, so repeat starts are fast.

The Docker wrapper passes the real host name into the toolchain container as `EPAR_HOST_NAME` so first-run defaults and generated host labels describe the machine running EPAR, not the temporary Go container. Set `EPAR_HOST_NAME` yourself before launching EPAR if you want to override that identity.

### Linux: File Ownership

The container runs as root (the Go toolchain image only lets root write to its module/build cache directories). On Docker Desktop for macOS, bind-mounted files still come out owned by your normal user. On native Linux hosts, that isn't true — root-owned files would otherwise land in `.local/` and `work/`. `scripts/run-with-docker.sh` handles this by `chown`-ing those two directories back to the invoking user after each run; no action needed, but don't rely on other directories under the repo getting the same treatment.

### Windows: WSL Vs. Native PowerShell

Two separate entry points, pick based on how you're working:

- From WSL2, Git Bash: use `./start`. It behaves like the Linux case above (needs Docker Desktop's WSL2 integration enabled for that distro if running from WSL2).
- From native PowerShell or cmd: use `.\start.ps1` or `start.cmd`, which use `scripts/run-with-docker.ps1` instead of the bash script.

`start.ps1` and `scripts/run-with-docker.ps1` are less exercised than the bash/macOS path — if you hit an issue, check whether Docker Desktop's file sharing is enabled for the drive the repo lives on.

## Path A: Download A Release Archive

1. Go to the [EPAR Releases page](https://github.com/solutionforest/ephemeral-action-runner/releases) and download the archive for your OS/architecture (e.g. `ephemeral-action-runner_<version>_macos_arm64.tar.gz`).
2. Extract it and open a terminal in the extracted folder.
3. Run the bundled wrapper script, which handles logging and error reporting:
   - macOS/Linux: `./run-epar`
   - Windows: `run-epar.cmd` (double-click, or run from PowerShell/cmd)

These archives are unsigned and downloaded, so macOS/Windows/antivirus are more likely to flag them than Path B's containerized `go run` (which never writes or downloads an executable). Release notes on GitHub call this out explicitly. If the OS blocks the binary, see [Unblocking The Binary](#unblocking-the-binary) below.

## Unblocking The Binary

Only relevant to Path A. If your OS or antivirus blocks the downloaded binary:

- **macOS Gatekeeper**: right-click the binary in Finder and choose Open, then confirm in the dialog. Or clear the quarantine flag directly: `xattr -d com.apple.quarantine ./ephemeral-action-runner`.
- **Windows SmartScreen**: click "More info" then "Run anyway" in the SmartScreen dialog, or right-click the `.exe` → Properties → check "Unblock" → OK.
- **Windows Defender / Norton / other AV**: add an exclusion for the EPAR folder or binary path in your AV product's settings. Only do this for a binary downloaded from the official Releases page above.

## macOS Login Item Startup

The `.command` login item described in [macOS Startup](macos-startup.md) delegates to `./start`, so it gets the same automatic fallback. See that doc's [No Go Install](macos-startup.md#no-go-install) section for the `EPAR_USE_DOCKER_RUN` override.
