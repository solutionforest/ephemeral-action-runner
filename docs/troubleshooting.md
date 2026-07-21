# Troubleshooting

This page is organized by symptom, host OS, and EPAR provider. Start with the sections that match the machine and provider you are using.

## Quick Diagnostics

Start with the commands that match your host and provider.

### All Hosts

EPAR logs are written under `work/logs` by default. The latest top-level error report is usually:

```text
work/logs/epar-last-error.log
```

Image build logs use provider-specific names, for example:

```text
work/logs/builds/epar-docker-dind-catthehacker-ubuntu.docker-build.log
work/logs/builds/epar-wsl-catthehacker-ubuntu.wsl-build.log
```

Check the EPAR version and selected config:

```bash
./start --help
go run ./cmd/ephemeral-action-runner version
```

If you are running without local Go, use `./start --help`; the wrapper will run EPAR through the containerized Go toolchain.

### Docker-Backed Workflows

Use these on any host when the provider is Docker-DinD, when WSL image preparation starts from a Docker image, or when the no-Go wrapper is in use:

```bash
docker version
docker info
docker system df
docker image ls
```

To see the free space available to containers on the active Docker daemon:

```bash
docker run --rm ghcr.io/catthehacker/ubuntu:full-latest df -h /
```

For a custom source image, replace `ghcr.io/catthehacker/ubuntu:full-latest` with the value from `image.sourceImage`.

### Windows Hosts

For Windows hosts that use WSL2, Docker Desktop's WSL2 backend, or the WSL provider:

```powershell
wsl --version
wsl -l -v
docker context ls
docker run --rm ghcr.io/catthehacker/ubuntu:full-latest df -h /
```

`ghcr.io/catthehacker/ubuntu:full-latest` is the default source image for EPAR's default config. If your config uses a custom source image, replace it with your configured `image.sourceImage`.

Docker Desktop's WSL2 backend stores Docker data in a WSL-backed virtual disk. Windows Explorer free space and container-visible free space are related, but they are not the same number.

### Linux Docker Engine Hosts

On native Linux, Docker data usually lives under Docker's root directory. Check it directly:

```bash
docker info --format '{{.DockerRootDir}}'
df -h "$(docker info --format '{{.DockerRootDir}}')"
```

### macOS Docker Hosts

Docker Desktop and OrbStack keep Linux container data inside their own VM/storage area. Use Docker's own view first:

```bash
docker system df
docker run --rm ghcr.io/catthehacker/ubuntu:full-latest df -h /
```

If the container-visible disk is full, adjust or clean the Docker/OrbStack storage from that product's settings. Finder free space by itself may not reflect the Linux VM storage available to containers.

## Docker Container Fails Because Its Architecture Does Not Match The Runner

### Symptoms

A Docker or Docker Compose service may fail immediately with one of these messages:

```text
exec /bin/sh: exec format error
exec user process caused: exec format error
cannot execute binary file: Exec format error
```

Docker may also warn that the requested image platform does not match the detected host platform. That warning alone is not a failure: the container can still run when a compatible emulation handler is already registered.

These related messages point to different problems:

- `no matching manifest for linux/arm64/v8` or `no matching manifest for linux/amd64` means the image does not publish the requested platform. QEMU cannot supply a missing image manifest; choose an available platform or publish a multi-platform image.
- `qemu-x86_64: Could not open '/lib64/ld-linux-x86-64.so.2'` means translation started but the expected foreign-architecture loader or userspace is unavailable or incompatible. Registration alone may not make that image work.
- Exit code `139` indicates a segmentation fault. Emulation can expose workload-specific incompatibilities, but this code by itself does not prove an architecture mismatch.

### Confirm The Host And Image Platforms

Check the runner architecture and the Docker daemon that will execute the container:

```bash
uname -m
docker info --format '{{.OSType}}/{{.Architecture}}'
```

Inspect the locally selected image platform:

```bash
docker image inspect --format '{{.Os}}/{{.Architecture}}' IMAGE
```

Inspect all platforms published by a registry image:

```bash
docker buildx imagetools inspect IMAGE
```

For Docker Compose, also inspect the resolved configuration and look for a service-level `platform:` value:

```bash
docker compose config
```

An x64 Linux Docker daemon normally runs `linux/amd64` images natively, and an ARM64 daemon normally runs `linux/arm64` images natively. Pulling or loading a foreign image does not prove that the daemon can execute it.

### Match GitHub-Hosted Linux Behavior With Explicit QEMU Setup

GitHub's Ubuntu runner image installs Docker, but its published installation script and software inventory do not promise pre-registered foreign-architecture emulators. When a trusted Linux job must run foreign-architecture containers, configure the requirement explicitly before the first such container starts:

```yaml
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - name: Set up ARM64 container emulation
        uses: docker/setup-qemu-action@96fe6ef7f33517b61c61be40b68a1882f3264fb8 # v4
        with:
          image: docker.io/tonistiigi/binfmt@sha256:400a4873b838d1b89194d982c45e5fb3cda4593fbfd7e08a02e76b03b21166f0
          platforms: arm64

      - name: Verify the foreign container
        run: docker run --rm --platform linux/arm64 alpine:3.22 uname -m
```

The expected output is `aarch64`. Add only the platforms the workflow needs; emulated compilation and compute-heavy workloads can be substantially slower than native execution. Keep architecture-sensitive jobs on native runners when performance or full compatibility matters.

`docker/setup-qemu-action` registers user-mode QEMU interpreters through Linux `binfmt_misc`. It helps Linux containers launch foreign-architecture user-space executables; it does not change the runner's CPU architecture, create a foreign-architecture VM, or make arbitrary host executables and libraries compatible. The action uses a privileged helper container, so use it only in trusted workflows and pin reviewed action and helper-image revisions according to your dependency policy.

Provider notes:

- Docker-DinD: run the setup action inside the EPAR job before Docker Compose or other foreign-image commands. It configures the disposable runner's Docker execution environment; no EPAR configuration switch is required.
- WSL: run the setup action inside the WSL runner when its Linux Docker daemon must execute a foreign image. An x64 WSL runner does not gain ARM64 container support merely by pulling or loading an ARM64 image.
- Tart: Tart runs an ARM64 VM on Apple Silicon. Its optional Rosetta path is experimental and is not equivalent to QEMU/binfmt compatibility. Prefer Docker-DinD or a native matching architecture when a workload is not compatible.
- GitHub-hosted Windows and macOS: GitHub documents Docker container actions and service containers as Linux-runner features. A Windows or macOS hardware label alone is therefore not a substitute for a Linux Docker daemon with emulation configured.

Official references:

- [Docker Setup QEMU action](https://github.com/docker/setup-qemu-action)
- [Docker multi-platform build strategies](https://docs.docker.com/build/building/multi-platform/)
- [GitHub-hosted runner labels and limitations](https://docs.github.com/en/actions/reference/runners/github-hosted-runners)
- [GitHub self-hosted runner container requirements](https://docs.github.com/en/actions/reference/runners/self-hosted-runners#requirements-for-self-hosted-runner-machines)
- [GitHub Ubuntu runner Docker installation](https://github.com/actions/runner-images/blob/main/images/ubuntu/scripts/build/install-docker.sh)

## Docker Image Build Runs Out Of Space

### Symptom

During `start` or `image build`, the log contains:

```text
E: You don't have enough free space in /var/cache/apt/archives/.
```

or another package install fails with `No space left on device`.

### What It Means

This error is raised inside the temporary container or guest that is building the runner image. It usually means the Docker daemon or VM backing that build is out of writable layer space. It does not necessarily mean the host OS drive has no free space.

Check the active Docker daemon:

```bash
docker run --rm ghcr.io/catthehacker/ubuntu:full-latest df -h /
docker system df
```

If `/` inside the container is nearly full, clean up Docker data or increase the Docker VM/data-disk limit before retrying.

### Cleanup Direction

Review Docker's usage first:

```bash
docker system df
docker system df -v
```

Docker prune commands remove resources that Docker considers unused. Depending on the command, that can include stopped containers, unused images, build cache, unused networks, or unused volumes. Review the Docker command help and the data on the machine before pruning, especially if you expect to restart stopped containers or keep data in Docker volumes.

## Docker Image Build Fails With SSL Certificate Errors

### Symptom

During `start`, `image build`, or a CI job, HTTPS access fails with certificate errors such as:

```text
curl: (60) SSL certificate problem: unable to get local issuer certificate
```

```text
Certificate verification failed: The certificate is NOT trusted.
The certificate issuer is unknown. Could not handshake: Error in the certificate verification.
```

### What It Means

Antivirus, endpoint-security, firewall, or corporate-proxy software may be inspecting HTTPS and re-signing certificates with a root CA trusted by the host but not present in the Ubuntu runner image.

### How to Fix

Do not disable certificate verification. Use EPAR's host trust overlay so the disposable Docker-DinD runners automatically inherit the host's trusted root CAs while retaining Ubuntu's standard roots.

New interactive Docker-DinD configurations enable the overlay by default. For an older Windows or macOS configuration, add:

```yaml
image:
  hostTrustMode: overlay
  hostTrustScopes: [system, user]
```

Linux supports only the system scope, so use `hostTrustScopes: [system]` instead. Rebuild the image after changing the configuration:

```powershell
go run ./cmd/ephemeral-action-runner image build --replace
```

Without Go installed, run `scripts\run-with-docker.ps1 image build --replace` on Windows or `scripts/run-with-docker.sh image build --replace` on macOS or Linux. Use the official wrapper because it collects trust from the real host rather than the temporary Linux toolchain container.

EPAR uses the resulting consolidated Ubuntu trust bundle during both image construction and CI jobs. Node.js, Python Requests, and pip receive compatible runtime defaults without overwriting values already supplied by the source image or runner environment.

Programs with private certificate stores can still require application-specific configuration. Java keystores remain a separate concern.

### Advanced: Add a Certificate Explicitly

Use `image.trustedCaCertificatePaths` when a required CA is not trusted by the selected host stores or when the configuration must pin a specific CA independently of host trust.

Export the CA as PEM, Base-64 encoded X.509 `.CER`, or DER-encoded `.CER`, place it in the repository, and add it to the configuration:

```yaml
image:
  hostTrustMode: overlay
  hostTrustScopes: [system, user]
  trustedCaCertificatePaths:
    - .local/private-root.cer
```

Explicit certificates are validated and added to the same Ubuntu trust bundle; they are combined with, not substituted for, the host trust overlay.

### Host Trust Overlay Is Missing, Stale, Or Mismatched

This section applies when a Docker-DinD config contains:

```yaml
image:
  hostTrustMode: overlay
```

EPAR fails closed when host collection returns no roots, the official no-Go bridge is missing, its feed is invalid or more than 30 seconds old, or a runner's 20-second lease does not match the image's trust generation. A pre-job mismatch can fail an already assigned GitHub job before repository steps run. Inspect the controller output, runner guest log, and the no-Go watcher's log in the host trust cache for the first collection, feed, or lease error.

Check these boundaries:

- Windows and macOS support `hostTrustScopes: [system, user]`; Linux supports `[system]` only.
- Overlay mode requires `provider.type: docker-dind` and `runner.ephemeral: true`.
- Use the official `./start`, `start.ps1`, or release launcher for the no-Go path. A bare Linux toolchain container cannot inspect Windows Certificate Stores or macOS Keychain and must not substitute its own CA bundle.
- On an uncommon Linux distribution, set `EPAR_HOST_TRUST_BUNDLE` to the distribution-generated PEM CA bundle before launching EPAR.
- Confirm host and guest clocks are correct; feed and lease expiry checks use timestamps and reject stale data.

Do not disable the pre-job gate or TLS verification. Restore host collection, then let EPAR build and register the current immutable trust generation.

Host Docker daemon trust is separate from runner trust. If `docker pull` of the source image fails, configure the authorized CA for Docker Desktop, OrbStack, or the host Docker Engine first; the Ubuntu overlay does not exist until after that pull succeeds.

## Windows Docker Desktop WSL2 Disk Is Smaller Than Expected

### Symptom

On a Windows machine where WSL2 storage was set up before 2021, the Docker container filesystem may report about 251 GB total:

```powershell
docker run --rm ghcr.io/catthehacker/ubuntu:full-latest df -h /
```

Example:

```text
Filesystem      Size  Used Avail Use% Mounted on
overlay         251G  211G   28G  89% /
```

On a Windows machine where WSL2 storage was set up after the 2022 WSL default-size change, the same command may report about 1007 GB total:

```text
Filesystem      Size  Used Avail Use% Mounted on
overlay        1007G  127G  830G  14% /
```

### Why It Happens

This is a Windows Docker Desktop / WSL2 storage detail, not an EPAR image-size issue. The command reports the size of Docker Desktop's Linux container storage, not the size of `ghcr.io/catthehacker/ubuntu:full-latest`.

For Windows machines where WSL2 storage was set up before 2021, the default WSL2 virtual disk maximum may be about 256 GB. For WSL2 setups created after the WSL 0.58.0 change, released in 2022, Microsoft's documentation says the default maximum for each WSL2 VHD is 1 TB. This can explain why one Windows machine reports about 251 GB while another reports about 1007 GB for the container-visible filesystem.

Windows Explorer free space by itself is not enough to confirm Docker has build space. The container-visible filesystem must have enough free space for the image pull, build layers, package manager cache, and final runner image.

For more background, see:

- <https://github.com/microsoft/WSL/issues/4373>
- <https://learn.microsoft.com/windows/wsl/disk-space>
- <https://docs.docker.com/desktop/features/wsl/>

## Docker-DinD Build Fails With `unknown flag: --progress`

### Symptom

The Docker-DinD image build fails with:

```text
unknown flag: --progress
```

### What It Means

This happens when the Docker client used for the build routes `docker build` through the legacy builder, or when the client does not have Buildx-style build support. It is most visible when EPAR is run through a containerized Go toolchain whose bundled Docker client differs from the host `docker.exe`.

Current EPAR builds use legacy-builder-compatible Docker build arguments. If you still see this error, confirm you are running a revision that includes that fix and check which Docker client is actually executing the command:

```bash
docker version
docker build --help
docker buildx version
```

## Docker-DinD Startup Fails

### Privileged Containers

Docker-DinD requires the host Docker runtime to allow privileged Linux containers. Confirm the Docker host supports:

```bash
docker run --rm --privileged alpine:3.20 true
```

### Nested Docker Storage Driver

If Docker-DinD starts but nested Docker operations fail with overlay mount errors, keep the default inner daemon storage driver:

```text
EPAR_DOCKERD_STORAGE_DRIVER=vfs
```

Use `overlay2` or `auto` in a derived image only after proving that storage driver works on the exact host runtime.

## WSL Provider Image Build Fails Early

This section applies to Windows hosts using `provider.type: wsl`.

If the default WSL image build fails before importing or starting the temporary distro, confirm Docker is reachable because the default WSL full image converts a Docker image into a rootfs tar:

```powershell
docker version
docker pull ghcr.io/catthehacker/ubuntu:full-latest
wsl -l -v
```

### WSL Import Exits With `0xffffffff`

If the Docker export completes but the first temporary-distro import fails like this:

```text
wsl.exe --import ... --version 2 failed: exit status 0xffffffff:
```

WSL may be in an unstable service or VM session. The error can also appear as
`Wsl/Service/CreateInstance/E_UNEXPECTED` or `Catastrophic failure` when
starting an existing distro. When the import itself fails, the advertised WSL
build and guest logs may be empty or absent because no guest was created yet.

Reset the WSL session before deleting or rebuilding a completed source rootfs:

1. Stop EPAR and quit Docker Desktop cleanly from its tray menu.
2. Run:

   ```powershell
   wsl --shutdown
   ```

3. Start Docker Desktop again and wait until it is ready.
4. Verify WSL and Docker. Replace `Ubuntu-24.04` if your installed distro has a
   different name:

   ```powershell
   wsl -d Ubuntu-24.04 --user root --exec /bin/true
   $LASTEXITCODE
   docker version
   ```

5. When the WSL command returns `0` and Docker is ready, rerun `./start` or
   `.\start`. EPAR reuses a matching cached
   `work/images/*.source.rootfs.tar`, avoiding another large Docker export.

`wsl --shutdown` stops every running WSL distro, including Docker Desktop's WSL
backend. Save work in other distros first. If the failure persists after the
reset, run `wsl --update`, shut WSL down again, reboot Windows, and retry once.
For persistent `0x8000FFFF` or `E_UNEXPECTED` failures, follow
[Microsoft's WSL troubleshooting guidance](https://learn.microsoft.com/windows/wsl/troubleshooting#error-code-0x8000ffff-unexpected-failure).

If the WSL image build fails after import but before systemd is ready, inspect:

```text
work/logs/builds/<image>.wsl-build.log
work/logs/builds/<temporary-distro>.guest.log
```

## GitHub Runner Registration Fails

Confirm the GitHub App has organization self-hosted runner read/write permission and that the private key path in the config is readable from the EPAR process:

```yaml
github:
  appId: 123456
  organization: your-org
  privateKeyPath: .local/github-app.pem
```

If stale runner records remain after an interrupted run:

```bash
go run ./cmd/ephemeral-action-runner cleanup
```

Cleanup only targets runner names matching `pool.namePrefix`, so keep that prefix unique per machine/config within the GitHub organization.
