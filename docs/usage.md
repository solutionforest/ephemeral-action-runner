# Usage

Use this page for normal EPAR tasks. Start with the host and provider you already have; the [documentation hub](README.md) links to the provider-specific guides.

## Prerequisites

| Task | Required tool or access |
| --- | --- |
| Run a source archive | Go 1.25 or newer, or Docker for the no-Go controller builder |
| Register or inspect GitHub runners | A GitHub App with organization self-hosted runner read/write permission |
| Docker Container | Docker Engine, Docker Desktop, or OrbStack with privileged Linux-container support |
| Docker Sandboxes | Docker and the `sbx` CLI with at least one diagnostic pass and zero failures; the wizard builds and imports the selected EPAR template |
| WSL | Native Windows, WSL2, and Docker when preparing the default WSL image |
| Tart | Native Apple Silicon macOS and Tart |

Get the source from the [EPAR releases page](https://github.com/solutionforest/ephemeral-action-runner/releases), extract the source archive, and work from that folder. You do not need Packer, GitHub CLI, or `sshpass`.

## Start a pool

On macOS, Linux, WSL, or Git Bash, run:

```bash
./start
```

On native Windows PowerShell, run:

```powershell
.\start.ps1
```

The wrapper uses local Go when available, otherwise it uses Docker to build and cache a native controller under `.local/bin`. See [Running EPAR Without Installing Go](advanced/no-go-install.md) for the fallback details. The equivalent direct source command is:

```bash
go run ./cmd/ephemeral-action-runner start
```

When `.local/config.yml` is absent and the terminal is interactive, `./start` launches the same first-run wizard as `init`. It asks for the GitHub App and an explicit runner group, shows every provider with its tooling and daemon prerequisite result, and refuses unavailable selections. Storage does not make a provider unavailable. Docker Container, Docker Sandboxes, and WSL share one Catthehacker image/profile and custom-script flow, followed by an informational physical-growth estimate and confirmation. The wizard writes the desired configuration first; direct `init` then exits, while embedded `./start` continues through the ordinary image/template provisioning and pool startup path. When `sbx` is installed, the wizard runs `sbx daemon start --detach` before Docker Sandboxes diagnostics so a stopped daemon does not require a manual retry.

See [Docker Sandboxes](providers/docker-sandboxes.md) for source profiles, capacity, local receipts, and platform validation status.

## Create or choose configuration

Create configuration without starting runners:

```bash
go run ./cmd/ephemeral-action-runner init
```

Pass a config path and an instance count through the wrapper:

```bash
./start --config .local/ci.yml --instances 2
```

Equivalent direct command:

```bash
go run ./cmd/ephemeral-action-runner start --config .local/ci.yml --instances 2
```

On Windows PowerShell, use backslash paths when that is clearer:

```powershell
.\start.ps1 --config .local\ci.yml --instances 2
go run ./cmd/ephemeral-action-runner start --config .local\ci.yml --instances 2
```

If `--instances` is omitted, `start`, `pool up`, and `pool verify` use `pool.instances` from the selected config. EPAR resolves configuration from `--config`, `EPAR_CONFIG`, `.local/config.yml`, then `~/.config/ephemeral-action-runner/config.yml`. Tracked files in `configs/` are examples; keep App values and key paths in an ignored local file. See [Configuration](configuration.md) for every setting and [Runner Group Security](runner-groups.md) before broadening repository access.

Storage-consuming commands fail before their provider side effects when an authoritative physical surface cannot retain `storage.minimumFree`. The one-invocation `--allow-insufficient-storage` option keeps all probes and warnings but permits only storage admission to continue; provider diagnostics, GitHub policy, ownership, lifecycle, and cleanup protections remain enforced. The option is available on `start`, `pool up`, `pool verify`, `image build`, and `image update-upstream`, including the equivalent `./start ...` wrapper forms.

Press `Ctrl-C` once to stop a foreground pool and wait for cleanup confirmation. Use `--keep-on-exit` only to retain owned resources for deliberate debugging.

## Verify before sending jobs

Verify one disposable runner without GitHub registration:

```bash
go run ./cmd/ephemeral-action-runner pool verify --instances 1 --cleanup
```

Verify registration and online/idle state:

```bash
go run ./cmd/ephemeral-action-runner pool verify --instances 2 --register-only --cleanup
```

`--cleanup` removes verification resources after the check. Docker Sandboxes uses its exact ownership records; legacy providers use the configured pool-name boundary. Use [Operations](operations.md) for the distinction and recovery guidance.

## Run, inspect, and clean up

`start` is the normal command because it checks the reusable image or template first. `pool up` is for a pool you have deliberately prepared:

```bash
go run ./cmd/ephemeral-action-runner pool up --instances 2
go run ./cmd/ephemeral-action-runner status
go run ./cmd/ephemeral-action-runner cleanup
```

Use `status --no-github` or `cleanup --no-github` when you intentionally need to skip GitHub runner status or deletion. `pool down` is an alias for cleanup.

For a command-construction preview on compatible providers, add `--dry-run`:

```bash
go run ./cmd/ephemeral-action-runner pool verify --dry-run --instances 1
```

Docker Sandboxes intentionally does not support dry-run instance creation because EPAR must read back the exact active template-cache identity. Use its admission and template checks instead.

## Target the right runner

GitHub matches every value in `runs-on` against a runner's labels. The smallest workflow selector is:

```yaml
runs-on: [self-hosted]
```

Add a provider or workload label to avoid routing work to the wrong environment:

```yaml
runs-on: [self-hosted, linux, epar-docker-container-catthehacker-ubuntu]
```

EPAR adds an `epar-host-<machine>` label by default. Use it only when a job must target one specific host. Give each independent pool in the same organization a unique `pool.namePrefix`; this is also its cleanup boundary.

## Common next tasks

- [Customize a runner image](image-build.md).
- [Configure Docker registry mirrors](advanced/docker-registry-mirrors.md).
- [Start EPAR after login on Windows](advanced/windows-startup.md) or [macOS](advanced/macos-startup.md).
- [Inspect logs, capacity, cleanup, and recovery](operations.md).
- [Diagnose a symptom](troubleshooting.md).
