# Configuration

EPAR stores local settings in `.local/config.yml` by default. When the file does not exist, the first-run wizard always displays Docker Container, Docker Sandboxes, WSL2, and experimental Tart with a prerequisite result, while refusing unavailable selections. Docker Sandboxes is the recommended Enter default without an operating-system allowlist when Docker is ready, the exact supported `sbx` version is installed, the host architecture maps to an available Linux guest template, and `sbx diagnose --output json` reports at least one pass and zero failures. Diagnostic warnings remain visible but do not disable the choice. The wizard performs read-only local discovery of an active lock-selected EPAR template, its full local image identity and platform, and the current host-global Balanced-policy fingerprint before writing configuration. New Docker Sandboxes configs set `networkBaseline: open`, which adds an EPAR-owned sandbox-scoped `allow **` rule for public HTTP/HTTPS compatibility plus deny-wins host-alias guardrails without changing the host-global policy. The rolling Catthehacker source-channel labels do not refresh automatically; the source lock is an approved snapshot.

## Pre-release provider rename

The Docker Container provider has an intentional pre-release naming migration. EPAR does not retain a compatibility alias for the pre-rename provider identifier.

Before editing an existing configuration:

1. Stop the current controller.
2. Use the previous release to remove its managed runners and runner containers.
3. Verify that no prior managed GitHub runner records or host containers remain.
4. Update `provider.type`, `image.outputImage`, `provider.sourceImage`, the runner label, and pool prefix to the `docker-container` values in the current example configuration.
5. Rebuild the renamed image, then start the renamed provider.

The old and renamed controllers must not overlap: their provider labels and controller-lock identity differ, so concurrent operation can create duplicate runners or leave resources outside the active controller's ownership boundary.

Use `.local/config.yml` for real GitHub App values, local paths, labels, and runner counts. Tracked files under `configs/` are examples.

## Config Lookup

EPAR looks for config in this order:

1. `--config <path>`
2. `EPAR_CONFIG`
3. `./.local/config.yml`
4. `~/.config/ephemeral-action-runner/config.yml`

## Sections

| Section | Purpose |
| --- | --- |
| `github` | GitHub App ID, organization, private key path, and optional GitHub API/web URLs. |
| `provider` | How EPAR creates disposable runners: `docker-container`, `docker-sandboxes`, `wsl`, or `tart`. |
| `image` | Source image/rootfs, output image, runner version, and optional install scripts. |
| `pool` | Runner count, instance name prefix, and replacement retry policy. |
| `storage` | Provider-neutral free-space reserve, grace period, retained generations, and cache limits. |
| `logging` | Manager and transcript sinks, formats, rotation, retention, and log directory. |
| `runner` | GitHub Actions labels, runner group, default-label policy, and whether to add the host-machine label. |
| `security` | Runner-group policy requirements checked before runner registration. |
| `docker` | Optional Docker registry mirrors and private Docker daemon proxy settings. |
| `dockerSandboxes` | Pinned template, policy, staging, capacity, and resource settings for Docker Sandboxes. |
| `timeouts` | Boot, GitHub online, and command timeout values in seconds. |

## Common Edits

Change how many runners stay online:

```yaml
pool:
  instances: 2
```

Set a unique instance name prefix for each machine/config in the same GitHub organization:

```yaml
pool:
  namePrefix: buildbox01-a4f9c2
```

The first-run wizard derives `pool.namePrefix` from the sanitized machine name and a six-character cryptographically random hexadecimal suffix, making it recognizable while reducing collision risk when multiple EPAR configurations are created on the same host. `pool.namePrefix` is the literal beginning of every generated local instance and GitHub runner name. It must be 2-40 characters; the shared name generator appends `-YYYYMMDD-HHMMSS-###`, so a generated name is at most 60 characters and remains within GitHub's runner-name limit and the Docker Sandboxes 63-character limit. Do not reuse the same prefix on different machines or for separate EPAR supervisors in the same organization. Legacy-provider cleanup uses this boundary, while Docker Sandboxes binds and removes exact identities through its ledger. A shared prefix can therefore let one legacy supervisor delete another machine's GitHub runner record and makes every provider's ownership ambiguous.

Configure replacement retry behavior after a transient GitHub or network outage:

```yaml
pool:
  replacementRetryInitialSeconds: 15
  replacementRetryMaxSeconds: 1800
  replacementRetryMultiplier: 2
  replacementRetryJitterPercent: 20
```

These values default to `15`, `1800`, `2`, and `20`, so existing configurations remain valid without changes. `replacementRetryInitialSeconds` must be positive, `replacementRetryMaxSeconds` must be at least the initial delay, `replacementRetryMultiplier` must be at least `1`, and `replacementRetryJitterPercent` must be from `0` through `100`.

Configure capacity and bounded retention:

```yaml
storage:
  minimumFree: 20GiB
  gracePeriod: 168h
  keepPrevious: 0
  automaticHousekeeping: conservative
  buildCacheLimit: 64GiB
  goCacheLimit: 10GiB
```

Existing configurations use these defaults without requiring a new section. See [Storage](storage.md) for reporting and exact cleanup behavior.

The supervisor backs off only replacement allocation after transient network errors and GitHub HTTP `429` or `5xx` responses. The nominal delay doubles from 15 seconds to a 30-minute cap with the configured jitter; a longer GitHub `Retry-After` response takes precedence. Authentication and deterministic configuration failures remain fail-fast after safe rollback. Initial `pool up` startup also remains fail-fast rather than entering an unattended retry loop.

`pool.instances` is an absolute local physical-instance cap, not only an online-runner target. Provisioning, ready, draining, quarantined, and cleanup-pending instances all count toward it. Host-trust generation rotation does not receive surge capacity: an old busy runner keeps its slot until it exits or is safely removed.

Add or change workflow labels:

```yaml
runner:
  labels:
    - self-hosted
    - linux
    - epar-docker-container-catthehacker-ubuntu
```

Disable the automatic host-machine label:

```yaml
runner:
  includeHostLabel: false
```

Register runners in an organization runner group and omit GitHub's automatic `self-hosted`, operating-system, and architecture labels:

```yaml
runner:
  group: your-runner-group
  labels: [epar-core-unique-label]
  includeHostLabel: false
  noDefaultLabels: true

security:
  runnerGroup:
    enforcement: enforce
    requireExplicitGroup: true
    requireNonDefaultGroup: true
    requiredRepositoryAccess: selected
    requirePublicRepositoriesDisabled: true
```

The group must already exist and allow the target repository to use it. `requiredRepositoryAccess` is a maximum breadth: `selected` allows only selected-repository groups, `private` also allows all-private groups, and `all` accepts any repository visibility. The public-repository requirement remains independent. Existing configs without `security.runnerGroup` use strict recommended requirements in `warn` mode; new wizard configs use `enforce`. See [Runner Group Security](runner-groups.md) for default-group choices, enterprise inheritance, overrides, and failure behavior.

`runner.noDefaultLabels` defaults to `false`; when it is `true`, workflows must target labels explicitly configured under `runner.labels` and may also target the runner group.

Use a different config file:

```bash
go run ./cmd/ephemeral-action-runner start --config .local/wsl.yml
```

Configure logging and retention in the top-level `logging` section. The complete schema and local/Kubernetes examples are in [Logging](logging.md). Unknown configuration keys are rejected. For compatibility, a legacy `pool.logDir` value is used as `logging.directory` with a migration warning when the new key is absent; the file is not rewritten automatically. A configuration containing both keys is rejected as ambiguous.

### Host trust inheritance

Docker Container runners can inherit the host's trusted TLS root anchors:

```yaml
image:
  hostTrustMode: overlay
  hostTrustScopes: [system, user]
```

`image.hostTrustMode` accepts `disabled` or `overlay`. Existing configs default to `disabled`. A new interactive Docker Container initialization asks whether to enable host trust inheritance; pressing Enter accepts the displayed `yes` default. Enabling the policy is the one-time consent for EPAR to follow later host root additions, removals, and rotations automatically.

The supported scopes are:

| Controller host | `system` | `user` |
| --- | --- | --- |
| Windows | Local-machine trusted roots, excluding Windows-disallowed certificates | Current-user trusted roots, excluding Windows-disallowed certificates |
| macOS | System Roots plus CA certificates explicitly trusted for TLS server use in the administrator domain, excluding explicit deny | CA certificates in the user's keychain search list explicitly trusted for TLS server use, excluding explicit deny |
| Linux | The distribution's generated system CA bundle | Not supported |

Use `[system, user]` on Windows or macOS when the runner should inherit the
same two trust scopes as the account running EPAR. Linux configs must use
`[system]`. Overlay mode is supported only for `provider.type: docker-container` and
requires `runner.ephemeral: true`.
If macOS has disabled user-level Trust Settings, the `user` scope contributes
no certificates until that host policy is enabled again.

The resulting Ubuntu runner trust is additive:

```text
Ubuntu default roots
+ host roots from the current EPAR generation
+ image.trustedCaCertificatePaths
```

This is root-anchor inheritance, not exact emulation of Windows or macOS TLS
policy. macOS positive trust settings constrained by hostname, application, or
allowed error are not promoted into Ubuntu's unconstrained global root store.
Removing a host root does not remove an independently bundled Ubuntu root or a
certificate still listed under `trustedCaCertificatePaths`.

### Explicit CA paths

Trust an additional enterprise TLS inspection or private package-registry CA:

```yaml
image:
  trustedCaCertificatePaths:
    - .local/enterprise-root.pem
```

Paths may be repository-relative, absolute, or under `~/`. EPAR validates PEM
or DER X.509 CA certificates before building, normalizes them to deterministic
`.crt` files, and installs them before any `apt` or `curl` step. These paths are
independent of host trust inheritance and remain trusted until removed from the
config.

Route the private Docker daemon through an enterprise network proxy:

```yaml
docker:
  httpProxy: http://proxy.example.test:3128
  httpsProxy: http://proxy.example.test:3128
  noProxy: localhost,127.0.0.1,.example.test
```

These optional values become `HTTP_PROXY`, `HTTPS_PROXY`, and `NO_PROXY` on the
outer runner container, so its inner `dockerd` inherits them at first
startup. Proxy URLs must not contain credentials. Keep machine-specific proxy
addresses in ignored `.local/config.yml`, not tracked example files.

## Provider Defaults

For `provider.type: docker-container`, EPAR defaults to Catthehacker's full Ubuntu runner image and creates a Docker Container image named `epar-docker-container-catthehacker-ubuntu`.

For `provider.type: docker-sandboxes`, EPAR rejects `provider.sourceImage` and requires an explicitly built and loaded Candidate A template plus its full OCI configuration digest, verified policy fingerprint, staging root, and resource limits. The missing-config wizard offers only the active lock-selected source channels for the host platform: Catthehacker `full-latest` (recommended) and `act-22.04` (current lean profile). Each source channel maps to one exact versioned EPAR template tag; raw Catthehacker images cannot be used directly. The wizard filters historical cached EPAR tags, writes the selected template tag and full local digest to configuration, and does not refresh the approved source-lock snapshot automatically. It reports the shared host template-cache size separately from per-sandbox storage and asks one capacity question. With matching capacity evidence, it derives the total `rootDisk` from measured guest `/` use, a 25% margin, and the recommended 20 GiB writable headroom, then asks for Docker-disk capacity. Without exact root evidence, it asks for provisional total root capacity and writes the Docker-disk default. It writes the 50 GiB host-free floor in both cases; runtime admission strengthens this to at least 10% of the backing volume. Configuration validation enforces a 20 GiB minimum root capacity independently from the 100 GiB Docker-disk minimum. A configured Docker Sandboxes pool never falls back to Docker Container. See the [Docker Sandboxes provider](providers/docker-sandboxes.md) and the tracked example.

For `provider.type: wsl`, EPAR defaults to Catthehacker's full Ubuntu runner image, converts it into a WSL rootfs, and stores the output under `work/images/`.

For the experimental `provider.type: tart`, EPAR defaults to `ghcr.io/cirruslabs/ubuntu:latest`, a basic Ubuntu ARM64 VM image. EPAR installs its runner lifecycle but does not add the broad tool and dependency set found in GitHub's hosted runner images. If you require a GitHub-runner-like environment, build and maintain a bootable Tart image yourself by adapting the scripts in [actions/runner-images](https://github.com/actions/runner-images), then set `image.sourceImage` to that Tart image. Rosetta-based amd64 execution also has compatibility limits and must be validated against the exact workflow.

See the provider docs for details:

- [Docker Container Provider](providers/docker-container.md)
- [Docker Sandboxes Provider](providers/docker-sandboxes.md)
- [WSL Provider](providers/wsl.md)
- [Tart Provider](providers/tart.md)
