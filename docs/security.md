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

The GitHub App private key remains on the host. Guest instances receive only short-lived registration tokens at runtime. Do not bake tokens or private keys into runner images.
