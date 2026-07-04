# Usage

This page is the operational walkthrough. Start with the supported host you already have:

- Tart on Apple Silicon macOS
- WSL2 on Windows
- Docker-DinD on a Docker-capable host

## Prerequisites

Install the host tools you need:

| Required for | Tool |
| --- | --- |
| All hosts | Go 1.22 or newer, Git |
| macOS provider | Tart |
| Windows provider | WSL2 |
| Docker-DinD provider | Docker Engine, OrbStack, or Docker Desktop with privileged container support |
| Optional Docker registry mirrors | A running mirror service on the host, LAN, intranet, or cloud registry cache |
| Runner registration | GitHub App with organization self-hosted runner read/write permission |

Packer, GitHub CLI, and sshpass are not required.

Set up the GitHub App before registering runners. The image build command can run without GitHub credentials, but `pool verify --register-only`, `pool up`, `status`, and GitHub cleanup need the app settings. See [GitHub App Setup](github-app.md).

## Configure

Copy one example config into `.local/config.yml`, then edit the GitHub App fields and any labels you want to expose to workflows.

| Host and image | Example config |
| --- | --- |
| macOS Tart, runner-only | `configs/tart.example.yml` |
| macOS Tart, web/E2E with Rosetta amd64 Docker support | `configs/tart.web-e2e.example.yml` |
| Windows WSL2, runner-only | `configs/wsl.example.yml` |
| Windows WSL2, web/E2E | `configs/wsl.web-e2e.example.yml` |
| Docker-DinD, runner-only | `configs/docker-dind.example.yml` |
| Docker-DinD, web/E2E | `configs/docker-dind.web-e2e.example.yml` |

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

Docker-DinD:

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

## Build The CLI

```bash
go build -o ./bin/ephemeral-action-runner ./cmd/ephemeral-action-runner
```

Use `./bin/ephemeral-action-runner` in the examples below, or put `bin` on `PATH`.

## Prepare A WSL Source Tar

Skip this section for Tart and Docker-DinD.

The WSL provider builds reusable rootfs tar images. Create the clean Ubuntu 24.04 source tar once:

```powershell
New-Item -ItemType Directory -Force work/images
wsl --install -d Ubuntu-24.04 --no-launch
wsl --export Ubuntu-24.04 work/images/ubuntu-24.04-clean.rootfs.tar
```

After that, EPAR imports disposable temporary distros for image builds and pool instances.

## Build The Runner Image

Runner-only Tart and WSL images do not need the upstream `actions/runner-images` checkout:

```bash
./bin/ephemeral-action-runner image build --replace
```

If `provider.type: docker-dind`, or if `image.customInstallScripts` includes EPAR's Docker/browser or web/E2E scripts, update the pinned upstream checkout first:

```bash
./bin/ephemeral-action-runner image update-upstream
./bin/ephemeral-action-runner image build --replace
```

The Tart web/E2E example sets `provider.rosettaTag: rosetta`. Tart builds with that option start with `tart run --rosetta rosetta`, install Rosetta guest support, and validate that Docker can run a `linux/amd64` Alpine container returning `x86_64`.

Tart output is a local Tart image name, such as `epar-ubuntu-24-arm64`. Confirm it with:

```bash
tart list
```

WSL output is a rootfs tar path, such as:

```text
work/images/epar-ubuntu-24-wsl.tar
```

Docker-DinD output is a Docker image tag, such as `epar-docker-dind-ubuntu-24`. Confirm it with:

```bash
docker image ls epar-docker-dind-ubuntu-24
```

Build logs are written under `work/logs`.

## Customize The Image

The public default image is runner-only. Add tooling through `image.customInstallScripts`:

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
- Docker-DinD images validate the private inner Docker daemon inside each runner container.
- Tart Rosetta images validate `docker run --platform linux/amd64 alpine:3.20` and expect `uname -m` to return `x86_64`.
- Web/E2E images also validate `node`, `npm`, `zip`, `unzip`, `tar`, `rsync`, and `mysql`.

When `docker.registryMirrors` is configured, EPAR applies the mirror configuration before runtime validation.

If a Docker-DinD workflow depends on amd64-only images while the host is ARM64, validate host emulation inside a running EPAR instance:

```bash
docker exec <epar-instance> docker run --rm --platform linux/amd64 alpine:3.20 uname -m
```

The expected output is `x86_64`.

## Run A Foreground Pool

```bash
./bin/ephemeral-action-runner pool up --instances 2
```

`pool up` keeps the requested number of runners online. Each GitHub ephemeral runner exits after one job. EPAR then retires that instance and creates a fresh replacement.

Stop the supervisor with Ctrl-C. By default, EPAR cleans up active instances and matching GitHub runner records before it exits.

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

Use provider-specific labels in workflows. For the Tart web/E2E Rosetta image, target the existing web/E2E label plus the Rosetta label when the job needs amd64 Docker images:

```yaml
runs-on: [self-hosted, linux, ARM64, epar-tart-ubuntu-24.04-web-e2e, epar-tart-rosetta-amd64]
```

For Docker-DinD web/E2E images, target the Docker-DinD label:

```yaml
runs-on: [self-hosted, linux, ARM64, epar-docker-dind-ubuntu-24.04-web-e2e]
```

When that Docker-DinD runner is used for amd64-only runtime images, keep the workflow's Docker platform explicit, for example `DOCKER_PLATFORM=linux/amd64` or the equivalent variable used by your compose scripts, and verify the host runtime supports amd64 emulation as described above.

Do not use `ubuntu-latest` for these self-hosted runners.

## Dry Run

Use `--dry-run` to inspect provider command construction without mutating local instances:

```bash
./bin/ephemeral-action-runner pool verify --dry-run --instances 2
```
