# Operations

## Logs

Host-side provider logs go under `work/logs` by default. Runner logs inside the Ubuntu guest are under:

- `/var/log/actions-runner/run.log`
- `/opt/actions-runner/_diag`

Guest provisioning command output is streamed to
`work/logs/<instance-name>.guest.log`. If runner launch or GitHub online
readiness fails, EPAR first appends bounded diagnostics to that host guest log:
runner PID/process state, tails from `run.log` and the latest `Runner_*.log`,
and the Docker-DinD daemon log when present. Diagnostic collection is
best-effort and does not replace the original readiness error.

On systemd instances, the runner process is launched with `systemd-run` as `actions-runner.service` so provider `exec` calls return immediately after the service starts. On non-systemd instances such as Docker-DinD containers, EPAR starts `run.sh` in the background, writes `/var/run/actions-runner.pid`, and appends output to `/var/log/actions-runner/run.log`.

Docker-DinD containers also write inner Docker daemon logs to `/var/log/epar-dockerd.log` inside the runner container. Host-side Docker commands only show the outer runner container; job-created Compose resources live in the inner daemon.

When `docker.registryMirrors` is configured, EPAR writes `/etc/docker/daemon.json` inside each instance before runtime validation. For Docker-DinD, inspect both `/etc/docker/daemon.json` and `/var/log/epar-dockerd.log` inside the outer runner container.

## Supervisor Exit

`pool up` cleans up prefixed instances and GitHub runner records when it exits. Use `--keep-on-exit` only when intentionally debugging a live instance after the supervisor stops. While the supervisor is not running, EPAR cannot retire or replace completed runners.

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
- If an image build fails with `E: You don't have enough free space in /var/cache/apt/archives/.`, check the Docker daemon or VM storage with `docker system df` and `docker run --rm gitea/runner-images:ubuntu-latest-full df -h /`. On Windows Docker Desktop with WSL2, the container-visible disk can be much smaller than Windows Explorer free space; see [Windows Docker Desktop WSL2 Disk Is Smaller Than Expected](troubleshooting.md#windows-docker-desktop-wsl2-disk-is-smaller-than-expected).
- If Docker validation fails for a Docker-enabled image, inspect `work/logs/<image>.guest.log`.
- If browser validation fails on ARM64, confirm `epar-browser` exists inside the guest and inspect `/opt/epar/browser`.
- If a Docker Compose job uses an amd64-only runtime image on an ARM64 Tart runner and fails with `exec format error` or repeated container exits such as status `139`, use a runner label that supports that image instead of changing application runtime settings only for runner compatibility. Suitable targets include Docker-DinD with verified `linux/amd64` emulation, WSL x64, an x64 Linux host, or a Tart image with Rosetta enabled and validated.
- If a workflow uses fixed Compose project names, fixed container names, or fixed ports, Docker-DinD is often a better fit than a shared host Docker socket because each runner gets a private inner Docker daemon. Verify by starting two unregistered instances, running the same compose stack in both, and confirming host Docker only shows the outer EPAR runner containers.
- If repeated jobs still pull slowly after configuring a registry mirror, verify the mirror is reachable from inside the runner instance and that it supports the requested registry, image platform, and authentication model. Docker daemon mirrors primarily target Docker Hub; other registry caches may require workflow image references to use the cache registry URL.
- If a mirrored workflow only improves modestly, check where the time is going. Registry mirrors mainly reduce image pull time; container startup, Compose health checks, database initialization, volume sync, browser tests, private image authentication, and CPU-bound or emulated workloads can still dominate the total job time.
- If GitHub registration fails, confirm the app has permission to manage organization self-hosted runners and that the private key path is readable by the host user.
- If stale runners remain, run `ephemeral-action-runner cleanup`.
- If using Tart `softnet`, verify the host has the privileges Tart requires.
- If default WSL image build fails before import, confirm Docker Desktop, Docker Engine, or another Docker daemon is reachable so EPAR can export `gitea/runner-images:ubuntu-latest-full` into a rootfs tar. For lean WSL configs, confirm the clean Ubuntu rootfs was exported from an Ubuntu 24.04 WSL distro.
- If WSL image build fails before systemd is ready, confirm WSL2 is enabled and inspect `work/logs/<image>.guest.log`.
- If Docker-DinD startup fails, confirm the host Docker runtime supports privileged containers and inspect `/var/log/epar-dockerd.log` inside the runner container.
- If Docker-DinD `docker run` fails with nested overlay mount errors, keep the default `EPAR_DOCKERD_STORAGE_DRIVER=vfs`. Only switch to `overlay2` or `auto` in a derived image after proving that storage driver works on the exact host runtime.
- If the default WSL or Docker-DinD build cannot validate Docker, confirm the source image still provides `docker`, `dockerd`, Compose, Buildx, and `iptables`. If you intentionally use a clean Ubuntu source image instead of Gitea's runner image, run `image update-upstream` first and use a config that installs Docker from EPAR's pinned `actions/runner-images` Docker install harness.
