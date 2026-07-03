# Ephemeral Action Runner

Ephemeral Action Runner (EPAR) manages ephemeral GitHub Actions self-hosted
runners on local machines. It keeps a small warm pool of disposable runner
instances, registers them with GitHub, deletes each instance after one job, and
creates replacements.

The currently implemented providers are Tart on Apple Silicon macOS and WSL2 on
Windows. The code is structured so additional providers such as Hyper-V,
libvirt, or Multipass can be added without rewriting the GitHub runner
lifecycle.

Start here:

- [docs/usage.md](docs/usage.md)
- [docs/github-app.md](docs/github-app.md)
- [docs/design.md](docs/design.md)
- [docs/image-build.md](docs/image-build.md)
- [docs/operations.md](docs/operations.md)
- [docs/security.md](docs/security.md)
- [docs/providers/tart.md](docs/providers/tart.md)
- [docs/providers/wsl.md](docs/providers/wsl.md)
- [docs/background.md](docs/background.md)

Tracked configs are examples only. Put real GitHub App IDs, private key paths,
and local runner settings in `.local/config.yml`, `configs/*.local.yml`, or
`~/.config/ephemeral-action-runner/config.yml`; those paths are not intended for
Git.
