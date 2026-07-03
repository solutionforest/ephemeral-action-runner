# WSL Provider

The WSL provider targets Windows hosts running WSL2. It manages disposable
Ubuntu distros for trusted GitHub Actions jobs.

The provider maps EPAR lifecycle operations to `wsl.exe`:

- clone/create: `wsl --import <name> <install-dir> <rootfs.tar> --version 2`
- start/exec: `wsl -d <name> --user root --exec <command>`
- stop: `wsl --terminate <name>`
- delete: `wsl --unregister <name>`
- export image: `wsl --export <name> <rootfs.tar>`
- list: `wsl --list --verbose`

When a disposable runner is started, EPAR also keeps a quiet host-side
`wsl.exe -d <name>` process open. This prevents WSL from auto-stopping an
imported distro that is otherwise only running systemd services. `pool up`,
`pool verify --cleanup`, and `cleanup` terminate that keepalive by terminating
or unregistering the distro.

## Configuration

Use `configs/wsl.example.yml` as the starting point:

```yaml
image:
  sourceImage: work/images/ubuntu-24.04-clean.rootfs.tar
  outputImage: work/images/epar-ubuntu-24-wsl.tar

provider:
  type: wsl
  sourceImage: work/images/epar-ubuntu-24-wsl.tar
  installRoot: work/wsl
```

`image.sourceImage` is the clean Ubuntu tar used only for image building.
`image.outputImage` is the reusable runner tar produced by `image build`.
`provider.sourceImage` is the tar imported for disposable runner instances.

## Systemd And Docker

The WSL image build writes `/etc/wsl.conf` with systemd enabled, restarts the
temporary distro, then installs Docker Engine and the GitHub Actions runner
inside the distro. EPAR validates the built image with:

```bash
docker info
docker run --rm hello-world
chromium --headless --no-sandbox --dump-dom https://www.w3.org/
```

The provider does not mount the Windows Docker Desktop socket. Jobs run against
Docker Engine inside the WSL distro.

## Caveats

- WSL2 is not the same isolation boundary as a full VM per job.
- WSL distros share the WSL kernel and host integration surface.
- Use this provider for trusted internal jobs unless your environment has
  reviewed and accepted the isolation model.
- Expect one long-lived host `wsl.exe` process per running disposable runner.
  This is intentional and keeps the WSL distro alive while it waits for jobs.
- Cleanup only unregisters distros whose names match `pool.namePrefix`.

References:

- [WSL basic commands](https://learn.microsoft.com/en-us/windows/wsl/basic-commands)
- [Systemd support in WSL](https://learn.microsoft.com/en-us/windows/wsl/systemd)
