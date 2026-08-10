# Core Runner Verification

The **Core runner verification** workflow proves EPAR's central contract against GitHub: create an isolated Docker Container runner, register it for one job, replace it after that job, run a second job on the replacement, and clean up the runner records and outer containers.

This is an infrastructure canary, not a language or framework compatibility matrix. Its workload checks checkout and artifact transfer, then exercises the private Docker daemon with Buildx, Docker Compose, a health check, and an HTTP request.

## Architecture

The workflow has three jobs:

1. **Core runner controller** runs on a fresh trusted GitHub-hosted runner, builds EPAR and the pinned lightweight image, pre-cleans the `epar-ci-core` boundary, and supervises one ephemeral runner.
2. **Core canary 1** runs on that runner, validates its runtime, and uploads its runner name and a nonce.
3. **Core canary 2** runs on the replacement, confirms the runner name changed, downloads the artifact, and exercises Buildx and Compose.

The controller confirms that both canaries used the expected group and per-run label, ran on distinct `epar-ci-core-*` runners, and succeeded. Workflow concurrency is serialized because every run shares the fixed cleanup prefix.

## Required GitHub Setup

### Controller

The controller uses GitHub's `ubuntu-latest` runner. No persistent self-hosted controller is required.

### Runner Group

Create an organization runner group named `epar-ci-canary` and restrict it to `solutionforest/ephemeral-action-runner`.

The workflow registers temporary runners with no GitHub default labels and only a per-run label such as `epar-core-123456-1`. Canary jobs target both the group and that unique label.

This repository is public, so the canary config deliberately sets `security.runnerGroup.requirePublicRepositoriesDisabled: false` while retaining enforcement, explicit naming, non-default-group use, and selected-repository access. This narrow exception is for the protected canary only and is not the recommended public-repository deployment model.

### GitHub App And Protected Environment

Install a GitHub App with organization self-hosted-runner read/write permission. Create an environment named `epar-live-ci`, restrict it to `develop`, `main`, and `refs/pull/*/merge`, and add:

| Kind | Name | Value |
| --- | --- | --- |
| Environment secret | `EPAR_GITHUB_APP_PRIVATE_KEY` | Complete GitHub App PEM private key |
| Environment variable | `EPAR_GITHUB_APP_ID` | Numeric App ID |
| Environment variable | `EPAR_GITHUB_ORGANIZATION` | Organization login |

Keep the PEM in the environment secret with its original line breaks. The workflow writes it to a restricted temporary file on the disposable controller and removes it during cleanup.

## Trust Boundaries And Triggers

The controller is privileged and secret-bearing. It receives the GitHub App key and an Actions write token, and it starts privileged containers in its disposable GitHub-hosted VM.

The controller and canaries run only for trusted repository changes: pushes to `develop` or `main`, same-repository pull requests targeting those branches, and manual dispatch after the workflow exists on the default branch. Fork pull requests trigger the workflow but skip all three verification jobs before they can receive environment secrets or use the canary group.

The workflow does not use `pull_request_target`. Keep the same-repository guard on every verification job.

The canaries never receive the GitHub App key. They receive only the permissions needed for checkout and artifact operations.

## Expected Result And Cleanup

A successful run reports two different runner names beginning with `epar-ci-core-`. Both belong to `epar-ci-canary`, carry the run's unique label, and complete successfully. The second also passes the artifact, Buildx, Compose, health, and HTTP checks.

The controller cleans before and after the canaries. It stops the supervisor, deletes GitHub runner registrations within the exact `epar-ci-core` prefix boundary, removes matching outer containers, and deletes its temporary key, config, and logs.

Before failed-run cleanup removes logs, the controller prints sanitized bounded tails from the supervisor and runner logs. A controller failure attempts cleanup before canceling the workflow so unmatched canary jobs do not remain queued.

A sudden controller loss can bypass application cleanup. GitHub discards the hosted VM and its containers; the next run pre-cleans stale GitHub runner registrations within the same prefix.

## Troubleshooting

### Controller Remains Queued

- Confirm the repository can use standard GitHub-hosted runners.
- Check GitHub Actions service status and hosted-runner concurrency.

### Canaries Remain Queued

- Check the controller's sanitized supervisor and runner-log tails.
- Confirm `epar-ci-canary` exists and permits this repository.
- Confirm the GitHub App is installed and can manage organization runners.
- Confirm the environment values contain the numeric App ID, organization login, and complete PEM.
- Check for an online `epar-ci-core-*` runner carrying the unique label shown in the workflow.

### Image Or Docker Workload Fails

- Check controller disk space and Docker health.
- Confirm outbound access to the pinned Catthehacker and BusyBox images.
- Inspect the grouped Docker Container runner-log tail in the controller output.

### Workflow Is Canceled

Cancellation after a controller error is expected. Diagnose the first controller error; cancellation prevents unmatched canary jobs from remaining queued.

## Manual Cleanup

Create an untracked config based on `configs/docker-container.core.example.yml` with the same organization, GitHub App key path, and `pool.namePrefix: epar-ci-core`, then run:

```bash
./start cleanup \
  --config .local/core-cleanup.yml \
  --project-root .
```

This removes matching organization runner records. The controller's Docker containers existed only on its discarded hosted VM. If cleanup still fails, remove remaining `epar-ci-core-*` records from the organization's Actions runner settings. Do not use a broader prefix.
