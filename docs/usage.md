# Usage

This page is the operational walkthrough. Start with the supported host you already have:

- Docker-DinD on a Docker-capable host
- WSL2 on Windows
- Tart on Apple Silicon macOS

## Prerequisites

Install the host tools you need:

| Required for | Tool |
| --- | --- |
| Release archive install | No Go toolchain required |
| Source build | Go 1.22 or newer, Git |
| Updating the pinned `actions/runner-images` checkout | Git |
| macOS provider | Tart |
| Windows provider | WSL2 |
| Windows WSL2 default image build | Docker Desktop, Docker Engine, or another working Docker daemon for the one-time Docker image export |
| Docker-DinD provider | Docker Engine, OrbStack, or Docker Desktop with privileged container support |
| Optional Docker registry mirrors | A running mirror service on the host, LAN, intranet, or cloud registry cache |
| Runner registration | GitHub App with organization self-hosted runner read/write permission |

Packer, GitHub CLI, and sshpass are not required.

Set up the GitHub App before registering runners. The image build command can run without GitHub credentials, but `pool verify --register-only`, `pool up`, `status`, and GitHub cleanup need the app settings. See [GitHub App Setup](github-app.md).

## Install The CLI

For normal use, download the archive for your host from the GitHub Releases page and extract it:

| Host | Release asset |
| --- | --- |
| Windows x64 | `ephemeral-action-runner_<version>_windows_amd64.zip` |
| Linux x64 | `ephemeral-action-runner_<version>_linux_amd64.tar.gz` |
| Linux ARM64 | `ephemeral-action-runner_<version>_linux_arm64.tar.gz` |
| macOS Apple Silicon | `ephemeral-action-runner_<version>_macos_arm64.tar.gz` |
| macOS Intel | `ephemeral-action-runner_<version>_macos_amd64.tar.gz` |

Each release bundle includes the binary, a `run-epar` helper wrapper, `configs/`, `scripts/`, `docs/`, `examples/`, `README.md`, `LICENSE`, and `third_party/runner-images.lock`. Generated runner images, WSL tar files, local configs, and private keys are not included.

Verify the downloaded archive against `checksums.txt` from the same release, then confirm the binary:

```bash
./ephemeral-action-runner version
```

On Windows PowerShell:

```powershell
.\ephemeral-action-runner.exe version
```

If you are developing EPAR from source, build the CLI instead:

```bash
go build -o ./bin/ephemeral-action-runner ./cmd/ephemeral-action-runner
```

The examples below use `./bin/ephemeral-action-runner` for source checkouts. In an extracted release bundle, use `./run-epar` on macOS/Linux or `.\run-epar.cmd` on Windows for normal interactive use. Use `./ephemeral-action-runner` or `.\ephemeral-action-runner.exe` directly for automation.

## One-Command Start

For the default Docker-DinD setup, run EPAR from the extracted release directory or source checkout:

```bash
./run-epar
```

On Windows PowerShell:

```powershell
.\run-epar.cmd
```

If no config exists, EPAR starts the initializer, asks for the GitHub App ID, organization, and private key path, then writes `.local/config.yml`. It then checks the configured image, builds or replaces it when the image is missing or no longer matches the config, and starts the configured number of runners. The default config uses `pool.instances: 1`.

The wrapper passes every argument to the real executable and writes `work/logs/epar-last-run.log`. On failure it prints the log path and opens the log for desktop users when possible. Set `EPAR_NO_OPEN_LOG=1` to disable opening the log.

Use `start` when you want to choose a config or runner count:

```bash
./run-epar start --config .local/config.yml --instances 2
```

On Windows PowerShell:

```powershell
.\run-epar.cmd start --config .local\wsl.yml --instances 2
```

Stop the foreground process with Ctrl-C. Cleanup is enabled by default.

If `--instances` is omitted, `start`, `pool up`, and `pool verify` use `pool.instances` from the config. Passing `--instances N` overrides the config for that run.

## Configure Only

Use `init` when you only want to create the default Docker-DinD config without building an image or starting runners:

```bash
./ephemeral-action-runner init
```

On Windows PowerShell:

```powershell
.\ephemeral-action-runner.exe init
```

For WSL, Tart, or custom labels, copy one example config into `.local/config.yml`, then edit the GitHub App fields and any labels you want to expose to workflows.

| Host and image | Example config |
| --- | --- |
| macOS Tart, runner-only | `configs/tart.example.yml` |
| macOS Tart, web/E2E with Rosetta amd64 Docker support | `configs/tart.web-e2e.example.yml` |
| Windows WSL2, default full Gitea runner image | `configs/wsl.example.yml` |
| Windows WSL2, lean runner-only tar | `configs/wsl.lean.example.yml` |
| Windows WSL2, lean web/E2E tar | `configs/wsl.web-e2e.example.yml` |
| Docker-DinD, default full Gitea runner image | `configs/docker-dind.example.yml` |
| Docker-DinD, smaller web/E2E custom image | `configs/docker-dind.web-e2e.example.yml` |

macOS:

```bash
mkdir -p .local
cp configs/tart.example.yml .local/config.yml
```

Windows:

```powershell
New-Item -ItemType Directory -Force .local
Copy-Item configs/wsl.example.yml .local/config.yml
```

Default Docker-DinD manually:

```bash
mkdir -p .local
cp configs/docker-dind.example.yml .local/config.yml
```

EPAR looks for config in this order:

1. `--config <path>`
2. `EPAR_CONFIG`
3. `./.local/config.yml`
4. `~/.config/ephemeral-action-runner/config.yml`

Tracked configs are examples only. Keep real app IDs and private key paths in an ignored config file.

## Optional Docker Registry Mirrors

If repeated jobs spend time pulling the same Docker Hub images into fresh runner Docker daemons, configure mirrors in your ignored local config:

```yaml
docker:
  registryMirrors:
    - http://host.docker.internal:5050
```

This is optional. Without it, EPAR behaves normally and pulls directly from registries. Mirror benefits vary by workflow and mainly affect Docker image pull time; they do not make application startup, volume sync, health checks, browser tests, or CPU-bound work faster.

EPAR only configures runner-side Docker daemons; it does not run or secure the mirror service. Docker Engine, Docker Desktop, or OrbStack can run a local `registry:2` pull-through cache on the EPAR host, or you can use a mirror reachable on the LAN/intranet. For private images, keep using `docker login` inside the workflow unless your mirror is deliberately configured and secured with upstream credentials. See [Docker Registry Mirrors](advanced/docker-registry-mirrors.md).

## Prepare A WSL Source

Skip this section for Tart and Docker-DinD.

The default WSL config starts from `gitea/runner-images:ubuntu-latest-full`. During `image build`, EPAR runs Docker on the Windows host to pull that image, create a temporary container, export its filesystem into a rootfs tar, and then import that tar into WSL for EPAR's normal runner bootstrap. Docker is needed for this preparation step. Running WSL runner instances afterward does not require Docker Desktop unless your jobs need it.

If you use `configs/wsl.lean.example.yml`, `configs/wsl.web-e2e.example.yml`, or another `image.sourceType: rootfs-tar` config, create the clean Ubuntu 24.04 source tar once:

```powershell
New-Item -ItemType Directory -Force work/images
wsl --install -d Ubuntu-24.04 --no-launch
wsl --export Ubuntu-24.04 work/images/ubuntu-24.04-clean.rootfs.tar
```

After that, EPAR imports disposable temporary distros for image builds and pool instances.

## Build The Runner Image Manually

The `start` command builds or replaces the configured image automatically. Use this section when developing from source, debugging image builds, or intentionally separating image preparation from runner startup.

Default WSL and Docker-DinD builds and runner-only Tart builds do not need the upstream `actions/runner-images` checkout:

```bash
./bin/ephemeral-action-runner image build --replace
```

If `image.customInstallScripts` includes EPAR's Docker/browser or web/E2E scripts, update the pinned upstream checkout first:

```bash
./bin/ephemeral-action-runner image update-upstream
./bin/ephemeral-action-runner image build --replace
```

The Tart web/E2E example sets `provider.rosettaTag: rosetta`. Tart builds with that option start with `tart run --rosetta rosetta`, install Rosetta guest support, and validate that Docker can run a `linux/amd64` Alpine container returning `x86_64`.

Tart output is a local Tart image name, such as `epar-ubuntu-24-arm64`. Confirm it with:

```bash
tart list
```

The default WSL output is a rootfs tar path:

```text
work/images/epar-wsl-gitea-ubuntu.tar
```

When the WSL source is a Docker image, EPAR also writes an intermediate source rootfs tar and env cache next to the output image, for example `work/images/epar-wsl-gitea-ubuntu.source.rootfs.tar` and `.env`. Later builds reuse that source cache; delete those files when you intentionally want to reconvert the Docker image.

EPAR also writes image manifests so `start` can tell whether the local image still matches the config. Docker-DinD stores the manifest hash as a Docker image label and stores the manifest at `/opt/epar/image-manifest.json`. WSL stores `/opt/epar/image-manifest.json` inside the exported image and writes a sidecar next to the tar.

Docker-DinD output is a Docker image tag, such as `epar-docker-dind-gitea-ubuntu`. Confirm it with:

```bash
docker image ls epar-docker-dind-gitea-ubuntu
```

Build logs are written under `work/logs`.

## Customize The Image

WSL and Docker-DinD use the full Gitea runner image by default. Tart and the WSL lean examples are runner-only. Use `image.customInstallScripts` when you want a different image shape, such as the smaller WSL or Docker-DinD web/E2E examples:

```yaml
image:
  customInstallScripts:
    - scripts/guest/ubuntu/install-web-e2e.sh
    - examples/custom-install/install-extra-apt-tools.sh
```

Scripts run as root during image build, after the GitHub Actions runner is installed and before validation/finalization. See [Image Build](image-build.md) for the full layering model and custom script guidance.

## Verify Runners

For a local runtime check without GitHub registration:

```bash
./bin/ephemeral-action-runner pool verify --instances 1 --cleanup
```

For a full registration check:

```bash
./bin/ephemeral-action-runner pool verify --instances 2 --register-only --cleanup
```

Healthy output should show each generated instance name moving through:

1. clone
2. start
3. runtime validation
4. GitHub online/idle, when registration is enabled
5. cleanup

Runtime validation always checks the base runner files and runner user. Images with optional feature markers also validate those features:

- Docker/browser images validate Docker, Compose v2, Buildx, `hello-world`, and a headless browser.
- Default WSL full images validate Docker, Compose v2, Buildx, and `hello-world`.
- Docker-DinD images validate the private inner Docker daemon inside each runner container.
- Tart Rosetta images validate `docker run --platform linux/amd64 alpine:3.20` and expect `uname -m` to return `x86_64`.
- Web/E2E images also validate `node`, `npm`, `zip`, `unzip`, `tar`, `rsync`, and `mysql`.

When `docker.registryMirrors` is configured, EPAR applies the mirror configuration before runtime validation.

If a Docker-DinD workflow depends on amd64-only images while the host is ARM64, validate host emulation inside a running EPAR instance:

```bash
docker exec <epar-instance> docker run --rm --platform linux/amd64 alpine:3.20 uname -m
```

The expected output is `x86_64`.

## Run A Foreground Pool Manually

```bash
./bin/ephemeral-action-runner pool up --instances 2
```

`start` is the recommended public command because it also checks the image before starting runners. `pool up` is the lower-level supervisor command for users who already prepared the image.

`pool up` keeps the requested number of runners online. Each GitHub ephemeral runner exits after one job. EPAR then retires that instance and creates a fresh replacement.

Stop the supervisor with Ctrl-C. By default, EPAR cleans up active instances and matching GitHub runner records before it exits.

For startup after login, see [Windows Startup](advanced/windows-startup.md) or [macOS Startup](advanced/macos-startup.md).

Use these flags only for debugging:

- `--keep-on-exit`: leave instances running when the supervisor exits.
- `--replace-completed=false`: do not create replacements after completed jobs.

## Status And Cleanup

```bash
./bin/ephemeral-action-runner status
./bin/ephemeral-action-runner cleanup
```

Cleanup only touches local instances and GitHub runners whose names match `pool.namePrefix`.

## Runner Labels

By default, EPAR appends an `epar-host-<machine>` label to the configured labels. The machine name is lowercased, unsafe characters are replaced with `-`, and the final label is kept within GitHub's 256-character label limit. Set `runner.includeHostLabel: false` to disable it.

Use provider-specific labels in workflows. For the Tart web/E2E Rosetta image, target the existing web/E2E label plus the Rosetta label when the job needs amd64 Docker images:

```yaml
runs-on: [self-hosted, linux, ARM64, epar-tart-ubuntu-24.04-web-e2e, epar-tart-rosetta-amd64]
```

For the default WSL image, target the default WSL label:

```yaml
runs-on: [self-hosted, linux, X64, epar-wsl-gitea-ubuntu]
```

For the default Docker-DinD image, target the default Docker-DinD label:

```yaml
runs-on: [self-hosted, linux, epar-docker-dind-gitea-ubuntu]
```

For Docker-DinD web/E2E images, target the custom web/E2E label:

```yaml
runs-on: [self-hosted, linux, epar-docker-dind-gitea-ubuntu-web-e2e]
```

When that Docker-DinD runner is used for amd64-only runtime images, keep the workflow's Docker platform explicit, for example `DOCKER_PLATFORM=linux/amd64` or the equivalent variable used by your compose scripts, and verify the host runtime supports amd64 emulation as described above.

Do not use `ubuntu-latest` for these self-hosted runners.

## Dry Run

Use `--dry-run` to inspect provider command construction without mutating local instances:

```bash
./bin/ephemeral-action-runner pool verify --dry-run --instances 2
```

## Release Publishing

Maintainers publish release bundles by pushing a version tag. Beta releases can be cut from `develop`; GitHub Releases are tag-based and are not tied to the repository default branch.

```bash
git push origin develop
git tag -a v0.1.0-beta.1 -m "v0.1.0-beta.1"
git push origin v0.1.0-beta.1
```

The release workflow builds five archives, uploads `checksums.txt`, and marks tags with a prerelease suffix such as `-beta.1` as GitHub prereleases. It does not upload generated Docker images, WSL rootfs tars, local configs, or private keys.

To test release packaging locally, run the same script used by the workflow:

```bash
VERSION=v0.0.0-local \
COMMIT="$(git rev-parse HEAD)" \
BUILD_DATE="$(date -u +'%Y-%m-%dT%H:%M:%SZ')" \
bash scripts/build-release-archives.sh
```

The default output is `dist/`, which is ignored by Git.

On Windows, run that command from Git Bash when using the Windows Go toolchain. WSL Bash can accidentally call `go.exe` through WSL interop, which does not handle the `GOOS` and `GOARCH` assignments correctly.
