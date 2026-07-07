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
| `pool` | Runner count, instance name prefix, and log directory. |
| `runner` | GitHub Actions labels and whether to add the host-machine label. |
| `docker` | Optional Docker registry mirrors applied inside disposable runners. |
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

Add or change workflow labels:

```yaml
runner:
  labels:
    - self-hosted
    - linux
    - epar-docker-dind-gitea-ubuntu
```

Disable the automatic host-machine label:

```yaml
runner:
  includeHostLabel: false
```

Use a different config file:

```bash
go run ./cmd/ephemeral-action-runner start --config .local/wsl.yml
```

## Provider Defaults

For `provider.type: docker-dind`, EPAR defaults to Gitea's full Ubuntu runner image and creates a Docker-DinD image named `epar-docker-dind-gitea-ubuntu`.

For `provider.type: wsl`, EPAR defaults to Gitea's full Ubuntu runner image, converts it into a WSL rootfs, and stores the output under `work/images/`.

For `provider.type: tart`, start from `configs/tart.example.yml` and adjust labels or image scripts as needed.

See the provider docs for details:

- [Docker-DinD Provider](providers/docker-dind.md)
- [WSL Provider](providers/wsl.md)
- [Tart Provider](providers/tart.md)
