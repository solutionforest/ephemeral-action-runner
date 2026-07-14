# Security

EPAR is intended for trusted jobs by default. It adds cleanup and isolation around GitHub self-hosted runners, but it does not make an existing host safe for arbitrary untrusted workflows.

GitHub's self-hosted runner warning still applies: GitHub recommends using self-hosted runners only with private repositories because public repository forks can run code on the runner machine through pull request workflows. Read the official GitHub guidance before exposing any self-hosted runner to public or untrusted workflows: [Adding self-hosted runners](https://docs.github.com/actions/hosting-your-own-runners/adding-self-hosted-runners).

## What EPAR Improves

Disposable instances reduce host pollution, stale runner state, and accidental cross-job interference. After a job completes, EPAR retires the instance and creates a replacement. For Docker-DinD, job-created containers, networks, volumes, and inner image cache live inside the runner container's private Docker daemon and are removed with that runner instance.

## What EPAR Does Not Guarantee

A workflow controls the runner environment while it runs and can access any secrets exposed to that workflow. Ephemeral cleanup reduces persistence risk after the job, but it is not a hostile-code sandbox.

Do not mount host source directories, Docker sockets, private keys, or long-lived cloud credentials into runner instances unless that is inside your trust boundary.

Use GitHub runner groups, repository restrictions, environment protections, and minimal secrets. Avoid routing public pull request workflows, forked contributions, or unknown third-party workflow code to EPAR runners.

## Provider Notes

EPAR intentionally does not implement a Docker-socket provider. A runner that controls the host Docker socket can usually control the host.

Docker-DinD uses a privileged outer container with a private inner Docker daemon. That gives good cleanup and Docker resource separation for each job, but it is still trusted-job infrastructure because `--privileged` weakens container isolation.

Tart runs jobs inside VMs on Apple Silicon macOS. That is a stronger host boundary than Docker-DinD, but workflows still control the guest and any secrets exposed to the job.

WSL2 has a weaker isolation story than one full VM per job. Treat the WSL provider as trusted-job infrastructure unless your environment has reviewed and accepted that model.

## Images And Secrets

`image.customInstallScripts` run as root during image build and their effects are captured in the reusable image. Use them only for non-secret tooling and configuration. Do not bake Docker credentials, GitHub tokens, private keys, or project secrets into runner images.

Certificates configured through `image.trustedCaCertificatePaths` are embedded
in the reusable image and become public trust anchors for every process in its
runner instances. CA certificates are not treated as secrets. Add only CA roots
or intermediates that your organization has explicitly authorized, and rebuild
the image when they are rotated or revoked.

`image.hostTrustMode: overlay` is a broader policy choice: after the operator
enables it, EPAR follows every root anchor in the configured host scopes,
including later additions, removals, and rotations. Windows and macOS user scope
can include roots installed by software running as that account. Enable it only
when the host trust administrators are also authorized to control runner trust.

Host trust inheritance is additive to Ubuntu's default roots and explicit CA
paths. It does not emulate every Windows or macOS certificate-policy constraint,
and removing a host root cannot revoke an identical Ubuntu-bundled or explicitly
configured anchor. EPAR applies host changes through immutable runner generations:
running jobs keep their starting trust, while stale idle runners are replaced.

The GitHub App private key remains on the host. Guest instances receive only short-lived registration tokens at runtime. Do not bake tokens or private keys into runner images.

## Registry Mirrors

Docker registry mirrors are optional infrastructure outside EPAR. Treat them as part of your trusted CI environment.

Do not assume a mirror makes private image pulls safe or anonymous. A private image still needs authorization from the workflow's `docker login` or from credentials configured on the mirror itself. If the mirror is configured with upstream registry credentials, secure the mirror because it may be able to serve private images that credential can access.

Host-side Docker login state is not copied into EPAR instances. Keep Docker Hub, cloud registry, and package registry credentials in GitHub secrets or in a deliberately secured mirror service.
