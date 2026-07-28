# Adding A Provider

Read [Development and Extension Principles](../development-principles.md) and [Design](../design.md).

Put provider commands and host integration in `internal/provider/<provider>`. Shared onboarding, naming, image, pool lifecycle, GitHub, state, capacity, and retention behavior stays in its common package.

A provider is complete only when it:

- Registers its constructor, configuration rules, wizard contribution, reusable-artifact capabilities, and platform status in the provider registry.
- Implements every required lifecycle, exact inventory, artifact-requirement, and storage-surface contract without silent fallback.
- Uses the wizard’s complete machine-derived prefix and the shared `pool.RunnerName` format.
- Preserves strict `pool.instances`, durable exact identities, quarantine on uncertainty, resumable cleanup, diagnostics, and replacement.
- Reuses Catthehacker defaults when it consumes Docker runner images, unless an intentional exception is documented and tested.
- Adds provider contract tests, configuration and wizard tests, race tests, wrapper checks, and relevant live-platform evidence.

Provider cleanup must target exact identities. Never replace an unavailable exact operation with a prefix deletion, wildcard, broad prune, reset, or deletion of an unknown/shared resource.
