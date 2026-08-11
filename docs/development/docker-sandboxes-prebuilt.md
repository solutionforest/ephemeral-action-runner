# Docker Sandboxes prebuilt image publication and acceptance

EPAR publishes its Docker Sandboxes template as an immutable multi-platform OCI package with signed SBOM, SLSA provenance, and catalog evidence. This is an upstream-driven package workflow, not a source release: it creates no Git commit, pull request, repository tag, GitHub Release, or source-tree change.

The canonical source is `ghcr.io/catthehacker/ubuntu`. The public package is `ghcr.io/solutionforest/ephemeral-action-runner/docker-sandboxes-template`. Docker Hub is never used as a source fallback because its OCI identities may differ from GHCR even when the logical image content matches.

Act (`act-latest`) is the initial supported profile. Full (`full-latest`) remains disabled and candidate-only until it completes an independent acceptance cycle.

## Workflow triggers and hosted build gates

`.github/workflows/docker-sandboxes-images.yml` polls every six hours at minute 37, supports manual dispatch, and publishes for recipe-related pushes only on `main`. GitHub executes the cron only from the default branch. Pull requests to `develop` or `main` that change a publisher, recipe, or committed-asset path run publisher, signed-evidence, and asset validation without logging in to GHCR, building a package, or pushing any manifest. This prevents a normal `develop` to `main` promotion from publishing the same source change twice.

The amd64 build runs on GitHub-hosted `ubuntu-latest`; the arm64 build runs on GitHub-hosted `ubuntu-24.04-arm`. The workflow has no persistent self-hosted runner dependency and no `EPAR_PREBUILT_LIVE` switch. Hosted jobs resolve the GHCR source descriptor, build from its immutable digest, inspect both runnable platform manifests by digest, assemble an index from those exact digests, require exactly two descriptors, run package smoke checks, generate and sign one index-level SLSA provenance statement and one index-level SPDX SBOM, verify their referrers, and publish an immutable signed candidate catalog. Platform builds disable BuildKit's additional per-platform SBOM/provenance indexes because EPAR's trust decision uses the separately signed index-level evidence and catalog. The index-level SPDX document records the package, recipe, and exact platform identities rather than reproducing BuildKit's more detailed component inventory; this deliberate narrower audit scope avoids four additional per-publication GHCR version rows without weakening the evidence enforced by EPAR.

The workflow grants `contents: read`, `packages: write`, `attestations: write`, and `id-token: write` to publication jobs, while pull-request validation overrides permissions to `contents: read`. Third-party actions are pinned to full commit SHAs. Publication is serialized by one non-cancelling concurrency group; validation uses one group per pull request and cancels only a superseded validation run for that same pull request.

## Candidate publication contract

Every new package is first recorded as `candidate`. Before manual acceptance, publication may create:

- an immutable package index and canonical tag such as `act-latest-pkg-<64 hex index digest>`;
- exact amd64 and arm64 platform manifests;
- signed SLSA provenance and SPDX SBOM referrers;
- an immutable catalog object and canonical tag such as `catalog-v1-pkg-<64 hex catalog digest>`.

Candidate publication does not move `catalog-v1` or `act-latest`. This permits first-catalog bootstrap: a candidate can be acquired through its exact signed immutable catalog even when no moving catalog exists yet.

Catalog readers resolve an exact catalog manifest, validate its artifact/config/single-layer media contract, and fetch that layer by descriptor digest into a caller-chosen file. They never extract the publisher-supplied OCI layer title as a filesystem path. New catalogs are published from controlled relative filenames, while this descriptor path remains compatible with the initial catalogs that recorded absolute runner-temporary titles.

### GHCR version graph

GitHub Packages displays every OCI manifest as a package version, so one logical candidate appears as several rows. The expected retained graph contains the final multi-platform package index, its amd64 and arm64 runnable manifests, signed SLSA and SPDX referrer manifests and their discovery index, the immutable catalog manifest, and the catalog signature and discovery index. Tags named `act-latest-pkg-<digest>` and `catalog-v1-pkg-<digest>` are write-once audit identities. Run-specific `candidate-<profile>-<run>-<attempt>` tags are staging and diagnostic aliases, while `sha256-<subject digest>` tags are attestation-discovery indexes used by the verifier; neither is an end-user image channel. An untagged child manifest can still be required by a retained index and must not be deleted merely because the GitHub Packages page calls it untagged.

Production builds always use the resolved immutable upstream digest. The upstream tag is re-resolved before publication. If it moves during the build, the package remains an immutable candidate and no moving alias advances.

The stable tuple is `(source index and platform digests, recipe digest and revision, runtime contract, template schema, runner version and asset digests, locked tool digests)`. Recipe, runtime, runner, tooling, schema, or platform changes always require fresh manual acceptance. After one tuple has completed two-platform acceptance, a later source-only Catthehacker digest change may auto-promote only when the EPAR-controlled tuple is identical and every hosted package/evidence gate passes. Full never auto-promotes while disabled.

## Explicit candidate-acceptance configuration

Candidate mode is an operator-only acceptance path. The wizard never generates it. It requires an exact package digest, exact immutable candidate catalog, and exact GitHub workflow evidence ref; it never follows `act-latest`, never falls back to local building, and persists `candidate` status in the receipt.

Create separate configs derived from `.local/config.sbx.yml`. Replace every placeholder below with values from the candidate publication summary. The digest label uses the first 12 hexadecimal characters after `sha256:`.

```yaml
image:
  distribution: prebuilt
  sourceType: docker-image
  sourceImage: ghcr.io/catthehacker/ubuntu:act-latest
  sourcePlatform: linux/amd64
  prebuiltReference: ghcr.io/solutionforest/ephemeral-action-runner/docker-sandboxes-template:act-latest
  prebuiltDigest: sha256:<candidate-index-digest>
  prebuiltAcceptance: true
  prebuiltCatalogReference: ghcr.io/solutionforest/ephemeral-action-runner/docker-sandboxes-template:catalog-v1-pkg-<catalog-digest>
  prebuiltEvidenceRef: refs/heads/<publication-branch>
  updateFrequency: manual
  customInstallScripts: []
  trustedCaCertificatePaths: []
  hostTrustMode: overlay
  hostTrustScopes: [system]

pool:
  instances: 1
  namePrefix: epar-prebuilt-act-<digest12>-amd64

runner:
  group: epar-dev-test
  labels: [epar-prebuilt-act-<digest12>-amd64]
  includeHostLabel: false
  ephemeral: true
  noDefaultLabels: true

security:
  runnerGroup:
    enforcement: enforce
    requireExplicitGroup: true
    requireNonDefaultGroup: true
    requiredRepositoryAccess: selected
    requirePublicRepositoriesDisabled: true

provider:
  type: docker-sandboxes
  platform: linux/amd64
```

For the Mac config, change `sourcePlatform` and `provider.platform` to `linux/arm64`, use a distinct `pool.namePrefix`, and change the unique label suffix to `-arm64`. The organization runner group `epar-dev-test` must allow only `solutionforest/ephemeral-action-runner-test` and must not allow public repositories. The host machines run EPAR normally; they are not registered directly as persistent GitHub Actions runners.

Run `./start --config <candidate-config>` to provision and start the temporary acceptance pool; it automatically performs the required image acquisition before creating a Sandbox. An explicit `./start image build --config <candidate-config>` is optional when an operator wants to provision and inspect the template without starting the pool. Successful acquisition verifies the immutable catalog and package evidence, materializes/imports the selected platform, performs exact `sbx template ls` readback, and writes `.local/state/image/<config-id>/docker-sandboxes/active.json`. A missing, mismatched, unsigned, revoked, or wrong-ref candidate fails closed without a local Catthehacker build or provider fallback.

## Four-run acceptance suite

Start one temporary EPAR controller from each candidate config. Wait until its one ephemeral runner is online in the `epar-dev-test` group under the exact digest-bound label. Dispatch exactly these four runs in `solutionforest/ephemeral-action-runner-test`, overriding only `runner_label` and leaving every other input at its default:

| Workflow | amd64 | arm64 |
| --- | --- | --- |
| `playwright-docker.yml` | one run on `epar-prebuilt-act-<digest12>-amd64` | one run on `epar-prebuilt-act-<digest12>-arm64` |
| `dockerhub-private-pull.yml` | one run on `epar-prebuilt-act-<digest12>-amd64` | one run on `epar-prebuilt-act-<digest12>-arm64` |

For every run, the human reviewer verifies the private repository, exact workflow file, successful conclusion, unique runner label, expected generated runner name, and the candidate/platform identity in the EPAR receipt. Each ephemeral job must be replaced normally while the controller is running. If the default private Docker Hub fixture is not arm64-compatible, acceptance is blocked; do not waive or silently skip that run.

After both workflows pass on both platforms, stop both EPAR controllers. Verify that their GitHub runner records, exact Sandboxes, candidate staging directories, and unreferenced obsolete template generations are cleaned up. Hash each reviewed `active.json` receipt with SHA-256 and retain the two hashes with the four GitHub run IDs/URLs.

No cross-repository PAT or GitHub App secret is added. The protected promotion reviewer uses authenticated GitHub access to inspect the four private-repository runs before approving the environment and entering their evidence into the workflow dispatch.

## Protected promotion

Stable promotion must run from `main`, and the package evidence must have been produced from `refs/heads/main`. Feature-branch acceptance may be reused only when the main publication has the identical package index digest and complete tuple.

Dispatch `docker-sandboxes-images.yml` on `main` with:

- `promote_candidate: true`;
- `profile: act`;
- the exact `candidate_digest` and `candidate_catalog_reference`;
- `acceptance_evidence_json` containing the four reviewed workflow run IDs, two EPAR receipt SHA-256 values, and the exact ephemeral runner name used by every workflow run;
- `promotion_confirmation: PROMOTE`.

The evidence input is one JSON object so the workflow remains below GitHub's ten-input limit:

```json
{"amd64PlaywrightRunId":123,"amd64PlaywrightRunnerName":"<exact generated name>","amd64DockerHubRunId":124,"amd64DockerHubRunnerName":"<different exact generated name>","amd64ReceiptSha256":"sha256:<64 hex>","arm64PlaywrightRunId":125,"arm64PlaywrightRunnerName":"<exact generated name>","arm64DockerHubRunId":126,"arm64DockerHubRunnerName":"<different exact generated name>","arm64ReceiptSha256":"sha256:<64 hex>"}
```

Before GitHub requests approval for the protected environment, an unprotected `Prepare protected promotion review` job verifies the signed candidate catalog and package evidence and writes a job summary containing the exact package, catalog, source, recipe, runtime, runner, platform, four acceptance-run links, runner names, and receipt hashes. The reviewer opens that completed job summary, follows the four authenticated private-repository links, completes its checklist, and only then approves the waiting `epar-prebuilt-promotion` deployment. The prepare job cannot approve a deployment or move a package tag.

The `epar-prebuilt-promotion` environment must require an authorized reviewer. After approval, the protected job independently repeats the immutable catalog and package checks, rechecks the upstream source, appends two platform acceptance records, requires exactly the two approved workflows per platform, and performs protected catalog compare-and-swap. It then signs and verifies the promoted catalog, moves `catalog-v1`, and moves `act-latest` last. Incomplete, failed, misrouted, single-platform, wrong-workflow, alias-raced, or source-raced evidence cannot promote.

Catalog pointer and package-alias updates remain journaled by workflow rollback/reconciliation logic. A failure before stable pointer movement leaves the candidate immutable. A failure between pointer movements restores or idempotently reconciles the previous verified state.

## Publisher CLI

```text
go run ./cmd/epar-prebuilt-publisher plan --catalog catalog-state.json --input publication-input.json --output plan.json
go run ./cmd/epar-prebuilt-publisher accept --catalog catalog-state.json --input acceptance.json
go run ./cmd/epar-prebuilt-publisher promote --protected --catalog catalog-state.json --plan candidate-plan.json
go run ./cmd/epar-prebuilt-publisher catalog --catalog catalog-state.json --output catalog.canonical.json
go run ./cmd/epar-prebuilt-publisher verify-catalog --repository ghcr.io/solutionforest/ephemeral-action-runner/docker-sandboxes-template --reference ghcr.io/solutionforest/ephemeral-action-runner/docker-sandboxes-template:catalog-v1-pkg-<64 hex> --ref refs/heads/main --allowed-events schedule,workflow_dispatch,push
go run ./cmd/epar-prebuilt-publisher verify-package --reference ghcr.io/solutionforest/ephemeral-action-runner/docker-sandboxes-template@sha256:<64 hex> --entry publication-entry.json --repository ghcr.io/solutionforest/ephemeral-action-runner/docker-sandboxes-template --ref refs/heads/main --allowed-events schedule,workflow_dispatch,push
```

`accept` appends immutable human-reviewed platform evidence. It accepts only `playwright-docker.yml` and `dockerhub-private-pull.yml` in `solutionforest/ephemeral-action-runner-test`, requires successful run evidence and exact receipt/runner identity, and does not itself move an alias. `promote --protected` requires complete hosted gates plus both reviewed platform records.

## Retention and revocation

Immutable accepted, active, superseded rollback, and revoked package and catalog objects are retained for audit and recovery. The workflow performs no broad deletion. Unaccepted CI candidates may be removed only by future reachability-aware maintenance after a documented retention window; that maintenance must preserve every object referenced by a retained package index, attestation-discovery index, signed catalog, active receipt, or rollback identity. Manual deletion of individual untagged GitHub Packages rows is unsafe. Revocation appends `revoked` or `critical-revoked` status to a newly signed catalog; it does not delete the immutable package. Candidate acquisition rejects a revoked status in its exact signed catalog. Normal stable consumers enforce the current signed moving-catalog status, with `critical-revoked` blocking new Sandbox admissions.

The public base contains no workstation CA, enterprise credential, proxy endpoint, `NO_PROXY`, forward-bypass configuration, pool data, or custom install script. Host trust is a runtime overlay. Custom scripts create a local derivative from the already verified package digest and use BuildKit secret mounts for private build trust; no script or CA change silently redownloads the unchanged public base.
