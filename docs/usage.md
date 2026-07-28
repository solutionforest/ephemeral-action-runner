# Usage

This page is the operational walkthrough. Start with the supported host you already have:

- Docker Container on a Docker-capable host
- WSL2 on Windows
- Tart on Apple Silicon macOS

## Prerequisites

Install the host tools you need:

| Required for | Tool |
| --- | --- |
| Source archive quick start | Go 1.25 or newer, or Docker (see [no-Go-install](advanced/no-go-install.md)) |
| Updating the pinned `actions/runner-images` checkout | Git |
| macOS provider | Tart |
| Windows provider | WSL2 |
| Windows WSL2 default image build | Docker Desktop, Docker Engine, or another working Docker daemon for the one-time Docker image export |
| Docker Container provider | Docker Engine, OrbStack, or Docker Desktop with privileged container support |
| Optional Docker registry mirrors | A running mirror service on the host, LAN, intranet, or cloud registry cache |
| Runner registration | GitHub App with organization self-hosted runner read/write permission |

Packer, GitHub CLI, and sshpass are not required.

Set up the GitHub App before registering runners. The image build command can run without GitHub credentials, but `pool verify --register-only`, `pool up`, `status`, and GitHub cleanup need the app settings. See [GitHub App Setup](github-app.md).

## Get The Source

For normal use, open the [EPAR Releases page](https://github.com/solutionforest/ephemeral-action-runner/releases), select the release you want, and download GitHub's automatically generated **Source code (zip)** or **Source code (tar.gz)**. Extract the source archive and open a terminal in the extracted folder:

```bash
cd path/to/ephemeral-action-runner-<tag>
go run ./cmd/ephemeral-action-runner version
```

The examples below use `go run ./cmd/ephemeral-action-runner` for the public source-first path.

Don't want to install Go at all? See [Running EPAR Without Installing Go](advanced/no-go-install.md) for running the source archive in a container.

## One-Command Start

Run EPAR from the source folder. On macOS, Linux, WSL, or Git Bash, use the `./start` wrapper; on native Windows PowerShell/cmd, use `.\start.ps1` or `start.cmd`. Either uses Go if installed, or uses a containerized Go toolchain to build and cache a CGO-disabled native controller under `.local/bin` automatically (see [Running EPAR Without Installing Go](advanced/no-go-install.md)):

```bash
./start
```

Equivalent without the wrapper:

```bash
go run ./cmd/ephemeral-action-runner
```

If no config exists, EPAR starts the initializer, asks for the GitHub App ID, organization, and private key path, then lists the organization's live runner groups. The group choice is explicit and has no Enter-for-default or offline fallback. Default and broad-access groups require warning confirmation; public-enabled groups are blocked by the generated safety policy. See [Runner Group Security](runner-groups.md). The provider menu always displays the same four choices and prints a prerequisite status beneath each one: option 1 Docker Container requires Docker; option 2 Docker Sandboxes requires Docker, a supported host architecture, the exact supported `sbx` version, and healthy `sbx diagnose --output json` results; option 3 WSL2 requires native Windows, Docker, and `wsl.exe --status` reporting default version 2; option 4 experimental Tart requires native macOS and a successful `tart --version`. Unavailable choices remain visible but cannot be selected. Docker Sandboxes is the Enter default on any OS when those capability checks pass: diagnostics must contain at least one pass and zero failures, while warnings such as an available update are accepted. Setup performs read-only checks for compatible `sbx` admission, an exact locally built and loaded EPAR template, the matching full local Docker image identity and platform, and the current host-global Balanced-policy fingerprint. If discovery fails, the wizard retains the Docker Sandboxes selection and offers to retry the checks without repeating the provider menu; declining exits without writing a config or falling back. The wizard asks only one capacity question, chosen from the available measurement evidence, and writes the other recommended values for later editing. New Docker Sandboxes configs default to sandbox-scoped open public HTTP/HTTPS egress with deny-wins host-alias guardrails so ordinary CI dependency endpoints do not require reactive allowlisting; this does not change the machine's global Docker Sandboxes policy. The wizard asks whether a new Docker Container or Docker Sandboxes config should inherit the controller host's trusted TLS roots and defaults to yes; existing configs remain disabled unless they explicitly set `image.hostTrustMode: overlay`. EPAR then checks provider admission and runner-group policy, prepares the configured environment, and starts the configured number of runners. The default config uses `pool.instances: 1`.

Pass flags through `./start` to choose a config or runner count:

```bash
./start --config .local/config.yml --instances 2
```

Equivalent without the wrapper:

```bash
go run ./cmd/ephemeral-action-runner start --config .local/config.yml --instances 2
```

On Windows PowerShell:

```powershell
.\start.ps1 --config .local\wsl.yml --instances 2
```

Equivalent without the wrapper:

```powershell
go run ./cmd/ephemeral-action-runner start --config .local\wsl.yml --instances 2
```

Stop the foreground process with Ctrl-C. Cleanup is enabled by default.

If `--instances` is omitted, `start`, `pool up`, and `pool verify` use `pool.instances` from the config. Passing `--instances N` overrides the config for that run.

## Configure Only

Use `init` when you only want to create a config without building an image or starting runners. It uses the same prerequisite-aware provider menu as missing-config `./start`; Docker Sandboxes is the recommended default on any OS when Docker and the supported `sbx` diagnostics are ready, and Docker Container, WSL2, and experimental Tart remain visible with their current availability status:

```bash
go run ./cmd/ephemeral-action-runner init
```

On Windows PowerShell:

```powershell
go run ./cmd/ephemeral-action-runner init
```

For other WSL or Tart variants, or for custom labels, copy one example config into `.local/config.yml`, then edit the GitHub App fields, `runner.group`, and any labels you want to expose to workflows. Example group names are placeholders.

| Host and image | Example config |
| --- | --- |
| Windows Docker Sandboxes, recommended full Catthehacker template | `configs/docker-sandboxes.example.yml` |
| macOS Tart, experimental basic Ubuntu ARM64 image | `configs/tart.example.yml` |
| macOS Tart, web/E2E with Rosetta amd64 Docker support | `configs/tart.web-e2e.example.yml` |
| Windows WSL2, default full Catthehacker runner image | `configs/wsl.example.yml` |
| Windows WSL2, lean runner-only tar | `configs/wsl.lean.example.yml` |
| Windows WSL2, lean web/E2E tar | `configs/wsl.web-e2e.example.yml` |
| Docker Container, default full Catthehacker runner image | `configs/docker-container.example.yml` |
| Docker Container, Docker-focused Catthehacker Act image | `configs/docker-container.act.example.yml` |
| Docker Container, smaller web/E2E custom image | `configs/docker-container.web-e2e.example.yml` |

Tart is experimental. Its default image is a basic Ubuntu ARM64 OS image with the EPAR runner lifecycle, not the dependency-rich environment described by [`actions/runner-images`](https://github.com/actions/runner-images). If your workflows depend on that environment, adapt the upstream build scripts to create and maintain your own bootable Tart image and point `image.sourceImage` at it; EPAR does not automatically create one.

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

Default Docker Container manually:

```bash
mkdir -p .local
cp configs/docker-container.example.yml .local/config.yml
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

Skip this section for Tart and Docker Container.

The default WSL config starts from `ghcr.io/catthehacker/ubuntu:full-latest`. During `image build`, EPAR runs Docker on the Windows host to pull that image, create a temporary container, export its filesystem into a rootfs tar, and then import that tar into WSL for EPAR's normal runner bootstrap. Docker is needed for this preparation step. Running WSL runner instances afterward does not require Docker Desktop unless your jobs need it.

If you use `configs/wsl.lean.example.yml`, `configs/wsl.web-e2e.example.yml`, or another `image.sourceType: rootfs-tar` config, create the clean Ubuntu 24.04 source tar once:

```powershell
New-Item -ItemType Directory -Force work/images
wsl --install -d Ubuntu-24.04 --no-launch
wsl --export Ubuntu-24.04 work/images/ubuntu-24.04-clean.rootfs.tar
```

After that, EPAR imports disposable temporary distros for image builds and pool instances.

## Build The Runner Image Manually

The `start` command builds or replaces the configured image automatically. Use this section when developing from source, debugging image builds, or intentionally separating image preparation from runner startup.

Default WSL and Docker Container builds and runner-only Tart builds do not need the upstream `actions/runner-images` checkout:

```bash
go run ./cmd/ephemeral-action-runner image build --replace
```

If `image.customInstallScripts` includes EPAR's Docker/browser or web/E2E scripts, update the pinned upstream checkout first:

```bash
go run ./cmd/ephemeral-action-runner image update-upstream
go run ./cmd/ephemeral-action-runner image build --replace
```

The Tart web/E2E example sets `provider.rosettaTag: rosetta`. Tart builds with that option start with `tart run --rosetta rosetta`, install Rosetta guest support, and validate that Docker can run a `linux/amd64` Alpine container returning `x86_64`.

Tart output is a local Tart image name, such as `epar-ubuntu-24-arm64`. Confirm it with:

```bash
tart list
```

The default WSL output is a rootfs tar path:

```text
work/images/epar-wsl-catthehacker-ubuntu.tar
```

When the WSL source is a Docker image, EPAR also writes an intermediate source rootfs tar and env cache next to the output image, for example `work/images/epar-wsl-catthehacker-ubuntu.source.rootfs.tar` and `.env`. Later builds reuse that source cache; delete those files when you intentionally want to reconvert the Docker image.

EPAR also writes image manifests so `start` can tell whether the local image still matches the config. Docker Container stores the manifest hash as a Docker image label and stores the manifest at `/opt/epar/image-manifest.json`. WSL stores `/opt/epar/image-manifest.json` inside the exported image and writes a sidecar next to the tar.

Docker Container output is a Docker image tag, such as `epar-docker-container-catthehacker-ubuntu`. Confirm it with:

```bash
docker image ls epar-docker-container-catthehacker-ubuntu
```

Build logs are written under `work/logs/builds` by default. Run `ephemeral-action-runner logs path` to resolve a customized logging root and see [Logging](logging.md) for rotation and retention.

## Customize The Image

Docker Sandboxes adapts a pinned full Catthehacker source into its recommended template; WSL and Docker Container use the full Catthehacker runner image by default. For Docker-focused jobs, `configs/docker-container.act.example.yml` uses the smaller Catthehacker Act image, which includes Node and the Docker Engine/CLI/Compose/Buildx stack EPAR needs. It does not guarantee browser dependencies; use `configs/docker-container.web-e2e.example.yml` for Playwright or other browser tests. Tart and the WSL lean examples are runner-only. Use `image.customInstallScripts` when you want a different WSL or Docker Container image shape, such as the smaller web/E2E examples. Docker Sandboxes customization uses its separate template build pipeline:

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
go run ./cmd/ephemeral-action-runner pool verify --instances 1 --cleanup
```

For a full registration check:

```bash
go run ./cmd/ephemeral-action-runner pool verify --instances 2 --register-only --cleanup
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
- Docker Container images validate the private inner Docker daemon inside each runner container.
- Tart Rosetta images validate `docker run --platform linux/amd64 alpine:3.20` and expect `uname -m` to return `x86_64`.
- Web/E2E images also validate `node`, `npm`, `zip`, `unzip`, `tar`, `rsync`, and `mysql`.

When `docker.registryMirrors` is configured, EPAR applies the mirror configuration before runtime validation.

If a Docker Container workflow depends on amd64-only images while the host is ARM64, validate host emulation inside a running EPAR instance:

```bash
docker exec <epar-instance> docker run --rm --platform linux/amd64 alpine:3.20 uname -m
```

The expected output is `x86_64`.

## Run A Foreground Pool Manually

```bash
go run ./cmd/ephemeral-action-runner pool up --instances 2
```

`start` is the recommended public command because it also checks the image before starting runners. `pool up` is the lower-level supervisor command for users who already prepared the image.

`pool up` keeps the requested number of runners online. Each GitHub ephemeral runner exits after one job. EPAR then retires that instance and creates a fresh replacement. The requested count is a strict physical local-instance cap: provisioning, ready, draining, quarantined, and cleanup-pending resources all consume a slot, including old runners during host-trust rotation.

When GitHub registration or readiness has a transient network, `429`, or `5xx` failure during supervised replacement, EPAR pauses allocation and retries with the configured exponential backoff while continuing monitoring and cleanup. It does not create extra candidates while remote state is uncertain; see [Configuration](configuration.md#common-edits) and [Operations](operations.md#capacity-reconciliation-and-outage-recovery) for retry settings and recovery steps.

Stop the supervisor with Ctrl-C. By default, EPAR cleans up active instances and matching GitHub runner records before it exits.

For startup after login, see [Windows Startup](advanced/windows-startup.md) or [macOS Startup](advanced/macos-startup.md).

Use these flags only for debugging:

- `--keep-on-exit`: leave instances running when the supervisor exits.
- `--replace-completed=false`: do not create replacements after completed jobs.

## Status And Cleanup

```bash
go run ./cmd/ephemeral-action-runner status
go run ./cmd/ephemeral-action-runner cleanup
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
runs-on: [self-hosted, linux, X64, epar-wsl-catthehacker-ubuntu]
```

For the default Docker Container image, target the default Docker Container label:

```yaml
runs-on: [self-hosted, linux, epar-docker-container-catthehacker-ubuntu]
```

For the recommended Docker Sandboxes full template, target the provider label:

```yaml
runs-on: [self-hosted, linux, X64, epar-docker-sandboxes]
```

For the Docker-focused Act image, target its dedicated label:

```yaml
runs-on: [self-hosted, linux, epar-docker-container-catthehacker-act]
```

For Docker Container web/E2E images, target the custom web/E2E label:

```yaml
runs-on: [self-hosted, linux, epar-docker-container-catthehacker-ubuntu-web-e2e]
```

When that Docker Container runner is used for amd64-only runtime images, keep the workflow's Docker platform explicit, for example `DOCKER_PLATFORM=linux/amd64` or the equivalent variable used by your compose scripts, and verify the host runtime supports amd64 emulation as described above.

Do not use `ubuntu-latest` for these self-hosted runners.

## Dry Run

Use `--dry-run` to inspect provider command construction without mutating local instances:

```bash
go run ./cmd/ephemeral-action-runner pool verify --dry-run --instances 2
```

## Maintainer Source-Only Releases

Releases are manually dispatched from GitHub Actions and contain no uploaded assets. GitHub automatically provides **Source code (zip)** and **Source code (tar.gz)** downloads for each release tag.

```bash
git tag -a v0.1.0-beta.1 -m "v0.1.0-beta.1"
git push origin v0.1.0-beta.1
```

Before dispatching, a repository administrator must enable immutable releases in the repository. In **Actions → Release → Run workflow**, supply an existing remote tag that matches `[v]MAJOR.MINOR.PATCH` or `[v]MAJOR.MINOR.PATCH-(alpha|beta|rc).N`, then type `publish source-only release` exactly. The workflow requires annotated tags, verifies the remote tag, confirms its commit is reachable from `origin/main`, checks out and tests that exact commit, and refuses to overwrite an existing release.

Alpha, beta, and RC tags are published as prereleases and are not marked latest. To promote a prerelease to a stable tag without changing the commit, provide the existing prerelease tag in `promotion_from`; the workflow verifies that the tag and its GitHub Release exist, that its normalized `MAJOR.MINOR.PATCH` core matches the stable tag, and that both tags point to the same commit, then creates stable promotion notes instead of a generated delta.
