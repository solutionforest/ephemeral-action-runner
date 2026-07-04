# Docker Registry Mirrors

EPAR can optionally configure Docker registry mirrors inside each disposable runner instance. This is useful when repeated jobs spend time pulling the same Docker Hub images into fresh Docker daemons.

This feature is disabled by default. If `docker.registryMirrors` is empty, EPAR leaves Docker daemon configuration unchanged.

## Expected Impact

Registry mirrors mostly help the Docker image pull portion of a job. They do not speed up application startup, database initialization, volume sync, browser tests, health checks, amd64 emulation, or other work that happens after the images are already available in the runner's Docker daemon.

The improvement varies case by case. Jobs that repeatedly pull large public Docker Hub images into fresh EPAR instances can improve noticeably once the mirror is warm. Jobs dominated by private image pulls, container startup, CPU-bound work, or integration-test waiting may only improve modestly.

Treat this as an optional optimization, not a required fast path. EPAR works normally without a mirror.

## Prerequisite: A Running Mirror Service

`docker.registryMirrors` only tells the Docker daemon inside each EPAR runner where to look for a mirror. It does not create the mirror.

Before enabling this config, provide one of these:

- a local mirror container running on the same host as EPAR;
- a mirror service running on another machine in the same LAN or intranet;
- a managed registry cache, such as a cloud registry pull-through cache.

For a local mirror on the EPAR host, Docker Engine, Docker Desktop, or OrbStack is enough to run the mirror container. No extra EPAR package is required.

For an intranet mirror, runners should use the mirror's LAN DNS name or IP address. This is often better for multiple office machines because all EPAR hosts can share one warm cache.

The mirror must be reachable from the runner instance, not only from the host shell.

## What EPAR Configures

Add mirror URLs to your ignored local config:

```yaml
docker:
  registryMirrors:
    - http://host.docker.internal:5050
```

When a runner instance starts, EPAR copies `/opt/epar/configure-docker-daemon.sh` into the instance, writes `/etc/docker/daemon.json`, and reloads or restarts Docker before runtime validation.

The generated daemon config is equivalent to:

```json
{
  "registry-mirrors": ["http://host.docker.internal:5050"]
}
```

The same config surface works for Docker-DinD, Tart, and WSL when Docker is installed in the runner instance.

## What EPAR Does Not Run

EPAR does not start or manage the mirror service. You can use any mirror that Docker Engine can reach, such as:

- a local Docker Hub pull-through cache;
- an organization-managed registry cache;
- a cloud registry cache, such as Amazon ECR pull-through cache.

If the mirror is not running or is not reachable from the runner, Docker falls back or fails according to Docker Engine's normal mirror behavior.

## Local Docker Hub Cache

For local development, a Docker Hub pull-through cache can run on the same host as EPAR. Docker, Docker Desktop, or OrbStack is enough to run the mirror container; no extra EPAR dependency is required.

For a quick public-image cache:

```bash
docker run -d \
  --name epar-dockerhub-cache \
  --restart unless-stopped \
  -p 5050:5000 \
  -e REGISTRY_PROXY_REMOTEURL=https://registry-1.docker.io \
  -v epar-dockerhub-cache:/var/lib/registry \
  registry:2
```

Check that the host port reaches the registry cache before pointing runners at it:

```bash
curl -i http://127.0.0.1:5050/v2/
```

The response should include `Docker-Distribution-Api-Version: registry/2.0`. On macOS, port `5000` can collide with AirPlay/AirTunes and return `Server: AirTunes/...`; in that case, choose another host port such as `5050`.

A file-based registry config for the same public-image cache looks like:

```yaml
version: 0.1
storage:
  filesystem:
    rootdirectory: /var/lib/registry
  delete:
    enabled: true
http:
  addr: :5000
proxy:
  remoteurl: https://registry-1.docker.io
```

Run the cache with your chosen storage path and publish port, then configure EPAR with the address reachable from the runner:

```yaml
docker:
  registryMirrors:
    - http://host.docker.internal:5050
```

For Docker-DinD, EPAR adds Docker's `host.docker.internal:host-gateway` alias when any configured mirror uses `host.docker.internal`. On macOS Docker Desktop and OrbStack this name is usually already available; on Linux Docker Engine the alias helps runner containers reach a host-published mirror.

For Tart and WSL, `host.docker.internal` may not resolve the way it does in Docker containers. Use a LAN address, DNS name, or other route that is reachable from the guest.

## Provider Addressing

The mirror URL must be valid from the runner instance's point of view:

| Provider | Same-host mirror URL guidance |
| --- | --- |
| Docker-DinD | `http://host.docker.internal:5050` is a good one-machine choice when the local cache publishes host port `5050`. EPAR adds Docker's `host-gateway` alias for this name when needed. |
| Tart | Use an IP address or DNS name reachable from inside the VM, such as the host's LAN IP or an intranet DNS name. `host.docker.internal` is not guaranteed. |
| WSL | Use an address reachable from inside the WSL distro. Depending on Windows and WSL networking, this may be the Windows host address, a LAN IP, or an intranet DNS name. `host.docker.internal` is not guaranteed. |

For a shared office cache, prefer a stable LAN DNS name:

```yaml
docker:
  registryMirrors:
    - http://docker-cache.office.example:5000
```

For one-machine Docker-DinD development, a host-published local cache is usually enough:

```yaml
docker:
  registryMirrors:
    - http://host.docker.internal:5050
```

## Private Images

A mirror cannot bypass registry authorization.

For Docker Hub private images, keep doing `docker login` inside the GitHub Actions job with repository or organization secrets. Host-side `docker login` is not copied into EPAR runners, and EPAR does not bake Docker credentials into images.

Private pulls can use a mirror in two common ways:

- The workflow logs in inside the runner, and the selected mirror/proxy supports authenticated private pulls.
- The mirror itself is configured with upstream Docker Hub credentials.

The second option is sensitive. Docker's mirror documentation warns that private resources available to the configured Docker Hub user become available through that mirror unless you secure it. If you use mirror-side credentials, protect the mirror with authentication, network controls, and least-privilege registry credentials.

If neither the runner job nor the mirror has credentials for a private image, the pull should fail.

## Docker Hub Versus Other Registries

Docker Engine's `registry-mirrors` setting is primarily for Docker Hub mirrors. It does not transparently rewrite arbitrary image references such as `ghcr.io/...` or cloud-registry names.

For other upstream registries, prefer the registry's own pull-through cache feature and route workflow image names to that cache explicitly. For example, Amazon ECR pull-through cache can sync from Docker Hub, GitHub Container Registry, GitLab Container Registry, Quay, Kubernetes registry, Chainguard, Amazon ECR Public, and other ECR registries, but authenticated upstreams require AWS-side credentials.

EPAR intentionally does not rewrite Docker image names inside workflows. That avoids surprising security and debugging behavior.

## Verification

Start a runner instance with mirrors configured:

```bash
./bin/ephemeral-action-runner pool verify --instances 1 --cleanup
```

For Docker-DinD, inspect the inner daemon:

```bash
docker exec <epar-instance> docker info
docker exec <epar-instance> cat /etc/docker/daemon.json
```

For Tart or WSL, inspect the guest with the provider's normal shell/exec workflow and check:

```bash
docker info
cat /etc/docker/daemon.json
```

To test caching, pull the same image from two fresh runner instances and inspect the mirror logs. The second pull should reuse cached layers when the mirror supports the image and platform being requested.

## References

- [Docker: Mirror the Docker Hub library](https://docs.docker.com/docker-hub/image-library/mirror/)
- [Docker dockerd configuration reference](https://docs.docker.com/reference/cli/dockerd/)
- [Amazon ECR pull-through cache](https://docs.aws.amazon.com/AmazonECR/latest/userguide/pull-through-cache.html)
