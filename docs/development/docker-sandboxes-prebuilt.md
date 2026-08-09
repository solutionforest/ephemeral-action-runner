# Docker Sandboxes prebuilt image publication

This workflow publishes the EPAR Docker Sandboxes template as an immutable, multi-platform OCI package and records the result in a signed catalog. It is an upstream-driven publisher, not a source release workflow: it never creates a Git commit, branch, tag, GitHub Release, pull request, or source-repository mutation.

The canonical source is `ghcr.io/catthehacker/ubuntu`. The package repository is `ghcr.io/solutionforest/ephemeral-action-runner/docker-sandboxes-template`. The Act profile (`act-latest`) is the only profile eligible for automatic advancement, and only when every protected gate and branch policy passes. Full (`full-latest`) is disabled in the catalog policy and is candidate-only until an independent promotion decision enables it.

## Trigger, permission, and concurrency contract

`.github/workflows/docker-sandboxes-images.yml` runs on schedule `37 */6 * * *`, on `workflow_dispatch`, and on pushes to `main` or `develop` that change the workflow, `templates/docker-sandboxes/**`, `scripts/docker-sandboxes/**`, `internal/prebuilt/**`, or `cmd/epar-prebuilt-publisher/**`. Dispatch accepts `profile: act|full`, an explicit `force_candidate` boolean, and the protected manual-promotion fields described below.

The workflow grants exactly `contents: read`, `packages: write`, `attestations: write`, and `id-token: write`. Every third-party action is pinned to a full commit SHA. The concurrency key `docker-sandboxes-prebuilt-publisher` is serialized and does not cancel an in-flight publication, so catalog compare-and-swap decisions cannot overlap.

Only a run whose ref is `refs/heads/main` may publish a signed catalog or request automatic alias movement. A `develop` push or any other recipe branch publishes immutable candidate images/evidence only; a trusted main run must perform catalog promotion. An explicit `force_candidate: true` suppresses automatic alias movement without discarding already verified evidence gates, so the exact candidate remains eligible for later protected promotion. A source movement observed after the build, an unavailable live-gate pool, or the disabled Full policy also forces a candidate. A skipped or failed live-gate job therefore leaves the candidate and existing aliases unchanged.

One-time repository setup is required before enabling the schedule: make the package repository visible to the intended consumers (public visibility is required for anonymous runtime pulls), allow GitHub Actions to read/write that package, and keep workflow permissions for package write, attestations write, and OIDC identity token issuance enabled. Configure the `EPAR_PREBUILT_LIVE` and live-runner variables only after dedicated amd64 and arm64 Sandbox hosts have been enrolled with the labels documented below. The workflow itself does not change repository visibility, package settings, runner registration, or organization policy.

## Immutable publication lifecycle

1. Resolve the selected upstream tag (`act-latest` or an explicitly dispatched `full-latest`) with the OCI descriptor API and `oras resolve`. The source index must contain exactly one Linux amd64 and one Linux arm64 descriptor; their digests are retained in the evidence tuple.
2. Read `templates/docker-sandboxes/sources.lock.json`, the recipe files, runner release asset digests, and locked helper inputs. The Dockerfile is built from immutable source and tool digests; a mutable source tag is never used as a `FROM` input.
3. Build amd64 and arm64 candidates independently with Buildx. Buildx emits per-platform SBOM/provenance, and the workflow merges exactly those two manifests into one immutable index. A duplicate, missing, non-Linux, or extra platform fails the job.
4. Derive the package tag `<profile>-latest-pkg-<full 64 hex index digest>`. The package reference recorded in the catalog is `ghcr.io/solutionforest/ephemeral-action-runner/docker-sandboxes-template@sha256:<64 hex>`. An existing canonical tag is idempotent only when it resolves to that same index digest; a different digest is a collision and fails closed.
5. Sign the index with two GitHub Actions OCI referrers: an EPAR SLSA v1 predicate and an SPDX SBOM. On a rerun, evidence is reused only when the signed catalog already records this exact package index, its exact provenance/SBOM descriptor digests are present in `oras discover`, and `verify-package` validates those claims; otherwise the workflow records the pre-attestation descriptor set and requires exactly one new descriptor of each required class. A computed hash of a bundle or package digest is not accepted as evidence.
6. Upload the candidate input and evidence receipts. Controlled self-hosted Linux runners labelled `self-hosted`, `linux`, `epar-docker-sandboxes`, and `amd64`/`arm64` tag each pulled platform image with an exact run/profile/digest tag, record its config digest and 12-character Sandbox cache ID, load and identify the exact template, then run `TestLiveRunnerTemplateIsolation` inside the real Sandbox (where the private dockerd and `verify-template.sh` contract exists). These jobs are enabled only when repository variable `EPAR_PREBUILT_LIVE` is the string `true` and repository variables `EPAR_LIVE_DOCKER_SANDBOXES_STAGING_ROOT`, `EPAR_LIVE_DOCKER_SANDBOXES_REGISTRY_IMAGE`, and `EPAR_LIVE_DOCKER_SANDBOXES_HTPASSWD_IMAGE` are set; absent labels or variables intentionally retain a candidate.
7. The promotion job merges both live receipts, verifies the package's signed SLSA/SPDX claims with `go run ./cmd/epar-prebuilt-publisher verify-package`, and then calls the deterministic publisher plan/promote commands. Incomplete live or attestation gates can append a candidate but cannot create an active alias.
8. Canonicalize and sign the catalog before exposing its moving tag. Publish `catalog-v1-pkg-<full 64 hex canonical catalog digest>` with manifest artifact type `application/vnd.epar.prebuilt.catalog.v1`, config media type `application/vnd.epar.prebuilt.catalog.config.v1+json`, and exactly one JSON layer media type `application/vnd.epar.prebuilt.catalog.v1+json`; verify it with `verify-catalog`, and only then move `catalog-v1` to the verified immutable manifest. The package profile alias is moved last, and only after optimistic-concurrency checks against the expected prior alias digest.

## Publisher CLI contract

The workflow treats the Go command as a strict API. All commands fail on malformed JSON, missing digests, failed signature verification, or a registry race.

```text
go run ./cmd/epar-prebuilt-publisher plan --catalog catalog-state.json --input publication-input.json --output plan.json
go run ./cmd/epar-prebuilt-publisher promote --catalog catalog-state.json --plan plan.json
go run ./cmd/epar-prebuilt-publisher promote --protected --catalog catalog-state.json --plan candidate-promotion-plan.json
go run ./cmd/epar-prebuilt-publisher catalog --catalog catalog-state.json --output catalog.canonical.json
go run ./cmd/epar-prebuilt-publisher verify-catalog --repository ghcr.io/solutionforest/ephemeral-action-runner/docker-sandboxes-template --profile act --reference ghcr.io/solutionforest/ephemeral-action-runner/docker-sandboxes-template:catalog-v1-pkg-<64 hex> --ref refs/heads/main --allowed-events schedule,workflow_dispatch,push
go run ./cmd/epar-prebuilt-publisher verify-package --reference ghcr.io/solutionforest/ephemeral-action-runner/docker-sandboxes-template@sha256:<64 hex> --entry publication-entry.json --repository ghcr.io/solutionforest/ephemeral-action-runner/docker-sandboxes-template --ref refs/heads/main --allowed-events schedule,workflow_dispatch,push
```

`plan` writes a complete `PublicationPlan` containing `action`, `reason`, `expectedSourceDigest`, `expectedAliasDigest` (when an alias exists), `sourceReference`, and an immutable `entry`. Actions are `noop`, `candidate`, or `advance-alias`. `promote` atomically appends the candidate or performs the protected alias compare-and-swap; an upstream recheck race fails the alias path and leaves the old alias intact. `catalog` writes canonical JSON and prints `catalogDigest=sha256:<64 hex>`. `verify-catalog` verifies the moving catalog's signed OCI referrer, canonical catalog digest, and effective alias package evidence. `verify-package` verifies signed package referrers and their EPAR SLSA/SPDX claims against the supplied entry before an active alias is allowed.

The `PublicationInput` fields are `profile`, `channel`, `sourceReference`, `sourceTag`, `packageRepository`, `packageReference`, `packageIndexDigest`, `packagePlatforms`, `recipe`, `runner`, `tools`, `evidence`, `gates`, `candidateId`, and `publishedAt`. `sourceReference` is the mutable source tag while the source is unchanged; if the tag moves after the immutable build, the workflow replaces it with `ghcr.io/catthehacker/ubuntu@sha256:<original index digest>` and forces `candidate`.

## Protected manual promotion

Recipe, runner, tool, runtime, and template changes remain candidates until an operator deliberately promotes one exact digest. Dispatch the workflow on `main` with `promote_candidate: true`, `profile: act`, `candidate_digest: sha256:<64 lowercase hex>`, and `promotion_confirmation: PROMOTE`; the `epar-prebuilt-promotion` environment must require an authorized reviewer. The manual job does not rebuild or select a mutable alias: it verifies the signed current catalog, requires the exact digest to be a candidate with every gate true, verifies package referrers and claims directly, rechecks the recorded immutable source, performs publisher protected CAS, signs/verifies the new catalog, and only then moves `catalog-v1` and `act-latest`. Full is rejected while its policy is disabled. A wrong digest, missing confirmation, untrusted branch, source movement, alias race, or failed gate aborts without registry alias mutation.

## Tuple and promotion rules

The tuple is `(source index and platform digests, recipe digest and revision, runtime contract, template schema, runner version and asset digests, locked tool digests)`. Any retained candidate, active, or superseded catalog entry with the complete tuple is a catalog-wide no-op, using the last status transition as the effective status so revoked and critical-revoked entries never suppress a rebuild; `force_candidate` is the explicit exception. A source-only tuple movement may advance `act-latest` automatically only when the prior and new entries have identical recipe/runtime/runner/tool identities and all source, build, platform, live, SBOM, provenance, attestation, and catalog-verification gates pass. Recipe, runner, tool, runtime-contract, or template-schema changes are candidates for manual promotion even when all tests pass. Full remains disabled and never advances automatically.

## Gate evidence

The catalog records `sourceResolved`, `sourceRechecked`, `buildSucceeded`, `platformsValidated`, `importReadback`, `runtimeValidated`, `provenanceGenerated`, `sbomGenerated`, and `attestationVerified`; live receipts additionally require exact imported-template cleanup before they can set the runtime/import gates. The active-entry validator requires every gate, exactly amd64 and arm64 platforms, matching source platform digests, and non-empty immutable provenance/SBOM/attestation descriptor digests. Missing `sbx`, a failed native runtime/isolation check, a missing referrer, a failed Sigstore identity/claim check, incomplete exact cleanup, or an unavailable controlled runner is recorded as false and keeps the entry candidate-only.

The SLSA predicate uses `https://slsa.dev/provenance/v1` and carries EPAR's `buildDefinition.externalParameters.source`, `recipe`, `runner`, `tools`, and `platforms`, plus `buildDefinition.resolvedDependencies` with standard SHA-256 digest maps. The SPDX document includes exact SHA-256 packages named `epar-package-index`, `epar-runtime-config`, `epar-platform-linux-amd64`, and `epar-platform-linux-arm64`, plus a digest-scoped namespace. Runtime consumers compare these claims with the catalog entry; a valid signature over unrelated or empty predicates is insufficient.

## Consumption and pinning

Consumers should resolve `catalog-v1`, verify its Sigstore referrer and canonical digest, reject revoked or critical-revoked entries, then use the catalog's package `repository@sha256:<index digest>` reference. Do not treat `act-latest` or `full-latest` as a reproducibility pin. Verify the package index, both platform descriptors, source labels, SLSA/SPDX referrers, and the catalog status before importing with `sbx template load`.

The prebuilt image is the reusable Docker Sandboxes base. CA/proxy policy, host-trust overlays, custom-install scripts, and other site-specific customization remain outside the base-image download boundary and are applied by the normal EPAR artifact/update path.

The workflow may use pinned Docker Hub helper images such as `docker.io/tonistiigi/binfmt@sha256:<digest>` and locked BuildKit/Golang inputs for emulation and building. This is not a source or package fallback: the canonical source and EPAR package are GHCR-only, and a GHCR resolution or verification error fails closed.

## Retention, revocation, and rollback

Immutable package tags and immutable catalog tags are retained as audit records; the workflow never performs broad deletion. Staging candidate tags and workflow artifacts are retained for 14 days. Manual cleanup must name the exact staging tag after confirming that no catalog entry, receipt, or incident investigation references it; never prune by wildcard or delete an immutable digest that appears in the signed catalog.

For revocation, append a catalog transition to `revoked` or `critical-revoked` with a reason, publish and sign the new canonical catalog, verify it, and move `catalog-v1`. Do not delete the immutable package or catalog object; consumers fail closed on the signed revocation status. Keep the old moving alias unchanged until the revocation catalog is verified.

For rollback, select a prior non-revoked immutable package digest from the signed catalog, prepare a protected catalog plan with the expected current alias digest, and run the same `promote`/canonicalize/sign/verify sequence. GHCR cannot atomically move `catalog-v1` and the profile alias together, so the verified signed catalog is the durable commit authority. Ordinary command/readback failures arm an in-process trap that restores and verifies the prior pointers; runner loss or termination between tag moves is recovered idempotently at the start of the next automatic or manual promotion run by comparing the public alias with the signed catalog before no-op evaluation. This also completes an interrupted first publication whose signed catalog exists but whose profile alias is absent. Move the package alias only after catalog verification and re-run the live runtime/import checks. A rollback is an OCI/catalog operation; it does not revert source Git history or create a Git tag/release.

## Operator checks

```text
oras resolve ghcr.io/catthehacker/ubuntu:act-latest
oras manifest fetch ghcr.io/catthehacker/ubuntu@sha256:<source index digest> --format json
oras resolve ghcr.io/solutionforest/ephemeral-action-runner/docker-sandboxes-template:act-latest-pkg-<64 hex>
oras discover ghcr.io/solutionforest/ephemeral-action-runner/docker-sandboxes-template@sha256:<package index digest> --format json
docker pull ghcr.io/solutionforest/ephemeral-action-runner/docker-sandboxes-template@sha256:<package index digest>
EPAR_LIVE_DOCKER_SANDBOXES=1 EPAR_LIVE_DOCKER_SANDBOXES_TEMPLATE=epar-prebuilt-live:act-amd64-<index hex> EPAR_LIVE_DOCKER_SANDBOXES_TEMPLATE_DIGEST=sha256:<platform config digest> EPAR_LIVE_DOCKER_SANDBOXES_STAGING_ROOT=/absolute/staging EPAR_LIVE_DOCKER_SANDBOXES_REGISTRY_IMAGE=<immutable registry image> EPAR_LIVE_DOCKER_SANDBOXES_HTPASSWD_IMAGE=<immutable htpasswd image> go test ./internal/provider/dockersandboxes -run '^TestLiveRunnerTemplateIsolation$' -count=1 -timeout=30m
sbx template load ./epar-sandbox-template.tar
sbx template ls --json
```

These commands are read-only except for the explicit local `sbx template load`. They do not dispatch the workflow or publish registry content. Registry publication remains an authorized CI action protected by the workflow permissions and serialized concurrency policy.
