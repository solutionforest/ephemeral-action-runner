# Tart Provider

The Tart provider targets Apple Silicon macOS hosts and can run Ubuntu ARM64 or macOS ARM64 VMs.

EPAR currently validates the Ubuntu path:

- clone a reusable Tart image
- start the VM headless
- use the Tart guest agent for `exec` and IP discovery
- validate the base GitHub Actions runner runtime
- register an ephemeral GitHub runner from the host
- delete the VM after the runner exits

Use `configs/tart.example.yml` for the runner-only image or `configs/tart.web-e2e.example.yml` for the opt-in web/E2E install script. Tart on Apple Silicon is ARM64, so workflows that require amd64-only Docker images should avoid the ARM64 label or handle cross-architecture execution in the workflow.

When Docker/browser support is selected on ARM64, EPAR exposes a Chromium-compatible browser through `epar-browser`, `chromium`, and `chromium-browser`; it is not guaranteed to be Google Chrome.

The default network mode is Tart NAT. `softnet` is accepted by the provider, but it can require host-side privileges.
