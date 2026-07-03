# Adding A Provider

Providers implement the shared interface in `internal/provider`. The controller
expects a provider to clone or create an instance, start it, execute guest
commands, return an address when available, stop/delete it, and list existing
instances for prefix-safe cleanup.

Keep provider behavior idempotent where possible. Cleanup must only remove
instances whose names match `pool.namePrefix`.

Use provider-specific docs for host setup, image format, and isolation caveats.
