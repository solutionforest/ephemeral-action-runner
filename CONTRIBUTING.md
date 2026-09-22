# Contributing to EPAR

Thanks for taking the time to contribute.

## Before You Start

- Use GitHub issues to discuss bugs, documentation gaps, and proposed changes before starting substantial work.
- Do not report security vulnerabilities in public issues. Follow [Security](docs/security.md) instead.
- EPAR runs GitHub Actions jobs on trusted infrastructure. Changes that affect runners, credentials, container privileges, workflow permissions, or cleanup boundaries need clear security reasoning and tests.

## Development Workflow

1. Fork the repository and create a focused branch from `develop`.
2. Read and preserve the [Development and Extension Principles](docs/development/principles.md).
3. Keep the change small and document any operational or security behavior that it changes.
4. Run the relevant tests locally. The baseline Go test suite is `go test ./...`.
5. Open a pull request targeting `develop` and complete the pull-request template.

Fork pull requests run the safe hosted verification workflow. The live EPAR canary is reserved for branches in this repository because it uses a protected environment and disposable privileged containers.

Timing-sensitive unit tests should use Go's `testing/synctest` clock or explicit synchronization instead of assuming that a hosted runner will schedule work within a few milliseconds. Keep cancellation, late-result rejection, and retry assertions separate from successful-cache assertions; a correctly canceled attempt is not a successful admission. Tests that exercise external processes or network I/O need real synchronization and bounded deadlines rather than a synthetic clock.

Host trust verification runs uncached Go tests on Linux, macOS, and Windows, repeats scheduler and lease regression tests 25 times on each platform, and runs the race detector on Linux. Each attempt retains structured Go test output for 14 days as workflow artifacts. For a flakiness fix, rerun the complete workflow after its first success and check for two consecutive successful attempts on the same commit; do not use failed-job-only reruns as evidence of a clean full-suite attempt.

## Pull Request Expectations

- Explain the problem, the approach, and how you tested it.
- Add or update tests when behavior changes.
- Keep credentials, private keys, tokens, and machine-specific configuration out of commits.
- Update the relevant documentation when a user-visible or operational behavior changes.
- Document and test every intentional platform or security exception.

By contributing, you agree to follow the [Code of Conduct](CODE_OF_CONDUCT.md).

See the [development documentation](docs/development/) for the architecture, provider extension checklist, verification infrastructure, and release process.
