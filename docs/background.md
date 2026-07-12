# Background

The original macOS-runner direction used macOS VM images because those tools are commonly found when searching for Mac self-hosted runners. That is a poor fit for Docker Compose jobs inside the guest: Docker Desktop and OrbStack on macOS rely on their own Linux VM, so using them inside a macOS Tart VM becomes nested virtualization.

For Docker container actions, service containers, and Compose-heavy jobs, use an EPAR image built with `scripts/guest/ubuntu/install-docker-browser.sh` or `scripts/guest/ubuntu/install-web-e2e.sh` so Docker Engine is installed directly inside the Linux guest. On an M3 Mac that means Ubuntu ARM64. Workflows must target self-hosted ARM labels such as:

```yaml
runs-on: [self-hosted, linux, ARM64, m3-ubuntu-24.04-docker]
```

Do not label these runners as `ubuntu-latest`. GitHub-hosted `ubuntu-latest` is a GitHub-managed image environment, and x64 assumptions may break on ARM64.

For Docker-heavy Linux CI, Docker-DinD is the recommended first path when the host already has a Docker runtime that supports privileged containers. It keeps workflow Docker resources inside a private inner daemon per runner instance, so existing Compose stacks with fixed project names or ports usually need fewer repository changes.

WSL on Windows x64 remains a good EPAR provider for workflows that need native x64 Linux Docker images. Tart on Apple Silicon remains useful when you specifically want VM-based runners on a Mac host, but the VM is ARM64 unless you opt into and validate Rosetta support. Workflows that depend on amd64-only images should target a distinct label such as a Docker-DinD label with verified amd64 emulation, `epar-tart-rosetta-amd64`, a WSL x64 label, or another x64 Linux runner label.

On Apple Silicon hosts, amd64 containers inside Docker-DinD depend on the host runtime's emulation support; validate `docker run --platform linux/amd64 alpine:3.20 uname -m` inside a running EPAR instance before routing amd64-only workflows there.

## OCI Clarification

OCI is a registry and artifact ecosystem, not a guarantee that an artifact can run as both a container and a VM. Docker container images and Tart VM images can both live in OCI-compatible registries, but their contents are different. Tart can pull Tart-created VM images from OCI registries; it cannot run arbitrary Docker container images as VMs.

## Browser Caveat

GitHub's upstream `install-google-chrome.sh` currently assumes x64 Linux Chrome/Chromium artifacts in important places. Docker/browser-enabled EPAR images therefore use that upstream script on x64 only. On ARM64, Ubuntu's `chromium-browser` package redirects to snap and can hang when the snap store is unreachable, so EPAR installs Playwright-managed Chromium and exposes it through `epar-browser`, `chromium`, and `chromium-browser` wrappers. Runtime validation exercises a real headless Chromium browser against a locally generated marker page when the Docker/browser feature marker is present. Network and TLS behavior remains the responsibility of each workflow.
