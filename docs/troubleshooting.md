# Troubleshooting

Start with the symptom that most closely matches the failure. Regardless of provider, keep TLS verification enabled, preserve the first relevant log, and do not use broad Docker/WSL resets or prune commands as a first response.

## Contents

- [Quick diagnostics](#quick-diagnostics)
- [Windows no-Go startup prints an HTTP/2 named-pipe diagnostic](#windows-no-go-startup-prints-an-http2-named-pipe-diagnostic)
- [A Docker workload fails with an architecture error](#a-docker-workload-fails-with-an-architecture-error)
- [Docker Sandboxes is unavailable or its preflight fails](#docker-sandboxes-is-unavailable-or-its-preflight-fails)
- [Docker Sandboxes rejects template, policy, or capacity](#docker-sandboxes-rejects-template-policy-or-capacity)
- [Docker Sandboxes creation fails after a runtime-helper prompt](#docker-sandboxes-creation-fails-after-a-runtime-helper-prompt)
- [Docker Sandboxes rejects a staging workspace because SSH-agent forwarding is present](#docker-sandboxes-rejects-a-staging-workspace-because-ssh-agent-forwarding-is-present)
- [Docker Hub login succeeds but a private pull is denied in Docker Sandboxes](#docker-hub-login-succeeds-but-a-private-pull-is-denied-in-docker-sandboxes)
- [An idle runner reports GitHub or Sandbox health warnings](#an-idle-runner-reports-github-or-sandbox-health-warnings)
- [GitHub Actions runner release resolution reports HTTP 403 or 429](#github-actions-runner-release-resolution-reports-http-403-or-429)
- [A scheduled image check or update fails](#a-scheduled-image-check-or-update-fails)
- [A runner is held for diagnostics or an acknowledgement](#a-runner-is-held-for-diagnostics-or-an-acknowledgement)
- [Docker image build runs out of space](#docker-image-build-runs-out-of-space)
- [Storage keeps growing after updates](#storage-keeps-growing-after-updates)
- [Docker image build fails with TLS certificate errors](#docker-image-build-fails-with-tls-certificate-errors)
- [Windows Docker Desktop WSL2 disk is smaller than expected](#windows-docker-desktop-wsl2-disk-is-smaller-than-expected)
- [Docker Container startup fails](#docker-container-startup-fails)
- [WSL provider image build fails early](#wsl-provider-image-build-fails-early)
- [GitHub runner registration fails](#github-runner-registration-fails)

## Quick diagnostics

EPAR writes logs under `work/logs` by default. Start with `work/logs/epar-last-error.log`, then inspect the matching build log in `work/logs/builds/` or instance transcript in `work/logs/instances/`. Manager events are console-only by default; raw transcripts are file-only unless `logging.transcriptSinks` includes `console`.

Long Buildx operations show a bounded console summary with downloaded bytes, completed layers, the active BuildKit step, elapsed time, and growing direct-archive bytes when an export is in progress. The complete raw progress remains in the printed build-log path.

```bash
./start --help
./start version
docker version
docker info
docker system df
```

Without local Go, use `./start --help`; the wrapper selects the containerized toolchain. For Windows WSL2, a WSL-backed Docker daemon, or the WSL provider, also run:

```powershell
wsl --version
wsl -l -v
docker context ls
docker run --rm ghcr.io/catthehacker/ubuntu:full-latest df -h /
```

Container-visible free space is the relevant value for Docker builds. Windows Explorer or Finder free space does not necessarily equal the free space in a Linux VM backing the daemon.

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

Check the diagnostic result before editing configuration:

```bash
sbx diagnose --output json
```

EPAR requires a controller architecture with an available Linux guest template and at least one diagnostic pass with zero failures. Diagnostic warnings and skipped checks remain visible but do not disable the provider. Review the failed item and its hint in the JSON output, fix the prerequisite, then choose Refresh in the provider menu to recheck availability; do not manually force a provider selection or substitute Docker Container for a configured Docker Sandboxes pool.

## Docker Sandboxes rejects template, policy, or capacity

### Symptom

Startup reports a template identity/digest mismatch, policy-generation drift, an admission failure, or insufficient capacity.

### Diagnosis and remediation

Docker Sandboxes resolves the configured source selector, records the exact OCI identities in a local artifact receipt, and verifies the host-global policy fingerprint. If the desired source, platform, scripts, template inputs, runner inputs, or trust inputs change, rerun `./start`; EPAR builds and imports a replacement and activates it only after exact readback succeeds.

An imported Docker Sandboxes template does not require a matching Docker image. EPAR builds directly to a verified archive, imports that archive, and then removes the transient workspace. If startup reports a missing Docker staging image, the controller is stale; rebuild the native controller and rerun `./start`. If direct archive verification or `sbx template load` fails, use the printed Buildx transcript and archive error; EPAR does not fall back to the memory-heavy Docker load/save path.

Capacity admission accounts for phase-overlapping physical growth on each resolved capacity domain and applies the fixed `storage.minimumFree` reserve once per domain. The checkout, Docker Engine, Docker Desktop disk, Docker Sandboxes state/cache, WSL distribution backing, and Tart store can be on different filesystems. Docker Sandboxes root and inner-Docker sizes are independent sparse logical maxima and are not added as immediate host usage. Inspect the matching operation with `./start storage status --operation template-build --provider docker-sandboxes --config <path> --project-root <path>`, then use the exact prune preview or deliberately retry only that invocation with `--allow-insufficient-storage`. A `documented-default-assumed` warning means EPAR verified a local Docker Desktop context but could only measure the documented default/system location; it never grants cleanup authority. Avoid broad cleanup commands: they can delete stopped containers and intentionally retained resources.

## Docker Sandboxes creation fails after a runtime-helper prompt

### Symptom

On macOS or Linux, host security asks whether to allow a Docker Sandboxes helper such as `mkfs.ext4`, `mkfs.erofs`, or `containerd-shim-nerdbox-v1`; macOS may say that the helper “is an app downloaded from the Internet.” After a required prompt is denied or blocked, EPAR reports `create docker sandbox failed`, and `sbx` may report `500 Internal Server Error: failed to run sandbox container`. The runner is neither registered nor marked ready.

### Diagnosis and remediation

Docker Sandboxes uses `mkfs.ext4` to create an ext4 filesystem inside each sandbox's private Docker disk-image file, `mkfs.erofs` to construct the read-only template snapshot, and `containerd-shim-nerdbox-v1` to launch and manage the sandbox VM. Expected file targets are regular sandbox-owned files beneath the Docker Sandboxes runtime data directory—for example, current macOS releases may use `~/.sbx/run/d/containerd/.../images/<runner>-docker.img` and `~/.sbx/run/d/containerd/.../snapshots/<id>/layer.erofs`. With the current Homebrew `sbx` package, the runtime and shim are beneath `/opt/homebrew/Caskroom/sbx/<version>/`. A formatter must not target a physical device such as `/dev/disk*`, an EPAR checkout, a home-directory document, or another unrelated path.

If each executable belongs to the Docker Sandboxes installation you intentionally installed and any displayed target is the expected sandbox-owned file, allow the operation through the host's security or application-control prompt, then rerun the same EPAR start or verification command if creation already failed. Do not invoke a formatter or shim yourself, disable host security broadly, or approve a command with an unfamiliar target. The configured private Docker disk is sparse, so its logical maximum does not mean the formatter immediately consumes that amount of physical storage.

If no prompt appeared, or approval still produces the 500 error, preserve the failed runner evidence and inspect the Docker Sandboxes daemon/client logs and `sbx diagnose --output json`; the same top-level error can also represent a runtime, capacity, or host-policy failure. See [Private Filesystem and VM Helper Approval](providers/docker-sandboxes.md#private-filesystem-and-vm-helper-approval) for the provider contract.

## Docker Sandboxes rejects a staging workspace because SSH-agent forwarding is present

### Symptom

Sandbox creation reaches `verify dedicated docker sandbox staging workspace` and fails with a message that host SSH-agent forwarding is not permitted. Diagnostics may show `SSH_AUTH_SOCK=/run/ssh-agent.sock` or `SSH_AUTH_SOCK_GATEWAY=...` inside the guest even though the imported template does not define them.

### Diagnosis and remediation

Docker Sandboxes may forward the host SSH agent when its shared daemon inherits the host's agent environment. EPAR rejects the resulting sandbox because the forwarded socket or gateway could let a workflow use host SSH credentials. This is not evidence that the staging mount is missing or read-only, and deleting only `/run/ssh-agent.sock` is insufficient when the forwarding gateway remains configured.

Coordinate the interruption with every process using the shared Docker Sandboxes daemon, then restart it with all forwarding variables removed and retry EPAR:

```sh
sbx daemon stop
env -u SSH_AUTH_SOCK -u SSH_AUTH_SOCK_GATEWAY -u SSH_AGENT_PID sbx daemon start --detach
```

EPAR strips these variables from Docker Sandboxes commands it launches, but an already-running daemon retains the environment with which another shell or tool started it. Do not disable this admission check or forward an agent into a reusable runner template. If the failed creation predates the immutable-receipt fix, preserve its reported sandbox UUID and use exact provider cleanup; never delete a same-name resource by prefix alone.

## Docker Hub login succeeds but a private pull is denied in Docker Sandboxes

### Symptom

A workflow's Docker login step reports `Login Succeeded`, but a later pull of a private Docker Hub image fails with `insufficient_scope: authorization failed`, `pull access denied`, or an equivalent authorization response. The same workflow and credentials may succeed with Docker Container or a GitHub-hosted runner. Using `docker --config /home/agent/.docker pull ...` produces the same denial.

### Diagnosis and remediation

First verify only metadata, never credential contents: the listener should run as `agent` with `HOME=/home/agent` and `DOCKER_CONFIG=/home/agent/.docker`, and a post-login config should be owned by `agent` with restrictive permissions. If Docker reaches the registry and returns an authorization response, do not investigate CA copying unless an `x509` or TLS error is also present.

Inspect the host Docker Sandboxes daemon log for a message that the proxy is overriding a client-supplied registry credential with a host credential. When that message is present, the workflow credential was written correctly but dockerd used the credential-injecting forward path. An explicit guest config path cannot bypass that path.

On a current EPAR template, `docker info --format '{{.NoProxy}}'` must print `*`, `/etc/docker/daemon.json` must be a root-owned regular file, and `sbx policy log <sandbox-name>` must report `transparent` for `registry-1.docker.io`, `auth.docker.io`, and the blob host used by the pull. Docker Sandboxes documents transparent traffic as policy-enforced without credential injection. If dockerd reports another no-proxy value or the policy log reports `forward`, stop the workspace controller, let its exact cleanup finish, rerun `./start` to build and import the changed template, and test on a newly created runner. Do not reuse the old sandbox.

Do not set `DOCKER_SANDBOXES_NO_PROXY` expecting it to disable credential injection. That host variable only excludes destinations from an optional upstream proxy used after traffic reaches the mandatory Sandbox proxy. Replacing `docker/login-action` with `docker login`, combining login and pull in one shell step, or changing `DOCKER_CONFIG` also leaves an old daemon's forward route unchanged.

Keep the host `sbx login` identity intentionally different from the workflow identity when proving this fix. A successful private pull together with transparent policy-log entries proves that the guest credential is authoritative. Changing the host login to match the workflow can diagnose the old interception behavior, but it is a shared-identity workaround rather than the fix.

EPAR rejects global `sbx` secrets and removes inherited proxy variables from runner registration and the Actions listener. A root-capable workflow can still deliberately reconnect a client to Docker Sandboxes' forward proxy, and v0.37.1 has no documented per-sandbox switch that disables the interceptor. Use a least-privilege host `sbx` account and choose Docker Container if that residual capability is outside the trust boundary. See [Docker Hub Credentials and Transparent Egress](providers/docker-sandboxes.md#docker-hub-credentials-and-transparent-egress).

## An idle runner reports GitHub or Sandbox health warnings

A GitHub 429/5xx response or an `sbx` command timeout makes runner health temporarily unknown; it does not prove that the Actions listener stopped. EPAR keeps the exact runner, lets a trust lease expire closed when it cannot refresh it, and retries. Cleanup for an inactive listener requires two consecutive guest probes that successfully execute and explicitly report the process stopped. Review the instance guest transcript when warnings repeat; do not delete the runner merely because one API or Sandbox inspection failed.

`networkBaseline: open` is a sandbox-scoped public-egress compatibility rule with EPAR host-alias deny guardrails. It does not alter the host-global policy. If a required service is blocked, use a narrow `additionalAllow` hostname rule; do not allow `host.docker.internal`, `gateway.docker.internal`, `kubernetes.docker.internal`, or `host.containers.internal` through the Open-policy guardrails.

## GitHub Actions runner release resolution reports HTTP 403 or 429

The public `actions/runner` Releases API has a separate unauthenticated rate limit. An EPAR GitHub App installation token is not used for that public repository because it may not have permission to read it. On a 403, 429, timeout, or transient server response, EPAR prints the HTTP status and any safe rate-limit, retry, and request-ID headers, then uses the reviewed exact release in `third_party/actions-runner-release.lock.json`. This is an explicit metadata-only fallback: the selected version must match `image.runnerVersion` when that value is pinned, the expected asset name and canonical GitHub URL are checked, and the downloaded package must still match the lock's SHA-256 before it is installed.

If EPAR says the checked-in fallback is unusable, restore that file from the EPAR release or update to a release with a refreshed lock; do not replace the digest with an unverified value. Authentication, malformed successful API responses, selector mismatches, missing assets, and other non-transient API failures remain errors rather than using the fallback.

## A scheduled image check or update fails

Run `./start status` to see the last successful remote check, next check or retry, pending immutable identity, deferred reason, and last error. A failed scheduled check or build keeps the previous exactly verified generation available and retries with bounded backoff; a missing artifact or changed local configuration still fails closed. Use `./start image update` to retry an immediate remote check, or correct local input errors and rerun `./start`.

## A runner is held for diagnostics or an acknowledgement

### Symptom

An instance is retained, quarantined, or shown as requiring an acknowledgement after a provisioning, policy, or runtime failure.

### Diagnosis and remediation

Preserve the instance and inspect `work/logs/instances/<instance>.guest.log`, the matching runner diagnostics, and controller output before acknowledging or removing it. EPAR deliberately keeps uncertain ownership, failed cleanup, and unverified remote state inside the strict `pool.instances` cap instead of creating a replacement storm.

If an incident requires stopping new work immediately, stop the controller with `Ctrl-C` or the service manager that launched it. This prevents replacement; it does not erase retained evidence. Use the configured EPAR cleanup command only after identifying the exact affected pool. Do not use a broad `docker system prune`, WSL unregister, or reset as an incident-disable switch.

Set `EPAR_DISABLE_DOCKER_SANDBOXES=1` before starting EPAR when Docker Sandboxes admission must remain disabled during an incident or compatibility investigation. This fails the provider closed without changing configuration or deleting evidence.

After reviewing retained Docker Sandboxes diagnostics, acknowledge that review only for the exact configured pool:

```bash
./start cleanup --acknowledge-failed-diagnostics
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

## Storage keeps growing after updates

### Symptom

Old EPAR images, Docker Sandboxes templates, staging archives, or no-Go controller files remain after an update or an interrupted start.

### Diagnosis and remediation

Run `./start` once more. Startup reconciles incomplete exact-owned work and retires an unreferenced superseded generation after its replacement passes readback. It does not delete shared images, prefix-only historical resources, active containers, active sandboxes, or resources referenced by another configuration.

Inspect the exact classification before removing anything manually:

```bash
./start storage status
./start storage prune
./start storage prune --legacy
```

Use normal `storage prune --execute` only for exact catalog-owned resources. Legacy prefix-era entries require the plan hash printed by `storage prune --legacy`; they are not removed automatically. Do not use broad Docker prune/reset commands or VHDX compaction as a substitute for this review.

## Docker image build fails with TLS certificate errors

### Symptom

HTTPS access fails with `curl: (60)`, `certificate verification failed`, or an unknown issuer during an EPAR build or job.

### Diagnosis and remediation

Do not disable certificate verification. First identify which trust boundary failed.

For a no-Go native-controller build, `./start` automatically reads host system roots, excludes explicitly distrusted certificates, validates the short-lived feed in an offline container, and mounts only the resulting CA bundle into the Go compiler container. Runner CA inheritance remains independent. If the build still reports an unknown issuer, inspect `work/logs/epar-native-controller-build.log`: the wrapper prints the requested host, presented certificate subject and issuer, SHA-256 fingerprint, validity, and verification result, and on Windows it lists matching roots from `LocalMachine\Root` and `CurrentUser\Root`. A remaining failure means the expected issuer was absent, distrusted, malformed, expired, or not the certificate actually presented; EPAR never disables TLS verification or retries insecurely.

For an EPAR Buildx failure, leave `image.hostTrustMode` unchanged. EPAR automatically supplies host system roots to its config-owned builder and prints the full build transcript path before `docker buildx build`. The console and error report include a bounded redacted tail. Inspect the underlying `x509` line together with the registry host, builder identity, and active trust generation:

```powershell
docker buildx ls
Get-ChildItem .local/storage/buildx -Recurse -Filter metadata.json | Get-Content
Get-ChildItem .local/storage/buildkit -Recurse -Filter buildkitd.toml | Get-Content
```

The owned metadata records the exact registry set, configuration digest, certificate bundle, and trust generation. Rerunning the same command reconciles that exact builder and preserves its BuildKit state; EPAR never changes Docker's shared/default builder. If the source-image `docker pull` itself fails before Buildx starts, configure the authorized CA in the host daemon because builder trust cannot repair host-daemon trust.

Configure runner overlay only when jobs inside an ephemeral runner must inherit host roots:

```yaml
image:
  hostTrustMode: overlay
  hostTrustScopes: [system, user]
```

Use `[system]` on Linux. Overlay mode collects the current host roots, validates them before registration, and combines them with Ubuntu roots and any `image.trustedCaCertificatePaths`; it is root-anchor inheritance rather than exact Windows/macOS TLS-policy emulation. It requires `runner.ephemeral: true`. Omitted or disabled mode remains valid for Docker Sandboxes and does not install the job-start trust hook.

Use the normal host entry point so EPAR can inspect the real Windows certificate stores or macOS Keychain:

```powershell
.\start.ps1
.\start.ps1 image build --replace
```

On no-Go Windows, use `.\start.ps1 image build --replace`; on macOS/Linux, use `./start image build --replace`. The wrapper uses a native-host trust feed while compiling the native controller; the resulting native controller reads host trust directly for `start`, `image build`, `pool up`, and `pool verify`, even when runner overlay is disabled. Direct `scripts/run-with-docker.*` calls are wrapper-development diagnostics. The legacy containerized controller still requires the separate native-host feed bridge. A bare Linux toolchain container is not a replacement for either path.

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

For `Wsl/Service/CreateInstance/E_UNEXPECTED`, `Catastrophic failure`, or import exit `0xffffffff`, stop EPAR, save work in other distros, then run `wsl --shutdown`. This stops every running WSL distro, including any Docker backend using WSL. Restart the affected Docker host runtime, verify a normal distro command returns `0`, then rerun `./start`; a matching cached source rootfs is reused. If it persists, update WSL, shut it down again, reboot, and consult [Microsoft's WSL troubleshooting guidance](https://learn.microsoft.com/windows/wsl/troubleshooting#error-code-0x8000ffff-unexpected-failure).

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
./start cleanup
```

Cleanup is bounded by the configured pool and durable exact lifecycle identities; it does not authorize a broad prefix deletion, wildcard, Docker prune, or removal of unknown/shared resources. Keep `pool.namePrefix` unique per controller and organization.
