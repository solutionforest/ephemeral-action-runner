# Docker Container Provider

Docker Container creates one privileged Ubuntu runner container per EPAR instance. The outer container starts a private inner Docker daemon, so workflow containers, networks, volumes, and image cache stay inside that disposable runner.

## When To Use It

Choose Docker Container on a Docker-capable host when privileged containers are acceptable and you want strong per-runner Docker resource separation. It is a practical fit for Compose-heavy jobs, including jobs that reuse fixed Compose project names or ports. EPAR does not support a host-Docker-socket provider.

## Support Status

This is a supported provider on hosts whose Docker runtime can run privileged Linux containers. It is trusted-job infrastructure: `--privileged` weakens the normal container boundary, so do not use it for arbitrary untrusted workflow code.

## Prerequisites

- A working Docker-compatible daemon that permits `docker run --privileged`.
- Enough host Docker storage for the reusable image and the requested disposable runners.
- A GitHub App and runner group configured as described in [Runner Group Security](../runner-groups.md).

## Minimal Configuration

Start with [`configs/docker-container.example.yml`](../../configs/docker-container.example.yml):

```yaml
image:
  sourceType: docker-image
  sourceImage: ghcr.io/catthehacker/ubuntu:full-latest
  outputImage: epar-docker-container-catthehacker-ubuntu

provider:
  type: docker-container
  sourceImage: epar-docker-container-catthehacker-ubuntu
  network: default
```

`provider.platform` is optional and maps to Docker's `--platform` for the reusable image and runner containers. Give cross-architecture configurations a distinct workflow label and verify actual execution on the intended host.

Use `configs/docker-container.act.example.yml` for a smaller Docker-focused Catthehacker base, or `configs/docker-container.web-e2e.example.yml` when browser/E2E tooling is required. The full configuration, host-trust settings, proxies, and registry mirrors belong in [Configuration](../configuration.md).

## Normal Workflow

1. Create a local configuration with the wizard or copy an example.
2. Run `./start`. EPAR builds or refreshes the reusable image when its manifest no longer matches the configuration, then starts the pool.
3. Target the configured label in a workflow, for example `runs-on: [self-hosted, linux, epar-docker-container-catthehacker-ubuntu]`.

The outer container has no host Docker socket mount and does not publish host ports by default. The inner daemon defaults to the reliable nested-Docker `vfs` storage driver. Use a different `EPAR_DOCKERD_STORAGE_DRIVER` only in a derived image after validating the exact host runtime.

## Limitations

- The inner Docker daemon is private, but its CPU, memory, and disk use still comes from the host.
- The runner's inner image cache disappears with the instance.
- Docker Desktop, OrbStack, and Linux Docker Engine can differ in privileged-container and foreign-architecture behavior.
- Registry mirrors and proxy services are external infrastructure; EPAR configures the runner daemon but does not operate or secure those services.

## Verification

```bash
ephemeral-action-runner pool verify --instances 1 --cleanup
ephemeral-action-runner pool verify --instances 1 --register-only --cleanup
```

For an ARM64 host that must run amd64 Docker images, verify execution inside a live EPAR runner rather than relying on image pull success:

```bash
docker exec <epar-instance> docker run --rm --platform linux/amd64 alpine:3.20 uname -m
```

Expected output is `x86_64`.

## Troubleshooting

See [Troubleshooting](../troubleshooting.md) for privileged-container checks, nested-Docker storage-driver failures, architecture emulation, disk pressure, and TLS errors.
