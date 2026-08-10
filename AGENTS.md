# Repository Agent Guidance

Before planning or changing provider, startup, configuration, pool, runner-image, or lifecycle behavior, read [Development and Extension Principles](docs/development/principles.md), [Contributing](CONTRIBUTING.md), [Design](docs/development/design.md), and [Adding a Provider](docs/development/adding-provider.md).

Treat the missing-config `./start` wizard, the no-Go native-controller path, Catthehacker defaults for providers that can consume Docker images, runner-artifact customization, the shared machine-derived pool-prefix generator, the shared `pool.RunnerName` format, runner routing, strict capacity, logging, host trust, registration, replacement, diagnostics, exact cleanup, and no-silent-fallback behavior as product-wide contracts. Compare an extension with the common manager path and at least one established provider instead of validating only its provider-local implementation.

A provider or onboarding change is incomplete until its wizard and generated configuration, reusable artifact/update path, shared lifecycle behavior, documentation, normal and race tests, wrapper syntax, and relevant live platform evidence are addressed. Keep unvalidated platforms or capabilities explicitly preview-only. Document and test every intentional exception to the shared contracts.
