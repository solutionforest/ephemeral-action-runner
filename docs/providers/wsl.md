# WSL Provider

The WSL provider targets Windows hosts running WSL2. It manages disposable Ubuntu distros for trusted GitHub Actions jobs.

The provider maps EPAR lifecycle operations to `wsl.exe`:

- clone/create: `wsl --import <name> <install-dir> <rootfs.tar> --version 2`
- start/exec: `wsl -d <name> --user root --exec <command>`
- stop: `wsl --terminate <name>`
- delete: `wsl --unregister <name>`
- export image: `wsl --export <name> <rootfs.tar>`
- list: `wsl --list --verbose`

When a disposable runner is started, EPAR also keeps a quiet host-side `wsl.exe -d <name>` process open. This prevents WSL from auto-stopping an imported distro that is otherwise only running systemd services. `pool up`, `pool verify --cleanup`, and `cleanup` terminate that keepalive by terminating or unregistering the distro.

## Configuration

Use `configs/wsl.example.yml` as the starting point:

```yaml
image:
  sourceType: docker-image
  sourceImage: ghcr.io/catthehacker/ubuntu:full-latest
  sourcePlatform: linux/amd64
  outputImage: work/images/epar-wsl-catthehacker-ubuntu.tar
  customInstallScripts:
    # - examples/custom-install/install-extra-apt-tools.sh

runner:
  labels: [self-hosted, linux, X64, epar-wsl-catthehacker-ubuntu]
  includeHostLabel: true

provider:
  type: wsl
  sourceImage: work/images/epar-wsl-catthehacker-ubuntu.tar
  installRoot: work/wsl
```

`image.sourceType: docker-image` tells EPAR to convert the source Docker image into a WSL-importable rootfs tar during `image build`. `image.outputImage` is the reusable runner tar produced by `image build`. `provider.sourceImage` is the tar imported for disposable runner instances.

Use `configs/wsl.lean.example.yml` when you want the smaller tar-first path. Existing WSL configs that point `image.sourceImage` at a `.tar`, `.tar.gz`, or `.tgz` file are treated as `image.sourceType: rootfs-tar` for backward compatibility. Use `configs/wsl.web-e2e.example.yml` when workflows need the larger lean web/E2E install script and its `epar-wsl-ubuntu-24.04-web-e2e` label.

## Docker Image Sources

For the default full WSL image, EPAR uses Docker on the Windows host during `image build`:

1. `docker pull --platform linux/amd64 ghcr.io/catthehacker/ubuntu:full-latest`
2. `docker create` a temporary stopped container
3. `docker container inspect` to capture image environment metadata
4. `docker export` the container filesystem into an intermediate rootfs tar
5. `docker rm -f -v` cleanup

That exported rootfs is then imported into a temporary WSL distro. EPAR copies `/opt/epar` scripts, enables systemd, installs the GitHub Actions runner, writes the captured env to `/opt/epar/source-image.env`, validates Docker Engine from the base image, finalizes the image, and exports `image.outputImage`.

The intermediate source tar and env metadata are cached beside `image.outputImage`. Delete `*.source.rootfs.tar` and `*.source.rootfs.tar.env` when you intentionally want to reconvert the Docker image.

Docker is required only for the Docker-image conversion step. Running WSL pool instances afterward does not require Docker Desktop unless your workflows need Docker Desktop or another host-side Docker service.

## Systemd And Docker

The WSL image build writes `/etc/wsl.conf` with systemd enabled and `appendWindowsPath=false`, restarts the temporary distro, then installs the GitHub Actions runner inside the distro. Disabling Windows PATH injection keeps validation and jobs from accidentally resolving host-installed tools such as Windows Docker or Node.

The default WSL full image expects Docker Engine, dockerd, Compose v2, Buildx, and iptables to already exist in `ghcr.io/catthehacker/ubuntu:full-latest`. EPAR validates those tools and marks the image with `/opt/epar/features/docker-engine` so `pool verify` proves:

```bash
sudo -u runner -H docker version
sudo -u runner -H docker compose version
sudo -u runner -H docker buildx version
sudo -u runner -H docker run --rm hello-world
```

Browser support is validated only when `image.customInstallScripts` includes `scripts/guest/ubuntu/install-docker-browser.sh` or `scripts/guest/ubuntu/install-web-e2e.sh`:

```bash
sudo -u runner -H docker version
sudo -u runner -H docker compose version
sudo -u runner -H docker buildx version
sudo -u runner -H docker run --rm hello-world
printf '%s\n' '<p>EPAR browser validation marker</p>' >/tmp/epar-browser-validation.html
sudo -u runner -H chromium --headless --no-sandbox --dump-dom file:///tmp/epar-browser-validation.html
```

The provider does not mount the Windows Docker Desktop socket. Docker-enabled jobs run against Docker Engine inside the WSL distro.

Runner startup sources `/opt/epar/source-image.env` before launching `/opt/actions-runner/run.sh`. This lets GitHub Actions jobs inherit source image variables such as `ImageOS`, `ImageVersion`, `RUNNER_TOOL_CACHE`, browser paths, and Java paths. WSL keeps its own systemd and host keepalive model; it does not reuse Docker-DinD's container entrypoint.

WSL x64 is the preferred EPAR target for workflows that pull amd64-only Docker runtime images.

An x64 WSL runner can store an ARM64 image with `docker pull` or `docker load`, but it cannot run that image natively. Running ARM64 containers requires an explicitly configured emulation layer such as QEMU registered through `binfmt_misc`, or a native ARM64 runner. EPAR does not install cross-architecture emulation in WSL by default, so validate both the architecture and execution path before routing ARM64-dependent jobs to an x64 WSL label.

If `docker.registryMirrors` is configured, EPAR applies it to Docker Engine inside each disposable WSL distro before validation. Use a mirror URL reachable from inside WSL, such as an organization DNS name or a host/LAN address. See [Docker Registry Mirrors](../advanced/docker-registry-mirrors.md).

## Caveats

- WSL2 is not the same isolation boundary as a full VM per job.
- WSL distros share the WSL kernel and host integration surface.
- Use this provider for trusted internal jobs unless your environment has reviewed and accepted the isolation model.
- The default Docker-image source needs Docker Desktop, Docker Engine, or another reachable Docker daemon during `image build`.
- The full Catthehacker runner image is large and needs enough disk for the pulled Docker image, the intermediate source rootfs tar, the temporary WSL import, and the final WSL tar.
- Expect one long-lived host `wsl.exe` process per running disposable runner. This is intentional and keeps the WSL distro alive while it waits for jobs.
- Cleanup only unregisters distros whose names match `pool.namePrefix`.

References:

- [WSL basic commands](https://learn.microsoft.com/en-us/windows/wsl/basic-commands)
- [Systemd support in WSL](https://learn.microsoft.com/en-us/windows/wsl/systemd)
