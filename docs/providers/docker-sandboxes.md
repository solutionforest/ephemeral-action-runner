# Docker Sandboxes Provider

Docker Sandboxes places each GitHub Actions listener inside a dedicated microVM sandbox with a private guest filesystem and Docker daemon. It is EPAR's primary provider on Linux, macOS, and Windows hosts when local capability checks pass, and its strongest current host-isolation boundary. Its protection still depends on the installed Docker Sandboxes runtime, the host platform, EPAR's configuration, and the resources deliberately exposed to the workflow.

```mermaid
flowchart TB
  subgraph Host["Native controller host"]
    EPAR["EPAR controller"]
    SBX["Docker Sandboxes"]
    Ledger["Exact ownership ledger"]
    Cache["Shared template cache"]
  end
  subgraph Sandbox["One disposable microVM"]
    Runner["Ephemeral Actions runner"]
    Docker["Private Docker daemon"]
    Jobs["Workflow and service containers"]
  end
  EPAR --> SBX
  EPAR --> Ledger
  Cache --> SBX
  SBX --> Runner
  Runner --> Docker
  Docker --> Jobs
```

## When To Use It

Choose Docker Sandboxes when its local checks pass and you want a microVM boundary around the runner and its Docker workload. The first-run wizard recommends it when the supported-platform, Docker, and machine-readable `sbx` readiness checks pass. Startup then performs the remaining storage, template, policy-rule, runtime, and registration admission checks. Measured insufficient storage blocks admission, while an unmeasurable local capacity domain warns and leaves the measurable-domain checks in force; other admission failures remain blocking. A configured Docker Sandboxes pool never silently falls back to Docker Container or another provider.

## Support Status

EPAR recommends this provider in the wizard by capability, not by an operating-system allowlist: Docker must work, `sbx diagnose --output json` must report at least one passing check and no failed checks, and the controller architecture must have a matching native guest template. After configuration is saved, ordinary startup additionally requires storage, template, and configured architecture-capability admission before any runner starts. The wizard uses best-effort QEMU on every host: sandbox runtimes with usable `binfmt_misc` enable the bundled handlers, while runtimes without it continue as verified native runners with a warning. Native lifecycle support does not certify foreign workloads.

## Prerequisites

- Docker installed and running.
- Docker Sandboxes CLI whose `sbx diagnose --output json` result reports at least one passing check and no failed checks. Before the first-run provider assessment, the wizard runs `sbx daemon start --detach` when the `sbx` executable is installed, then runs diagnostics. Warnings and skipped checks do not make the provider unavailable.
- A native `amd64` or `arm64` controller with matching `linux/amd64` or `linux/arm64` image support. EPAR does not use emulation to admit a mismatched template.
- Enough capacity to resolve, build, export, import, and retain the selected runner template.
- Enough physical backing storage in every measurable Engine, project, Sandbox cache, and Sandbox state capacity domain for the phase-overlapping template and sandbox bootstrap work while retaining `storage.minimumFree` once per domain. Use `./start storage status --operation template-build --provider docker-sandboxes --config <path> --project-root <path>` to inspect the same plan; an unmeasurable local domain is shown as unknown with its reason and warns without blocking the operation. Sparse root and inner-Docker logical maxima are reported separately and are not counted as immediate host allocation.
- A GitHub runner group that meets enforced policy. Docker Sandboxes requires `security.runnerGroup.enforcement: enforce` and `runner.ephemeral: true`.

The wizard can select a signed GHCR prebuilt template from the EPAR catalog or build and import the template locally. The recipes in `templates/docker-sandboxes` remain reproducible local-build inputs, while the catalog's immutable package entries are the prebuilt image authority.

## Private Filesystem and VM Helper Approval

Every sandbox receives a private Docker daemon backed by its own Linux filesystem image and a read-only template filesystem. During first creation on macOS or Linux, Docker Sandboxes may launch helpers including `mkfs.ext4` to format the private Docker disk image, `mkfs.erofs` to construct an EROFS template snapshot, and `containerd-shim-nerdbox-v1` to launch and manage the sandbox VM. The operating system, endpoint-security software, or application-control policy may ask the signed-in user to approve each helper; macOS Gatekeeper may say that the executable “is an app downloaded from the Internet.” These commands are launched by the Docker Sandboxes runtime, not by a workflow and not directly by EPAR. With the current Homebrew `sbx` package on macOS, the runtime and shim are installed beneath `/opt/homebrew/Caskroom/sbx/<version>/`, sandbox state is beneath Docker Sandboxes' user data directories, the ext4 target resembles `~/.sbx/run/d/containerd/.../images/<runner>-docker.img`, and the EROFS target resembles `~/.sbx/run/d/containerd/.../snapshots/<id>/layer.erofs`.

Review the complete executable and any target shown by every prompt. Approve it only when the executable belongs to the Docker Sandboxes installation you intentionally installed and any file target is beneath that runtime's sandbox data directory. A formatter must not target a physical device such as `/dev/disk*`, another user-data path, or an unrelated file. The configured `dockerSandboxes.dockerDisk` value is the sparse logical maximum for the private Docker filesystem, not an immediate allocation of that entire size. Denying or blocking any required helper prevents that sandbox from starting and commonly surfaces through `sbx` as `500 Internal Server Error: failed to run sandbox container`; EPAR then fails closed without registering the runner. Correct the host approval policy and retry the exact EPAR command rather than running a formatter or shim manually.

## Minimal Configuration

Start with `./start` or [`configs/docker-sandboxes.example.yml`](../../configs/docker-sandboxes.example.yml). Configuration expresses the desired source; EPAR stores immutable build and cache identities in its local receipt.

```yaml
provider:
  type: docker-sandboxes
  platform: linux/amd64

image:
  distribution: local-build
  sourceType: docker-image
  sourceImage: ghcr.io/catthehacker/ubuntu:full-latest
  sourcePlatform: linux/amd64
  runnerVersion: latest
  updateFrequency: weekly
  updateTime: "07:00"
  customInstallScripts:
    # - examples/custom-install/install-extra-apt-tools.sh

dockerSandboxes:
  policyGeneration: sha256:<balanced-policy-fingerprint>
  networkBaseline: open
  architectureEmulation: best-effort
  stagingRoot: .local/docker-sandboxes-staging
  cpus: 4
  memory: 8GiB
  rootDisk: auto
  dockerDisk: 50GiB
  maxConcurrentCreates: 2
```

`provider.sourceImage` is invalid for this provider; use the common `image` section. `rootDisk: auto` derives a sparse logical root maximum from the selected artifact. `dockerDisk` is an independent sparse workload limit whose default is 50 GiB and minimum is 1 GiB. Neither virtual maximum is treated as immediately consumed host space; the only physical reserve is `storage.minimumFree`, whose generated default is 1 GiB. See [Configuration](../configuration.md) for the complete schema.

The initial wizard keeps `distribution: local-build` as the default and offers `P. EPAR verified prebuilt Act (preview)`. That option uses only `ghcr.io/solutionforest/ephemeral-action-runner/docker-sandboxes-template:act-latest`, verifies its immutable digest plus GitHub/Sigstore attestation in-process before activation, and materializes the first-use Sandbox runtime locally. Host and enterprise CA roots are overlaid where safe; custom scripts are intentionally empty in the base path and create only a small local derivative when configured. Prebuilt acquisition size is resolved during startup rather than presented as the local Buildx estimate. There is no silent fallback to a local build or another provider. See [Docker Sandboxes prebuilt image publication](../development/docker-sandboxes-prebuilt.md) for the public recipe, catalog, attestation, promotion, and rollback contracts.

`networkBaseline: open` adds EPAR-owned sandbox-scoped public egress plus deny-wins guardrails for host aliases; it does not change the host-global Docker Sandboxes policy. Use `balanced` with `additionalAllow` for default-deny public egress. Additional allow/deny entries are exact hostnames or `*.domain[:port]`; they cannot override the Open host-alias denies.

## Cross-Architecture Containers

`dockerSandboxes.architectureEmulation` is an explicit capability contract. The default `best-effort` mode copies the pinned `tonistiigi/binfmt:qemu-v10.2.3-68` installer and all static QEMU interpreters from the immutable template, then each newly created sandbox tries the equivalent of `binfmt --install all` as root inside its private VM. This does not run a privileged container on the host. When handlers become active, EPAR records QEMU capability normally. When the sandbox kernel reports that `binfmt_misc` is unavailable, EPAR verifies the configured native guest and private Docker architecture, emits a warning, and continues.

`required` uses the same QEMU attempt but fails creation unless at least one bundled handler is active; select it only when foreign-architecture execution is a runner admission requirement. `native-only` skips the attempt and requires no EPAR-owned QEMU handler. Both native paths verify that the guest kernel and private Docker daemon match `provider.platform`. Configurations that omit the key default to `best-effort`, so ordinary native jobs are not blocked by a sandbox-runtime QEMU limitation.

Native image processes remain native because foreign binfmt handlers match only their foreign ELF signatures. Docker manifest selection is unchanged: EPAR does not set `DOCKER_DEFAULT_PLATFORM`, QEMU cannot create a missing manifest, and a Compose service should use its `platform` property when a multi-platform tag must select a foreign variant deliberately. A single-architecture local image can run without a service override when its image metadata already identifies the foreign platform. Treat actual workload startup, health checks, networking, and performance as the compatibility proof.

The provider keeps architecture admission behind an internal target-agnostic boundary. Future authoritative accelerators can be added without changing the shared sandbox lifecycle, but any capability change remains explicit and fail-closed.

## Normal Workflow

1. Run `./start` with no config and select Docker Sandboxes when its tooling and diagnostics pass. Choose a local Catthehacker `full-latest` or `act-latest` profile, or explicitly choose the verified prebuilt Act preview, then review the non-blocking physical-growth estimate or the startup-resolved prebuilt acquisition note. The generated config uses no custom install scripts; add them afterward when a small local derivative is intended. The wizard's initial screen keeps compatibility providers behind `C. Show compatibility providers`; their specialized and custom tags do not change Docker Sandboxes' requirement for a private Docker daemon and runtime closure.
2. The wizard writes the desired configuration. Embedded `./start` then enters the ordinary provisioning path, enforces measured storage capacity while warning about any unmeasurable local domain, builds and imports the template, and activates it only after exact readback. On macOS or Linux, review the narrowly scoped helper prompts described in [Private Filesystem and VM Helper Approval](#private-filesystem-and-vm-helper-approval) if the host presents them.
3. Prewarm the selected template without GitHub registration:

   ```powershell
   ./start pool verify --config .local\docker-sandboxes.yml --project-root . --instances 1 --cleanup
   ```

4. Start the pool with `./start`. EPAR reuses the verified imported template without a registry check until the configured update schedule is due; local input changes and missing templates still rebuild immediately.

Each allocation receives an empty owner-restricted staging directory, but Actions `_work` stays on the guest filesystem. EPAR verifies the guest, confirms that the configured sandbox-scoped policy rules are present, and verifies the private daemon and runner trust policy before requesting a short-lived registration token. With `image.hostTrustMode: overlay`, the common pool lifecycle installs the selected roots, verifies the immutable generation, and maintains the job-start lease; with the setting omitted or disabled, the template carries an explicit disabled-policy marker that the unconditional preparation hook accepts. The token remains on the native host except for registration through `sbx exec` standard input.

The listener identity is explicit and self-consistent: `agent` owns its home, XDG, runtime, and Docker configuration directories, and every workflow action and shell command inherits those exact paths. Template construction removes Docker credentials inherited from source-image user homes, and the registration path performs a second narrow scrub of `.docker/config.json` and `.dockercfg` across the actual passwd homes after sandbox boot while preserving `.docker/sandbox/locks`; verification rejects reusable artifacts that retain registry authentication or point identity-derived paths at another user. A workflow login can therefore write only to the disposable sandbox's Docker client configuration, and that file disappears with the sandbox. Registry authorization can still be changed by Docker Sandboxes' host-side credential proxy as described below.

Docker Sandboxes can automatically forward the host SSH agent when its shared daemon inherits `SSH_AUTH_SOCK`. That would expose a host credential capability to every sandbox created by that daemon, so EPAR rejects any guest containing `SSH_AUTH_SOCK`, `SSH_AUTH_SOCK_GATEWAY`, `SSH_AGENT_PID`, or `/run/ssh-agent.sock`. EPAR removes these variables whenever it launches Docker Sandboxes commands, so a stopped daemon that those commands auto-start is sanitized. EPAR cannot repair an already-running daemon that another shell or tool started with forwarding enabled. If creation reports the known `failed to run sandbox container` signature, EPAR preserves that original error and adds this SSH-daemon remediation hint; unrelated create failures do not receive it. EPAR never stops or restarts a running shared daemon automatically. Coordinate with every process using Docker Sandboxes on the host, then stop the daemon and restart it from a sanitized environment before retrying EPAR:

```sh
sbx daemon stop
env -u SSH_AUTH_SOCK -u SSH_AUTH_SOCK_GATEWAY -u SSH_AGENT_PID sbx daemon start --detach
```

Do not relax the verification or merely delete the relay socket: the gateway setting is itself a forwarding capability and the daemon's inherited environment is authoritative for subsequently created sandboxes. A stopped daemon may be started explicitly from the sanitized environment above or auto-started by an EPAR-launched `sbx` command; EPAR does not mutate a running shared daemon to recover from this failure.

## Docker Hub Credentials and Transparent Egress

Docker Sandboxes v0.37.1 provides three HTTP(S) egress paths. Its `forward` path can terminate TLS and replace a guest registry `Authorization` header with the host `sbx login` credential; `forward-bypass` and `transparent` do not inject credentials. All three remain subject to Docker Sandboxes network policy. A runner using the forward path can therefore report `Login Succeeded`, have a correct `/home/agent/.docker/config.json`, and still receive `insufficient_scope: authorization failed` because the host identity, not the workflow identity, performs the private pull.

EPAR's reusable template starts with Docker Engine's normal daemon proxy object and `no-proxy: "*"`; this bootstrap state keeps registry authorization away from Docker Sandboxes' credential-bearing `forward` path. On a Windows controller with `image.hostTrustMode: overlay`, EPAR then creates one controller-owned TCP relay on a stable port derived from the project and pool identity, issues a separate 256-bit token per sandbox, permits only that exact `host.docker.internal:<port>` endpoint, and activates a root-owned guest bridge with separate Docker and workflow listeners. The stable endpoint lets a kept sandbox rebind after a controller restart without accumulating stale allow rules; a port collision fails closed. Both listeners accept ordinary HTTP CONNECT requests but can reach the host relay only with the per-sandbox token; the host relay accepts only public TLS destinations on port 443 and rejects loopback, private, link-local, documentation, benchmark, multicast, and reserved address space. The Docker listener on `127.0.0.1:3129` terminates only the sandbox-private daemon's TLS with a per-sandbox ephemeral CA whose private key is root-only, then establishes and verifies a separate upstream TLS session through the native host by using the canonical host-derived CA bundle. This avoids Docker Engine's incompatible HelloRetryRequest path while keeping Docker authorization plaintext inside the sandbox; the controller relay transports only encrypted upstream TLS. EPAR restarts the private daemon from a clean environment with that listener as its HTTPS proxy, requires daemon proxy and trust readback, proves Registry TLS, and requires fresh Docker Sandboxes policy evidence that the exact relay port used `transparent`, with no fresh credential-bearing `forward` route. The pre-relay daemon configuration remains in a root-only transaction backup until that host-side policy proof commits; a later failure restores the daemon and removes the exact newly added policy rule. The relay dies with the controller and its token is revoked on stop, deletion, failed activation, or confirmed external sandbox removal.

Runner registration and the Actions listener receive Docker Sandboxes' gateway proxy explicitly for GitHub control-plane traffic; `forward-bypass` is an observed dynamic route classification, not a selectable `sbx create` mode. At the unconditional job-start boundary, the isolated hook checks the host-trust lease, validates the runner-owned `GITHUB_ENV` file-command path, and never passes the gateway proxy to workflow steps. Windows overlay runners require a live authenticated EPAR relay: host-level workflow HTTPS clients receive `HTTPS_PROXY`/`https_proxy=http://127.0.0.1:3130` and the canonical CA bundle, while HTTP and other proxy variables remain empty. That workflow listener preserves the client's end-to-end TLS session as raw tunnel bytes; a missing relay stops the job before user steps instead of falling back to the synthetic-certificate-prone transparent path. Configurations that do not require the Windows relay clear all proxy variables and use `NO_PROXY=*`. The private daemon independently keeps the TLS-terminating listener on `127.0.0.1:3129` as its HTTPS proxy, so workflow `docker login` remains authoritative without receiving the host `sbx login` identity.

The host variables `DOCKER_SANDBOXES_NO_PROXY` and `NO_PROXY` described in Docker Sandboxes' architecture documentation are not substitutes for this guest configuration. They only choose whether the host-side Sandbox proxy reaches its next hop directly or through an optional upstream proxy; they do not bypass the Sandbox proxy or its credential interceptor. Likewise, changing `DOCKER_CONFIG`, combining `docker login` and `docker pull` in one step, or replacing `docker/login-action` with a shell command cannot repair an old template that still sends dockerd through the forward path.

EPAR does not query or mutate host-global Docker Sandboxes secrets, unrelated sandboxes, or the `sbx login` identity. After creating its exact sandbox, provider admission rejects any nonempty authentication capability actually attached to that sandbox without exposing credential metadata, and it rejects host SSH-agent forwarding. It never copies workflow secrets back to the host. The transparent default is an operational isolation control, not a hard boundary against a hostile root-capable workflow: a job with root inside the microVM can deliberately set proxy variables or configure a client to reconnect to the Sandbox forward proxy, and Docker Sandboxes v0.37.1 documents no per-sandbox switch that disables the credential interceptor itself. Keep the mandatory host `sbx login` identity least-privileged. Use Docker Container or another provider if even deliberate use of that residual host credential capability is outside the workflow trust boundary.

For a Windows overlay runner, `sbx policy log <sandbox-name>` should report a fresh `transparent` connection to the exact controller relay port, normally rendered as `localhost:<port>`, and should not report fresh Docker Hub `forward` traffic from the activation proof. On platforms or configurations where the relay is inactive, registry/auth/blob hosts remain on the original transparent route. A daemon log saying it is overriding a client-supplied registry credential, or fresh policy entries showing `forward` for Docker Hub, identifies an old or altered template. Aligning `sbx login` with a workflow account can prove the interception diagnosis, but it establishes only shared-host authorization and is not EPAR's remediation for per-job credentials.

Some antivirus HTTPS inspectors return a synthetic error certificate such as one issued by `Norton Web/Mail Shield Untrusted Root` when Docker Sandboxes itself makes a transparent upstream connection. That issuer deliberately represents an upstream validation failure and EPAR never imports it. On Windows overlay runners, the EPAR relay instead opens the public TCP connection from the native controller host. Workflow clients retain end-to-end TLS and validate the normal host-approved inspection chain against `/opt/epar/trust/ca-bundle.pem`; the Docker bridge validates that same chain for its separate upstream session and presents the daemon with its root-only per-sandbox local authority. EPAR atomically refreshes the canonical bundle after every host-trust installation. Activation fails before registration unless both bridge listeners, daemon readback, Registry TLS proof, and fresh relay-route evidence all pass.

The verified contract covers EPAR-owned guest-system clients, the Actions listener environment, host-level workflow clients that honor standard proxy/CA variables, and the sandbox-private Docker daemon. It does not claim automatic CA installation inside arbitrary job containers, service containers, container actions, user-created `docker run` images, language-specific stores inside those images, or Dockerfile `RUN` stages. Those images must install or mount the required CA themselves. EPAR does not use GitHub's preview container-customization hooks because adopting them would make EPAR responsible for the complete container lifecycle rather than only trust propagation.

Template construction uses two independent trust paths. EPAR's project-owned BuildKit builder automatically receives host system roots for Docker Hub, GHCR, and the other pinned registries used by the build. The native controller downloads the locked Actions runner and `tini`, verifies their SHA-256 values, and then supplies them as local build inputs; the Dockerfile does not perform remote HTTPS downloads.

BuildKit streams the runner template directly to an attestation-free, verified archive; Docker Sandboxes does not require or retain a Docker staging image. Separate cache-backed BuildKit targets produce the max-mode provenance, SBOM, and software inventory without loading the runner image into Docker Engine. After `sbx template load` succeeds and EPAR reads back the exact imported template, startup housekeeping removes the transient archive workspace while retaining the active template and compact receipt evidence. Initial creation and every replacement trust the authoritative Sandbox cache readback, so the expected absence of a Docker image does not block a runner. Superseded templates are removed only after no configuration, lease, or live sandbox references them.

The opt-in `TestLiveRunnerTemplateIsolation` proof also exercises authenticated Docker-client state across separate commands. Set `EPAR_LIVE_DOCKER_SANDBOXES_REGISTRY_IMAGE` to an immutable Distribution Registry image reference and `EPAR_LIVE_DOCKER_SANDBOXES_HTPASSWD_IMAGE` to an immutable image containing `htpasswd`, in addition to the existing live-test template, digest, and staging variables. The test generates credentials in memory, creates a registry inside the sandbox-private daemon, logs in through standard input, pushes and separately pulls a private image, logs out, verifies the registry auth entry is absent, and exactly removes its registry container and images.

## Limitations

- `networkBaseline: open` permits public egress, which can exfiltrate secrets or data exposed to the workflow. Use least-privilege runner groups and secrets, and choose `balanced` with narrow allow rules for higher-risk workloads.
- Docker Sandboxes template cache storage is shared host state; it is not a per-sandbox root-disk measurement.
- macOS ARM64 is part of the current host support backed by completed cross-platform live lifecycle and cleanup testing. Operators should still run local admission and workload validation; this support statement does not claim independent certification. Current v0.37.1 evidence includes three consecutive private Docker Hub pulls with intentionally different host and workflow identities, transparent registry/auth/blob routing, AMD64 image pulls from an ARM64 guest daemon, ephemeral replacement, and exact sandbox cleanup.
- Transparent egress preserves ordinary per-job Docker Hub credentials, but a root-capable workflow can deliberately opt back into the Docker Sandboxes forward proxy. Use Docker Container when that residual host credential capability is outside the trust boundary.
- A stopped sandbox is diagnostic state, not proof of deletion. Unknown state consumes capacity and blocks replacement.
- `EPAR_DISABLE_DOCKER_SANDBOXES=1` fails admission closed during an incident or compatibility problem.

## Verification

Use the prewarm command above for an unregistered lifecycle check. To include GitHub registration, run:

```bash
./start pool verify --config .local/docker-sandboxes.yml --instances 1 --register-only --cleanup
```

The shared pool treats provisioning, ready, draining, quarantined, and cleanup-pending instances as capacity-consuming states. Cleanup uses durable exact sandbox, GitHub runner, and staging-directory identities; it never uses an `sbx` reset or broad prefix deletion.

## Troubleshooting

For symptoms and recovery, see [Troubleshooting](../troubleshooting.md). If failed diagnostics retain a sandbox, inspect `status` and preserve the reported evidence before using `cleanup --acknowledge-failed-diagnostics`; ordinary cleanup never applies that override.
