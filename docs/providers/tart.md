# Tart Provider (Experimental)

The Tart provider is experimental. It targets Apple Silicon macOS hosts and currently supports Ubuntu ARM64 guests. Tart itself can run macOS ARM64 VMs, but EPAR's image build, runner service, validation, and cleanup scripts currently depend on Ubuntu, systemd, and Linux process interfaces, so macOS guests are not yet an EPAR provider mode.

> [!WARNING]
> The default Tart source, `ghcr.io/cirruslabs/ubuntu:latest`, is a basic Ubuntu ARM64 OS image. It does not contain the broad dependency set normally present in GitHub's hosted images from [`actions/runner-images`](https://github.com/actions/runner-images), including many language SDKs, CLIs, browsers, and build tools. Tart uses Apple's Virtualization framework, so an Apple Silicon host runs an ARM64 VM. Rosetta translates supported x86_64 Linux user-space programs inside that ARM64 guest; it does not create an x64 VM, and not every amd64 image or workload is compatible.

EPAR currently validates the Ubuntu path:

- clone a reusable Tart image
- start the VM headless
- use the Tart guest agent for `exec` and IP discovery
- validate the base GitHub Actions runner runtime
- register an ephemeral GitHub runner from the host
- delete the VM after the runner exits

Use `configs/tart.example.yml` for the basic runner-only Ubuntu image or `configs/tart.web-e2e.example.yml` for the existing opt-in web/E2E and Rosetta experiment. EPAR installs the GitHub Actions runner and its lifecycle scripts, but the default does not install Docker, .NET, PowerShell, Go, browsers, or the rest of GitHub's hosted-runner tool inventory.

If a workflow needs a GitHub-runner-like environment, build and maintain your own bootable Tart source image. Adapt the Ubuntu build scripts and tool definitions from [`actions/runner-images`](https://github.com/actions/runner-images) to that image, validate the resulting ARM64 tools, push it as a Tart VM image, and set `image.sourceImage` to it. Alternatively, add narrowly scoped `image.customInstallScripts` for only the dependencies your workflows require. EPAR does not convert the Catthehacker Docker image or automatically reproduce the complete GitHub-hosted image for Tart.

When Docker/browser support is selected on ARM64, EPAR exposes a Chromium-compatible browser through `epar-browser`, `chromium`, and `chromium-browser`; it is not guaranteed to be Google Chrome.

The default network mode is Tart NAT. `softnet` is accepted by the provider, but it can require host-side privileges.

If Docker is installed in a custom Tart guest, optional `docker.registryMirrors` settings are applied to the guest Docker daemon when each disposable VM starts. Use a mirror URL that is reachable from inside the Tart VM; `host.docker.internal` is Docker-container-specific and may not resolve in Tart guests. See [Docker Registry Mirrors](../advanced/docker-registry-mirrors.md).

## Experimental Rosetta Support For Linux Amd64 Containers

Tart on Apple Silicon runs ARM64 VMs, but Tart can expose Apple's Linux Rosetta runtime to the guest with `tart run --rosetta <tag>`. EPAR supports this as an opt-in Tart-only setting:

```yaml
provider:
  type: tart
  rosettaTag: rosetta
```

When `provider.rosettaTag` is set, EPAR starts Tart instances with `--rosetta rosetta`, installs `/opt/epar/setup-rosetta.sh` during image build, enables `epar-rosetta.service`, and registers an x86_64 Linux `binfmt_misc` handler inside the guest. Images with the Rosetta feature marker validate:

```bash
sudo -u runner -H docker run --rm --platform linux/amd64 alpine:3.20 sh -c 'uname -m'
```

The expected output is `x86_64`.

Host prerequisites:

- Apple Silicon macOS
- Tart version with `--rosetta` support
- Apple's Rosetta package installed on the macOS host

This is experimental support for Linux amd64 user-space containers. It is not nested virtualization and it does not make the Ubuntu VM an x64 VM. For native amd64 performance and compatibility, use a Windows WSL x64 provider or another x64 Linux host. For Tart Rosetta-capable runners, expose a distinct label such as `epar-tart-rosetta-amd64` so workflows can opt into the behavior explicitly.
