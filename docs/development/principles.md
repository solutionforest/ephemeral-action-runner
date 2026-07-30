# Development and Extension Principles

EPAR extensions preserve the existing user flow and controller design.

## First Run

`./start` is the Quick Start and general source entry point. With no command, or with start flags only, it runs the `start` command and opens the missing-configuration wizard when needed; with an explicit command, it must forward that command and all arguments exactly as the binary and `go run ./cmd/ephemeral-action-runner` do. The local-Go and no-Go native-controller paths must behave the same, including native-host operational build trust, startup reconciliation of exactly owned stale work, and user-facing remediation commands that use the entry point the user actually invoked. A no-Go bootstrap TLS failure must preserve the native build transcript and report the requested host and presented certificate metadata without disabling verification or retrying insecurely. A selectable provider must appear in the wizard with its tooling and daemon prerequisite status; storage estimates never make provider selection unavailable or prevent configuration creation. Docker Container, Docker Sandboxes, and WSL use the same Catthehacker image, custom-script, and update-policy onboarding flow. The wizard writes the desired configuration first, then an embedded `./start` continues through the ordinary artifact provisioning and storage-admission path. Local artifact inputs always apply immediately; provider-neutral scheduling may defer only remote mutable source and Actions runner observations. Reject an unavailable platform or invalid image clearly and never silently switch a configured provider or artifact. Builder operational trust and optional runner trust are separate contracts: system roots always support the owned builder, while `image.hostTrustMode` controls only runner inheritance.

## Runner Names

The wizard defaults `pool.namePrefix` to `<sanitized-machine-name>-<six-random-hex>`, capped at 40 characters. The machine name shows where a runner belongs, the random suffix reduces collisions, and the cap leaves room for the GitHub runner suffix. Every provider must use the shared `internal/pool.RunnerName` function and keep the configured prefix literal.

## Components And Flow

Keep the flow `CLI -> pool manager -> provider -> guest/GitHub`. Provider-specific host operations belong in `internal/provider/<provider>`; the pool manager owns shared registration, readiness, replacement, status, and cleanup flow. Extend the shared interfaces instead of creating a second control path.
