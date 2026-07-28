# Troubleshooting

Start with the symptom that most closely matches the failure. EPAR is trusted-job infrastructure: keep TLS verification enabled, preserve the first relevant log, and do not use broad Docker/WSL resets or prune commands as a first response.

## Contents

- [Quick diagnostics](#quick-diagnostics)
- [Windows no-Go startup prints an HTTP/2 named-pipe diagnostic](#windows-no-go-startup-prints-an-http2-named-pipe-diagnostic)
- [A Docker workload fails with an architecture error](#a-docker-workload-fails-with-an-architecture-error)
- [Docker Sandboxes is unavailable or its preflight fails](#docker-sandboxes-is-unavailable-or-its-preflight-fails)
- [Docker Sandboxes rejects template, policy, or capacity](#docker-sandboxes-rejects-template-policy-or-capacity)
- [A runner is held for diagnostics or an acknowledgement](#a-runner-is-held-for-diagnostics-or-an-acknowledgement)
- [Docker image build runs out of space](#docker-image-build-runs-out-of-space)
- [Docker image build fails with TLS certificate errors](#docker-image-build-fails-with-tls-certificate-errors)
- [Windows Docker Desktop WSL2 disk is smaller than expected](#windows-docker-desktop-wsl2-disk-is-smaller-than-expected)
- [Docker Container startup fails](#docker-container-startup-fails)
- [WSL provider image build fails early](#wsl-provider-image-build-fails-early)
- [GitHub runner registration fails](#github-runner-registration-fails)

## Quick diagnostics

EPAR writes logs under `work/logs` by default. Start with `work/logs/epar-last-error.log`, then inspect the matching build log in `work/logs/builds/` or instance transcript in `work/logs/instances/`. Manager events are console-only by default; raw transcripts are file-only unless `logging.transcriptSinks` includes `console`.

```bash
./start --help
go run ./cmd/ephemeral-action-runner version
docker version
docker info
docker system df
```

Without local Go, use `./start --help`; the wrapper selects the containerized toolchain. For Windows WSL2, Docker Desktop's WSL2 backend, or the WSL provider, also run:

```powershell
wsl --version
wsl -l -v
docker context ls
docker run --rm ghcr.io/catthehacker/ubuntu:full-latest df -h /
```

Container-visible free space is the relevant value for Docker builds. Windows Explorer or Finder free space does not necessarily equal the free space in Docker Desktop, OrbStack, or another Linux VM backing the daemon.

## Windows no-Go startup prints an HTTP/2 named-pipe diagnostic

### Symptom

The Windows no-Go bootstrap prints a line like:

```text
http2: server: error reading preface from client //./pipe/dockerDesktopLinuxEngine: file has already been closed
```

### Diagnosis and remediation

Docker Desktop can emit this named-pipe transport diagnostic when a client connection closes. If the bootstrap `docker build` succeeds and the wizard or command continues, it is not an EPAR runner-group or GitHub API failure. The wrapper suppresses only this exact successful-build diagnostic and keeps full Docker stderr when the build fails.

If startup stops, verify the selected engine and context:

```powershell
docker version
docker info
docker context show
```

Resolve a failed command or unhealthy engine; do not treat every named-pipe line as harmless when Docker returned a nonzero exit code.

## A Docker workload fails with an architecture error

### Symptom

Docker or Compose exits with `exec format error`, `cannot execute binary file`, a platform-mismatch warning, `no matching manifest`, a QEMU loader error, or exit code `139`.

### Diagnosis and remediation

Inspect the runner, daemon, image manifest, and Compose platform setting:

```bash
uname -m
docker info --format '{{.OSType}}/{{.Architecture}}'
docker image inspect --format '{{.Os}}/{{.Architecture}}' IMAGE
docker buildx imagetools inspect IMAGE
docker compose config
```

`no matching manifest` means the image does not publish the requested platform; emulation cannot create a missing manifest. A platform warning alone does not prove failure, and exit code `139` alone does not prove an architecture mismatch. Use the exact image and workload evidence. For the architecture model, QEMU setup, provider scope, and verification commands, see [Cross-architecture containers](advanced/cross-architecture-containers.md).

## Docker Sandboxes is unavailable or its preflight fails

### Symptom

The wizard marks Docker Sandboxes unavailable, or `sbx diagnose --output json` reports failures.

### Diagnosis and remediation

Check the installed CLI and the diagnostic result before editing configuration:

```bash
sbx version
sbx diagnose --output json
```

EPAR requires the exact supported `sbx` version, a controller architecture with an available Linux guest template, and at least one diagnostic pass with zero failures. Diagnostic warnings remain visible but do not by themselves disable the provider. Fix the failed prerequisite, rerun the diagnostic, then restart `./start`; do not manually force a provider selection or substitute Docker Container for a configured Docker Sandboxes pool.

## Docker Sandboxes rejects template, policy, or capacity

### Symptom

Startup reports a template identity/digest mismatch, policy-generation drift, an admission failure, or insufficient capacity.

### Diagnosis and remediation

Docker Sandboxes uses an explicit local Candidate A template, full OCI configuration digest, and host-global Balanced-policy fingerprint. A mutable tag, a template copied from another host, a changed template digest, or a changed policy generation is intentionally rejected. Return to the wizard or the documented template promotion procedure to obtain a fresh, locally verified configuration; do not weaken the fingerprint or replace it with a guessed value.

Capacity admission accounts for the configured root disk, inner Docker disk, per-sandbox memory, concurrent creation limit, and host-free reserve. Inspect the storage location reported by the failure and free or expand the relevant backing storage. The minimums are `20GiB` root disk, `100GiB` Docker disk, and `50GiB` host free space; runtime can require more. Avoid broad cleanup commands: they can delete stopped containers and intentionally retained resources.

`networkBaseline: open` is a sandbox-scoped public-egress compatibility rule with EPAR host-alias deny guardrails. It does not alter the host-global policy. If a required service is blocked, use a narrow `additionalAllow` hostname rule; do not allow `host.docker.internal`, `gateway.docker.internal`, `kubernetes.docker.internal`, or `host.containers.internal` through the Open-policy guardrails.

## A runner is held for diagnostics or an acknowledgement

### Symptom

An instance is retained, quarantined, or shown as requiring an acknowledgement after a provisioning, policy, or runtime failure.

### Diagnosis and remediation

Preserve the instance and inspect `work/logs/instances/<instance>.guest.log`, the matching runner diagnostics, and controller output before acknowledging or removing it. EPAR deliberately keeps uncertain ownership, failed cleanup, and unverified remote state inside the strict `pool.instances` cap instead of creating a replacement storm.

If an incident requires stopping new work immediately, stop the controller with `Ctrl-C` or the service manager that launched it. This prevents replacement; it does not erase retained evidence. Use the configured EPAR cleanup command only after identifying the exact affected pool. Do not use a broad `docker system prune`, WSL unregister, or reset as an incident-disable switch.

Set `EPAR_DISABLE_DOCKER_SANDBOXES=1` before starting EPAR when Docker Sandboxes admission must remain disabled during an incident or compatibility investigation. This fails the provider closed without changing configuration or deleting evidence.

After reviewing retained Docker Sandboxes diagnostics, acknowledge that review only for the exact configured pool:

```bash
ephemeral-action-runner cleanup --acknowledge-failed-diagnostics
```

## Docker image build runs out of space

### Symptom

`start` or `image build` reports `No space left on device` or `E: You don't have enough free space in /var/cache/apt/archives/.`.

### Diagnosis and remediation

The temporary guest or Docker writable layer is full; this does not necessarily mean the host OS drive is full. Inspect the active Docker daemon:

```bash
docker run --rm ghcr.io/catthehacker/ubuntu:full-latest df -h /
docker system df
docker system df -v
```

Increase the relevant Docker/VM data-disk allocation or deliberately remove unneeded data after reviewing it. Docker prune commands can remove stopped containers, unused images, build cache, networks, and volumes; they are not a safe generic fix.

## Docker image build fails with TLS certificate errors

### Symptom

HTTPS access fails with `curl: (60)`, `certificate verification failed`, or an unknown issuer during an EPAR build or job.

### Diagnosis and remediation

Do not disable certificate verification. Configure host trust overlay for an ephemeral runner and rebuild the image:

```yaml
image:
  hostTrustMode: overlay
  hostTrustScopes: [system, user]
```

Use `[system]` on Linux. Overlay mode collects the current host roots, validates them before registration, and combines them with Ubuntu roots and any `image.trustedCaCertificatePaths`; it is root-anchor inheritance rather than exact Windows/macOS TLS-policy emulation. It requires `runner.ephemeral: true`.

Use the normal host entry point so EPAR can inspect the real Windows certificate stores or macOS Keychain:

```powershell
./start
go run ./cmd/ephemeral-action-runner image build --replace
```

On no-Go Windows, use `scripts\run-with-docker.ps1 image build --replace`; on macOS/Linux, use `scripts/run-with-docker.sh image build --replace`. A bare Linux toolchain container is not a replacement for the host-trust bridge. If the source-image `docker pull` itself fails, configure the authorized CA in Docker Desktop, OrbStack, or Docker Engine first.

## Windows Docker Desktop WSL2 disk is smaller than expected

### Symptom

`docker run --rm ghcr.io/catthehacker/ubuntu:full-latest df -h /` shows much less capacity than Windows Explorer.

### Diagnosis and remediation

Docker Desktop stores Linux container data in a WSL-backed virtual disk. Older WSL2 installations can have a smaller default VHD maximum than newer ones, but the reported container filesystem is the evidence that matters for image pulls and builds. Inspect Docker usage first, then change Docker Desktop/WSL storage using the product's supported settings. See [Microsoft WSL disk-space guidance](https://learn.microsoft.com/windows/wsl/disk-space) and [Docker Desktop WSL guidance](https://docs.docker.com/desktop/features/wsl/).

## Docker Container startup fails

### Privileged containers

Docker Container requires a host Docker runtime that permits privileged Linux containers:

```bash
docker run --rm --privileged alpine:3.20 true
```

### Nested Docker storage driver

If nested Docker operations fail with overlay-mount errors, retain the default inner storage driver:

```text
EPAR_DOCKERD_STORAGE_DRIVER=vfs
```

Use `overlay2` or `auto` only in a derived image after proving it works on the exact host runtime.

## WSL provider image build fails early

### Symptom

The WSL image build fails before import, during import with `0xffffffff`, or before systemd is ready.

### Diagnosis and remediation

The default WSL build obtains a Docker source image before importing it into WSL. Verify Docker and WSL first:

```powershell
docker version
docker pull ghcr.io/catthehacker/ubuntu:full-latest
wsl -l -v
```

For `Wsl/Service/CreateInstance/E_UNEXPECTED`, `Catastrophic failure`, or import exit `0xffffffff`, stop EPAR, save work in other distros, then run `wsl --shutdown`. This stops every running WSL distro, including Docker Desktop's backend. Restart Docker Desktop, verify a normal distro command returns `0`, then rerun `./start`; a matching cached source rootfs is reused. If it persists, update WSL, shut it down again, reboot, and consult [Microsoft's WSL troubleshooting guidance](https://learn.microsoft.com/windows/wsl/troubleshooting#error-code-0x8000ffff-unexpected-failure).

If a guest exists but systemd does not become ready, inspect `work/logs/builds/<image>.wsl-build.log` and `work/logs/builds/<temporary-distro>.guest.log`. Do not unregister a distro until you have identified the exact EPAR-owned target and accepted that unregistration is irreversible.

## GitHub runner registration fails

### Symptom

EPAR cannot request a registration token, add a runner to a group, or observe the runner online.

### Diagnosis and remediation

Verify GitHub App organization self-hosted-runner read/write permission and a readable private key:

```yaml
github:
  appId: 123456
  organization: your-org
  privateKeyPath: .local/github-app.pem
```

Then inspect runner-group policy and the first registration error. A strict policy can intentionally block a group that is default, overly broad, or public-repository enabled. See [Runner Group Security](runner-groups.md).

For a confirmed stale EPAR resource, run the configured cleanup command:

```bash
go run ./cmd/ephemeral-action-runner cleanup
```

Cleanup is bounded by the configured pool and durable exact lifecycle identities; it does not authorize a broad prefix deletion, wildcard, Docker prune, or removal of unknown/shared resources. Keep `pool.namePrefix` unique per controller and organization.
