# Tart Provider (Experimental)

Tart runs each disposable EPAR runner in an Ubuntu ARM64 VM on Apple Silicon macOS. EPAR's Tart integration is experimental.

## When To Use It

Choose Tart only when you have Apple Silicon macOS and specifically want Linux ARM64 VMs. Use Docker Container or WSL/x64 Linux when native amd64 workflow compatibility is more important.

## Support Status

EPAR supports Ubuntu ARM64 guests, not macOS guests. The default source is a basic Ubuntu VM image, not a GitHub-hosted runner image; it does not include the broad language, browser, Docker, and CLI inventory associated with `actions/runner-images`.

## Prerequisites

- Apple Silicon macOS and a working `tart` CLI.
- A bootable Ubuntu ARM64 Tart source image.
- Enough local VM storage for a reusable image and active disposable VMs.

## Minimal Configuration

Start with [`configs/tart.example.yml`](../../configs/tart.example.yml):

```yaml
image:
  sourceImage: ghcr.io/cirruslabs/ubuntu:latest
  outputImage: epar-ubuntu-24-arm64
  updateFrequency: weekly
  updateTime: "07:00"

provider:
  type: tart
  sourceImage: epar-ubuntu-24-arm64
  network: default
```

Use a distinct image and label when adding tools through `image.customInstallScripts`. If workflows need a GitHub-runner-like environment, build and maintain your own bootable Ubuntu Tart image; EPAR does not convert Catthehacker Docker images into Tart VMs.

## Normal Workflow

1. Create the configuration and run `./start` to build the reusable Tart image and start the pool.
2. Target the ARM64 Tart label in workflows.
3. Use `pool verify` before routing a new workload to the provider.

Tart clones the reusable image, starts the VM headless, uses the guest agent for command execution and IP discovery, and removes the VM after the ephemeral runner exits.

## Limitations

- This provider is experimental. It uses a per-runner VM boundary, but workflows still control the guest and any secrets or services exposed to the job.
- The default image is runner-only. Add only the dependencies your workflows need, or maintain a fuller source image yourself.
- `provider.network: softnet` may require additional host privileges; NAT is the default.
- Rosetta can translate some Linux amd64 user-space workloads in an ARM64 guest, but it does not turn the VM into an x64 VM or guarantee every amd64 workload.

## Verification

```bash
ephemeral-action-runner pool verify --instances 1 --cleanup
```

For the optional Rosetta experiment, use `configs/tart.web-e2e.example.yml` or set a distinct `provider.rosettaTag`. Verify a real container execution before routing amd64 workflows:

```bash
docker run --rm --platform linux/amd64 alpine:3.20 uname -m
```

Run that command inside the Tart guest; expected output is `x86_64`.

## Troubleshooting

See [Troubleshooting](../troubleshooting.md) for platform and runtime failures. Keep Rosetta-capable runners behind a dedicated label so workflows opt in deliberately.
