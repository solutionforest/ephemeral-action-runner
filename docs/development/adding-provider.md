# Adding A Provider

Read [Development and Extension Principles](principles.md) and [Design](design.md).

Put provider commands and host integration in `internal/provider/<provider>`. Shared onboarding, naming, image, pool lifecycle, GitHub, state, capacity, and retention behavior stays in its common package.

A provider is complete only when it:

- Registers its constructor, configuration rules, prerequisite strategy, onboarding strategy, host-trust applicability, review contribution, reusable-artifact capabilities, and platform status in the provider registry.
- Implements every required lifecycle, exact inventory, artifact-requirement, storage-surface, ownership receipt, and crash-recovery contract without silent fallback.
- Uses the wizard’s complete machine-derived prefix and the shared `pool.RunnerName` format.
- Preserves strict `pool.instances`, durable exact identities, quarantine on uncertainty, resumable cleanup, diagnostics, and replacement.
- Reuses Catthehacker defaults when it consumes Docker runner images, unless an intentional exception is documented and tested.
- Adds provider contract tests, configuration and wizard tests, race tests, wrapper checks, and relevant live-platform evidence.

Prefer an existing onboarding strategy when the provider has the same domain behavior. Docker-image consumers normally reuse the Catthehacker strategy, including image profiles and estimates; the common wizard supplies empty custom scripts, the default update policy, and host-trust defaults according to provider applicability without additional prompts. Add a new typed strategy only when the provider requires different inputs or artifact behavior; the common wizard continues to own prompts, section history, `0` and `/back` navigation, review, and final creation. A provider still needs an explicit generated-configuration renderer until configuration serialization is consolidated, and its output must be compared with the shared manager path and an established provider.

Provider cleanup must target exact identities and record enough immutable evidence for common startup housekeeping to distinguish current, superseded, incomplete, and unknown resources. Never replace an unavailable exact operation with a prefix deletion, wildcard, broad prune, reset, or deletion of an unknown/shared resource.
