# WSL2 Provider

WSL2 is a compatibility provider that runs each disposable GitHub Actions runner in an imported Ubuntu WSL distribution on a native Windows host. EPAR uses the shared pool lifecycle, including strict capacity, registration, replacement, and cleanup. The first-run wizard keeps it behind `C. Show compatibility providers`; existing configurations continue to use the documented runtime and cleanup path.

## When To Use It

Choose WSL2 for an existing Windows-hosted Linux deployment when you want native x64 Linux execution and a WSL distribution per runner. It is often the clearest compatibility choice for workflows that need native amd64 Docker execution on Windows; new setup should start with Docker Sandboxes when its capability checks pass.

## Support Status

WSL2 compatibility is supported only on native Windows with WSL default version 2. It is not equivalent to one full VM per job: distros share the WSL kernel and host integration surface, so use it for trusted jobs.

## Prerequisites

- Native Windows, `wsl.exe --status` reporting default version 2, and an Ubuntu-compatible WSL environment.
- Docker installed and running for the default Catthehacker Docker-image source during `image build`; later runner startup does not require Docker on the host.
- Enough storage for the pulled source image, intermediate rootfs tar, temporary WSL build distro, reusable tar, and active pool.

## Minimal Configuration

Start with [`configs/wsl.example.yml`](../../configs/wsl.example.yml):

```yaml
image:
  sourceType: docker-image
  sourceImage: ghcr.io/catthehacker/ubuntu:full-latest
  sourcePlatform: linux/amd64
  outputImage: work/images/epar-wsl-catthehacker-ubuntu.tar
  updateFrequency: weekly
  updateTime: "07:00"

provider:
  type: wsl
  sourceImage: work/images/epar-wsl-catthehacker-ubuntu.tar
  installRoot: work/wsl
```

Use `configs/wsl.lean.example.yml` with a pre-exported Ubuntu rootfs tar for a smaller path, or `configs/wsl.web-e2e.example.yml` for browser/E2E tooling. See [Configuration](../configuration.md) for labels, runner-group policy, capacity, and mirrors.

## Normal Workflow

1. Run `./start`; EPAR converts the default Docker source into a rootfs tar, builds the reusable WSL runner tar, and starts the pool.
2. Target the configured WSL label, normally `epar-wsl-catthehacker-ubuntu`.
3. Stop the supervisor with Ctrl-C, then wait for cleanup to finish before closing the terminal.

EPAR imports each runner from `provider.sourceImage`, enables systemd in the reusable image, and keeps a quiet host-side WSL process alive while a runner waits for work. That process is intentional: it prevents an otherwise idle systemd distro from stopping automatically.

## Limitations

- The default full image needs Docker only for source conversion; an image build can fail when the host daemon's storage is full even if Windows has free disk space.
- EPAR does not install cross-architecture emulation. An x64 WSL runner can pull an ARM64 image but cannot execute it natively.
- The default Docker-enabled runner uses Docker Engine inside WSL, not a mounted Windows Docker socket.

## Verification

```powershell
./start pool verify --instances 1 --cleanup
```

For a lean rootfs source, export Ubuntu 24.04 once before building:

```powershell
wsl --export Ubuntu-24.04 work/images/ubuntu-24.04-clean.rootfs.tar
```

## Troubleshooting

See [Troubleshooting](../troubleshooting.md) for Docker source conversion, WSL import failures, WSL/Docker storage, and architecture errors. Do not use `wsl --unregister` on an unrelated distro as general EPAR cleanup.
