# Ephemeral Action Runner

Ephemeral Action Runner (EPAR) keeps a warm pool of disposable GitHub Actions self-hosted runners on your own machines.

It is built for teams that want fast self-hosted Linux runners without keeping long-lived runner VMs around. EPAR creates an instance, registers it as an ephemeral GitHub runner, lets GitHub run one job on it, deletes the instance after that job, and creates a replacement.

```mermaid
flowchart LR
  Workflow["GitHub Actions job"] --> Runner["Ephemeral runner"]
  EPAR["EPAR supervisor"] --> Provider["Local provider<br/>Tart or WSL2"]
  Provider --> Runner
  Runner --> Job["One job runs"]
  Job --> Delete["Delete instance"]
  Delete --> Replace["Create replacement"]
  Replace --> Runner
```

## Why Use EPAR

- **Disposable runners:** every runner is expected to handle one job and then disappear.
- **Warm pool:** `pool up` keeps ready runners online so jobs do not wait for a full image build.
- **Use spare hosts:** turn a supported Mac or Windows machine into a pool of disposable Linux GitHub runners.
- **Image control:** the default image is runner-only, and extra tooling is added with explicit install scripts.
- **GitHub App auth:** the host uses a GitHub App to request short-lived runner registration tokens.

## Supported Hosts

Use the provider that matches the machine you have available. EPAR is meant to make spare supported hosts useful as GitHub self-hosted runner capacity, not to make users compare providers in the abstract.

| Machine you have | EPAR provider | Runner architecture | Notes |
| --- | --- | --- | --- |
| Apple Silicon macOS | Tart | Ubuntu ARM64 | Good for Linux jobs that can run on ARM64. Workflows needing amd64-only Docker images should use a different label or handle cross-arch in the workflow. |
| Windows with WSL2 | WSL2 | Ubuntu x64 | Good for Linux jobs and Docker workflows that pull `linux/amd64` images. Use for trusted internal jobs unless your environment has accepted the WSL isolation model. |

Future providers can fit the same model: if EPAR supports the machine, that machine can contribute disposable runner capacity with its own labels.

## Image Choice

EPAR does not force Docker, browsers, Node, or project tools into the public default image.

| Image style | Config | Includes |
| --- | --- | --- |
| Runner-only base | `configs/tart.example.yml` or `configs/wsl.example.yml` | GitHub Actions runner and minimal runtime dependencies |
| Docker/browser | add `scripts/guest/ubuntu/install-docker-browser.sh` to `image.customInstallScripts` | Docker Engine, Docker CLI, Compose v2, Buildx, and a Chromium-compatible browser |
| Web/E2E | `configs/tart.web-e2e.example.yml` or `configs/wsl.web-e2e.example.yml` | Docker/browser plus Node.js/npm, zip, rsync, and mysql-client |
| Custom | add your own script path to `image.customInstallScripts` | Whatever your script installs inside Ubuntu |

Example:

```yaml
image:
  customInstallScripts:
    - scripts/guest/ubuntu/install-web-e2e.sh
    - examples/custom-install/install-extra-apt-tools.sh
```

## Quick Start

1. Build the CLI:

   ```bash
   go build -o ./bin/ephemeral-action-runner ./cmd/ephemeral-action-runner
   ```

2. Create a GitHub App that can manage organization self-hosted runners. See [docs/github-app.md](docs/github-app.md).

3. Copy one example config into `.local/config.yml` and fill in the GitHub App values.

   macOS with Tart:

   ```bash
   mkdir -p .local
   cp configs/tart.example.yml .local/config.yml
   ```

   Windows with WSL2:

   ```powershell
   New-Item -ItemType Directory -Force .local
   Copy-Item configs/wsl.example.yml .local/config.yml
   ```

4. For WSL, export a clean Ubuntu 24.04 rootfs once:

   ```powershell
   New-Item -ItemType Directory -Force work/images
   wsl --install -d Ubuntu-24.04 --no-launch
   wsl --export Ubuntu-24.04 work/images/ubuntu-24.04-clean.rootfs.tar
   ```

5. Build the runner image:

   ```bash
   ./bin/ephemeral-action-runner image build --replace
   ```

   If your selected install scripts use EPAR's Docker/browser or web/E2E scripts, first run:

   ```bash
   ./bin/ephemeral-action-runner image update-upstream
   ```

6. Verify two registered runners and clean them up:

   ```bash
   ./bin/ephemeral-action-runner pool verify --instances 2 --register-only --cleanup
   ```

7. Start a foreground pool:

   ```bash
   ./bin/ephemeral-action-runner pool up --instances 2
   ```

## How The Pool Behaves

`pool up` is intentionally foreground. Keep it running while you want runners available. Stop it with Ctrl-C to clean up matching local instances and GitHub runner records.

```mermaid
sequenceDiagram
  participant E as EPAR
  participant P as Provider
  participant R as Runner instance
  participant G as GitHub
  E->>P: clone and start instance
  P-->>R: instance boots
  E->>R: validate runtime
  E->>G: request registration token
  E->>R: configure runner as ephemeral
  R-->>G: runner comes online
  G->>R: assign one workflow job
  R-->>G: runner exits after job
  E->>P: delete instance
  E->>G: remove stale runner record if needed
  E->>P: create replacement
```

Cleanup is prefix-safe: EPAR only touches instances and GitHub runners whose names match `pool.namePrefix`.

## Documentation

Start with:

- [Usage](docs/usage.md): commands for setup, image builds, verification, and pool operation.
- [GitHub App Setup](docs/github-app.md): minimum GitHub App permissions and config fields.
- [Image Build](docs/image-build.md): runner-only base images, install scripts, web/E2E images, and customization.

Provider details:

- [Tart Provider](docs/providers/tart.md): Apple Silicon macOS and Ubuntu ARM64.
- [WSL Provider](docs/providers/wsl.md): Windows WSL2, rootfs tar images, and WSL caveats.

Operational context:

- [Design](docs/design.md): lifecycle and liveness model.
- [Operations](docs/operations.md): logs, cleanup, and troubleshooting.
- [Security](docs/security.md): trust boundaries and secret handling.
- [Background](docs/background.md): why Linux guests are preferred for Docker and Compose-heavy jobs.
- [Adding A Provider](docs/providers/adding-provider.md): provider interface expectations.

Tracked configs are examples only. Put real GitHub App IDs, private key paths, and local runner settings in `.local/config.yml`, `configs/*.local.yml`, or `~/.config/ephemeral-action-runner/config.yml`; those paths are not intended for Git.
