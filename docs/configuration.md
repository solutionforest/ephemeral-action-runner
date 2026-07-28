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

`github`, `image`, `pool`, `storage`, `logging`, `runner`, `security`, `provider`, `docker`, `dockerSandboxes`, and `timeouts` are the only accepted top-level sections. `security` contains only the `runnerGroup` subsection. Values are strings unless this reference says integer, number, boolean, or list. Quote a value when it needs YAML-like punctuation; EPAR removes one matching pair of single or double quotes.

`pool.logDir` is a deprecated compatibility input. If `logging.directory` is absent, EPAR uses it and emits a warning; using both is rejected. `pool.vmPrefix` is an accepted alias for `pool.namePrefix`. `image.profile` and the old `docker-socket` provider are rejected rather than silently migrated.

## Provider matrix

| Provider | Host and artifact model | Image defaults | Provider-only configuration |
| --- | --- | --- | --- |
| `docker-container` | A Docker-compatible host creates an outer disposable runner with its own inner Docker daemon. | `docker-image`, `ghcr.io/catthehacker/ubuntu:full-latest`, output `epar-docker-container-catthehacker-ubuntu`. | Optional `provider.platform`; `docker` proxy and mirror settings apply to its private daemon. |
| `docker-sandboxes` | A supported `sbx` host creates a pinned Linux sandbox template. It is preview-only until the exact host/platform combination has independent live evidence. | No source image or build artifact is accepted; the wizard selects and pins a local Candidate A template. | `dockerSandboxes` is required; `provider.platform` is `linux/amd64` or `linux/arm64`; runner-group enforcement must be `enforce`. |
| `wsl` | Windows WSL2 imports a Docker image or rootfs tar into disposable Linux distros. | Docker source defaults to Catthehacker full Ubuntu, x64, with output under `work/images/`. | `provider.installRoot` controls WSL storage. |
| `tart` | Experimental Apple Silicon Linux VM path. | `ghcr.io/cirruslabs/ubuntu:latest`, output `epar-ubuntu-24-arm64`. | `provider.network` and optional `provider.rosettaTag`. Validate the exact workload before relying on Rosetta. |

Docker Sandboxes never falls back to Docker Container. Its wizard is available only when the supported `sbx` version, host mapping, local template identity, policy fingerprint, capacity evidence, and `sbx diagnose --output json` prerequisite checks pass. Warnings remain visible; failures prevent selection.

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
| `runnerVersion` | string; `latest` | Runner image builds. | Runner release selector. Pin a version when repeatability matters. |
| `customInstallScripts` | list of non-empty paths; empty | Optional image customization. | Scripts run while creating the runner image; treat them as trusted build input. |
| `trustedCaCertificatePaths` | list of non-empty paths; empty | Optional additional TLS roots. | PEM or DER CA files are validated and installed in the Ubuntu trust bundle. They supplement, not replace, host trust. |
| `hostTrustMode` | `disabled` or `overlay`; `disabled` | Optional host-root inheritance for ephemeral runners. | `overlay` requires `runner.ephemeral: true`; it collects a current host-trust generation before registration and fails closed on an invalid or stale result. |
| `hostTrustScopes` | list of `system`, `user`; `[system]` | Required and non-empty with `hostTrustMode: overlay`. | Windows/macOS may use `[system, user]`; Linux supports `[system]` only. This is root-anchor inheritance, not exact host TLS-policy emulation. |

Host trust is a common ephemeral-runner contract, not a Docker Container-only configuration rule. The interactive Docker Container and Docker Sandboxes paths offer it, while the configuration validator applies the same overlay and ephemeral requirements independently of provider type. A host Docker daemon must separately trust a private registry before EPAR can pull an image; the guest overlay cannot repair a failed source-image pull.

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
| `minimumFree` | positive byte size; `20GiB` | All providers. | Provider-neutral admission reserve. Docker Sandboxes raises it to `minHostFreeSpace` when that is larger. |
| `gracePeriod` | positive Go duration; `168h` | Conservative housekeeping. | Minimum age before eligible EPAR-owned artifacts can be considered for removal. |
| `keepPrevious` | integer; `0` | Conservative housekeeping. Must be non-negative. | Number of prior reusable artifact generations to retain. |
| `automaticHousekeeping` | `conservative` or `disabled`; `conservative` | All providers. | Conservative mode touches only expired, unleased, exactly owned artifacts; it does not run a broad Docker or WSL prune. |
| `buildCacheLimit` | positive byte size; `64GiB` | Image-building cache. | Bounded EPAR build cache target. |
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
| `type` | `tart`, `wsl`, `docker-container`, or `docker-sandboxes`; `tart` before provider defaults | Required. | Selects the provider. `docker-socket` is intentionally rejected because EPAR uses a private daemon for Docker Container. |
| `sourceImage` | string; image output for image-building providers, empty for Docker Sandboxes | Required except Docker Sandboxes. | Reusable artifact cloned by Tart, WSL, or Docker Container. Docker Sandboxes rejects it. |
| `network` | string; `default` | Tart image build and runtime. | Tart network mode. Do not assume this configures Docker or Docker Sandboxes networking. |
| `rosettaTag` | simple virtiofs tag; empty | Tart only. | Enables the experimental Tart Rosetta path. Validate each amd64 workload and label it distinctly. |
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
| `template` | immutable local template identity; no default | Required with Docker Sandboxes. | Exact Candidate A template selected by the wizard; raw Catthehacker images are not runnable templates. |
| `templateDigest` | lowercase `sha256:<64-hex>`; no default | Required with Docker Sandboxes. | Full local OCI configuration digest for the template identity. |
| `policyGeneration` | lowercase `sha256:<64-hex>`; no default | Required with Docker Sandboxes. | Fingerprint of the verified host-global Balanced policy. Policy drift blocks admission. |
| `networkBaseline` | `open` or `balanced`; `open` | Docker Sandboxes. | `open` adds a sandbox-scoped public-egress rule while denying host aliases; it does not change the host-global policy. |
| `additionalAllow` | unique hostname or `*.domain`, optional port; empty | Docker Sandboxes. | Adds sandbox-scoped allow resources. With `open`, it cannot re-allow EPAR's host-alias deny guardrails. |
| `additionalDeny` | unique hostname or `*.domain`, optional port; empty | Docker Sandboxes. | Adds sandbox-scoped deny resources. A resource cannot be in both allow and deny lists. |
| `stagingRoot` | canonical project-relative `.local/...` path; `.local/docker-sandboxes-staging` | Docker Sandboxes. | Per-create staging root; cannot be absolute, escape `.local`, or overlap `.local/bin` or `.local/state`. |
| `cpus` | positive integer; `4` | Docker Sandboxes. | CPU allocation for each sandbox. |
| `memory` | positive byte size; `8GiB` | Docker Sandboxes. | Per-sandbox memory allocation written by the wizard. |
| `rootDisk` | byte size at least `20GiB`; no schema default | Docker Sandboxes. | Guest root capacity. Wizard sizing uses measured guest use plus margin/headroom when evidence exists. |
| `dockerDisk` | byte size at least `100GiB`; no schema default | Docker Sandboxes. | Inner Docker disk capacity. |
| `minHostFreeSpace` | byte size at least `50GiB`; no schema default | Docker Sandboxes. | Host admission floor; effective reserve is the larger of this and `storage.minimumFree`. Runtime may require a stricter backing-volume percentage. |
| `maxConcurrentCreates` | positive integer; `2` | Docker Sandboxes. | Limits concurrent sandbox creation to control capacity pressure. |

### `timeouts`

| Property | Type and default | Required or applies when | Effect and caution |
| --- | --- | --- | --- |
| `bootSeconds` | integer; `180` | All providers. | Time allowed for instance boot/readiness. |
| `githubOnlineSeconds` | integer; `180` | All providers. | Time allowed for GitHub runner online readiness. |
| `commandSeconds` | integer; `900` | All providers. | Default bound for provider command execution. |

## Cross-field rules

- `provider.sourceImage` is required for Tart, WSL, and Docker Container, and forbidden for Docker Sandboxes.
- `provider.rosettaTag` is accepted only for Tart. `provider.platform` is accepted only for Docker Container or Docker Sandboxes. Docker Sandboxes accepts only `linux/amd64` and `linux/arm64`.
- Docker Sandboxes requires `runner.ephemeral: true`, `security.runnerGroup.enforcement: enforce`, a valid pinned template/digest/policy generation, resource values, and a lowercase-compatible pool prefix.
- `image.sourcePlatform` requires `image.sourceType: docker-image`; all byte-size fields require a positive `B`, `KiB`, `MiB`, `GiB`, or `TiB` value.
- Host-trust overlay requires a non-empty, duplicate-free scope list and `runner.ephemeral: true`; `user` is not supported on Linux.
- `pool.namePrefix` is an ownership boundary. Tart, WSL, and Docker Container use the configured prefix to select legacy owned resources; Docker Sandboxes uses its durable ledger of exact owned identities. Do not share a prefix between controllers or assume broad prefix cleanup is safe.
- `runner.labels` must never be empty, even when `runner.noDefaultLabels` is false.

## Provider defaults

The configuration loader starts with Tart defaults, then applies provider-specific defaults for WSL, Docker Container, and Docker Sandboxes only when the corresponding key was not set explicitly. The first-run wizard writes a concrete configuration and derives a machine-based pool prefix; use its generated values as the normal starting point.

| Provider | Source and output | Default labels and prefix |
| --- | --- | --- |
| Docker Container | Catthehacker full Ubuntu to `epar-docker-container-catthehacker-ubuntu`. | `self-hosted`, `linux`, `epar-docker-container-catthehacker-ubuntu`; prefix `epar-docker-container`. |
| Docker Sandboxes | Pinned Candidate A template, digest, and policy generation; no `provider.sourceImage`. | `self-hosted`, `linux`, matching `X64`/`ARM64`, `epar-docker-sandboxes`; prefix `epar-docker-sandboxes`. |
| WSL Docker source | Catthehacker full Ubuntu, `linux/amd64`, output `work/images/epar-wsl-catthehacker-ubuntu.tar`. | `self-hosted`, `linux`, `X64`, `epar-wsl-catthehacker-ubuntu`; prefix `epar-wsl`. |
| WSL rootfs tar | `work/images/ubuntu-24.04-clean.rootfs.tar`, output `work/images/epar-ubuntu-24-wsl.tar`. | `self-hosted`, `linux`, `X64`, `epar-wsl-ubuntu-24.04-base`; prefix `epar-wsl`. |
| Tart | `ghcr.io/cirruslabs/ubuntu:latest` to `epar-ubuntu-24-arm64`. | `self-hosted`, `linux`, `ARM64`, `epar-tart-ubuntu-24.04-base`; prefix `epar`. |

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

For Docker Sandboxes, run `./start` and let the wizard write the pinned template, digest, policy fingerprint, and capacity settings. Do not hand-copy a template tag from another host or replace its digest with a mutable tag.
