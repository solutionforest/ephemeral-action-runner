# Operations

## Logs

Host-side provider logs go under `work/logs` by default. Runner logs inside the Ubuntu guest are under:

- `/var/log/actions-runner/run.log`
- `/opt/actions-runner/_diag`

Guest provisioning command output is streamed to `work/logs/<instance-name>.guest.log`.

On systemd instances, the runner process is launched with `systemd-run` as `actions-runner.service` so provider `exec` calls return immediately after the service starts. On non-systemd instances such as Docker-DinD containers, EPAR starts `run.sh` in the background, writes `/var/run/actions-runner.pid`, and appends output to `/var/log/actions-runner/run.log`.

Docker-DinD containers also write inner Docker daemon logs to `/var/log/epar-dockerd.log` inside the runner container. Host-side Docker commands only show the outer runner container; job-created Compose resources live in the inner daemon.

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

Do not set `namePrefix` to a broad value such as `ubuntu` or `runner`.

For Docker-DinD, cleanup removes the outer runner container with `docker rm -f -v`. That also removes the private inner Docker daemon's containers, networks, volumes, and image cache for that EPAR instance.

## Troubleshooting

- If a Docker/browser or web/E2E image build fails before package installation, run `image update-upstream`.
- If Docker validation fails for a Docker-enabled image, inspect `work/logs/<image>.guest.log`.
- If browser validation fails on ARM64, confirm `epar-browser` exists inside the guest and inspect `/opt/epar/browser`.
- If a Docker Compose job uses an amd64-only runtime image on an ARM64 Tart runner and fails with `exec format error` or repeated container exits such as status `139`, use a runner label that supports that image instead of changing application runtime settings only for runner compatibility. Suitable targets include Docker-DinD with verified `linux/amd64` emulation, WSL x64, an x64 Linux host, or a Tart image with Rosetta enabled and validated.
- If a workflow uses fixed Compose project names, fixed container names, or fixed ports, Docker-DinD is often a better fit than a shared host Docker socket because each runner gets a private inner Docker daemon. Verify by starting two unregistered instances, running the same compose stack in both, and confirming host Docker only shows the outer EPAR runner containers.
- If GitHub registration fails, confirm the app has permission to manage organization self-hosted runners and that the private key path is readable by the host user.
- If stale runners remain, run `ephemeral-action-runner cleanup`.
- If using Tart `softnet`, verify the host has the privileges Tart requires.
- If WSL image build fails before systemd is ready, confirm WSL2 is enabled and that the clean Ubuntu rootfs was exported from an Ubuntu 24.04 WSL distro.
- If Docker-DinD startup fails, confirm the host Docker runtime supports privileged containers and inspect `/var/log/epar-dockerd.log` inside the runner container.
- If a Docker-DinD build fails before Docker is installed, run `image update-upstream` because Docker-DinD images use EPAR's pinned `actions/runner-images` Docker install harness.
