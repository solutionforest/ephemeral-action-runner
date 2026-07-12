# Docker-DinD Provider

The Docker-DinD provider creates one privileged Ubuntu-based runner container per EPAR instance. That outer container starts its own private Docker daemon, and the GitHub Actions runner executes inside the same container.

This is useful when a host already has a reliable Docker runtime and you want disposable runner environments without creating full VMs. It is also useful for Docker Compose-heavy jobs because each runner instance has a separate inner Docker daemon. Deleting the EPAR container deletes that instance's job containers, networks, volumes, and inner image cache.

EPAR does not support a host Docker socket provider.

## When To Choose It

Choose Docker-DinD first for Docker-heavy Linux workflows when privileged containers are acceptable on the host. It is especially useful when the target repository already has Compose scripts, fixed project names, fixed internal ports, or amd64-only runtime images. In those cases, selecting a compatible runner label and Docker platform is usually cleaner than changing application runtime settings for CI compatibility.

Choose Tart or WSL instead when you specifically need their host model: Tart for VM-based Apple Silicon runners, WSL for Windows-hosted Linux runners, and x64 WSL/Linux hosts for native amd64 performance.

## Configuration

Use `configs/docker-dind.example.yml` for a base runner image:

```yaml
image:
  sourceType: docker-image
  sourceImage: ghcr.io/catthehacker/ubuntu:full-latest
  outputImage: epar-docker-dind-catthehacker-ubuntu

provider:
  type: docker-dind
  sourceImage: epar-docker-dind-catthehacker-ubuntu
  network: default
```

Use `configs/docker-dind.act.example.yml` for a smaller Docker-focused runner. Its Catthehacker Act base includes Node plus Docker Engine/CLI/Compose/Buildx, but does not guarantee a browser runtime:

```yaml
image:
  sourceType: docker-image
  sourceImage: ghcr.io/catthehacker/ubuntu:act-latest
  outputImage: epar-docker-dind-catthehacker-act

runner:
  labels: [self-hosted, linux, epar-docker-dind-catthehacker-act]

provider:
  sourceImage: epar-docker-dind-catthehacker-act
```

Use `configs/docker-dind.web-e2e.example.yml` as a smaller customized-image example. It starts from `ghcr.io/catthehacker/ubuntu:act-latest` and layers only the web/E2E add-on:

```yaml
image:
  sourceType: docker-image
  sourceImage: ghcr.io/catthehacker/ubuntu:act-latest
  outputImage: epar-docker-dind-catthehacker-ubuntu-web-e2e
  customInstallScripts:
    - scripts/guest/ubuntu/install-web-e2e.sh

runner:
  labels: [self-hosted, linux, epar-docker-dind-catthehacker-ubuntu-web-e2e]
  includeHostLabel: true

provider:
  sourceImage: epar-docker-dind-catthehacker-ubuntu-web-e2e
```

`provider.platform` is optional and maps to Docker's `--platform` flag for image builds and runner containers. Use a label that reflects the actual platform your workflows should target.

Optional Docker registry mirrors are configured under the provider-neutral `docker` section:

```yaml
docker:
  registryMirrors:
    - http://host.docker.internal:5050
```

When a Docker-DinD mirror URL uses `host.docker.internal`, EPAR adds Docker's `host-gateway` alias to the outer runner container so the inner daemon can reach a host-published mirror on Linux Docker Engine. See [Docker Registry Mirrors](../advanced/docker-registry-mirrors.md).

If the inner daemon must use an enterprise HTTP proxy, configure its startup
environment explicitly:

```yaml
docker:
  httpProxy: http://proxy.example.test:3128
  httpsProxy: http://proxy.example.test:3128
  noProxy: localhost,127.0.0.1,.example.test
```

EPAR sets these values on the outer container before it starts, allowing
`dockerd` to inherit them on its first launch. Empty values preserve direct
networking. Proxy URLs are limited to credential-free HTTP(S) roots; use network
controls rather than embedding proxy passwords. Put host-specific endpoints in
ignored `.local/config.yml`. If the proxy performs TLS inspection, also configure
the authorized root under `image.trustedCaCertificatePaths` so verified HTTPS
continues to work.

## Image Build

Docker-DinD images are Docker image tags, not Tart images or rootfs tar files:

```bash
./bin/ephemeral-action-runner image build --replace
docker image ls epar-docker-dind-catthehacker-ubuntu
```

The default build starts from `ghcr.io/catthehacker/ubuntu:full-latest`, installs the GitHub Actions runner and EPAR helper scripts, and reuses the base image's Docker Engine/CLI/Compose/Buildx. The generated image also includes `/opt/epar/container-entrypoint.sh`, which starts the private inner `dockerd` when the runner container starts.

Run `image update-upstream` only when selected install scripts need EPAR's pinned `actions/runner-images` checkout, such as the web/E2E script.

## Runtime Behavior

EPAR maps provider operations to Docker commands:

- clone/create: `docker create --privileged --label epar.provider=docker-dind ...`
- start: `docker start`, then wait for inner `docker info`
- exec: `docker exec`
- address: `docker inspect`
- stop: `docker stop`
- delete: `docker rm -f -v`
- list: `docker ps -a --filter label=epar.provider=docker-dind`

The provider does not mount `/var/run/docker.sock`, an OrbStack socket, or any host Docker socket into the runner container. It also does not publish host ports by default. If two jobs use the same Docker Compose project name or container ports, they are separated by their private inner Docker daemons.

The inner daemon starts with `EPAR_DOCKERD_STORAGE_DRIVER=vfs` by default. `vfs` is slower than `overlay2`, but it is the most reliable default for nested Docker on Docker Desktop, OrbStack, and other privileged-container hosts where overlay mounts can fail inside the runner container. Advanced users can bake `EPAR_DOCKERD_STORAGE_DRIVER=overlay2` or `EPAR_DOCKERD_STORAGE_DRIVER=auto` into a derived image after validating that the exact host runtime supports it.

On Apple Silicon hosts using Docker Desktop or OrbStack, the inner daemon may be able to run `linux/amd64` containers through the host runtime's emulation support. Validate this on the exact host before routing amd64-only workflows to Docker-DinD:

```bash
docker exec <epar-instance> docker run --rm --platform linux/amd64 alpine:3.20 uname -m
```

Expected output:

```text
x86_64
```

The runner process uses EPAR's non-systemd fallback:

- `/opt/epar/run-runner.sh` starts `/opt/actions-runner/run.sh` in the background.
- `/var/run/actions-runner.pid` records the runner PID.
- `/opt/epar/check-runner.sh` reports liveness.
- `/var/log/actions-runner/run.log` records runner output.

## Verification

Local runtime check without GitHub registration:

```bash
./bin/ephemeral-action-runner pool verify --instances 1 --cleanup
```

Full registration check:

```bash
./bin/ephemeral-action-runner pool verify --instances 2 --register-only --cleanup
```

Dry-run command construction:

```bash
./bin/ephemeral-action-runner pool verify --dry-run --instances 1
```

The dry run should show `docker create` with `--privileged` and no host socket mount.

For Docker Compose-heavy jobs that use fixed project names or ports, a useful isolation smoke test is to start two unregistered Docker-DinD instances, run the same compose stack in both with the same project name, and confirm the host Docker daemon only shows the two outer EPAR containers. The job-created containers should appear only when you run `docker exec <epar-instance> docker ps` against each instance.

## Caveats

- Docker-DinD requires privileged containers. Treat it as trusted-job infrastructure.
- It is not a security boundary for hostile code.
- Inner Docker image cache is per runner instance and disappears on cleanup.
- Optional registry mirrors can reduce repeated pull time, but they are external services that must be secured and monitored separately.
- Cross-architecture containers, for example `linux/amd64` images on an ARM64 host, depend on the host Docker runtime's emulation support.
- Host Docker resource usage still matters because each runner container and inner daemon consumes CPU, memory, and disk on the same host.
- Docker Desktop, OrbStack, and Linux Docker Engine can have different privileged-container behavior. Validate on the exact host runtime you plan to use.
