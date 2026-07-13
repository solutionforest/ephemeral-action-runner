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
work/logs/epar-docker-dind-catthehacker-ubuntu.docker-build.log
work/logs/epar-wsl-catthehacker-ubuntu.wsl-build.log
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

During `start` or `image build`, an `apt-get update` or `curl` step fails with certificate errors. Common messages include:

```text
curl: (60) SSL certificate problem: unable to get local issuer certificate
```

```text
Certificate verification failed: The certificate is NOT trusted.
The certificate issuer is unknown. Could not handshake: Error in the certificate verification.
```

You may also see these errors for `packages.microsoft.com` or `dl.google.com` during `apt-get update`, even before the GitHub runner download step.

### What It Means

The network between the container and the remote server is intercepting TLS connections. Antivirus, endpoint security, or corporate proxies re-sign HTTPS traffic with a local root CA that is not included in the Ubuntu CA bundle inside the runner image. The base image `ghcr.io/catthehacker/ubuntu:full-latest` trusts only standard public CAs, so the intercepted certificate chain fails verification.

You can confirm the intercept from inside a container:

```bash
docker run --rm --user root ghcr.io/catthehacker/ubuntu:full-latest bash -c \
  "openssl s_client -connect api.github.com:443 -servername api.github.com </dev/null 2>/dev/null | openssl x509 -noout -issuer"
```

If the issuer is not a well-known public CA, for example:

```text
OU = generated by Norton Antivirus for SSL/TLS scanning, O = Norton Web/Mail Shield, CN = Norton Web/Mail Shield Root
```

then TLS inspection is active.

### How to Fix

Do not disable certificate verification. Instead, add the inspection root CA to EPAR's image build.

1. Export the root certificate from the antivirus or security product.
   - On Windows, the Certificate Export Wizard can save it as a **Base-64 encoded X.509 `.CER`** file. A Base-64 encoded `.CER` file uses the same base-64 text payload as a `.PEM` file and works with EPAR's certificate handling.
   - DER-encoded `.CER` files are also accepted by EPAR, which normalizes them automatically.
2. Place the exported certificate in the repository, for example `.local/antivirus-root.cer` or `.local/antivirus-root.pem`.
3. Add it to `image.trustedCaCertificatePaths` in your EPAR config:

```yaml
image:
  trustedCaCertificatePaths:
    - .local/antivirus-root.cer
    # or, if you exported it as a PEM file:
    # - .local/antivirus-root.pem
```

EPAR validates the certificate, installs it into the build context, and runs `update-ca-certificates` before any `apt` or `curl` step. Re-run the build with `--replace` if the previous failed image layer is cached.

This also applies to other TLS-inspecting proxies, firewalls, or endpoint-security products that re-sign outbound HTTPS.

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
work/logs/<image>.wsl-build.log
work/logs/<temporary-distro>.guest.log
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
