# Operations

## Logs

Host-side provider logs go under `work/logs` by default. Runner logs inside the
Ubuntu guest are under:

- `/var/log/actions-runner/run.log`
- `/opt/actions-runner/_diag`

Guest provisioning command output is streamed to
`work/logs/<instance-name>.guest.log`.

The runner process is launched with `systemd-run` as `actions-runner.service` so
provider `exec` calls return immediately after the service starts.

## Supervisor Exit

`pool up` cleans up prefixed instances and GitHub runner records when it exits.
Use `--keep-on-exit` only when intentionally debugging a live instance after the
supervisor stops. While the supervisor is not running, EPAR cannot retire or
replace completed runners.

## Cleanup Safety

Cleanup only touches local instances and GitHub runner records matching
`pool.namePrefix`:

```yaml
pool:
  namePrefix: epar-tart
```

Generated names look like:

```text
epar-tart-20260703-010500-001
```

Do not set `namePrefix` to a broad value such as `ubuntu` or `runner`.

## Troubleshooting

- If image build fails before package installation, run `image update-upstream`.
- If Docker validation fails, inspect `work/logs/<image>.guest.log`.
- If browser validation fails on ARM64, confirm `epar-browser` exists inside the
  guest and inspect `/opt/epar/browser`.
- If GitHub registration fails, confirm the app has permission to manage
  organization self-hosted runners and that the private key path is readable by
  the host user.
- If stale runners remain, run `ephemeral-action-runner cleanup`.
- If using Tart `softnet`, verify the host has the privileges Tart requires.
- If WSL image build fails before systemd is ready, confirm WSL2 is enabled and
  that the clean Ubuntu rootfs was exported from an Ubuntu 24.04 WSL distro.
