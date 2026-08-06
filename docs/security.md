# Security

EPAR provides disposable GitHub self-hosted runners with provider-dependent isolation. Docker Sandboxes is the primary provider on Linux, macOS, and Windows hosts when its capability checks pass; it places each runner inside a dedicated microVM sandbox and provides EPAR's strongest current host-isolation boundary. Docker Container and WSL remain compatibility infrastructure for trusted workflows, while Tart is retired and retained only for existing configurations. No provider is guaranteed to be universally safe for arbitrary hostile workflows.

GitHub's self-hosted runner warning still applies: GitHub recommends using self-hosted runners only with private repositories because public repository forks can run code on the runner machine through pull request workflows. Read the official GitHub guidance before exposing any self-hosted runner to public or untrusted workflows: [Adding self-hosted runners](https://docs.github.com/actions/hosting-your-own-runners/adding-self-hosted-runners).

## Reporting A Vulnerability

Security fixes are applied to the latest released version and the current default branch.

Do not disclose suspected vulnerabilities in a public issue, discussion, pull request, or commit. Use this repository's private vulnerability reporting flow: open the **Security** tab, select **Report a vulnerability**, and provide a clear description, affected versions, reproduction steps or a proof of concept, and the potential impact.

If private reporting is unavailable, contact a repository maintainer privately through GitHub and include the same information. We will acknowledge the report, investigate it, and coordinate a fix and disclosure timeline with you.

## What EPAR Improves

Disposable instances reduce host pollution, stale runner state, and accidental cross-job interference. After a job completes, EPAR retires the instance and creates a replacement. For Docker Sandboxes, the private daemon, filesystem, and job state disappear with the microVM; compatibility providers retain their documented cleanup boundaries.

## What EPAR Does Not Guarantee

A workflow controls its runner environment while it runs and can access any secrets and reachable services exposed to that workflow. Ephemeral cleanup alone is not a sandbox boundary: Docker Sandboxes supplies a dedicated microVM boundary, while compatibility providers use their documented isolation models and Tart remains a retired runtime path.

Do not mount host source directories, Docker sockets, private keys, or long-lived cloud credentials into runner instances unless that is inside your trust boundary.

Use GitHub runner groups, repository restrictions, environment protections, and minimal secrets. EPAR's [runner-group security preflight](runner-groups.md) checks the configured routing policy before registration, but it does not make public pull request workflows, forked contributions, or unknown third-party workflow code trustworthy.

## Provider Notes

EPAR intentionally does not implement a Docker-socket provider. A runner that controls the host Docker socket can usually control the host.

Docker Container uses a privileged outer container with a private inner Docker daemon. That gives good cleanup and Docker resource separation for each job, but it is still trusted-job infrastructure because `--privileged` weakens container isolation.

Docker Sandboxes places the listener, guest filesystem, and private Docker daemon inside a dedicated microVM sandbox. It provides EPAR's strongest current host boundary and is the primary provider on Linux, macOS, and Windows when the supported-platform, Docker, and machine-readable `sbx` readiness checks pass. Startup then performs the remaining storage, template, policy-rule, native architecture, runtime, and registration admission checks and fails closed. QEMU is a workload compatibility capability rather than part of the isolation boundary: `best-effort` warns and continues when sandbox-local `binfmt_misc` is unavailable, `required` makes it an admission requirement, and `native-only` skips it.

Docker Sandboxes may forward a host SSH agent when its shared daemon inherits `SSH_AUTH_SOCK`. EPAR strips SSH-agent variables from child commands and rejects any sandbox exposing the socket, gateway, or agent PID; operators must restart an already-running daemon with those variables unset rather than weakening the check.

Tart is a retired Apple Silicon macOS VM path retained for existing configurations. It provides a VM boundary, but workflows still control the guest and any secrets exposed to the job.

WSL2 has a weaker isolation story than one full VM per job. Treat the WSL provider as trusted-job infrastructure unless your environment has reviewed and accepted that model.

## Images And Secrets

`image.customInstallScripts` run as root during image build and their effects are captured in the reusable image. Use them only for non-secret tooling and configuration. Do not bake Docker credentials, GitHub tokens, private keys, or project secrets into runner images.

Certificates configured through `image.trustedCaCertificatePaths` are embedded in the reusable image and become public trust anchors for every process in its runner instances. CA certificates are not treated as secrets. Add only CA roots or intermediates that your organization has explicitly authorized, and rebuild the image when they are rotated or revoked.

`image.hostTrustMode: overlay` is a broader policy choice: the first-run wizard enables it for providers that support host-trust inheritance, and EPAR then follows every root anchor in the configured host scopes, including later additions, removals, and rotations. Windows and macOS user scope can include roots installed by software running as that account. Use it only when the host trust administrators are also authorized to control runner trust; edit the generated configuration if this trust model is unsuitable.

Host trust inheritance is additive to Ubuntu's default roots and explicit CA paths. It does not emulate every Windows or macOS certificate-policy constraint, and removing a host root cannot revoke an identical Ubuntu-bundled or explicitly configured anchor. EPAR applies host changes through immutable runner generations: running jobs keep their starting trust, while stale idle runners are replaced. EPAR-owned guest clients use the stable `/opt/epar/trust/ca-bundle.pem` path. On Windows Docker Sandboxes overlay runners, an authenticated controller-host relay also ensures public TLS uses the native-host network inspection path; activation is fail-closed before registration and the relay accepts only public port-443 destinations. Workflow clients retain end-to-end TLS through a raw guest listener. The sandbox-private Docker daemon uses a separate root-only listener that terminates its TLS with an ephemeral per-sandbox authority, verifies a new upstream TLS session against the canonical host bundle, and sends the controller only encrypted upstream TLS.

The GitHub App private key remains on the host. Guest instances receive only short-lived registration tokens at runtime. Do not bake tokens or private keys into runner images.

## Registry Mirrors

Docker registry mirrors are optional infrastructure outside EPAR. Treat them as part of your trusted CI environment.

Do not assume a mirror makes private image pulls safe or anonymous. A private image still needs authorization from the workflow's `docker login` or from credentials configured on the mirror itself. If the mirror is configured with upstream registry credentials, secure the mirror because it may be able to serve private images that credential can access.

Host-side Docker login state is not copied into EPAR instances. Keep Docker Hub, cloud registry, and package registry credentials in GitHub secrets or in a deliberately secured mirror service. Docker Sandboxes has a host forward proxy that can replace a guest's Docker Hub authorization with the host `sbx login` identity without copying that credential into the guest. EPAR does not query or mutate host-global secret inventory; after creating its exact sandbox, provider admission rejects any nonempty service-secret or authentication capability actually attached to that sandbox without exposing credential metadata. EPAR passes the Docker Sandboxes gateway proxy only to runner registration and the Actions listener. On Windows overlay runners, workflow steps instead use EPAR's raw authenticated local relay listener, while the private daemon uses its separate root-only TLS-terminating listener; both reach the credential-free controller-host relay. Other configurations retain cleared workflow proxies and transparent daemon routing. This preserves ordinary per-job registry authentication but is not a hard boundary against a root-capable workflow deliberately reconnecting to the Docker Sandboxes gateway proxy. EPAR does not mutate unrelated sandboxes or `sbx login`; use a least-privilege `sbx` account and choose Docker Container when that residual host credential capability is outside the trust boundary. Arbitrary nested images do not automatically receive the host CA bundle. See the [Docker Sandboxes provider guide](providers/docker-sandboxes.md#docker-hub-credentials-and-transparent-egress).
