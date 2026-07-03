# Image Build

EPAR builds a reusable Ubuntu runner image for the selected provider.

For Tart, the image build has two image names:

- `image.sourceImage`: clean upstream VM image, default
  `ghcr.io/cirruslabs/ubuntu:latest`.
- `image.outputImage`: reusable runner base image, default
  `epar-ubuntu-24-arm64`.

These are Tart VM image names. They are stored in Tart's local VM registry and
are visible with `tart list`; they are not emitted as repository-local files.

For WSL, the image build uses tar files:

- `image.sourceImage`: clean Ubuntu 24.04 rootfs tar, default
  `work/images/ubuntu-24.04-clean.rootfs.tar`.
- `image.outputImage`: reusable EPAR runner rootfs tar, default
  `work/images/epar-ubuntu-24-wsl.tar`.

The WSL build imports the clean tar into a temporary distro, enables systemd,
installs the runtime, validates it, exports the reusable tar, and unregisters
the temporary distro. Pool instances import from `provider.sourceImage`, which
should point at the built reusable tar.

## Upstream Runner Images

EPAR expects a pinned checkout of `actions/runner-images`:

```bash
ephemeral-action-runner image update-upstream
```

That writes the checked-out commit to `third_party/runner-images.lock`. The
checkout directory itself is ignored by Git.

The build copies only the required upstream Ubuntu script subset into the
guest:

- `images/ubuntu/scripts/helpers`
- `images/ubuntu/scripts/build/install-docker.sh`
- `images/ubuntu/scripts/build/install-google-chrome.sh`
- `images/ubuntu/toolsets`

## Installed Runtime

The build installs:

- Docker through upstream `install-docker.sh`
- upstream Google Chrome on x64
- Playwright-managed Chromium on ARM64, exposed as `epar-browser`, `chromium`,
  and `chromium-browser`
- GitHub Actions runner Linux package from `actions/runner`

The ARM64 Docker harness prefers upstream `toolset-2404-arm64.json`. If an
older upstream checkout does not contain that file, EPAR falls back to a minimal
ARM-aware Docker toolset.

The harness skips upstream Docker image cache pulls by default. Set
`EPAR_SKIP_UPSTREAM_DOCKER_IMAGE_CACHE=false` inside the guest environment before
`install-base.sh` if exact upstream cache behavior is required.

At the end of a build, `/opt/epar/finalize-image.sh` stops Docker/containerd,
clears Docker's persisted default bridge database, removes temporary validation
files, and syncs the filesystem. This avoids cloned instances inheriting stale
`docker0` bridge metadata from build-time validation.

## WSL Bootstrap

On Windows, create the clean Ubuntu tar once before `image build`. The supported
path is to install an Ubuntu 24.04 WSL distro, export it, then use that tar as
`image.sourceImage`:

```powershell
wsl --install -d Ubuntu-24.04 --no-launch
wsl --export Ubuntu-24.04 work/images/ubuntu-24.04-clean.rootfs.tar
```

After the export exists, EPAR uses disposable imported distros for image builds
and runner instances. The WSL provider uses `provider.installRoot`, default
`work/wsl`, for those imported distro files.

References:

- [WSL basic commands](https://learn.microsoft.com/en-us/windows/wsl/basic-commands)
- [Systemd support in WSL](https://learn.microsoft.com/en-us/windows/wsl/systemd)
