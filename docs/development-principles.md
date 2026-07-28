# Development and Extension Principles

EPAR extensions preserve the existing user flow and controller design.

## First Run

`./start` is the Quick Start and general source entry point. With no command, or with start flags only, it runs the `start` command and opens the missing-configuration wizard when needed; with an explicit command, it must forward that command and all arguments exactly as the binary and `go run ./cmd/ephemeral-action-runner` do. The local-Go and no-Go native-controller paths must behave the same, and user-facing remediation commands must use the entry point the user actually invoked. A selectable provider must appear in the wizard with its prerequisite status. Reject an unavailable selection clearly and never silently switch a configured provider.

## Runner Names

The wizard defaults `pool.namePrefix` to `<sanitized-machine-name>-<six-random-hex>`, capped at 40 characters. The machine name shows where a runner belongs, the random suffix reduces collisions, and the cap leaves room for the GitHub runner suffix. Every provider must use the shared `internal/pool.RunnerName` function and keep the configured prefix literal.

## Components And Flow

Keep the flow `CLI -> pool manager -> provider -> guest/GitHub`. Provider-specific host operations belong in `internal/provider/<provider>`; the pool manager owns shared registration, readiness, replacement, status, and cleanup flow. Extend the shared interfaces instead of creating a second control path.
