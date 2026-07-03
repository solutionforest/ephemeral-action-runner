# Tart Provider

The Tart provider targets Apple Silicon macOS hosts and can run Ubuntu ARM64 or
macOS ARM64 VMs.

EPAR currently validates the Ubuntu path:

- clone a reusable Tart image
- start the VM headless
- use the Tart guest agent for `exec` and IP discovery
- validate Docker and a Chromium-compatible headless browser
- register an ephemeral GitHub runner from the host
- delete the VM after the runner exits

The default network mode is Tart NAT. `softnet` is accepted by the provider, but
it can require host-side privileges.
