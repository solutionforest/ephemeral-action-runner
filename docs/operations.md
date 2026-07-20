# Operations

## Logs

Host-side logs go under `work/logs` by default. Run `ephemeral-action-runner logs path` to print the resolved directory. When `managerSinks` includes `file`, manager events use `work/logs/epar.log`; the default manager sink is console-only. Instance transcripts use `work/logs/instances/`, build/source transcripts use `work/logs/builds/`, startup timing JSONL uses `work/logs/benchmarks/`, and timestamped error reports use `work/logs/errors/`. See [Logging](logging.md) for sink, rotation, retention, and shipping configuration. Runner logs inside the Ubuntu guest are under:

- `/var/log/actions-runner/run.log`
- `/opt/actions-runner/_diag`

Guest provisioning command output is streamed to `work/logs/instances/<instance-name>.guest.log`. If runner launch or GitHub online readiness fails, EPAR first appends bounded diagnostics to that host guest log:
runner PID/process state, tails from `run.log` and the latest `Runner_*.log`,
and the Docker-DinD daemon log when present. Diagnostic collection is
best-effort and does not replace the original readiness error.

On systemd instances, the runner process is launched with `systemd-run` as `actions-runner.service` so provider `exec` calls return immediately after the service starts. On non-systemd instances such as Docker-DinD containers, EPAR starts `run.sh` in the background, writes `/var/run/actions-runner.pid`, and appends output to `/var/log/actions-runner/run.log`.

Docker-DinD containers also write inner Docker daemon logs to `/var/log/epar-dockerd.log` inside the runner container. Host-side Docker commands only show the outer runner container; job-created Compose resources live in the inner daemon.

When `docker.registryMirrors` is configured, EPAR writes `/etc/docker/daemon.json` inside each instance before runtime validation. For Docker-DinD, inspect both `/etc/docker/daemon.json` and `/var/log/epar-dockerd.log` inside the outer runner container.

## Supervisor Exit

`pool up` cleans up prefixed instances and GitHub runner records when it exits. Use `--keep-on-exit` only when intentionally debugging a live instance after the supervisor stops. While the supervisor is not running, EPAR cannot retire or replace completed runners.

## Capacity, Reconciliation, and Outage Recovery

`pool.instances` is a strict cap on prefix-owned local resources. EPAR counts provisioning, ready, draining, quarantined, and cleanup-pending instances, so a GitHub outage or a failed cleanup cannot create a replacement storm. Host-trust rotation has no temporary surge allowance; old busy-generation instances retain their slots until they finish or can be safely retired.

Only one controller may mutate a given canonical config, provider, and `pool.namePrefix` at a time. If another EPAR controller holds that lock, stop or reconfigure the other controller instead of forcing concurrent starts; use a distinct unique prefix for intentionally independent pools.

At startup and before allocating a replacement, EPAR reconciles the provider inventory with exact-name GitHub runner records. Healthy pairs are adopted, stopped or proven unregistered local resources are removed, and exact stale GitHub records are deleted when GitHub is reachable. When GitHub is unavailable, an ambiguous local instance is quarantined and continues to consume capacity instead of being deleted or replaced. If old resources already exceed the configured cap, the supervisor creates nothing until safe cleanup or normal draining restores capacity.

For transient network failures and GitHub `429` or `5xx` responses during supervised replacement, EPAR pauses allocation and retries with exponential backoff. The default nominal delays are 15, 30, 60, 120, 240, 480, 960, and 1800 seconds, each with ±20% jitter; a longer `Retry-After` response wins. Monitoring and housekeeping continue during that pause, and a successful adoption or fully online replacement resets the delay. Startup remains fail-fast after compensating rollback, and invalid configuration or GitHub authentication failures do not retry indefinitely.

## Cleanup Safety

Cleanup only touches local instances and GitHub runner records matching `pool.namePrefix`:

```yaml
pool:
  namePrefix: epar-tart
```

Generated names look like:

```text
epar-tart-20260703-010500-001
```

Do not set `namePrefix` to a broad value such as `ubuntu` or `runner`. Keep it within 40 characters so EPAR can append its generated runner-name suffix.
Also do not reuse the same `namePrefix` on different machines or for separate EPAR supervisors in the same GitHub organization. GitHub cleanup is prefix-based, so a shared prefix lets one machine delete another machine's runner records.

For Docker-DinD, cleanup removes the outer runner container with `docker rm -f -v`. That also removes the private inner Docker daemon's containers, networks, volumes, and image cache for that EPAR instance.

## Troubleshooting

This section is a compact checklist. For symptom-first diagnostics with host/provider-specific commands, see [Troubleshooting](troubleshooting.md).

- If a Docker/browser or web/E2E image build fails before package installation, run `image update-upstream`.
- If an image build fails with `E: You don't have enough free space in /var/cache/apt/archives/.`, check the Docker daemon or VM storage with `docker system df` and `docker run --rm ghcr.io/catthehacker/ubuntu:full-latest df -h /`. On Windows Docker Desktop with WSL2, the container-visible disk can be much smaller than Windows Explorer free space; see [Windows Docker Desktop WSL2 Disk Is Smaller Than Expected](troubleshooting.md#windows-docker-desktop-wsl2-disk-is-smaller-than-expected).
- If Docker validation fails for a Docker-enabled image, inspect `work/logs/builds/<image>.guest.log`.
- If browser validation fails on ARM64, confirm `epar-browser` exists inside the guest and inspect `/opt/epar/browser`.
- If a Docker Compose job uses an amd64-only runtime image on an ARM64 Tart runner and fails with `exec format error` or repeated container exits such as status `139`, use a runner label that supports that image instead of changing application runtime settings only for runner compatibility. Suitable targets include Docker-DinD with verified `linux/amd64` emulation, WSL x64, an x64 Linux host, or a Tart image with Rosetta enabled and validated.
- If a workflow uses fixed Compose project names, fixed container names, or fixed ports, Docker-DinD is often a better fit than a shared host Docker socket because each runner gets a private inner Docker daemon. Verify by starting two unregistered instances, running the same compose stack in both, and confirming host Docker only shows the outer EPAR runner containers.
- If repeated jobs still pull slowly after configuring a registry mirror, verify the mirror is reachable from inside the runner instance and that it supports the requested registry, image platform, and authentication model. Docker daemon mirrors primarily target Docker Hub; other registry caches may require workflow image references to use the cache registry URL.
- If a mirrored workflow only improves modestly, check where the time is going. Registry mirrors mainly reduce image pull time; container startup, Compose health checks, database initialization, volume sync, browser tests, private image authentication, and CPU-bound or emulated workloads can still dominate the total job time.
- If GitHub registration fails, confirm the app has permission to manage organization self-hosted runners and that the private key path is readable by the host user.
- If GitHub returns transient `500`, `502`, `503`, or `429` errors while the supervisor is replacing a runner, leave the supervisor running so it can retain the strict cap, reconcile exact runner names after recovery, and retry on its configured backoff. Do not manually start a second supervisor with the same `pool.namePrefix`.
- If registration fails before the runner listener starts, EPAR rolls back the local candidate immediately and later reconciles a possible exact-name GitHub record. If the listener already started but GitHub readiness is uncertain, EPAR quarantines that candidate; inspect the instance transcript and wait for GitHub recovery before manually deleting it.
- If stale runners remain, run `ephemeral-action-runner cleanup`.
- If using Tart `softnet`, verify the host has the privileges Tart requires.
- If default WSL image build fails before import, confirm Docker Desktop, Docker Engine, or another Docker daemon is reachable so EPAR can export `ghcr.io/catthehacker/ubuntu:full-latest` into a rootfs tar. For lean WSL configs, confirm the clean Ubuntu rootfs was exported from an Ubuntu 24.04 WSL distro.
- If WSL image build fails before systemd is ready, confirm WSL2 is enabled and inspect `work/logs/builds/<image>.guest.log`.
- If Docker-DinD startup fails, confirm the host Docker runtime supports privileged containers and inspect `/var/log/epar-dockerd.log` inside the runner container.
- If Docker-DinD `docker run` fails with nested overlay mount errors, keep the default `EPAR_DOCKERD_STORAGE_DRIVER=vfs`. Only switch to `overlay2` or `auto` in a derived image after proving that storage driver works on the exact host runtime.
- If the default WSL or Docker-DinD build cannot validate Docker, confirm the source image still provides `docker`, `dockerd`, Compose, Buildx, and `iptables`. If you intentionally use a clean Ubuntu source image instead of Catthehacker's runner image, run `image update-upstream` first and use a config that installs Docker from EPAR's pinned `actions/runner-images` Docker install harness.
