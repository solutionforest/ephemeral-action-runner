# Cross-architecture containers

Use this guide when a trusted EPAR job must run a container image whose CPU architecture differs from the Linux Docker daemon that executes it. It explains the boundary between image selection, emulation, runner labels, and provider support.

## Start with evidence

Determine the runner and daemon architecture, inspect the image manifest, and inspect any Compose override before choosing a workaround:

```bash
uname -m
docker info --format '{{.OSType}}/{{.Architecture}}'
docker image inspect --format '{{.Os}}/{{.Architecture}}' IMAGE
docker buildx imagetools inspect IMAGE
docker compose config
```

An x64 Linux daemon normally runs `linux/amd64` images natively; an ARM64 daemon normally runs `linux/arm64` images natively. A `platform:` value in Compose can override the image's normal selection. Pulling or loading a foreign image proves only that the daemon obtained it, not that it can execute it.

| Symptom | Meaning | Correct next action |
| --- | --- | --- |
| `no matching manifest for linux/amd64` or `linux/arm64` | The registry does not publish the requested platform. | Choose an available image/platform or publish a multi-platform image. QEMU cannot create a missing manifest. |
| `exec format error` or `cannot execute binary file` | The daemon tried to launch an incompatible executable without a usable handler. | Confirm the selected platform and install/verify an emulator only if the workload supports it. |
| `qemu-x86_64: Could not open '/lib64/ld-linux-x86-64.so.2'` | Translation started but the foreign userspace/loader is missing or incompatible. | Use a compatible image or native runner; binfmt registration alone is insufficient. |
| Exit code `139` | A process segfaulted. | Treat architecture as one hypothesis, not proof; inspect the workload log and run a minimal container test. |
| Docker platform warning | Requested and detected platforms differ. | Verify execution; the warning alone is not a failure. |

## Set up and verify Linux user-mode emulation

For a trusted Linux job that intentionally runs a foreign Linux container, configure QEMU/binfmt before the first foreign container. Pin the action and helper image according to the repository's dependency policy:

```yaml
jobs:
  test:
    runs-on: [self-hosted, linux, epar-docker-container-catthehacker-ubuntu]
    steps:
      - name: Set up ARM64 container emulation
        uses: docker/setup-qemu-action@96fe6ef7f33517b61c61be40b68a1882f3264fb8 # v4
        with:
          image: docker.io/tonistiigi/binfmt@sha256:400a4873b838d1b89194d982c45e5fb3cda4593fbfd7e08a02e76b03b21166f0
          platforms: arm64

      - name: Verify the foreign container
        run: docker run --rm --platform linux/arm64 alpine:3.22 uname -m
```

The verification should print `aarch64`. Select only the foreign platforms that the workflow needs. QEMU/binfmt translates Linux user-space executables; it does not change the runner CPU, create a foreign VM, make arbitrary host programs compatible, or guarantee workload performance. Emulated builds, databases, browsers, and compute-heavy tools can be slower or unsupported.

The setup helper is privileged. Use it only in trusted workflows and treat its pinned action/image revisions as reviewed dependencies. Prefer a native matching runner whenever compatibility, performance, or a security boundary matters.

## Provider and platform scope

| Execution surface | What to do |
| --- | --- |
| Docker Container | Run the setup action inside the disposable job before Docker or Compose uses a foreign image. No EPAR configuration key enables universal emulation. |
| WSL | Run the setup action inside the WSL runner if its Linux Docker daemon needs a foreign image. An x64 WSL runner does not gain ARM64 execution merely by pulling an ARM64 image. |
| Tart on Apple Silicon | The guest is ARM64. The optional Rosetta path is experimental and not equivalent to QEMU/binfmt. Use a distinct label and validate the exact image/workload. |
| Docker Sandboxes | The pinned template and `provider.platform` determine the guest architecture. Treat unsupported host/template combinations as preview-only and use only independently validated combinations. |
| GitHub-hosted Windows or macOS | These labels do not replace a Linux Docker daemon for container actions or service containers. Use a suitable Linux execution surface. |

Keep architecture-specific jobs on a distinct `runs-on` label. Do not label an ARM64 runner as `ubuntu-latest`: GitHub's `ubuntu-latest` is a GitHub-managed environment, and x64 assumptions can fail on ARM64.

## Operational examples

For an amd64-only service on an ARM64 host, first try a published ARM64 or multi-platform image. If none exists, use a trusted Linux runner with the emulation setup above, then prove the actual service starts and passes its health check. If the service is performance-sensitive or fails under emulation, route it to a native x64 Docker Container, WSL x64, or another native x64 Linux runner instead.

For an ARM64 image on an x64 Linux runner, follow the same process with `platforms: arm64` and `--platform linux/arm64`. Never treat a successful `docker pull` as the proof; run a container and check both the expected architecture output and the real workload.

## References

- [Docker Setup QEMU action](https://github.com/docker/setup-qemu-action)
- [Docker multi-platform build strategies](https://docs.docker.com/build/building/multi-platform/)
- [GitHub-hosted runner labels and limitations](https://docs.github.com/en/actions/reference/runners/github-hosted-runners)
- [GitHub self-hosted runner container requirements](https://docs.github.com/en/actions/reference/runners/self-hosted-runners#requirements-for-self-hosted-runner-machines)

