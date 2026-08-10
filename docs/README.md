# EPAR documentation

Use these guides after the short [README quick start](../README.md). Docker Sandboxes is the primary provider for Linux, macOS, and Windows hosts when its capability checks pass; open a compatibility guide only when an existing configuration needs that provider.

## Start and configure

- [Usage](usage.md): start, initialize, verify, inspect, clean up, labels, and dry runs.
- [GitHub App setup](github-app.md): create the App EPAR uses for short-lived registration tokens.
- [Runner group security](runner-groups.md): restrict which repositories can route work to your runners.
- [Configuration](configuration.md): edit local configuration and provider defaults.

## Choose a provider

- [Docker Sandboxes](providers/docker-sandboxes.md): the primary provider with dedicated microVM runners and guided template provisioning on Linux, macOS, and Windows hosts.

Four provider identities remain accepted at runtime, while three are onboarding-capable. The first-run wizard shows only Docker Sandboxes initially; choose `C. Show compatibility providers` to reveal [Docker Container](providers/docker-container.md) and [WSL2](providers/wsl.md) for existing compatibility deployments. [Tart](providers/tart.md) is retired and has no onboarding path; its guide remains for existing configurations and exact runtime/cleanup compatibility.

## Operate and maintain

- [Operations](operations.md): supervision, capacity, cleanup, recovery, and maintenance.
- [Troubleshooting](troubleshooting.md): symptom-first diagnostics.
- [Logging](logging.md) and [Storage](storage.md): retention, capacity, and exact cleanup boundaries.
- [Image customization](image-build.md): build layers and custom install scripts.
- [Docker Sandboxes templates](advanced/docker-sandboxes-template.md): build, verify, import, size, and retain exact templates.
- [Cross-architecture containers](advanced/cross-architecture-containers.md): image platforms, emulation, labels, and verification.
- [Docker registry mirrors](advanced/docker-registry-mirrors.md): an optional pull-time optimization.
- [Windows startup](advanced/windows-startup.md), [macOS startup](advanced/macos-startup.md), and [no-Go startup](advanced/no-go-install.md): host-specific launch help.

## Safety and support

- [Security](security.md): Docker Sandboxes isolation, compatibility-provider caveats, secrets, and private vulnerability reporting.
- [Support](../SUPPORT.md): information to collect before opening an issue.

## Contribute

Read [Contributing](../CONTRIBUTING.md), then use the [developer documentation](development/README.md) for architecture, extension contracts, provider work, and the live core-runner canary.
