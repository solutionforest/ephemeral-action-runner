# Background

The original macOS-runner direction used macOS VM images because those tools are commonly found when searching for Mac self-hosted runners. That is a poor fit for Docker Compose jobs inside the guest: Docker Desktop and OrbStack on macOS rely on their own Linux VM, so using them inside a macOS Tart VM becomes nested virtualization.

For Docker container actions, service containers, and Compose-heavy jobs, the runner should be a Linux VM with Docker Engine installed directly. On an M3 Mac that means Ubuntu ARM64. Workflows must target self-hosted ARM labels such as:

```yaml
runs-on: [self-hosted, linux, ARM64, m3-ubuntu-24.04-docker]
```

Do not label these runners as `ubuntu-latest`. GitHub-hosted `ubuntu-latest` is a GitHub-managed image environment, and x64 assumptions may break on ARM64.

## OCI Clarification

OCI is a registry and artifact ecosystem, not a guarantee that an artifact can run as both a container and a VM. Docker container images and Tart VM images can both live in OCI-compatible registries, but their contents are different. Tart can pull Tart-created VM images from OCI registries; it cannot run arbitrary Docker container images as VMs.

## Browser Caveat

GitHub's upstream `install-google-chrome.sh` currently assumes x64 Linux Chrome/Chromium artifacts in important places. This project therefore uses that upstream script on x64 only. On ARM64, Ubuntu's `chromium-browser` package redirects to snap and can hang when the snap store is unreachable, so the image build installs Playwright-managed Chromium and exposes it through `epar-browser`, `chromium`, and `chromium-browser` wrappers. The runtime validation still exercises a real headless Chromium browser against `https://www.w3.org/`.
