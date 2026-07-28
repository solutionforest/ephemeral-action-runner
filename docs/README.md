# EPAR documentation

Use these guides after the short [README quick start](../README.md). Start with the task you need to complete, then open a provider guide only when choosing or changing the runner environment.

## Start and configure

- [Usage](usage.md): start, initialize, verify, inspect, clean up, labels, and dry runs.
- [GitHub App setup](github-app.md): create the App EPAR uses for short-lived registration tokens.
- [Runner group security](runner-groups.md): restrict which repositories can route work to your runners.
- [Configuration](configuration.md): edit local configuration and provider defaults.

## Choose a provider

- [Docker Container](providers/docker-container.md): disposable containers with a private Docker daemon.
- [Docker Sandboxes](providers/docker-sandboxes.md): dedicated microVM runners and the required prebuilt template.
- [WSL](providers/wsl.md): disposable Windows WSL2 runners.
- [Tart](providers/tart.md): experimental Apple Silicon ARM64 Linux VMs.

## Operate and maintain

- [Operations](operations.md): supervision, capacity, cleanup, recovery, and maintenance.
- [Troubleshooting](troubleshooting.md): symptom-first diagnostics.
- [Logging](logging.md) and [Storage](storage.md): retention, capacity, and exact cleanup boundaries.
- [Image customization](image-build.md): build layers and custom install scripts.
- [Docker Sandboxes templates](advanced/docker-sandboxes-template.md): build, review, load, size, and retain pinned templates.
- [Cross-architecture containers](advanced/cross-architecture-containers.md): image platforms, emulation, labels, and verification.
- [Docker registry mirrors](advanced/docker-registry-mirrors.md): an optional pull-time optimization.
- [Windows startup](advanced/windows-startup.md), [macOS startup](advanced/macos-startup.md), and [no-Go startup](advanced/no-go-install.md): host-specific launch help.

## Safety and support

- [Security](security.md): trusted-job boundary, secrets, provider caveats, and private vulnerability reporting.
- [Support](../SUPPORT.md): information to collect before opening an issue.

## Contribute

Read [Contributing](../CONTRIBUTING.md), then use the [developer documentation](development/README.md) for architecture, extension contracts, provider work, and the live core-runner canary.
