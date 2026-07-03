# Usage

## Prerequisites

Install:

- Go 1.22 or newer
- Git
- Tart, for the macOS provider
- WSL2, for the Windows provider

Packer, GitHub CLI, and sshpass are not required.

Runner registration also requires a GitHub App that can manage organization
self-hosted runners. See [GitHub App Setup](github-app.md).

## Configure

Copy the provider example to an ignored local config and fill in your GitHub App
settings. On macOS:

```bash
mkdir -p .local
cp configs/tart.example.yml .local/config.yml
```

On Windows with WSL:

```powershell
New-Item -ItemType Directory -Force .local
Copy-Item configs/wsl.example.yml .local/config.yml
```

EPAR looks for config in this order:

1. `--config <path>`
2. `EPAR_CONFIG`
3. `./.local/config.yml`
4. `~/.config/ephemeral-action-runner/config.yml`

Image-only commands do not require GitHub settings. Registration, GitHub status,
and GitHub cleanup do require `github.appId`, `github.organization`, and
`github.privateKeyPath`.

## Build The CLI

```bash
go build -o ./bin/ephemeral-action-runner ./cmd/ephemeral-action-runner
```

Use `./bin/ephemeral-action-runner` in the examples below, or put `bin` on PATH.

## Build The Runner Image

For Tart on macOS:

```bash
./bin/ephemeral-action-runner image update-upstream
./bin/ephemeral-action-runner image build --replace
```

The default output image is `epar-ubuntu-24-arm64`. This is a Tart VM image name,
not a file or folder in the repository. Confirm it with:

```bash
tart list
```

Build logs are written under `work/logs`:

```text
work/logs/epar-ubuntu-24-arm64.build.log
work/logs/epar-ubuntu-24-arm64.guest.log
```

For WSL on Windows, first create or export a clean Ubuntu 24.04 rootfs tar at
`work/images/ubuntu-24.04-clean.rootfs.tar`, then run the same image commands.
The default WSL output image is `work/images/epar-ubuntu-24-wsl.tar`, which EPAR
imports for disposable runner distros.

After changing only files under `scripts/guest/ubuntu`, refresh the existing
image without reinstalling packages:

```bash
./bin/ephemeral-action-runner image refresh-scripts
```

## Verify Two Runners

Register two ephemeral runners, verify both are online and idle, then clean up:

```bash
./bin/ephemeral-action-runner pool verify --instances 2 --register-only --cleanup
```

Expected healthy output includes progress for both generated instance names,
runtime validation, GitHub online/idle confirmation, cleanup, and log paths.

Local runtime validation inside each instance runs:

```bash
docker info
docker run --rm hello-world
chromium --headless --no-sandbox --dump-dom https://www.w3.org/
```

## Start A Foreground Pool

```bash
./bin/ephemeral-action-runner pool up --instances 2
```

`pool up` is a foreground supervisor. It starts the requested number of
ephemeral runners, monitors them, and keeps replacing completed runners. When a
GitHub Actions job is assigned to one of these ephemeral runners, the runner
exits after that one job finishes, whether the job succeeds or fails. EPAR then
stops/deletes that instance and creates a fresh replacement.
When you stop the supervisor with Ctrl-C, EPAR also cleans up the active
instances and matching GitHub runner records.

Use `--keep-on-exit` or `--replace-completed=false` only when debugging.

## Status And Cleanup

```bash
./bin/ephemeral-action-runner status
./bin/ephemeral-action-runner cleanup
```

Cleanup only touches local instances and GitHub runners whose names match
`pool.namePrefix`.

## Dry Run

Use `--dry-run` to inspect provider command construction without mutating local
instances:

```bash
./bin/ephemeral-action-runner pool verify --dry-run --instances 2
```
