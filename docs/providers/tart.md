# Tart Provider

The Tart provider targets Apple Silicon macOS hosts and can run Ubuntu ARM64 or macOS ARM64 VMs.

EPAR currently validates the Ubuntu path:

- clone a reusable Tart image
- start the VM headless
- use the Tart guest agent for `exec` and IP discovery
- validate the base GitHub Actions runner runtime
- register an ephemeral GitHub runner from the host
- delete the VM after the runner exits

Use `configs/tart.example.yml` for the runner-only image or `configs/tart.web-e2e.example.yml` for the opt-in web/E2E install script.

When Docker/browser support is selected on ARM64, EPAR exposes a Chromium-compatible browser through `epar-browser`, `chromium`, and `chromium-browser`; it is not guaranteed to be Google Chrome.

The default network mode is Tart NAT. `softnet` is accepted by the provider, but it can require host-side privileges.

## Rosetta For Linux Amd64 Containers

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

This is for Linux amd64 user-space containers. It is not nested virtualization and it does not make the Ubuntu VM an x64 VM. For native amd64 performance and compatibility, use a Windows WSL x64 provider or another x64 Linux host. For Tart Rosetta-capable runners, expose a distinct label such as `epar-tart-rosetta-amd64` so workflows can opt into the behavior explicitly.
