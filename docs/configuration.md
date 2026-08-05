# Configuration

EPAR reads a small, strict YAML subset: indentation uses spaces, unknown sections and keys fail, comments outside quotes are ignored, and list values may use either `[one, two]` or an indented list. Put real credentials and machine-specific paths in `.local/config.yml`; tracked configuration files are examples.

## Contents

- [Lookup and parser rules](#lookup-and-parser-rules)
- [Provider matrix](#provider-matrix)
- [Configuration reference](#configuration-reference)
- [Cross-field rules](#cross-field-rules)
- [Provider defaults](#provider-defaults)
- [Short recipes](#short-recipes)

## Lookup and parser rules

EPAR chooses the first available configuration path in this order: `--config <path>`, `EPAR_CONFIG`, `./.local/config.yml`, then `~/.config/ephemeral-action-runner/config.yml`. A relative file path in configuration is resolved from the project root when EPAR consumes it. `~` and `~/...` are expanded for the configuration path, `github.privateKeyPath`, and each `image.trustedCaCertificatePaths` entry; do not assume they expand in other configuration properties.

The canonical config path owns its lifecycle-state namespace and may have only one active controller, even if its contents change while that controller runs. A separate host-wide prefix reservation prevents another config or project from using the same normalized `pool.namePrefix`. Distinct configs with distinct prefixes may run concurrently; use unique routing labels and separate log directories so jobs and diagnostics remain unambiguous.

`github`, `image`, `pool`, `storage`, `logging`, `runner`, `security`, `provider`, `docker`, `dockerSandboxes`, and `timeouts` are the only accepted top-level sections. `security` contains only the `runnerGroup` subsection. Values are strings unless this reference says integer, number, boolean, or list. Quote a value when it needs YAML-like punctuation; EPAR removes one matching pair of single or double quotes.

`pool.logDir` is a deprecated compatibility input. If `logging.directory` is absent, EPAR uses it and emits a warning; using both is rejected. `pool.vmPrefix` is an accepted alias for `pool.namePrefix`. `image.profile` and the old `docker-socket` provider are rejected rather than silently migrated.

## Provider matrix

| Provider | Host and artifact model | Image defaults | Provider-only configuration |
| --- | --- | --- | --- |
| `docker-sandboxes` | A Linux, macOS, or Windows host with healthy `sbx diagnose --output json` results builds and imports a native Linux runner template with default-on QEMU/binfmt support for foreign container executables. Current support is backed by completed cross-platform live build, load, lifecycle, and cleanup testing and does not claim independent certification. | `image.sourceImage` selects a `ghcr.io/catthehacker/ubuntu` tag; EPAR records the exact artifact in local state. | `dockerSandboxes` is required; `provider.platform` is `linux/amd64` or `linux/arm64`; runner-group enforcement must be `enforce`. Architecture emulation has no configuration key. |
| `docker-container` | Compatibility provider: a Docker-compatible host creates an outer disposable runner with its own inner Docker daemon. | `docker-image`, `ghcr.io/catthehacker/ubuntu:full-latest`, output `epar-docker-container-catthehacker-ubuntu`. | Optional `provider.platform`; `docker` proxy and mirror settings apply to its private daemon. |
| `wsl` | Compatibility provider: Windows WSL2 imports a Docker image or rootfs tar into disposable Linux distros. | Docker source defaults to Catthehacker full Ubuntu, x64, with output under `work/images/`. | `provider.installRoot` controls WSL storage. |
| `tart` | Retired Apple Silicon Linux VM path retained for existing configurations and exact runtime/cleanup compatibility; it has no onboarding path. | `ghcr.io/cirruslabs/ubuntu:latest`, output `epar-ubuntu-24-arm64`. | `provider.network` and optional `provider.rosettaTag`. Validate the exact workload before relying on Rosetta. |

Four provider identities remain accepted at runtime and configuration, while three are onboarding-capable. Docker Sandboxes is wizard option `1` and the recommended default; the first screen offers `C. Show compatibility providers`, which reveals Docker Container (`2`) and WSL2 (`3`); Tart remains a runtime/configuration compatibility identity but is retired from onboarding. Docker Sandboxes never falls back to another provider. Its wizard is available when the required tooling, daemon diagnostics, and host-platform mapping pass; storage is estimated after image selection but never blocks provider selection or configuration creation. After the configuration is saved, ordinary startup performs storage admission, source resolution, policy fingerprinting, template construction, import, and exact readback before any runner starts. Warnings and skipped diagnostics remain visible, and provisioning failures preserve the desired configuration without silently running an old artifact.

## Configuration reference

### `github`

| Property | Type and default | Required or applies when | Effect and caution |
| --- | --- | --- | --- |
| `appId` | integer; no default | Required for GitHub operations. | GitHub App ID used to request short-lived runner registration tokens. |
| `organization` | string; no default | Required for GitHub operations. | GitHub organization that owns the runner group and runner records. |
| `privateKeyPath` | string; no default | Required for GitHub operations. | Private-key file readable by the EPAR process. Keep it under ignored `.local/` storage. |
| `apiBaseUrl` | string; `https://api.github.com` | Optional, for GitHub Enterprise Server API endpoints. | Trailing `/` is removed. |
| `webBaseUrl` | string; `https://github.com` | Optional, for GitHub Enterprise Server web endpoints. | Trailing `/` is removed. |

### `image`

| Property | Type and default | Required or applies when | Effect and caution |
| --- | --- | --- | --- |
| `sourceImage` | string; provider default | Image-building providers. | Docker image reference or WSL rootfs-tar path, selected by `sourceType`. |
| `sourceType` | `docker-image` or `rootfs-tar`; provider default | `rootfs-tar` is WSL-only in normal use. | A rootfs tar cannot use `sourcePlatform`. |
| `sourcePlatform` | Docker platform string; empty except WSL Docker source default `linux/amd64` | Only with `sourceType: docker-image`. | Requests the source image platform; pulling an image does not prove it can execute. See [Cross-architecture containers](advanced/cross-architecture-containers.md). |
| `outputImage` | string; provider default | Image-building providers. | EPAR-owned reusable runner artifact name or path. |
| `upstreamDir` | string; `third_party/runner-images` | Image builds that adapt upstream scripts. | Local checkout/cache location for the pinned upstream runner-image scripts. |
| `upstreamLock` | string; `third_party/runner-images.lock` | Image builds that adapt upstream scripts. | Lock file identifying the approved upstream revision. |
| `runnerVersion` | string; `latest` | Runner image builds. | Runner release selector. EPAR resolves it to an exact platform package and verified SHA-256 when a remote check is due. |
| `updateFrequency` | `daily`, `weekly`, `biweekly`, `monthly`, or `manual`; `weekly` | Mutable source-image tags and `runnerVersion: latest`. | Controls remote freshness checks only. Local configuration, scripts, trust inputs, EPAR assets, and missing or corrupt artifacts apply immediately. |
| `updateTime` | local 24-hour `HH:MM`; `"07:00"` | Automatic update frequencies. | Preferred local check time. Manual mode ignores it. |
| `customInstallScripts` | list of non-empty paths; empty | Optional image customization. | Scripts run while creating the runner image; treat them as trusted build input. |
| `trustedCaCertificatePaths` | list of non-empty paths; empty | Optional additional TLS roots. | PEM or DER CA files are validated, supplied to EPAR's operational builder, and installed in the runner artifact. They supplement, not replace, system or runner-overlay trust. |
| `hostTrustMode` | `disabled` or `overlay`; `disabled` | Optional host-root inheritance for ephemeral runners. | Controls runner inheritance only. `overlay` requires `runner.ephemeral: true`; it collects a current host-trust generation before registration and fails closed on an invalid or stale result. EPAR's owned builder independently receives operational system trust. |
| `hostTrustScopes` | list of `system`, `user`; `[system]` | Required and non-empty with `hostTrustMode: overlay`. | Windows/macOS may use `[system, user]`; Linux supports `[system]` only. This is root-anchor inheritance, not exact host TLS-policy emulation. |

Runner host trust is a common ephemeral-runner contract, not a Docker Container-only configuration rule. The first-run wizard enables it by default for Docker Sandboxes and the Docker Container compatibility provider, while the configuration validator applies the same overlay and ephemeral requirements independently of provider type. Operational builder trust is automatic and separate: system roots are supplied to EPAR's dedicated BuildKit builder even when runner overlay is disabled, user roots remain opt-in through runner overlay scope, and explicit CA paths apply to both paths. A host Docker daemon must separately trust a private registry before EPAR can pull an image; neither builder nor guest overlay can repair a failed host-daemon pull.

The default update policy checks remotely mutable inputs weekly at 07:00 local time. Repeated starts before the next check verify and reuse local artifacts without contacting the image registry or Actions runner release API. Run `./start image update` for an immediate check; `image build` remains the force-build command. Scheduled failures keep an exactly verified current generation available with visible persisted retry state, while user-requested local changes remain fail-closed.

### `pool`

| Property | Type and default | Required or applies when | Effect and caution |
| --- | --- | --- | --- |
| `instances` | integer; `1` | All providers. Must be at least `1`. | Strict cap for provisioning, ready, draining, quarantined, and cleanup-pending local instances. |
| `namePrefix` | 2-40 character name; provider default or wizard-derived machine name plus random suffix | All providers. | Literal prefix for local and GitHub identities. Keep it unique per machine/config and organization. Docker Sandboxes additionally permits only lowercase letters, digits, `-`, and `.`. |
| `vmPrefix` | deprecated alias for `namePrefix` | Existing configs only. | Do not set both aliases with conflicting intent; use `namePrefix` for new configs. |
| `logDir` | deprecated string path; no default | Existing configs only. | Used as `logging.directory` with a warning only when `logging.directory` is absent; using both is rejected. |
| `replacementRetryInitialSeconds` | integer; `15` | All providers. Greater than `0`. | Initial retry delay for transient replacement allocation failures. |
| `replacementRetryMaxSeconds` | integer; `1800` | All providers. At least the initial delay. | Upper retry-delay cap. |
| `replacementRetryMultiplier` | number; `2` | All providers. At least `1`. | Backoff multiplier. |
| `replacementRetryJitterPercent` | integer; `20` | All providers. `0` through `100`. | Randomizes retry delay to avoid synchronized retries. |

GitHub `429` and `5xx` responses and transient network failures back off replacement allocation; a longer `Retry-After` wins. Invalid configuration, authentication failures, and initial startup remain fail-fast after compensating rollback.

### `storage`

| Property | Type and default | Required or applies when | Effect and caution |
| --- | --- | --- | --- |
| `minimumFree` | positive byte size; `1GiB` | All providers. | Fixed provider-neutral physical free-space reserve. Existing explicit values remain authoritative; the default does not scale with volume size. |
| `gracePeriod` | positive Go duration; `168h` | Conservative housekeeping. | Minimum age before abandoned or incomplete EPAR temporary work can be removed. It does not delay cleanup of a verified superseded generation. |
| `keepPrevious` | integer; `0` | Conservative housekeeping. Must be non-negative. | `0` allows immediate exact retirement after replacement. A positive value defers automatic artifact retirement to the explicit storage-prune retention preview. |
| `automaticHousekeeping` | `conservative` or `disabled`; `conservative` | All providers. | Conservative mode reconciles interrupted exact-owned work at startup and removes unreferenced superseded resources after readback. It never runs a broad Docker or WSL prune. |
| `buildCacheLimit` | positive byte size; `20GiB` | Image-building cache. | Bounded EPAR BuildKit cache target. Existing explicit values remain authoritative. |
| `goCacheLimit` | positive byte size; `10GiB` | Native/no-Go Go build cache. | Bounded EPAR Go cache target. |

### `logging`

| Property | Type and default | Required or applies when | Effect and caution |
| --- | --- | --- | --- |
| `directory` | string; `work/logs` | All providers. Non-empty. | Root for manager, instance, build, error, and benchmark logs. |
| `managerSinks` | non-empty list of `console`, `file`; `[console]` | Manager events. | Choose user-facing manager event destinations. |
| `managerConsoleFormat` | `text` or `json`; `text` | Manager console sink. | Console encoding. |
| `managerConsoleTextFormat` | one-line template; empty | Only when manager console format is `text`. | May use `{time}`, `{level}`, `{message}`, `{attributes}` and must contain `{message}`. |
| `managerFileFormat` | `text` or `json`; `json` | Manager file sink. | File encoding. |
| `transcriptSinks` | non-empty list of `console`, `file`; `[file]` | Raw instance/build transcripts. | Default keeps verbose transcript events out of the console. |
| `transcriptConsoleFormat` | `text` or `json`; `text` | Transcript console sink. | Console encoding. |
| `transcriptConsoleTextFormat` | one-line template; empty | Only when transcript console format is `text`. | May use `{time}`, `{instance}`, `{component}`, `{stream}`, `{message}`, `{session}`, `{category}`, `{provider}`, `{attributes}` and must contain `{message}`. |
| `maxFileSizeMiB` | integer at least `1`; `100` | Rotated logs. | Per-file rotation threshold. |
| `maxBackups` | integer at least `1`; `3` | Rotated logs. | Number of rotated files to retain per stream. |
| `compressBackups` | boolean; `true` | Rotated logs. | Compresses rotated backups. |
| `retentionEnabled` | boolean; `true` | Log retention. | Enables periodic age and total-size retention. |
| `retentionMaxTotalMiB` | integer at least `1`; `1024` | Log retention. | Maximum retained logging size. |
| `managerMaxAgeDays` | integer at least `1`; `14` | Log retention. | Manager-log age limit. |
| `instanceMaxAgeDays` | integer at least `1`; `14` | Log retention. | Instance-transcript age limit. |
| `buildMaxAgeDays` | integer at least `1`; `14` | Log retention. | Build-transcript age limit. |
| `errorMaxAgeDays` | integer at least `1`; `30` | Log retention. | Error-report age limit. |
| `benchmarkMaxAgeDays` | integer at least `1`; `90` | Log retention. | Startup-benchmark age limit. |
| `retentionIntervalMinutes` | integer at least `1`; `60` | Log retention. | Periodic retention interval. |

### `runner`

| Property | Type and default | Required or applies when | Effect and caution |
| --- | --- | --- | --- |
| `labels` | non-empty list; provider default | All providers. Each label is at most 256 characters. | GitHub Actions routing labels. Keep architecture-sensitive workflows on an explicitly compatible label. |
| `group` | string; empty | Optional organization runner group. | Group must exist and pass the configured runner-group policy. |
| `includeHostLabel` | boolean; `true` | All providers. | Adds sanitized `epar-host-<machine>` unless already listed. Set false when workflows must not route by host. |
| `ephemeral` | boolean; `true` | All providers. | Required by Docker Sandboxes and host-trust overlay; each runner accepts one job. |
| `noDefaultLabels` | boolean; `false` | Optional GitHub registration behavior. | Omits GitHub's default self-hosted, OS, and architecture labels, so workflows must use explicitly configured labels. |

### `security.runnerGroup`

| Property | Type and default | Required or applies when | Effect and caution |
| --- | --- | --- | --- |
| `enforcement` | `warn` or `enforce`; `warn` | All providers; Docker Sandboxes requires `enforce`. | `warn` records a policy failure and continues; `enforce` blocks registration. |
| `requireExplicitGroup` | boolean; `true` | Runner-group preflight. | Requires `runner.group` rather than an implicit default group. |
| `requireNonDefaultGroup` | boolean; `true` | Runner-group preflight. | Rejects the organization default group when enforcement applies. |
| `requiredRepositoryAccess` | `selected`, `private`, or `all`; `selected` | Runner-group preflight. | Maximum allowed repository breadth: selected only, all-private or narrower, or any visibility. |
| `requirePublicRepositoriesDisabled` | boolean; `true` | Runner-group preflight. | Requires the group not to be usable by public repositories. |

If the complete subsection is absent, EPAR warns and uses the strict recommended checks in `warn` mode. New wizard configurations write an explicit policy. See [Runner Group Security](runner-groups.md).

### `provider`

| Property | Type and default | Required or applies when | Effect and caution |
| --- | --- | --- | --- |
| `type` | `docker-sandboxes`, `docker-container`, `wsl`, or `tart`; Docker Sandboxes is the wizard default and Tart is retired | Required. | Selects the provider. `docker-socket` is intentionally rejected because EPAR uses a private daemon for Docker Container. |
| `sourceImage` | string; image output for image-building providers, empty for Docker Sandboxes | Required for existing Tart, WSL, and Docker Container configurations. | Reusable artifact cloned by Tart, WSL, or Docker Container. Docker Sandboxes rejects it. |
| `network` | string; `default` | Tart image build and runtime. | Tart network mode. Do not assume this configures Docker or Docker Sandboxes networking. |
| `rosettaTag` | simple virtiofs tag; empty | Existing Tart configurations only. | Enables the retired Tart Rosetta compatibility path. Validate each amd64 workload and label it distinctly. |
| `installRoot` | string; `work/wsl` | WSL. | Project-relative WSL distribution storage root. |
| `platform` | Docker platform string; empty except Docker Sandboxes default `linux/amd64` | Docker Container or Docker Sandboxes only. | Docker Sandboxes accepts only `linux/amd64` or `linux/arm64`; it also determines the default architecture label. |

### `docker`

| Property | Type and default | Required or applies when | Effect and caution |
| --- | --- | --- | --- |
| `registryMirrors` | list of root `http`/`https` URLs; empty | Private Docker daemon users. | Mirrors must not include credentials, query, fragment, or a non-root path. See [Docker Registry Mirrors](advanced/docker-registry-mirrors.md). |
| `httpProxy` | root `http`/`https` URL; empty | Private Docker daemon users. | Becomes `HTTP_PROXY` for the outer Docker Container runner and its inner daemon. Credentials are rejected. |
| `httpsProxy` | root `http`/`https` URL; empty | Private Docker daemon users. | Becomes `HTTPS_PROXY`; credentials are rejected. |
| `noProxy` | comma-separated host/domain/IP/CIDR/`*`; empty | Private Docker daemon users. | Becomes `NO_PROXY`; whitespace, URLs, credentials, empty entries, and invalid CIDRs are rejected. |

### `dockerSandboxes`

| Property | Type and default | Required or applies when | Effect and caution |
| --- | --- | --- | --- |
| `image.sourceImage` | `ghcr.io/catthehacker/ubuntu:full-latest` | Required with Docker Sandboxes; must be the exact `full-latest` or `act-latest` profile. | Desired Catthehacker source selector; EPAR builds and imports the runnable template automatically. Specialized and custom tags remain available to Docker Container and WSL. |
| `policyGeneration` | lowercase `sha256:<64-hex>`; no default | Required with Docker Sandboxes. | Recorded fingerprint of the host-global Balanced policy. |
| `networkBaseline` | `open` or `balanced`; `open` | Docker Sandboxes. | `open` adds a sandbox-scoped public-egress rule while denying host aliases; it does not change the host-global policy. |
| `additionalAllow` | unique hostname or `*.domain`, optional port; empty | Docker Sandboxes. | Adds sandbox-scoped allow resources. With `open`, it cannot re-allow EPAR's host-alias deny guardrails. |
| `additionalDeny` | unique hostname or `*.domain`, optional port; empty | Docker Sandboxes. | Adds sandbox-scoped deny resources. A resource cannot be in both allow and deny lists. |
| `stagingRoot` | canonical project-relative `.local/...` path; `.local/docker-sandboxes-staging` | Docker Sandboxes. | Per-create staging root; cannot be absolute, escape `.local`, or overlap `.local/bin` or `.local/state`. |
| `cpus` | positive integer; `4` | Docker Sandboxes. | CPU allocation for each sandbox. |
| `memory` | positive byte size; `8GiB` | Docker Sandboxes. | Per-sandbox memory allocation written by the wizard. |
| `rootDisk` | `auto` or byte size at least `20GiB`; `auto` | Docker Sandboxes. | Sparse guest-root logical maximum. `auto` is recalculated for each artifact as the expanded image estimate plus 5 GiB build allowance and 20 GiB writable headroom, rounded up to 10 GiB. An explicit undersized value is rejected before creation. |
| `dockerDisk` | byte size at least `1GiB`; `50GiB` | Docker Sandboxes. | Independent sparse logical maximum for the Docker daemon inside the sandbox; it is workload capacity and is not derived from the base image. |
| `maxConcurrentCreates` | positive integer; `2` | Docker Sandboxes. | Limits concurrent sandbox creation to control capacity pressure. |

Docker Sandboxes templates always bundle the pinned `tonistiigi/binfmt` installer and static QEMU interpreters. EPAR runs `binfmt --install all` inside each sandbox VM and requires at least one enabled bundled handler before creation succeeds; it does not install handlers on the host or verify a fixed target matrix. Docker continues selecting image manifests normally, and EPAR never injects `DOCKER_DEFAULT_PLATFORM`. Use a Compose service's `platform` property or `docker run --platform` when a multi-platform tag must deliberately select a foreign variant.

### `timeouts`

| Property | Type and default | Required or applies when | Effect and caution |
| --- | --- | --- | --- |
| `bootSeconds` | integer; `180` | All providers. | Time allowed for instance boot/readiness. |
| `githubOnlineSeconds` | integer; `180` | All providers. | Time allowed for GitHub runner online readiness. |
| `commandSeconds` | integer; `900` | All providers. | Default bound for provider command execution. |

## Cross-field rules

- `provider.sourceImage` is required for existing Tart, WSL, and Docker Container configurations, and forbidden for Docker Sandboxes.
- `provider.rosettaTag` is accepted only for Tart. `provider.platform` is accepted only for Docker Container or Docker Sandboxes. Docker Sandboxes accepts only `linux/amd64` and `linux/arm64`.
- Docker Sandboxes requires `runner.ephemeral: true`, `security.runnerGroup.enforcement: enforce`, the exact Catthehacker `full-latest` or `act-latest` profile, policy generation, resource values, and a lowercase-compatible pool prefix.
- `image.sourcePlatform` requires `image.sourceType: docker-image`; all byte-size fields require a positive `B`, `KiB`, `MiB`, `GiB`, or `TiB` value.
- Host-trust overlay requires a non-empty, duplicate-free scope list and `runner.ephemeral: true`; `user` is not supported on Linux.
- `pool.namePrefix` is a host-wide controller and ownership boundary. Tart, WSL, and Docker Container use the configured prefix to select legacy owned resources; Docker Sandboxes uses its durable ledger of exact owned identities. EPAR rejects concurrent reuse across configs, projects, and providers; do not assume broad prefix cleanup is safe.
- `runner.labels` must never be empty, even when `runner.noDefaultLabels` is false.

## Provider defaults

The configuration loader begins with provider-neutral defaults and requires an explicit `provider.type`, then applies that provider's defaults only when the corresponding key was not set explicitly. Existing Tart configurations still receive their retained Tart defaults. The first-run wizard writes a concrete Docker Sandboxes configuration by default and derives a machine-based pool prefix; use its generated values as the normal starting point.

| Provider | Source and output | Default labels and prefix |
| --- | --- | --- |
| Docker Sandboxes (primary) | Desired image settings plus policy generation; exact template identities live in the local artifact receipt. | `self-hosted`, `linux`, matching `X64`/`ARM64`, `epar-docker-sandboxes`; prefix `epar-docker-sandboxes`. |
| Docker Container (compatibility) | Catthehacker full Ubuntu to `epar-docker-container-catthehacker-ubuntu`. | `self-hosted`, `linux`, `epar-docker-container-catthehacker-ubuntu`; prefix `epar-docker-container`. |
| WSL Docker source (compatibility) | Catthehacker full Ubuntu, `linux/amd64`, output `work/images/epar-wsl-catthehacker-ubuntu.tar`. | `self-hosted`, `linux`, `X64`, `epar-wsl-catthehacker-ubuntu`; prefix `epar-wsl`. |
| WSL rootfs tar (compatibility) | `work/images/ubuntu-24.04-clean.rootfs.tar`, output `work/images/epar-ubuntu-24-wsl.tar`. | `self-hosted`, `linux`, `X64`, `epar-wsl-ubuntu-24.04-base`; prefix `epar-wsl`. |
| Tart (retired) | `ghcr.io/cirruslabs/ubuntu:latest` to `epar-ubuntu-24-arm64`; existing configurations retain runtime/cleanup compatibility. | `self-hosted`, `linux`, `ARM64`, `epar-tart-ubuntu-24.04-base`; prefix `epar`. |

## Short recipes

Keep two Docker Container runners warm:

```yaml
pool:
  instances: 2
  namePrefix: buildbox01-a4f9c2
```

Use an explicit runner group with enforced least-breadth access:

```yaml
runner:
  group: trusted-ci
security:
  runnerGroup:
    enforcement: enforce
    requireExplicitGroup: true
    requireNonDefaultGroup: true
    requiredRepositoryAccess: selected
    requirePublicRepositoriesDisabled: true
```

Add a host and explicit enterprise root without weakening TLS verification:

```yaml
image:
  hostTrustMode: overlay
  hostTrustScopes: [system, user]
  trustedCaCertificatePaths: [.local/enterprise-root.pem]
```

On Linux, use `hostTrustScopes: [system]`. Do not disable certificate verification to work around a private CA.

Configure a private Docker proxy and mirror without embedding credentials:

```yaml
docker:
  registryMirrors: [https://mirror.example.test]
  httpProxy: http://proxy.example.test:3128
  httpsProxy: http://proxy.example.test:3128
  noProxy: localhost,127.0.0.1,.example.test
```

For Docker Sandboxes, run `./start` and let the wizard select the desired image, provision the native-platform runner template, and write the policy fingerprint and capacity settings. Do not hand-copy generated template identities between hosts; EPAR records them in the local artifact receipt.
