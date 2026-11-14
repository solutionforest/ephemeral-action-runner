# EPAR documentation

Use these guides after the short [README quick start](../README.md). Docker Sandboxes is the primary provider for Linux, macOS, and Windows hosts when its capability checks pass; use Tart macOS preview for native Apple Silicon mobile builds and compatibility guides for other provider requirements.

## Start and configure

- [Usage](usage.md): start, initialize, verify, inspect, clean up, labels, and dry runs.
- [GitHub App setup](github-app.md): create the App EPAR uses for short-lived registration tokens.
- [Runner group security](runner-groups.md): restrict which repositories can route work to your runners.
- [Configuration](configuration.md): edit local configuration and provider defaults.

## Choose a provider

- [Docker Sandboxes](providers/docker-sandboxes.md): the primary provider with dedicated microVM runners and guided template provisioning on Linux, macOS, and Windows hosts.

Four provider identities remain accepted at runtime, and all four have onboarding contributions. The first-run wizard shows only Docker Sandboxes initially; choose `C. Show compatibility providers` to reveal [Docker Container](providers/docker-container.md) and [WSL2](providers/wsl.md) for existing compatibility deployments. [Tart macOS preview](providers/tart.md) is option `4`, requires Apple Silicon, and does not support Docker containers; Ubuntu configurations retain runtime compatibility.

## Operate and maintain

- [Operations](operations.md): supervision, capacity, cleanup, recovery, and maintenance.
- [Troubleshooting](troubleshooting.md): symptom-first diagnostics.
- [Logging](logging.md), [Storage](storage.md), and [Generated files and recovery](generated-files.md): retention, capacity, directory ownership, safe deletion, exact reset, and cleanup boundaries.
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
