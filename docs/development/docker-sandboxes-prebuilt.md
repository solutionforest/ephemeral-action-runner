# Docker Sandboxes prebuilt image publication and acceptance

EPAR publishes its Docker Sandboxes template as an immutable multi-platform OCI package with signed SBOM, SLSA provenance, and catalog evidence. This is an upstream-driven package workflow, not a source release: it creates no Git commit, pull request, repository tag, GitHub Release, or source-tree change.

The canonical source is `ghcr.io/catthehacker/ubuntu`. The public package is `ghcr.io/solutionforest/ephemeral-action-runner/docker-sandboxes-template`. Docker Hub is never used as a source fallback because its OCI identities may differ from GHCR even when the logical image content matches.

Act (`act-latest`) and Full (`full-latest`) use the same publication, verification, and runtime contracts. A package declaring `docker-sandboxes-v1` and template schema 2 is compatible regardless of its exact recipe, source lock, runner, or tool identities; those identities remain signed provenance and must agree exactly across labels, catalog, provenance, SBOM, attestations, and receipts. The signed catalog records every package and runtime resolution follows only an active profile alias.

## Workflow triggers and hosted build gates

`.github/workflows/docker-sandboxes-images.yml` checks the Catthehacker day-of-month cadence at 23:37 UTC for Full and 23:57 UTC for Act, approximately 6–12 hours after the upstream 12:00 UTC schedule, supports manual dispatch, and publishes for recipe-related pushes only on `main`. Before source resolution or any GHCR write, it inspects the newest tag-moving upstream run on `master`. Full requires a fresh successful scheduled `copy-full-image.yml` run and the exact successful four-job GHCR copy matrix that contains `full-latest`. Act requires a fresh scheduled `build-ubuntu.yml` run with one successful `Build base 24.04` job, a successful `Build and push ubuntu:act-24.04` step, and the expected Act test path; the signed evidence records whether that upstream test path succeeded or was skipped, because the current upstream workflow condition skips it. Unrelated flavor failures do not block Act, and EPAR's unchanged two-platform package smoke checks remain the automatic image-health gate. A newer manual run, pending/failed/stale/malformed state, missing job or step, API error, or evidence older than ten days exits successfully as publication skipped and records the reason only in the GitHub Actions summary. The `*/7` day-of-month expression mirrors the upstream calendar pattern rather than one fixed weekday. GitHub executes both cron expressions only from the default branch and may delay scheduled starts under load. Pull requests to `develop` or `main` that change a publisher, recipe, or committed-asset path run publisher, signed-evidence, and asset validation without logging in to GHCR, building a package, or pushing any manifest.

An explicit `workflow_dispatch` may set `rebuild_source_digest` to the exact package index digest currently active for the selected profile. This mode is for rebuilding an EPAR recipe fix without adopting a newer upstream tag. Before reading the source manifest, the workflow verifies the signed moving catalog and active package evidence against the `main` workflow identity, requires the supplied package to be the selected profile's current active non-revoked alias target, and extracts its exact source index, amd64 digest, arm64 digest, and original compatible-promotion evidence. It then resolves only the immutable `ghcr.io/catthehacker/ubuntu@sha256:...` source and requires all three descriptors to match the signed catalog; it never treats the current upstream profile tag as authority. Missing, malformed, revoked, unsigned, cross-profile, source-mismatched, or historically unverified selections fail closed before hosted builds.

Pinned rebuilds preserve the original upstream evidence exactly, including its real completion time, and record a new honest promotion time through the existing schema-2 compatible-promotion record. The ordinary ten-day freshness policy is still evaluated at planning and again at promotion. If the inherited evidence is still fresh and every hosted, source-recheck, policy, main-branch, and compare-and-swap gate passes, the rebuilt package may advance the stable alias without changing the catalog trust format, so existing schema-2 consumers remain compatible. If the inherited evidence is older than ten days, the workflow may publish the immutable package and signed candidate catalog but cannot move the profile alias automatically; a later run with genuinely fresh upstream evidence or the existing legitimate protected-acceptance path is required. The workflow never changes `completedAt`, backdates the new promotion, invents acceptance records, or silently follows a current upstream tag.

Before allocating hosted build runners, the resolve job reads the signed moving catalog and compares the complete source, recipe, runtime, schema, runner, and locked-tool tuple. Only a verified active entry at the selected profile alias may suppress a build. A matching candidate is deliberately re-evaluated so a later run with fresh upstream evidence can promote it. Package verification checks registry metadata, signed referrers, and claims without pulling image layers; an ambiguous match fails closed and a package or evidence verification failure never silently falls back to rebuilding.

The amd64 build runs on GitHub-hosted `ubuntu-latest`; the arm64 build runs on GitHub-hosted `ubuntu-24.04-arm`. The workflow has no persistent self-hosted runner dependency and no `EPAR_PREBUILT_LIVE` switch. Full jobs first reclaim disposable hosted-runner tool caches, require at least 40 GiB free before allocating 8 GiB of swap, serialize BuildKit execution, and allow a three-hour timeout; failing that capacity gate leaves Full unpublished rather than silently changing its recipe or dropping a platform. Hosted jobs resolve the GHCR source descriptor, build from its immutable digest, inspect both runnable platform manifests by digest, assemble an index from those exact digests, require exactly two descriptors, run package smoke checks, generate and sign one index-level SLSA provenance statement and one index-level SPDX SBOM, verify their referrers, and publish an immutable signed candidate catalog. Platform builds disable BuildKit's additional per-platform SBOM/provenance indexes because EPAR's trust decision uses the separately signed index-level evidence and catalog. The index-level SPDX document records the package, recipe, and exact platform identities rather than reproducing BuildKit's more detailed component inventory; this deliberate narrower audit scope avoids four additional per-publication GHCR version rows without weakening the evidence enforced by EPAR.

The workflow grants `contents: read`, `packages: write`, `attestations: write`, and `id-token: write` to publication jobs, while pull-request validation overrides permissions to `contents: read`. Third-party actions are pinned to full commit SHAs. Publication is serialized by one non-cancelling concurrency group; validation uses one group per pull request and cancels only a superseded validation run for that same pull request.

## Candidate publication contract

Every new package is first assembled with an immutable candidate identity. After the hosted and upstream gates complete, a compatible v1/schema-2 package may be recorded and activated automatically; `force_candidate` retains it as a candidate and never moves an alias. Publication may create:

- an immutable package index and canonical tag such as `<profile>-latest-pkg-<64 hex index digest>`;
- exact amd64 and arm64 platform manifests;
- signed SLSA provenance and SPDX SBOM referrers;
- an immutable catalog object and canonical tag such as `catalog-v1-pkg-<64 hex catalog digest>`.

On trusted `main`, publication writes and verifies the immutable catalog, then moves `catalog-v1`, and moves the matching profile alias last only for an authorized compatible-v1 plan. Catalog and alias compare-and-swap, source recheck, readback, rollback, and interrupted-promotion reconciliation remain mandatory. A runtime-major mismatch, `force_candidate`, non-main run, source race, or incomplete gate may publish only immutable candidate state and cannot move the profile alias.

Catalog readers resolve an exact catalog manifest, validate its artifact/config/single-layer media contract, and fetch that layer by descriptor digest into a caller-chosen file. They never extract the publisher-supplied OCI layer title as a filesystem path. New catalogs are published from controlled relative filenames, while this descriptor path remains compatible with the initial catalogs that recorded absolute runner-temporary titles.

### GHCR version graph

GitHub Packages displays every OCI manifest as a package version, so one logical candidate appears as several rows. The expected retained graph contains the final multi-platform package index, its amd64 and arm64 runnable manifests, signed SLSA and SPDX referrer manifests and their discovery index, the immutable catalog manifest, and the catalog signature and discovery index. Tags named `act-latest-pkg-<digest>`, `full-latest-pkg-<digest>`, and `catalog-v1-pkg-<digest>` are write-once audit identities. Run-specific `candidate-<profile>-<run>-<attempt>` tags are staging and diagnostic aliases, while `sha256-<subject digest>` tags are attestation-discovery indexes used by the verifier; neither is an end-user image channel. An untagged child manifest can still be required by a retained index and must not be deleted merely because the GitHub Packages page calls it untagged.

Production builds always use a resolved immutable upstream digest. Normal mode re-resolves the upstream tag before publication; if it moves during the build, the package remains an immutable candidate and no moving alias advances. Pinned rebuild mode instead re-resolves the exact immutable index reference selected from the verified signed active catalog and requires it to remain the same digest.

The runtime compatibility contract is `(runtime contract, template schema)`. Under `docker-sandboxes-v1` and schema 2, source, recipe, helper, runner, source-lock, and locked-tool changes are compatible by default and may auto-promote after fresh upstream evidence and all hosted package/evidence gates pass. Their exact values remain immutable signed provenance and unexplained package nondeterminism for an unchanged complete build tuple still fails closed. A breaking controller/template contract must declare a new runtime major such as `docker-sandboxes-v2`; it remains candidate-only until real Docker Sandboxes acceptance and protected manual promotion.

The recipe digest includes only inputs that can affect the published artifact: `Dockerfile.prebuilt`, relevant `.dockerignore` behavior, guest and prebuilt runtime files, production Go sources compiled into the image, `helpers.sha256`, and the prebuilt compatibility profile. Tests, host-only validators, local-build profiles, publisher/catalog implementation, and policy files are excluded.

`templates/docker-sandboxes/prebuilt.lock.json` commits only stable recipe and profile identities. Mutable profile enablement, wizard-default, automatic-advancement, acceptance, and revocation state belongs exclusively to the signed catalog, so protected Full promotion does not make a committed lock file stale.

## Explicit candidate-acceptance configuration

Candidate mode is an operator-only acceptance path. The wizard never generates it. It requires an exact package digest, exact immutable candidate catalog, and exact GitHub workflow evidence ref; it never follows a moving package alias, never falls back to local building, and persists `candidate` status in the receipt.

Create separate configs derived from `.local/config.sbx.yml`. Replace every placeholder below with values from the candidate publication summary. The digest label uses the first 12 hexadecimal characters after `sha256:`.

```yaml
image:
  distribution: prebuilt
  sourceType: docker-image
  sourceImage: ghcr.io/catthehacker/ubuntu:<profile>-latest
  sourcePlatform: linux/amd64
  prebuiltReference: ghcr.io/solutionforest/ephemeral-action-runner/docker-sandboxes-template:<profile>-latest
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
  namePrefix: epar-prebuilt-<profile>-<digest12>-amd64

runner:
  group: epar-dev-test
  labels: [epar-prebuilt-<profile>-<digest12>-amd64]
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

Replace `<profile>` consistently with `act` or `full`. For the Mac config, change `sourcePlatform` and `provider.platform` to `linux/arm64`, use a distinct `pool.namePrefix`, and change the unique label suffix to `-arm64`. The organization runner group `epar-dev-test` must allow only `solutionforest/ephemeral-action-runner-test` and must not allow public repositories. The host machines run EPAR normally; they are not registered directly as persistent GitHub Actions runners.

Run `./start --config <candidate-config>` to provision and start the temporary acceptance pool; it automatically performs the required image acquisition before creating a Sandbox. An explicit `./start image build --config <candidate-config>` is optional when an operator wants to provision and inspect the template without starting the pool. Successful acquisition verifies the immutable catalog and package evidence, materializes/imports the selected platform, performs exact `sbx template ls` readback, and writes `.local/state/image/<config-id>/docker-sandboxes/active.json`. A missing, mismatched, unsigned, revoked, or wrong-ref candidate fails closed without a local Catthehacker build or provider fallback.

## Four-run acceptance suite

Start one temporary EPAR controller from each candidate config. Wait until its one ephemeral runner is online in the `epar-dev-test` group under the exact digest-bound label. Dispatch exactly these four runs in `solutionforest/ephemeral-action-runner-test`, overriding only `runner_label` and leaving every other input at its default:

| Workflow | amd64 | arm64 |
| --- | --- | --- |
| `playwright-docker.yml` | one run on `epar-prebuilt-<profile>-<digest12>-amd64` | one run on `epar-prebuilt-<profile>-<digest12>-arm64` |
| `dockerhub-private-pull.yml` | one run on `epar-prebuilt-<profile>-<digest12>-amd64` | one run on `epar-prebuilt-<profile>-<digest12>-arm64` |

For every run, the human reviewer verifies the private repository, exact workflow file, successful conclusion, unique runner label, expected generated runner name, and the candidate/platform identity in the EPAR receipt. Each ephemeral job must be replaced normally while the controller is running. If the default private Docker Hub fixture is not arm64-compatible, acceptance is blocked; do not waive or silently skip that run.

After both workflows pass on both platforms, stop both EPAR controllers. Verify that their GitHub runner records, exact Sandboxes, candidate staging directories, and unreferenced obsolete template generations are cleaned up. Hash each reviewed `active.json` receipt with SHA-256 and retain the two hashes with the four GitHub run IDs/URLs.

No cross-repository PAT or GitHub App secret is required for protected private-repository acceptance review. The automatic upstream gate first uses the workflow's built-in token after feature-branch API validation; if that token cannot reliably read public upstream Actions metadata and anonymous access is also unreliable, the repository must provide `UPSTREAM_ACTIONS_READ_TOKEN` with read-only public Actions access. Missing or invalid access fails closed as publication skipped.

## Protected promotion and break-glass recovery

Protected promotion is retained for runtime-major transitions and break-glass recovery. It must run from `main`, and the package evidence must have been produced from `refs/heads/main`. Feature-branch acceptance may be reused only when the main publication has the identical package index digest and complete tuple.

Dispatch `docker-sandboxes-images.yml` on `main` with:

- `promote_candidate: true`;
- `profile: act` or `profile: full`, matching the candidate;
- the exact `candidate_digest` and `candidate_catalog_reference`;
- `acceptance_evidence_json` containing the four reviewed workflow run IDs, two EPAR receipt SHA-256 values, and the exact ephemeral runner name used by every workflow run;
- `promotion_confirmation: PROMOTE`.

The evidence input is one JSON object so the workflow remains below GitHub's ten-input limit:

```json
{"amd64PlaywrightRunId":123,"amd64PlaywrightRunnerName":"<exact generated name>","amd64DockerHubRunId":124,"amd64DockerHubRunnerName":"<different exact generated name>","amd64ReceiptSha256":"sha256:<64 hex>","arm64PlaywrightRunId":125,"arm64PlaywrightRunnerName":"<exact generated name>","arm64DockerHubRunId":126,"arm64DockerHubRunnerName":"<different exact generated name>","arm64ReceiptSha256":"sha256:<64 hex>"}
```

Before GitHub requests approval for the protected environment, an unprotected `Prepare protected promotion review` job verifies the signed candidate catalog and package evidence and requires that the candidate catalog is the current `catalog-v1` head; this prevents an older ledger snapshot from overwriting newer candidates. It writes a job summary containing the exact package, catalog, source, recipe, runtime, runner, platform, four acceptance-run links, runner names, receipt hashes, and reviewed catalog head. The reviewer opens that completed job summary, follows the four authenticated private-repository links, completes its checklist, and only then approves the waiting `epar-prebuilt-promotion` deployment. The prepare job cannot approve a deployment or move a package tag.

The `epar-prebuilt-promotion` environment must require an authorized reviewer. After approval, the protected job independently repeats the immutable catalog and package checks, rechecks the upstream source, appends two profile-bound platform acceptance records, requires exactly the two approved workflows per platform, and performs protected catalog compare-and-swap. It then signs and verifies the promoted catalog, moves `catalog-v1`, and moves the matching `act-latest` or `full-latest` alias last. Incomplete, failed, misrouted, single-platform, wrong-profile, wrong-workflow, alias-raced, or source-raced evidence cannot promote.

Catalog pointer and package-alias updates remain journaled by workflow rollback/reconciliation logic. A failure before stable pointer movement leaves the candidate immutable. A failure between pointer movements restores or idempotently reconciles the previous verified state.

## Publisher CLI

```text
go run ./cmd/epar-prebuilt-publisher plan --catalog catalog-state.json --input publication-input.json --output plan.json
go run ./cmd/epar-prebuilt-publisher upstream-gate --profile act --output upstream-gate.json
go run ./cmd/epar-prebuilt-publisher rebuild-source --catalog verified-catalog.json --profile full --package-digest sha256:<64 hex> --output rebuild-source.json
go run ./cmd/epar-prebuilt-publisher verify-rebuild-source --selection rebuild-source.json --reference ghcr.io/catthehacker/ubuntu@sha256:<64 hex> --index-digest sha256:<64 hex> --amd64-digest sha256:<64 hex> --arm64-digest sha256:<64 hex>
go run ./cmd/epar-prebuilt-publisher accept --catalog catalog-state.json --input acceptance.json
go run ./cmd/epar-prebuilt-publisher promote --protected --catalog catalog-state.json --plan candidate-plan.json
go run ./cmd/epar-prebuilt-publisher catalog --catalog catalog-state.json --output catalog.canonical.json
go run ./cmd/epar-prebuilt-publisher verify-catalog --repository ghcr.io/solutionforest/ephemeral-action-runner/docker-sandboxes-template --reference ghcr.io/solutionforest/ephemeral-action-runner/docker-sandboxes-template:catalog-v1-pkg-<64 hex> --ref refs/heads/main --allowed-events schedule,workflow_dispatch,push
go run ./cmd/epar-prebuilt-publisher verify-package --reference ghcr.io/solutionforest/ephemeral-action-runner/docker-sandboxes-template@sha256:<64 hex> --entry publication-entry.json --repository ghcr.io/solutionforest/ephemeral-action-runner/docker-sandboxes-template --ref refs/heads/main --allowed-events schedule,workflow_dispatch,push
```

`upstream-gate` reads public upstream workflow/run/job/step metadata and emits an eligibility result plus exact evidence; an ineligible result is not an error, while unavailable or malformed API data fails closed. `rebuild-source` accepts only a catalog file already produced by signed `verify-catalog`, then binds an exact current active profile package to its recorded immutable source descriptors and historical compatible-promotion evidence. `accept` appends immutable human-reviewed platform evidence. It accepts only `playwright-docker.yml` and `dockerhub-private-pull.yml` in `solutionforest/ephemeral-action-runner-test`, requires successful run evidence and exact receipt/runner identity, and does not itself move an alias. `promote --protected` requires complete hosted gates plus both reviewed platform records.

## Retention and revocation

Immutable accepted, active, superseded rollback, and revoked package and catalog objects are retained for audit and recovery. The workflow performs no broad deletion. Unaccepted CI candidates may be removed only by future reachability-aware maintenance after a documented retention window; that maintenance must preserve every object referenced by a retained package index, attestation-discovery index, signed catalog, active receipt, or rollback identity. Manual deletion of individual untagged GitHub Packages rows is unsafe. Revocation appends `revoked` or `critical-revoked` status to a newly signed catalog; it does not delete the immutable package. Candidate acquisition rejects a revoked status in its exact signed catalog. Normal stable consumers enforce the current signed moving-catalog status, with `critical-revoked` blocking new Sandbox admissions.

The public base contains no workstation CA, enterprise credential, proxy endpoint, `NO_PROXY`, forward-bypass configuration, pool data, or custom install script. Host trust is a runtime overlay. Custom scripts create a local derivative from the already verified package digest and use BuildKit secret mounts for private build trust; no script or CA change silently redownloads the unchanged public base.
