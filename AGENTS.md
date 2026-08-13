# Repository Agent Guidance

Before planning or changing provider, startup, configuration, pool, runner-image, or lifecycle behavior, read [Development and Extension Principles](docs/development/principles.md), [Contributing](CONTRIBUTING.md), [Design](docs/development/design.md), and [Adding a Provider](docs/development/adding-provider.md).

Treat the missing-config `./start` wizard, the no-Go native-controller path, Catthehacker defaults for providers that can consume Docker images, runner-artifact customization, the shared machine-derived pool-prefix generator, the shared `pool.RunnerName` format, runner routing, strict capacity, logging, host trust, registration, replacement, diagnostics, exact cleanup, and no-silent-fallback behavior as product-wide contracts. Compare an extension with the common manager path and at least one established provider instead of validating only its provider-local implementation.

A provider or onboarding change is incomplete until its wizard and generated configuration, reusable artifact/update path, shared lifecycle behavior, documentation, normal and race tests, wrapper syntax, and relevant live platform evidence are addressed. Keep unvalidated platforms or capabilities explicitly preview-only. Document and test every intentional exception to the shared contracts.

## Agent-created temporary files

Do not create ad hoc top-level directories under `.local`. All scratch files, temporary build output, one-off test state, and agent-specific caches created while investigating, implementing, or validating work must live under `.local/tmp/`, grouped beneath a descriptive task or tool directory such as `.local/tmp/<agent-name>/<task-name>/`. This includes manually assigned `GOCACHE`, `GOTMPDIR`, `GOMODCACHE`, test state roots, downloaded diagnostic tools, generated fixtures, and transient command output.

Do not place agent-created temporary content in the product-owned `.local/bin`, `.local/cache`, `.local/state`, or `.local/storage` trees unless the product code being exercised creates that exact documented path as part of its normal behavior. Never introduce another top-level `.local/<tool-name>` or `.local/<purpose>` directory for convenience. Prefer the operating system temporary directory when the artifact does not need to remain with the checkout.

Before handing work back, inspect `.local/tmp/` and remove only the exact scratch artifacts created by the current task when they are no longer needed and no process is using them. Report any intentionally retained scratch directory and its purpose. Do not delete pre-existing or user-owned temporary content merely because it is under `.local/tmp/`.
