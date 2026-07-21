# Contributing to EPAR

Thanks for taking the time to contribute.

## Before You Start

- Use GitHub issues to discuss bugs, documentation gaps, and proposed changes before starting substantial work.
- Do not report security vulnerabilities in public issues. Follow [Security](docs/security.md) instead.
- EPAR runs GitHub Actions jobs on trusted infrastructure. Changes that affect runners, credentials, container privileges, workflow permissions, or cleanup boundaries need clear security reasoning and tests.

## Development Workflow

1. Fork the repository and create a focused branch from `develop`.
2. Keep the change small and document any operational or security behavior that it changes.
3. Run the relevant tests locally. The baseline Go test suite is `go test ./...`.
4. Open a pull request targeting `develop` and complete the pull-request template.

Fork pull requests run the safe hosted verification workflow. The live EPAR canary is reserved for branches in this repository because it uses a protected environment and disposable privileged containers.

## Pull Request Expectations

- Explain the problem, the approach, and how you tested it.
- Add or update tests when behavior changes.
- Keep credentials, private keys, tokens, and machine-specific configuration out of commits.
- Update the relevant documentation when a user-visible or operational behavior changes.

By contributing, you agree to follow the [Code of Conduct](CODE_OF_CONDUCT.md).
