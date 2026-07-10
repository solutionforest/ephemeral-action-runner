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

1. `Core runner controller` runs on a persistent, trusted self-hosted runner.
   It builds EPAR and the pinned lightweight core image, pre-cleans the
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

### Trusted controller runner

Provide trusted Linux X64 self-hosted runners with all of the following:

- the standard `self-hosted` label
- Docker access and support for privileged Linux containers
- Bash, curl, jq, and GNU `timeout`
- enough disk space to build and retain the core Docker image
- a current GitHub Actions runner compatible with actions implemented on
  Node.js 24

The repository must be allowed to use these runners. The controller uses
`runs-on: self-hosted`, so every eligible runner that may accept the job must
meet these requirements and be trusted with the GitHub App key and privileged
Docker access.

Because the controller is not pinned to one machine, a forced cancellation or
host outage can leave a local DinD container on the machine that accepted that
run. A later run on another self-hosted machine can remove the organization
runner registration, but it cannot remove that host-local container. Inspect
all eligible controller hosts after an unclean interruption.

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

### Optional network variables

Add these only when the controller's network requires them:

| Environment variable | Purpose |
| --- | --- |
| `EPAR_TRUSTED_CA_CERTIFICATE_PATH` | Absolute path on the controller host to a readable PEM CA certificate for private TLS inspection. The certificate is installed into the generated runner image; TLS verification remains enabled. |
| `EPAR_DOCKER_PROXY` | One unauthenticated HTTP(S) proxy URL inherited by the outer DinD daemon. URLs containing user information are rejected. |
| `EPAR_DOCKER_REGISTRY_MIRROR` | One HTTP(S) Docker registry mirror URL reachable from the nested Docker daemon. |

The CA variable is a host path, not the certificate contents. Ensure the same
path exists for the operating-system account running the controller service.
Do not put proxy credentials in these variables.

## Trust Boundaries and Triggers

The controller job is privileged and secret-bearing. It receives the GitHub
App key and a workflow token with Actions write permission, and it can start
privileged containers on the host. It must run only on a controlled machine and
only for trusted repository changes.

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
and deletes its temporary key, generated config, and logs. A controller failure
attempts cleanup before canceling the workflow so queued canary jobs do not
remain indefinitely. The next run also pre-cleans the same boundary.

A sudden controller-host outage or forced process termination can bypass that
cleanup. Always inspect both the organization runner list and the controller's
Docker containers after such an event.

## Troubleshooting

### Controller job remains queued

- Confirm a compatible Linux X64 runner labeled `self-hosted` is online and
  accessible to this repository.
- Confirm its runner service is current enough for the workflow's Node.js
  24-based actions.
- Confirm the runner account can execute `docker info` and the Docker runtime
  permits privileged containers.

### Controller starts, but canaries remain queued

- Open the controller log and check image-build, registration, and pool
  supervisor errors.
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
- Confirm outbound access to the pinned Gitea and BusyBox images.
- If TLS inspection is used, verify that
  `EPAR_TRUSTED_CA_CERTIFICATE_PATH` is readable by the controller service
  account and contains a CA certificate in PEM format.
- Verify the proxy or registry mirror is reachable from Docker, not merely from
  the interactive host shell. The optional proxy and mirror accept only one
  HTTP(S) URL and intentionally reject embedded credentials.
- For nested-Docker startup failures, confirm the host supports privileged
  containers and inspect the outer container before cleanup removes it.

### Workflow is canceled after a controller error

This is expected failure behavior. The controller cancels the run after its
bounded wait or another fatal orchestration error so unmatched canary jobs do
not stay queued. Diagnose the first controller error rather than treating the
cancellation itself as the root cause.

## Manual Cleanup

First verify the exact cleanup boundary. On the trusted controller host:

```bash
docker ps --all --format '{{.Names}}' | grep -E '^epar-ci-core(-|$)'
```

Use a local, untracked config based on
`configs/docker-dind.core.example.yml`, with the same organization, GitHub App
key path, and `pool.namePrefix: epar-ci-core`, then run:

```bash
go run ./cmd/ephemeral-action-runner cleanup \
  --config .local/core-cleanup.yml \
  --project-root .
```

This removes both matching organization runner records and local Docker-DinD
containers. If GitHub credentials are temporarily unavailable, local-only
cleanup is possible:

```bash
go run ./cmd/ephemeral-action-runner cleanup \
  --config .local/core-cleanup.yml \
  --project-root . \
  --no-github
```

Then remove remaining `epar-ci-core-*` runner records from the organization's
Actions runner settings. For direct Docker cleanup, review the names produced
by the listing command and run `docker rm --force --volumes <exact-name>` for
each confirmed match. Do not use a broader prefix: EPAR cleanup is deliberately
bounded to `epar-ci-core` and `epar-ci-core-*`.
