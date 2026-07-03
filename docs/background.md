# Background

The original macOS-runner direction used macOS VM images because those tools are commonly found when searching for Mac self-hosted runners. That is a poor fit for Docker Compose jobs inside the guest: Docker Desktop and OrbStack on macOS rely on their own Linux VM, so using them inside a macOS Tart VM becomes nested virtualization.

For Docker container actions, service containers, and Compose-heavy jobs, use an EPAR image built with `scripts/guest/ubuntu/install-docker-browser.sh` or `scripts/guest/ubuntu/install-web-e2e.sh` so Docker Engine is installed directly inside the Linux guest. On an M3 Mac that means Ubuntu ARM64. Workflows must target self-hosted ARM labels such as:

```yaml
runs-on: [self-hosted, linux, ARM64, m3-ubuntu-24.04-docker]
```

Do not label these runners as `ubuntu-latest`. GitHub-hosted `ubuntu-latest` is a GitHub-managed image environment, and x64 assumptions may break on ARM64.

WSL on Windows is the preferred EPAR provider for Docker-enabled workflows that need x64 Linux Docker images, including `linux/amd64` application runtime images. Tart on Apple Silicon remains ARM64. Workflows that depend on amd64-only images should target the WSL x64 label or handle cross-architecture execution explicitly in the workflow.

## OCI Clarification

OCI is a registry and artifact ecosystem, not a guarantee that an artifact can run as both a container and a VM. Docker container images and Tart VM images can both live in OCI-compatible registries, but their contents are different. Tart can pull Tart-created VM images from OCI registries; it cannot run arbitrary Docker container images as VMs.

## Browser Caveat

GitHub's upstream `install-google-chrome.sh` currently assumes x64 Linux Chrome/Chromium artifacts in important places. Docker/browser-enabled EPAR images therefore use that upstream script on x64 only. On ARM64, Ubuntu's `chromium-browser` package redirects to snap and can hang when the snap store is unreachable, so EPAR installs Playwright-managed Chromium and exposes it through `epar-browser`, `chromium`, and `chromium-browser` wrappers. Runtime validation exercises a real headless Chromium browser against `https://www.w3.org/` when the Docker/browser feature marker is present.
