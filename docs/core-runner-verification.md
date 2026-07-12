# Level 1 Core Runner Verification

The `Core runner verification` GitHub Actions workflow proves EPAR's central
contract against GitHub: create an isolated Docker-DinD runner, register it for
one job, replace it after that job, run a second job on the replacement, and
clean up the runner records and outer containers.

This is an infrastructure canary, not a language or framework compatibility
matrix. The workload checks checkout and artifact transfer, then exercises the
nested Docker daemon with Buildx, Docker Compose, a health check, and an HTTP
request.

## Architecture

The workflow has three jobs:

1. `Core runner controller` runs on a fresh, trusted GitHub-hosted runner. It
   builds EPAR and the pinned lightweight core image, pre-cleans the
   `epar-ci-core` boundary, and supervises one ephemeral runner.
2. `Core canary 1` runs on that ephemeral runner, validates its basic runtime,
   and uploads its runner name and a nonce.
3. `Core canary 2` waits for the first job, runs on the replacement runner,
   verifies that the runner name changed, downloads the artifact, and exercises
   Buildx and Compose.

The controller reads the workflow-job records to confirm that both canaries
used the expected group and unique per-run label, ran on distinct
`epar-ci-core-*` runners, and succeeded. Workflow concurrency is serialized
because every run intentionally shares the fixed cleanup prefix.

## Required GitHub Setup

### GitHub-hosted controller

The controller uses the standard GitHub-hosted `ubuntu-latest` runner. No
pre-existing self-hosted controller is required. Each run receives a fresh
Linux VM with Docker, Bash, curl, jq, and GNU `timeout`; the workflow installs
the Go version declared by `go.mod` before building EPAR.

### Restricted ephemeral-runner group

In the organization settings, create a runner group named
`epar-ci-canary` and restrict its repository access to
`solutionforest/ephemeral-action-runner`.

EPAR registers the temporary runners in this group with no GitHub default
labels and only a per-run label such as `epar-core-123456-1`. The canary jobs
target both the group and that unique label, so unrelated self-hosted runners
cannot accept them.

### GitHub App and protected environment

The GitHub App must be installed in the target organization and have
organization self-hosted-runner read/write permission. Create a GitHub Actions
environment named `epar-live-ci`, restrict it to trusted branches, and add:

| Kind | Name | Value |
| --- | --- | --- |
| Environment secret | `EPAR_GITHUB_APP_PRIVATE_KEY` | The complete PEM private key generated for the GitHub App. |
| Environment variable | `EPAR_GITHUB_APP_ID` | The numeric App ID. |
| Environment variable | `EPAR_GITHUB_ORGANIZATION` | The organization login, for example `solutionforest`. |

Keep the PEM as an environment secret, including its original line breaks. Do
not store it in repository variables, workflow YAML, a tracked config file, or
an artifact. The workflow materializes it in a mode-restricted temporary file
on the controller and removes that file during cleanup.

For the initial feature-branch test, allow
`feature/level-1-core-runner-verification`. Allow `develop` for the ongoing push
trigger. Requiring an environment reviewer is possible, but every matching
push will wait for that approval.

## Trust Boundaries and Triggers

The controller job is privileged and secret-bearing. It receives the GitHub
App key and a workflow token with Actions write permission, and it can start
privileged containers in its disposable GitHub-hosted VM. It runs only for
trusted repository changes and never for pull-request events.

The two canary jobs do not receive the GitHub App key. They receive only the
minimum workflow permissions needed for checkout and artifact operations, and
their Docker workloads run in the private daemon inside a disposable outer
container.

The workflow runs on:

- pushes to `feature/level-1-core-runner-verification` during initial rollout
- pushes to `develop`
- manual `workflow_dispatch` after the workflow is present on the repository's
  default branch

It intentionally has no `pull_request` or `pull_request_target` trigger. Do not
add one: code from an untrusted pull request must not reach the controller,
environment secrets, or privileged Docker host. Remove the temporary feature
branch trigger after the initial rollout is complete.

## Expected Result and Cleanup

A successful run reports both canary runner names in the job summary. They must
be different, begin with `epar-ci-core-`, belong to `epar-ci-canary`, and carry
the run's `epar-core-<run-id>-<attempt>` label. The second canary must also pass
the artifact, Buildx, Compose, container health, and HTTP checks.

The controller performs cleanup before and after the canaries. It stops the
pool supervisor, deletes GitHub runner registrations within the exact
`epar-ci-core` prefix boundary, removes matching outer Docker-DinD containers,
and deletes its temporary key, generated config, and logs. Before failed-run
cleanup deletes those logs, the controller prints a sanitized final 200 lines
from the pool-supervisor log and each available runner log. Runner launch or
online readiness failures first append bounded process state, `run.log`,
latest `Runner_*.log`, and Docker-DinD daemon tails to the host guest log, so
those diagnostics pass through the same sanitizer before cleanup. A controller
failure then attempts cleanup before canceling the workflow so queued canary
jobs do not remain indefinitely. The next run also pre-cleans the same boundary.

A sudden controller failure can bypass application cleanup. GitHub discards the
hosted VM and its Docker containers, while the next run pre-cleans stale GitHub
runner registrations within the same prefix boundary.

## Troubleshooting

### Controller job remains queued

- Confirm the organization and repository allow standard GitHub-hosted
  runners.
- Check GitHub Actions service status and the account's hosted-runner
  concurrency.

### Controller starts, but canaries remain queued

- Open the controller log and check image-build errors plus the grouped,
  sanitized pool-supervisor and runner-log tails printed before cleanup.
- Confirm `epar-ci-canary` exists with that exact spelling and permits this
  repository.
- Confirm the GitHub App is installed in the organization and can administer
  organization self-hosted runners.
- Confirm the environment's App ID is numeric, organization value is the login
  rather than a display name, and the private-key secret contains the complete
  PEM.
- In the organization runner list, look for an online
  `epar-ci-core-*` runner carrying the unique label shown in the workflow log.

### Image build or Docker workload fails

- Check controller disk space and Docker health.
- Confirm outbound access to the pinned Catthehacker and BusyBox images.
- For nested-Docker startup failures, inspect the grouped Docker-DinD runner-log
  tail in the controller output.

### Workflow is canceled after a controller error

This is expected failure behavior. The controller cancels the run after its
bounded wait or another fatal orchestration error so unmatched canary jobs do
not stay queued. Diagnose the first controller error rather than treating the
cancellation itself as the root cause.

## Manual Cleanup

Use a local, untracked config based on
`configs/docker-dind.core.example.yml`, with the same organization, GitHub App
key path, and `pool.namePrefix: epar-ci-core`, then run:

```bash
go run ./cmd/ephemeral-action-runner cleanup \
  --config .local/core-cleanup.yml \
  --project-root .
```

This removes matching organization runner records. Docker containers from the
controller run existed only on its disposable GitHub-hosted VM. If cleanup
still reports an error, remove remaining `epar-ci-core-*` records from the
organization's Actions runner settings. Do not use a broader prefix: EPAR
cleanup is deliberately bounded to `epar-ci-core` and `epar-ci-core-*`.
