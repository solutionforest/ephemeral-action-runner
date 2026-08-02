# Development and Extension Principles

EPAR extensions preserve the existing user flow and controller design.

## Universal Start Contract

The `start` wrapper family is EPAR's universal operator entry point for first-run configuration, normal controller operation, manual launch, and machine autorun. Use `./start` on macOS, Linux, WSL, and Git Bash, and `.\start.ps1` on native Windows. Operator documentation and startup automation must invoke the applicable wrapper rather than a locally built controller binary.

With no arguments, a wrapper invokes the controller's `start` command. When the first argument is a global flag, the wrapper inserts `start` before forwarding the complete argument array. When the first argument is an explicit command, the wrapper forwards that command and every argument exactly. Local-Go execution and the no-Go cached native-controller path must have the same command, argument, configuration, trust, reconciliation, and remediation behavior. Direct `go run` and `scripts/run-with-docker.*` invocations are development and diagnostic interfaces, not alternative operator entry points.

## First Run

The wrapper opens the missing-configuration wizard when needed. A no-Go bootstrap TLS failure must preserve the native build transcript and report the requested host and presented certificate metadata without disabling verification or retrying insecurely. A selectable provider must appear in the wizard with its tooling and daemon prerequisite status; storage estimates never make provider selection unavailable or prevent configuration creation. Docker Container, Docker Sandboxes, and WSL use the same Catthehacker source, custom-script, and update-policy onboarding strategy. Provider descriptors may narrow the shared profile choices when an artifact contract requires capabilities that not every upstream tag supplies: Docker Sandboxes admits only `full-latest` and `act-latest` because its reusable template requires the private Docker daemon and runtime closure, while Docker Container and WSL retain specialized and custom tags. The wizard keeps later answers in a navigable draft, shows provider-specific estimates only when applicable, and writes only after one provider-neutral final review. An embedded `./start` then continues through the ordinary artifact provisioning and storage-admission path. Local artifact inputs always apply immediately; provider-neutral scheduling may defer only remote mutable source and Actions runner observations. Reject an unavailable platform or invalid image clearly and never silently switch a configured provider or artifact. Builder operational trust and optional runner trust are separate contracts: system roots always support the owned builder, while `image.hostTrustMode` controls only runner inheritance.

## Runner Names

The wizard defaults `pool.namePrefix` to `<sanitized-machine-name>-<six-random-hex>`, capped at 40 characters. The machine name shows where a runner belongs, the random suffix reduces collisions, and the cap leaves room for the GitHub runner suffix. Every provider must use the shared `internal/pool.RunnerName` function and keep the configured prefix literal.

## Components And Flow

Keep the flow `CLI -> pool manager -> provider -> guest/GitHub`. Provider-specific host operations belong in `internal/provider/<provider>`; the pool manager owns shared registration, readiness, replacement, status, and cleanup flow. Extend the shared interfaces instead of creating a second control path.
