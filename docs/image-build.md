# Image Build

EPAR builds a reusable Ubuntu runner image for the selected provider. The image contains the GitHub Actions runner plus whatever tools you choose through install scripts.

For Tart, the image build has two image names:

- `image.sourceImage`: clean upstream VM image, default `ghcr.io/cirruslabs/ubuntu:latest`.
- `image.outputImage`: reusable runner base image, default `epar-ubuntu-24-arm64`.

These are Tart VM image names. They are stored in Tart's local VM registry and are visible with `tart list`; they are not emitted as repository-local files.

For WSL, the image build produces a rootfs tar. It can start from either a Docker image or an existing rootfs tar:

- `image.sourceType`: `docker-image` or `rootfs-tar`, default `docker-image` for WSL.
- `image.sourceImage`: source Docker image or rootfs tar, default `ghcr.io/catthehacker/ubuntu:full-latest`.
- `image.sourcePlatform`: Docker platform used when `sourceType` is `docker-image`, default `linux/amd64`.
- `image.outputImage`: reusable EPAR runner rootfs tar, default `work/images/epar-wsl-catthehacker-ubuntu.tar`.

For `docker-image` sources, EPAR pulls the source image, creates a temporary container, exports that container filesystem to an intermediate rootfs tar, and captures the image environment metadata. Later builds reuse the intermediate rootfs only when the cached source manifest still matches the source image, platform, and digest. The WSL build then imports the rootfs into a temporary distro, enables systemd, installs the runner runtime, writes the captured image env under `/opt/epar`, validates it, exports the reusable tar, and unregisters the temporary distro. Pool instances import from `provider.sourceImage`, which should point at the built reusable tar.

For Docker-DinD, the image build uses Docker images:

- `image.sourceType`: `docker-image`.
- `image.sourceImage`: maintained Catthehacker Ubuntu runner image, default `ghcr.io/catthehacker/ubuntu:full-latest`.
- `image.outputImage`: reusable EPAR runner container image tag, default `epar-docker-dind-catthehacker-ubuntu`.

Docker-DinD builds a thin wrapper over the source image, installs the GitHub Actions runner, reuses Docker Engine/CLI/Compose/Buildx from the base image when they are already present, adds a container entrypoint that starts `dockerd`, then runs configured install scripts and validation. Pool instances are privileged containers created from `provider.sourceImage`, which should match the built reusable Docker image tag.

Every build writes an EPAR image manifest. The `start` command compares that manifest with the current config and source image identity before creating runners. If the image is missing, has no manifest, or no longer matches, `start` rebuilds it with replace enabled. The lower-level `image build` command keeps its explicit safety behavior and still requires `--replace` when the output already exists.

```mermaid
flowchart TD
  Source["Clean Ubuntu source<br/>Tart image, WSL source, or Docker image"] --> Build["Temporary build instance or Docker build"]
  Build --> Scripts["EPAR guest scripts"]
  Scripts --> Runner["GitHub Actions runner"]
  Runner --> WSLFull["WSL Docker-source env and Docker validation"]
  WSLFull --> DockerDind["Docker-DinD-only private daemon layer"]
  DockerDind --> Rosetta["Optional Tart Rosetta layer"]
  Rosetta --> Custom["Optional custom install scripts"]
  Custom --> Validate["Runtime validation"]
  Validate --> Output["Reusable runner image"]
  Output --> Pool["Disposable pool instances"]
```

## Image Install Scripts

Several layers control what is pre-installed in the Ubuntu image:

1. `image.trustedCaCertificatePaths`, when configured, installs explicitly trusted enterprise CA certificates before guest install steps access the network.
2. `/opt/epar/install-base.sh` is intentionally lean. It does not install Docker, browsers, language runtimes, or project tools.
3. `/opt/epar/install-runner.sh` always installs the GitHub Actions runner.
4. WSL builds from Docker-image sources validate Docker Engine from the base image and preserve source image environment metadata for runner jobs.
5. Docker-DinD builds validate or install Docker Engine and add the private `dockerd` entrypoint.
6. Tart builds with `provider.rosettaTag` install the optional Rosetta amd64 container layer.
7. `image.customInstallScripts` adds optional tool layers.

```mermaid
flowchart LR
  Base["Runner-only base"] --> Runner["GitHub Actions runner"]
  Runner --> WSLFull["WSL Docker-source env and Docker validation"]
  WSLFull --> DinD["Docker-DinD-only daemon layer"]
  DinD --> Rosetta["Optional Tart Rosetta layer"]
  Rosetta --> Optional["customInstallScripts"]
  Optional --> Docker["Docker/browser layer"]
  Optional --> Web["web/E2E layer"]
  Optional --> Yours["your scripts"]
```

The public WSL and Docker-DinD default examples start from `ghcr.io/catthehacker/ubuntu:full-latest`, so they inherit Docker plus the broader Catthehacker runner tool stack. Tart and the WSL lean examples leave `image.customInstallScripts` empty, producing a runner-only Ubuntu image. Docker-DinD always needs Docker Engine because the provider depends on a private inner Docker daemon.

EPAR ships reusable install scripts for common cases:

- `scripts/guest/ubuntu/install-docker-browser.sh` installs Docker Engine, Docker CLI, Compose v2, Buildx, and a Chromium-compatible browser.
- `scripts/guest/ubuntu/install-web-e2e.sh` includes Docker/browser support and ensures Node.js/npm, `zip`, `rsync`, and `mysql-client` for common web app and browser E2E workflows.

Built-in install scripts call `scripts/guest/ubuntu/wait-apt-ready.sh` before package installs. It stops active `apt-daily` jobs for the current boot only, waits for dpkg locks to clear, and leaves Ubuntu's normal apt timer enablement unchanged in the finalized image.

### Enterprise CA certificates

If HTTPS traffic is inspected by an enterprise proxy, or install scripts use a
private registry with an internal CA, provide the trusted CA explicitly:

```yaml
image:
  trustedCaCertificatePaths:
    - .local/enterprise-root.pem
    - ~/company/intermediate.crt
```

Each path must be a readable PEM or DER X.509 CA certificate file, not a
directory. Bundled PEM CA files are supported. EPAR rejects invalid or non-CA
content before the build, normalizes certificate filenames from certificate
fingerprints, and runs Ubuntu's `update-ca-certificates` before `apt`, `curl`,
or runner downloads. Certificate paths and content digests are part of the EPAR
image manifest, so certificate rotation invalidates image reuse.

This keeps TLS verification enabled. Do not work around certificate errors with
`curl -k`, `NODE_TLS_REJECT_UNAUTHORIZED=0`, or package-manager verification
disabling.

```yaml
image:
  customInstallScripts:
    - scripts/guest/ubuntu/install-web-e2e.sh
```

The built-in `install-web-e2e.sh` script reuses base-image Node.js/npm only when its numeric Node major version meets the pinned toolset's `.node.default`; otherwise, it installs Node.js/npm through the pinned GitHub runner-image `install-nodejs.sh` script. It also adds `zip`, `rsync`, and `mysql-client`. It does not install MySQL server, project dependencies, `node_modules`, Playwright test packages, Playwright browser cache, Docker credentials, or application runtime secrets.

Use the web/E2E examples when workflows need this larger toolset:

```bash
cp configs/tart.web-e2e.example.yml .local/config.yml
```

```powershell
Copy-Item configs/wsl.web-e2e.example.yml .local/config.yml
```

`image.customInstallScripts` is a list of extra shell scripts:

```yaml
image:
  customInstallScripts:
    - scripts/guest/ubuntu/install-web-e2e.sh
    - examples/custom-install/install-extra-apt-tools.sh
```

Relative paths are resolved from the repository root and must stay inside the repository; absolute paths are also accepted. EPAR copies and runs these scripts as root, in the order listed, after the GitHub Actions runner is installed and before image validation/finalization. On Tart, if `provider.rosettaTag` is set, EPAR installs the Rosetta guest support before these custom scripts run.

Example script:

```bash
#!/usr/bin/env bash
set -euo pipefail

export DEBIAN_FRONTEND=noninteractive

apt-get update
apt-get install -y --no-install-recommends \
  make \
  pkg-config \
  shellcheck
```

The same script can be used by Tart, WSL, and Docker-DinD because it runs inside Ubuntu. If the customized image changes workflow capabilities, give it distinct image names and labels so workflows can opt into it explicitly:

```yaml
image:
  outputImage: work/images/epar-ubuntu-24-wsl-web-e2e-extra.tar
  customInstallScripts:
    - scripts/guest/ubuntu/install-web-e2e.sh
    - examples/custom-install/install-extra-apt-tools.sh

runner:
  labels: [self-hosted, linux, X64, epar-wsl-ubuntu-24.04-web-e2e-extra]

provider:
  sourceImage: work/images/epar-ubuntu-24-wsl-web-e2e-extra.tar
```

Do not bake secrets, private keys, Docker credentials, project `node_modules`, language package caches, or application runtime artifacts into the image. Those belong in the workflow, repository dependency lock files, or GitHub secrets.

Docker registry mirrors are runtime configuration, not image content. Keep them in local config under `docker.registryMirrors`; EPAR applies them to each disposable instance before validation. See [Docker Registry Mirrors](advanced/docker-registry-mirrors.md).

## Upstream Runner Images

Runner-only Tart images and the default WSL and Docker-DinD images do not require EPAR's pinned `actions/runner-images` checkout. The default WSL and Docker-DinD images start from `ghcr.io/catthehacker/ubuntu:full-latest`, which already includes Docker Engine, Compose, Buildx, Node/npm, and the broader Catthehacker runner tool stack.

The built-in Docker/browser and web/E2E scripts require a pinned checkout of `actions/runner-images`:

```bash
ephemeral-action-runner image update-upstream
```

That writes the checked-out commit to `third_party/runner-images.lock`. The checkout directory itself is ignored by Git.

When one of those built-in scripts is selected, the build copies only the required upstream Ubuntu script subset into the guest or Docker build context:

- `images/ubuntu/scripts/helpers`
- `images/ubuntu/scripts/build/install-docker.sh`
- `images/ubuntu/scripts/build/install-google-chrome.sh`
- `images/ubuntu/scripts/build/install-nodejs.sh`
- `images/ubuntu/toolsets`

## Docker-DinD Images

Use `configs/docker-dind.example.yml` for the default full Catthehacker runner container with a private Docker daemon. For smaller Docker-focused workloads, use `configs/docker-dind.act.example.yml`, which starts from `ghcr.io/catthehacker/ubuntu:act-latest` without custom install scripts. It includes Node plus Docker Engine/CLI/Compose/Buildx, but does not guarantee browser dependencies. Use `configs/docker-dind.web-e2e.example.yml` when workflows need Playwright or another browser workload; it starts from the same Act base and layers the web/E2E add-on.

```bash
cp configs/docker-dind.example.yml .local/config.yml
./bin/ephemeral-action-runner image build --replace
```

Run `image update-upstream` first when using `configs/docker-dind.web-e2e.example.yml`, because that optional layer installs browser and Node.js pieces from the pinned upstream runner-images scripts.

The output image is a Docker image tag:

```bash
docker image ls epar-docker-dind-catthehacker-ubuntu
```

The provider creates each runner instance with `docker create --privileged` and no host socket mount. The image entrypoint starts a private `dockerd`, waits for `docker info`, and keeps the container alive while EPAR configures and monitors the GitHub runner process. Workflow Docker resources live inside that inner daemon. The inner daemon defaults to the `vfs` storage driver because it is reliable for nested Docker across Docker Desktop, OrbStack, and Linux Docker hosts; users can bake a different `EPAR_DOCKERD_STORAGE_DRIVER` into a derived image after validating the host.

## WSL Images

Use `configs/wsl.example.yml` for the default full Catthehacker runner image converted into WSL:

```powershell
Copy-Item configs/wsl.example.yml .local/config.yml
./bin/ephemeral-action-runner image build --replace
```

The default WSL build uses Docker on the Windows host only to prepare the source rootfs. It runs `docker pull`, `docker create`, `docker export`, and cleanup for `ghcr.io/catthehacker/ubuntu:full-latest`, then imports the exported rootfs into WSL and applies EPAR's normal lifecycle layer. If Docker is unavailable, use Docker Desktop, Docker Engine, or switch to `image.sourceType: rootfs-tar` with a prepared rootfs tar.

The output image is a WSL-importable rootfs tar:

```text
work/images/epar-wsl-catthehacker-ubuntu.tar
```

EPAR also writes an intermediate source tar and env cache beside the output, for example:

```text
work/images/epar-wsl-catthehacker-ubuntu.source.rootfs.tar
work/images/epar-wsl-catthehacker-ubuntu.source.rootfs.tar.env
work/images/epar-wsl-catthehacker-ubuntu.source.rootfs.tar.source.json
work/images/epar-wsl-catthehacker-ubuntu.tar.epar-manifest.json
```

Later builds reuse that source cache when its source manifest still matches. Delete those files when you intentionally want to reconvert the Docker image.

WSL runner startup sources `/opt/epar/source-image.env` before launching the GitHub Actions runner. That preserves image metadata such as `ImageOS`, `ImageVersion`, runner tool cache paths, browser paths, and Java paths from the Docker image source. WSL does not use Docker-DinD's container entrypoint; it keeps the systemd and keepalive model used by other WSL images.

Use `configs/wsl.lean.example.yml` when you want the old smaller rootfs-tar path. That config expects you to export a clean Ubuntu 24.04 WSL distro to `work/images/ubuntu-24.04-clean.rootfs.tar`.

## Installed Runtime

The default WSL and Docker-DinD builds use `ghcr.io/catthehacker/ubuntu:full-latest` as the source image. It is larger than the medium Catthehacker act image, but it is the recommended default for public users because common tools such as Node/npm are already present. The WSL lean and web/E2E examples keep demonstrating smaller custom paths that layer only selected dependencies.

Catthehacker's `full-latest` and `act-latest` tags are rolling references. EPAR records the resolved source digest in its image manifest so a built image remains auditable, but a later `image build --replace` can consume a newer upstream digest. Pin `image.sourceImage` to a tested digest when rebuild reproducibility is required.

The default build installs:

- GitHub Actions runner Linux package from `actions/runner`
- minimal OS packages required by that runner package
- additional tools selected by `image.customInstallScripts`

The optional `install-docker-browser.sh` layer installs:

- Docker through upstream `install-docker.sh`
- upstream Google Chrome on x64
- Playwright-managed Chromium on ARM64, exposed as `epar-browser`, `chromium`, and `chromium-browser`

The WSL default and Docker-DinD provider validate Docker Engine/CLI/Compose/Buildx through `scripts/guest/ubuntu/install-docker-engine.sh`. Docker-DinD then starts the daemon at container runtime from `/opt/epar/container-entrypoint.sh`; WSL starts Docker through systemd inside the distro. Set `EPAR_FORCE_UPSTREAM_DOCKER_INSTALL=true` inside non-WSL-default image builds only if you intentionally want to replace the base image's Docker packages with the pinned upstream `actions/runner-images` Docker install harness.

The ARM64 Docker harness prefers upstream `toolset-2404-arm64.json`. If an older upstream checkout does not contain that file, EPAR falls back to a minimal ARM-aware Docker toolset.

The harness skips upstream Docker image cache pulls by default. Set `EPAR_SKIP_UPSTREAM_DOCKER_IMAGE_CACHE=false` inside the guest environment before `install-docker-browser.sh` if exact upstream cache behavior is required.

## Tart Rosetta Layer

Set `provider.rosettaTag: rosetta` on a Tart config to start image-build and pool instances with `tart run --rosetta rosetta`. During Tart image build EPAR installs:

- `/opt/epar/setup-rosetta.sh`
- `epar-rosetta.service`
- `/opt/epar/features/rosetta-amd64`

The setup script mounts the Tart Rosetta virtiofs share at `/run/rosetta`, mounts `binfmt_misc` if needed, and registers x86_64 ELF execution through `/run/rosetta/rosetta` with `OCF` flags.

Rosetta is not installed for WSL builds and is not enabled for Tart unless `provider.rosettaTag` is non-empty.

At the end of a build, `/opt/epar/finalize-image.sh` stops Docker/containerd if they exist, clears Docker's persisted default bridge database, removes temporary validation files, and syncs the filesystem. This avoids cloned instances inheriting stale `docker0` bridge metadata from build-time validation.

Runtime validation always verifies the base runner user and runner files. If the Docker/browser feature marker is present, validation also starts Docker and verifies Docker access as the same Linux user that runs the GitHub Actions runner:

```bash
sudo -u runner -H docker version
sudo -u runner -H docker compose version
sudo -u runner -H docker buildx version
sudo -u runner -H docker run --rm hello-world
printf '%s\n' '<p>EPAR browser validation marker</p>' >/tmp/epar-browser-validation.html
sudo -u runner -H chromium --headless --no-sandbox --dump-dom file:///tmp/epar-browser-validation.html
```

If the Rosetta feature marker is present, validation also verifies a real amd64 Linux container:

```bash
sudo -u runner -H docker run --rm --platform linux/amd64 alpine:3.20 sh -c 'uname -m'
```

The expected output is `x86_64`.

The bundled `scripts/guest/ubuntu/install-web-e2e.sh` script creates a feature marker so `pool verify` also validates `node`, `npm`, `zip`, `unzip`, `tar`, `rsync`, and `mysql` on cloned instances.

## WSL Bootstrap

The default WSL config does not require a manually exported Ubuntu tar. It uses Docker to convert `ghcr.io/catthehacker/ubuntu:full-latest` into a rootfs tar during `image build`.

For lean `image.sourceType: rootfs-tar` configs, create the clean Ubuntu tar once before `image build`. The supported path is to install an Ubuntu 24.04 WSL distro, export it, then use that tar as `image.sourceImage`:

```powershell
New-Item -ItemType Directory -Force work/images
wsl --install -d Ubuntu-24.04 --no-launch
wsl --export Ubuntu-24.04 work/images/ubuntu-24.04-clean.rootfs.tar
```

After the export exists, EPAR uses disposable imported distros for image builds and runner instances. The WSL provider uses `provider.installRoot`, default `work/wsl`, for those imported distro files.

References:

- [WSL basic commands](https://learn.microsoft.com/en-us/windows/wsl/basic-commands)
- [Systemd support in WSL](https://learn.microsoft.com/en-us/windows/wsl/systemd)
- [GitHub Ubuntu runner image build scripts](https://github.com/actions/runner-images/tree/main/images/ubuntu/scripts/build)
