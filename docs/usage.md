# Usage

Use this page for normal EPAR tasks. Start with the host and provider you already have; the [documentation hub](README.md) links to the provider-specific guides.

## Prerequisites

| Task | Required tool or access |
| --- | --- |
| Run a source archive | Go 1.25 or newer, or Docker for the no-Go controller builder |
| Register or inspect GitHub runners | A GitHub App with organization self-hosted runner read/write permission |
| Docker Container | Docker with privileged Linux-container support |
| Docker Sandboxes | Docker and the `sbx` CLI with at least one diagnostic pass and zero failures; the wizard builds and imports the selected EPAR template |
| WSL | Native Windows, WSL2, and Docker when preparing the default WSL image |
| Tart | Native Apple Silicon macOS and Tart |

Get the source from the [EPAR releases page](https://github.com/solutionforest/ephemeral-action-runner/releases), extract the source archive, and work from that folder. You do not need Packer, GitHub CLI, or `sshpass`.

EPAR works with any Docker installation that supports the selected provider.

## Start a pool

On macOS, Linux, WSL, Git Bash, or native Windows PowerShell, run:

```text
./start
```

PowerShell resolves `./start` to EPAR's internal Windows wrapper. Do not use bare `start` in PowerShell because that name is the `Start-Process` alias. The wrapper uses local Go when available, otherwise it uses Docker, to build a native controller under `.local/bin/<os>-<arch>/`; it then executes that project-local controller directly. See [Running EPAR Without Installing Go](advanced/no-go-install.md) for the fallback details.

When `.local/config.yml` is absent and the terminal is interactive, `./start` launches the same first-run wizard as `init`. It asks for the GitHub App and an explicit runner group. The runner-group list orders GitHub's Default group first, hides blocked groups and policy details initially, and lets you reveal either from the menu. The provider list shows every provider with its prerequisite status and refuses unavailable selections. Storage does not make a provider unavailable. Docker Container, Docker Sandboxes, and WSL share the Catthehacker image-selection and estimate flow, but Docker Sandboxes limits source selection to the `full-latest` and `act-latest` profiles proven to include its required private Docker daemon and runtime closure. Docker Container and WSL continue to offer specialized and custom tags. The wizard writes empty custom-install scripts, weekly updates at 07:00 local time, and host-trust overlay for applicable providers; edit the generated config afterward to change those advanced settings. Later option lists use `0` to go back, text prompts use `/back`, and a final provider-neutral review shows the applicable artifact estimate before one creation decision. Running `./start init` exits after writing, while an embedded first run continues through the ordinary image/template provisioning and pool startup path. When `sbx` is installed, the wizard runs `sbx daemon start --detach` before Docker Sandboxes diagnostics so a stopped daemon does not require a manual retry.

See [Docker Sandboxes](providers/docker-sandboxes.md) for source profiles, capacity, local receipts, and platform validation status.

## Create or choose configuration

Create configuration without starting runners:

```bash
./start init
```

Pass a config path and an instance count through the wrapper:

```bash
./start --config .local/ci.yml --instances 2
```

On Windows PowerShell, backslash paths may be clearer while the command remains the same:

```powershell
./start --config .local\ci.yml --instances 2
```

If `--instances` is omitted, `start`, `pool up`, and `pool verify` use `pool.instances` from the selected config. EPAR resolves configuration from `--config`, `EPAR_CONFIG`, `.local/config.yml`, then `~/.config/ephemeral-action-runner/config.yml`. Tracked files in `configs/` are examples; keep App values and key paths in an ignored local file. See [Configuration](configuration.md) for every setting and [Runner Group Security](runner-groups.md) before broadening repository access.

Multiple configs from the same checkout may run concurrently when they use different canonical config paths, unique `pool.namePrefix` values, unique workflow-routing labels, and preferably separate log directories. EPAR rejects a second controller for the same config path or prefix before provisioning or cleanup can mutate provider state. Config-scoped BuildKit builders and transient workspaces keep divergent registry, trust, and cache settings isolated.

Storage-consuming commands fail before their provider side effects when any authoritative capacity domain cannot retain `storage.minimumFree` after the operation's largest phase-overlapping allocation. The checkout filesystem is not assumed to contain Docker, Docker Sandboxes, WSL, or Tart storage. Inspect the same calculation with `./start storage status --operation <name>`; preserve `--config` and `--project-root` for non-default configurations. The one-invocation `--allow-insufficient-storage` option keeps all probes and warnings but permits only storage admission to continue; provider diagnostics, GitHub policy, ownership, lifecycle, and cleanup protections remain enforced. The option is available on `start`, `pool up`, `pool verify`, `image update`, `image build`, and `image update-upstream`, including the equivalent `./start ...` wrapper forms.

Each normal start also reconciles interrupted exact-owned work and retires unreferenced superseded artifacts after replacement readback. Use `./start storage status` to inspect the result. `./start storage prune --legacy` previews prefix-era resources, which remain manual and require the displayed plan hash before execution.

Press `Ctrl-C` once to stop a foreground pool, then wait for cleanup to finish before closing the terminal. Use `--keep-on-exit` only to retain owned resources for deliberate debugging.

## Update runner artifacts

By default, EPAR checks mutable source-image tags and `runnerVersion: latest` weekly at 07:00 local time. The first-run wizard writes this default; edit `image.updateFrequency` and `image.updateTime` afterward to choose daily, every two weeks, monthly, manual, or another local time. Local image settings, script or certificate content, platform, EPAR assets, and missing or corrupt artifacts always apply on the next start without waiting for the schedule.

Force an immediate remote check without forcing a rebuild:

```bash
./start image update
```

Manual policy means this command triggers remote checks. `./start image build` remains the force-build path. A running ephemeral pool checks when due, drains only after busy jobs finish, activates the verified replacement, and restores pool capacity; persistent runners record the update for the next process start.

## Verify before sending jobs

Verify one disposable runner without GitHub registration:

```bash
./start pool verify --instances 1 --cleanup
```

Verify registration and online/idle state:

```bash
./start pool verify --instances 2 --register-only --cleanup
```

`--cleanup` removes verification resources after the check. Docker Sandboxes uses its exact ownership records; legacy providers use the configured pool-name boundary. Use [Operations](operations.md) for the distinction and recovery guidance.

## Run, inspect, and clean up

`start` is the normal command because it checks the reusable image or template first. `pool up` is for a pool you have deliberately prepared:

```bash
./start pool up --instances 2
./start status
./start cleanup
```

Use `status --no-github` or `cleanup --no-github` when you intentionally need to skip GitHub runner status or deletion. `pool down` is an alias for cleanup.

For a command-construction preview on compatible providers, add `--dry-run`:

```bash
./start pool verify --dry-run --instances 1
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
