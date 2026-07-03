# Usage

This page is the operational walkthrough. Start with the supported host you already have:

- Tart on Apple Silicon macOS
- WSL2 on Windows

## Prerequisites

Install the host tools you need:

| Required for | Tool |
| --- | --- |
| All hosts | Go 1.22 or newer, Git |
| macOS provider | Tart |
| Windows provider | WSL2 |
| Runner registration | GitHub App with organization self-hosted runner read/write permission |

Packer, GitHub CLI, and sshpass are not required.

Set up the GitHub App before registering runners. The image build command can run without GitHub credentials, but `pool verify --register-only`, `pool up`, `status`, and GitHub cleanup need the app settings. See [GitHub App Setup](github-app.md).

## Configure

Copy one example config into `.local/config.yml`, then edit the GitHub App fields and any labels you want to expose to workflows.

| Host and image | Example config |
| --- | --- |
| macOS Tart, runner-only | `configs/tart.example.yml` |
| macOS Tart, web/E2E | `configs/tart.web-e2e.example.yml` |
| Windows WSL2, runner-only | `configs/wsl.example.yml` |
| Windows WSL2, web/E2E | `configs/wsl.web-e2e.example.yml` |

macOS:

```bash
mkdir -p .local
cp configs/tart.example.yml .local/config.yml
```

Windows:

```powershell
New-Item -ItemType Directory -Force .local
Copy-Item configs/wsl.example.yml .local/config.yml
```

EPAR looks for config in this order:

1. `--config <path>`
2. `EPAR_CONFIG`
3. `./.local/config.yml`
4. `~/.config/ephemeral-action-runner/config.yml`

Tracked configs are examples only. Keep real app IDs and private key paths in an ignored config file.

## Build The CLI

```bash
go build -o ./bin/ephemeral-action-runner ./cmd/ephemeral-action-runner
```

Use `./bin/ephemeral-action-runner` in the examples below, or put `bin` on `PATH`.

## Prepare A WSL Source Tar

Skip this section for Tart.

The WSL provider builds reusable rootfs tar images. Create the clean Ubuntu 24.04 source tar once:

```powershell
New-Item -ItemType Directory -Force work/images
wsl --install -d Ubuntu-24.04 --no-launch
wsl --export Ubuntu-24.04 work/images/ubuntu-24.04-clean.rootfs.tar
```

After that, EPAR imports disposable temporary distros for image builds and pool instances.

## Build The Runner Image

Runner-only images do not need the upstream `actions/runner-images` checkout:

```bash
./bin/ephemeral-action-runner image build --replace
```

If `image.customInstallScripts` includes EPAR's Docker/browser or web/E2E scripts, update the pinned upstream checkout first:

```bash
./bin/ephemeral-action-runner image update-upstream
./bin/ephemeral-action-runner image build --replace
```

Tart output is a local Tart image name, such as `epar-ubuntu-24-arm64`. Confirm it with:

```bash
tart list
```

WSL output is a rootfs tar path, such as:

```text
work/images/epar-ubuntu-24-wsl.tar
```

Build logs are written under `work/logs`.

## Customize The Image

The public default image is runner-only. Add tooling through `image.customInstallScripts`:

```yaml
image:
  customInstallScripts:
    - scripts/guest/ubuntu/install-web-e2e.sh
    - examples/custom-install/install-extra-apt-tools.sh
```

Scripts run as root during image build, after the GitHub Actions runner is installed and before validation/finalization. See [Image Build](image-build.md) for the full layering model and custom script guidance.

## Verify Runners

For a local runtime check without GitHub registration:

```bash
./bin/ephemeral-action-runner pool verify --instances 1 --cleanup
```

For a full registration check:

```bash
./bin/ephemeral-action-runner pool verify --instances 2 --register-only --cleanup
```

Healthy output should show each generated instance name moving through:

1. clone
2. start
3. runtime validation
4. GitHub online/idle, when registration is enabled
5. cleanup

Runtime validation always checks the base runner files and runner user. Images with optional feature markers also validate those features:

- Docker/browser images validate Docker, Compose v2, Buildx, `hello-world`, and a headless browser.
- Web/E2E images also validate `node`, `npm`, `zip`, `unzip`, `tar`, `rsync`, and `mysql`.

## Run A Foreground Pool

```bash
./bin/ephemeral-action-runner pool up --instances 2
```

`pool up` keeps the requested number of runners online. Each GitHub ephemeral runner exits after one job. EPAR then retires that instance and creates a fresh replacement.

Stop the supervisor with Ctrl-C. By default, EPAR cleans up active instances and matching GitHub runner records before it exits.

Use these flags only for debugging:

- `--keep-on-exit`: leave instances running when the supervisor exits.
- `--replace-completed=false`: do not create replacements after completed jobs.

## Status And Cleanup

```bash
./bin/ephemeral-action-runner status
./bin/ephemeral-action-runner cleanup
```

Cleanup only touches local instances and GitHub runners whose names match `pool.namePrefix`.

## Dry Run

Use `--dry-run` to inspect provider command construction without mutating local instances:

```bash
./bin/ephemeral-action-runner pool verify --dry-run --instances 2
```
