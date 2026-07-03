# Security

EPAR is intended for trusted private jobs by default.

Ephemeral instances reduce persistence risk, but a workflow still controls the
runner environment while it runs and can access any secrets exposed to that
workflow. Tighten GitHub runner groups, repository restrictions, and secret
exposure before using this for less-trusted workloads.

Do not mount host source directories, Docker sockets, private keys, or long-lived
cloud credentials into runner instances unless that is inside your trust
boundary.

`image.customInstallScripts` run as root during image build and their effects
are captured in the reusable image. Use them only for non-secret tooling and
configuration. Do not bake Docker credentials, GitHub tokens, private keys, or
project secrets into runner images.

The GitHub App private key remains on the host. Guest instances receive only
short-lived registration tokens at runtime. Do not bake tokens or private keys
into runner images.

WSL2 has a weaker isolation story than one full VM per job. Treat the WSL
provider as trusted-job infrastructure unless your environment has reviewed and
accepted that model.
