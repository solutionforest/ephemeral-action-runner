# Configuration

EPAR stores local settings in `.local/config.yml` by default. The first run creates that file for the default Docker-DinD setup when it does not exist.

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
| `provider` | How EPAR creates disposable runners: `docker-dind`, `wsl`, or `tart`. |
| `image` | Source image/rootfs, output image, runner version, and optional install scripts. |
| `pool` | Runner count, instance name prefix, and replacement retry policy. |
| `logging` | Manager and transcript sinks, formats, rotation, retention, and log directory. |
| `runner` | GitHub Actions labels, runner group, default-label policy, and whether to add the host-machine label. |
| `docker` | Optional Docker registry mirrors and Docker-DinD daemon proxy settings. |
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

`pool.namePrefix` is both the prefix for generated GitHub runner names and the cleanup boundary for GitHub runner records. It must be 2-40 characters and should leave room for EPAR's generated `-YYYYMMDD-HHMMSS-###` suffix. Do not reuse the same prefix on different machines or for separate EPAR supervisors in the same organization. If two machines share a prefix, one machine's cleanup can delete the other machine's GitHub runner record, causing the other supervisor to report that the runner record is gone and replace a healthy runner.

Configure replacement retry behavior after a transient GitHub or network outage:

```yaml
pool:
  replacementRetryInitialSeconds: 15
  replacementRetryMaxSeconds: 1800
  replacementRetryMultiplier: 2
  replacementRetryJitterPercent: 20
```

These values default to `15`, `1800`, `2`, and `20`, so existing configurations remain valid without changes. `replacementRetryInitialSeconds` must be positive, `replacementRetryMaxSeconds` must be at least the initial delay, `replacementRetryMultiplier` must be at least `1`, and `replacementRetryJitterPercent` must be from `0` through `100`.

The supervisor backs off only replacement allocation after transient network errors and GitHub HTTP `429` or `5xx` responses. The nominal delay doubles from 15 seconds to a 30-minute cap with the configured jitter; a longer GitHub `Retry-After` response takes precedence. Authentication and deterministic configuration failures remain fail-fast after safe rollback. Initial `pool up` startup also remains fail-fast rather than entering an unattended retry loop.

`pool.instances` is an absolute local physical-instance cap, not only an online-runner target. Provisioning, ready, draining, quarantined, and cleanup-pending instances all count toward it. Host-trust generation rotation does not receive surge capacity: an old busy runner keeps its slot until it exits or is safely removed.

Add or change workflow labels:

```yaml
runner:
  labels:
    - self-hosted
    - linux
    - epar-docker-dind-catthehacker-ubuntu
```

Disable the automatic host-machine label:

```yaml
runner:
  includeHostLabel: false
```

Register runners in an organization runner group and omit GitHub's automatic
`self-hosted`, operating-system, and architecture labels:

```yaml
runner:
  group: epar-ci-canary
  labels: [epar-core-unique-label]
  includeHostLabel: false
  noDefaultLabels: true
```

`runner.group` is optional. The group must already exist and allow the target
repository to use it. `runner.noDefaultLabels` defaults to `false`; when it is
`true`, workflows must target labels explicitly configured under
`runner.labels` (and may also target the runner group).

Use a different config file:

```bash
go run ./cmd/ephemeral-action-runner start --config .local/wsl.yml
```

Configure logging and retention in the top-level `logging` section. The complete schema and local/Kubernetes examples are in [Logging](logging.md). Unknown configuration keys are rejected. For compatibility, a legacy `pool.logDir` value is used as `logging.directory` with a migration warning when the new key is absent; the file is not rewritten automatically. A configuration containing both keys is rejected as ambiguous.

### Host trust inheritance

Docker-DinD runners can inherit the host's trusted TLS root anchors:

```yaml
image:
  hostTrustMode: overlay
  hostTrustScopes: [system, user]
```

`image.hostTrustMode` accepts `disabled` or `overlay`. Existing configs default
to `disabled`. A new interactive Docker-DinD initialization asks whether to
enable host trust inheritance; pressing Enter accepts the displayed `yes`
default. Enabling the policy is the one-time consent for EPAR to follow later
host root additions, removals, and rotations automatically.

The supported scopes are:

| Controller host | `system` | `user` |
| --- | --- | --- |
| Windows | Local-machine trusted roots, excluding Windows-disallowed certificates | Current-user trusted roots, excluding Windows-disallowed certificates |
| macOS | System Roots plus CA certificates explicitly trusted for TLS server use in the administrator domain, excluding explicit deny | CA certificates in the user's keychain search list explicitly trusted for TLS server use, excluding explicit deny |
| Linux | The distribution's generated system CA bundle | Not supported |

Use `[system, user]` on Windows or macOS when the runner should inherit the
same two trust scopes as the account running EPAR. Linux configs must use
`[system]`. Overlay mode is supported only for `provider.type: docker-dind` and
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

Route the private Docker-DinD daemon through an enterprise network proxy:

```yaml
docker:
  httpProxy: http://proxy.example.test:3128
  httpsProxy: http://proxy.example.test:3128
  noProxy: localhost,127.0.0.1,.example.test
```

These optional values become `HTTP_PROXY`, `HTTPS_PROXY`, and `NO_PROXY` on the
outer Docker-DinD container, so its inner `dockerd` inherits them at first
startup. Proxy URLs must not contain credentials. Keep machine-specific proxy
addresses in ignored `.local/config.yml`, not tracked example files.

## Provider Defaults

For `provider.type: docker-dind`, EPAR defaults to Catthehacker's full Ubuntu runner image and creates a Docker-DinD image named `epar-docker-dind-catthehacker-ubuntu`.

For `provider.type: wsl`, EPAR defaults to Catthehacker's full Ubuntu runner image, converts it into a WSL rootfs, and stores the output under `work/images/`.

For `provider.type: tart`, start from `configs/tart.example.yml` and adjust labels or image scripts as needed.

See the provider docs for details:

- [Docker-DinD Provider](providers/docker-dind.md)
- [WSL Provider](providers/wsl.md)
- [Tart Provider](providers/tart.md)
