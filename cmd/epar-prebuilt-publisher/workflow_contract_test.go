package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkflowRepairsCatalogFirstPromotionBeforeNoop(t *testing.T) {
	workflow := readPublisherWorkflow(t)
	immutableCatalogVerification := strings.Index(workflow, `verify-catalog --repository "$PACKAGE_REPOSITORY" --profile act --reference "${PACKAGE_REPOSITORY}@${catalog_digest}"`)
	reconcile := strings.Index(workflow, "- name: Reconcile an interrupted catalog-first alias promotion")
	noop := strings.Index(workflow, "- name: Determine whether the immutable tuple is already active")
	if immutableCatalogVerification < 0 || reconcile < 0 || noop < 0 || immutableCatalogVerification >= reconcile || reconcile >= noop {
		t.Fatalf("immutable catalog verification and alias reconciliation must run before no-op evaluation: verify=%d reconcile=%d noop=%d", immutableCatalogVerification, reconcile, noop)
	}
	manualCatalog := strings.Index(workflow, `CANDIDATE_CATALOG_REFERENCE: ${{ inputs.candidate_catalog_reference }}`)
	manualAcceptance := strings.Index(workflow, `epar-prebuilt-publisher accept --catalog "$catalog"`)
	manualPromotion := strings.Index(workflow, `epar-prebuilt-publisher promote --protected`)
	if manualCatalog < 0 || manualAcceptance < 0 || manualPromotion < 0 || manualCatalog >= manualAcceptance || manualAcceptance >= manualPromotion {
		t.Fatalf("manual promotion must verify an exact candidate catalog and append acceptance before promotion: catalog=%d acceptance=%d promotion=%d", manualCatalog, manualAcceptance, manualPromotion)
	}
}

func TestWorkflowVerifiesMatchingPackageBeforeNoop(t *testing.T) {
	workflow := strings.ReplaceAll(readPublisherWorkflow(t), "\r\n", "\n")
	noop := strings.Index(workflow, "      - name: Determine whether the immutable tuple is already active\n")
	build := strings.Index(workflow, "\n  build:\n")
	if noop < 0 || build <= noop {
		t.Fatalf("cannot isolate metadata-first no-op step: noop=%d build=%d", noop, build)
	}
	noopStep := workflow[noop:build]
	for _, required := range []string{
		`matching_entries="$RUNNER_TEMP/epar-catalog/matching-entries.json"`,
		`matching_entry="$RUNNER_TEMP/epar-catalog/matching-entry.json"`,
		`($effectiveStatus == "candidate" or $effectiveStatus == "active")`,
		`$entry.gates.sourceRechecked == true`,
		`$entry.gates.attestationVerified == true`,
		`go run ./cmd/epar-prebuilt-publisher verify-package`,
		`--reference "$package_reference"`,
		`> "$RUNNER_TEMP/epar-catalog/package-verification.json"`,
		`Catalog contains $match_count complete matching entries; refusing an ambiguous no-op.`,
	} {
		if !strings.Contains(noopStep, required) {
			t.Fatalf("metadata-first no-op contract is missing %q", required)
		}
	}
	if strings.Contains(noopStep, `$effectiveStatus == "superseded"`) {
		t.Fatal("superseded catalog entries must not suppress a rebuild")
	}
	verify := strings.Index(noopStep, `go run ./cmd/epar-prebuilt-publisher verify-package`)
	noOpAssignment := strings.Index(noopStep, "noop=true")
	if verify < 0 || noOpAssignment < 0 || verify >= noOpAssignment {
		t.Fatalf("immutable package verification must precede noop=true: verify=%d noop=%d", verify, noOpAssignment)
	}
}

func TestWorkflowGuardsCatalogPointerAndPublishesCandidateLedgerOnMain(t *testing.T) {
	workflow := strings.ReplaceAll(readPublisherWorkflow(t), "\r\n", "\n")
	promote := strings.Index(workflow, "      - name: Verify signed catalog and move only authorized aliases\n")
	manual := strings.Index(workflow, "\n  prepare-promotion-review:\n")
	if promote < 0 || manual <= promote {
		t.Fatalf("cannot isolate automatic catalog publication step: promote=%d manual=%d", promote, manual)
	}
	promoteStep := workflow[promote:manual]
	for _, required := range []string{
		`EXPECTED_CATALOG_MANIFEST: ${{ needs.resolve.outputs.catalog_manifest_digest }}`,
		`old_catalog_digest" != "$expected_catalog_manifest"`,
		`if [[ "$ALLOW_ALIAS" == true ]]; then`,
		`catalog-v1 was moved; the profile alias was not moved`,
		`catalog rollback skipped because the catalog pointer changed after this publication`,
	} {
		if !strings.Contains(promoteStep, required) {
			t.Fatalf("catalog publication safety contract is missing %q", required)
		}
	}
	reconcile := strings.Index(workflow, "      - name: Reconcile an interrupted catalog-first alias promotion\n")
	noop := strings.Index(workflow, "      - name: Determine whether the immutable tuple is already active\n")
	if reconcile < 0 || noop <= reconcile || !strings.Contains(workflow[reconcile:noop], `current_alias_digest" == "$observed"`) {
		t.Fatal("alias reconciliation must compare the observed alias head again before repair")
	}
}

func TestWorkflowCarriesManualCatalogHeadThroughProtectedPromotion(t *testing.T) {
	workflow := strings.ReplaceAll(readPublisherWorkflow(t), "\r\n", "\n")
	prepare := strings.Index(workflow, "  prepare-promotion-review:\n")
	manual := strings.Index(workflow, "\n  manual-promote:\n")
	if prepare < 0 || manual <= prepare {
		t.Fatalf("cannot isolate protected promotion preparation: prepare=%d manual=%d", prepare, manual)
	}
	prepareJob := workflow[prepare:manual]
	manualJob := workflow[manual:]
	for _, required := range []string{
		`catalog_manifest: ${{ steps.review.outputs.catalog_manifest }}`,
		`echo "catalog_manifest=$expected_catalog_manifest" >> "$GITHUB_OUTPUT"`,
		`Current catalog-v1 manifest at review`,
	} {
		if !strings.Contains(prepareJob, required) {
			t.Fatalf("protected review must record the catalog head: missing %q", required)
		}
	}
	for _, required := range []string{
		`EXPECTED_CATALOG_MANIFEST: ${{ needs.prepare-promotion-review.outputs.catalog_manifest }}`,
		`catalog-v1 changed after reviewer preparation`,
		`[[ "$current_catalog_digest" == "$old_catalog_digest" ]]`,
	} {
		if !strings.Contains(manualJob, required) {
			t.Fatalf("protected promotion must guard the reviewed catalog head: missing %q", required)
		}
	}
}

func TestWorkflowForceCandidatePreservesVerifiedEvidence(t *testing.T) {
	workflow := readPublisherWorkflow(t)
	for _, required := range []string{
		"target: runner-template",
		`platform_ref="${PACKAGE_REPOSITORY}@${platform_digest}"`,
		`verify-package --reference "$PACKAGE_REF"`,
		`--ref "$GITHUB_REF"`,
		`jq '.gates.attestationVerified=true'`,
	} {
		if !strings.Contains(workflow, required) {
			t.Fatalf("hosted package evidence verification changed: missing %q", required)
		}
	}
	candidateGate := `if [[ "$FORCE_CANDIDATE" == true || "$ALLOW_ALIAS" != true || "$SOURCE_RECHECKED" != true ]]; then`
	if !strings.Contains(workflow, candidateGate) {
		t.Fatal("force_candidate no longer suppresses automatic alias advancement")
	}
}

func TestWorkflowUsesHostedBuildsAndExternalEPARAcceptance(t *testing.T) {
	workflow := readPublisherWorkflow(t)
	for _, forbidden := range []string{"live-amd64:", "live-arm64:", "EPAR_PREBUILT_LIVE", "runs-on: [self-hosted, linux, epar-docker-sandboxes"} {
		if strings.Contains(workflow, forbidden) {
			t.Fatalf("publisher still depends on persistent native runner gate %q", forbidden)
		}
	}
	for _, required := range []string{
		`runs-on: ${{ matrix.runner }}`,
		`runner: ubuntu-latest`,
		`runner: ubuntu-24.04-arm`,
		`acceptance_evidence_json:`,
		`playwright-docker.yml`,
		`dockerhub-private-pull.yml`,
		`runnerGroup:"epar-dev-test"`,
		`schemaVersion:3,profile:$profile`,
		`runnerName:$amd64PlaywrightRunner`,
		`runnerName:$amd64DockerHubRunner`,
		`runnerName:$arm64PlaywrightRunner`,
		`runnerName:$arm64DockerHubRunner`,
		`catalog-v1 was moved; the profile alias was not moved`,
	} {
		if !strings.Contains(workflow, required) {
			t.Fatalf("publisher candidate acceptance contract is missing %q", required)
		}
	}
}

func TestWorkflowPublishesOnlyFromMainAndValidatesPullRequestsWithoutPublishing(t *testing.T) {
	workflow := strings.ReplaceAll(readPublisherWorkflow(t), "\r\n", "\n")
	for _, required := range []string{
		"push:\n    branches:\n      - main",
		"pull_request:\n    branches:\n      - develop\n      - main",
		"name: Validate prebuilt publication contract without publishing",
		"if: github.event_name == 'pull_request'",
		"permissions:\n      contents: read",
		"go test ./internal/prebuilt ./cmd/epar-prebuilt-publisher -count=1",
		"bash -n .github/scripts/fetch-prebuilt-catalog.sh",
		"scripts/docker-sandboxes/validate-prebuilt.ps1 -Platform linux/amd64",
		"scripts/docker-sandboxes/validate-prebuilt.ps1 -Platform linux/arm64",
		"if: github.event_name != 'pull_request' && inputs.promote_candidate != true",
	} {
		if !strings.Contains(workflow, required) {
			t.Fatalf("workflow branch/publication isolation is missing %q", required)
		}
	}
	pushStart := strings.Index(workflow, "  push:\n")
	pullRequestStart := strings.Index(workflow, "  pull_request:\n")
	if pushStart < 0 || pullRequestStart < 0 || pushStart >= pullRequestStart {
		t.Fatalf("cannot isolate push trigger: push=%d pull_request=%d", pushStart, pullRequestStart)
	}
	if strings.Contains(workflow[pushStart:pullRequestStart], "      - develop\n") {
		t.Fatal("develop pushes must not publish GHCR candidates")
	}
}

func TestWorkflowPublicationCannotMutateGitOrSourceReleaseState(t *testing.T) {
	workflow := strings.ReplaceAll(readPublisherWorkflow(t), "\r\n", "\n")
	if !strings.Contains(workflow, "permissions:\n  contents: read") {
		t.Fatal("publisher no longer has read-only repository contents permission")
	}
	for _, forbidden := range []string{"git push", "git commit", "gh pr create", "gh release create", "gh api --method POST /repos/"} {
		if strings.Contains(workflow, forbidden) {
			t.Fatalf("package-only workflow contains forbidden source/release mutation %q", forbidden)
		}
	}
}

func TestWorkflowFetchesCatalogsByValidatedDescriptorAndPublishesSafeRelativeTitles(t *testing.T) {
	workflow := readPublisherWorkflow(t)
	if strings.Contains(workflow, "oras pull") {
		t.Fatal("catalog retrieval must not extract OCI layer titles as filesystem paths")
	}
	if strings.Contains(workflow, "--allow-path-traversal") || strings.Contains(workflow, "--disable-path-validation") {
		t.Fatal("catalog publication and retrieval must not disable ORAS path validation")
	}
	if got := strings.Count(workflow, `bash .github/scripts/fetch-prebuilt-catalog.sh "$PACKAGE_REPOSITORY"`); got != 2 {
		t.Fatalf("both unsigned collision checks must use the descriptor-addressed fetcher: got %d calls", got)
	}
	if got := strings.Count(workflow, `verify-catalog --repository "$PACKAGE_REPOSITORY"`); got < 5 {
		t.Fatalf("signed catalog reads must remain in-process verified: got %d verifier calls", got)
	}
	for _, required := range []string{
		`cd "$RUNNER_TEMP/epar-promotion"`,
		`--config "catalog-config.json:$CATALOG_CONFIG_MEDIA_TYPE" "catalog.json:$CATALOG_LAYER_MEDIA_TYPE"`,
		`cd "$RUNNER_TEMP"`,
		`--config "manual-catalog-config.json:$CATALOG_CONFIG_MEDIA_TYPE" "manual-catalog.canonical.json:$CATALOG_LAYER_MEDIA_TYPE"`,
	} {
		if !strings.Contains(workflow, required) {
			t.Fatalf("catalog publication must use controlled relative OCI layer titles: missing %q", required)
		}
	}
	if got := strings.Count(workflow, `oras push "$immutable_ref"`); got != 2 {
		t.Fatalf("automatic and manual catalog publication must retain exactly two guarded pushes: got %d", got)
	}
	if got := strings.Count(workflow, `oras manifest fetch "$immutable_ref" >`); got != 2 {
		t.Fatalf("automatic and manual catalog readback must inspect two raw OCI manifests: got %d", got)
	}
	if strings.Contains(workflow, `oras manifest fetch "$immutable_ref" --format json`) {
		t.Fatal("catalog readback must not confuse ORAS formatted metadata with the raw OCI manifest")
	}
	for _, required := range []string{`(.config.mediaType == $config)`, `((.layers | length) == 1)`, `(.layers[0].mediaType == $layer)`} {
		if !strings.Contains(workflow, required) {
			t.Fatalf("catalog raw-manifest readback is missing %q", required)
		}
	}
}

func TestCatalogFetcherRejectsLayerPathsAndValidatesExactBlob(t *testing.T) {
	script := readCatalogFetchScript(t)
	for _, required := range []string{
		`oras manifest fetch "$reference" > "$manifest_file"`,
		`catalog reference must be an exact digest`,
		`select(.artifactType == $artifact)`,
		`select((.layers | length) == 1)`,
		`oras blob fetch --output "$catalog_file" "${repository}@${layer_digest}"`,
		`actual_digest="sha256:$(sha256sum "$catalog_file"`,
		`(.schemaVersion == 1) and (.artifactKind == "docker-sandboxes-template")`,
	} {
		if !strings.Contains(script, required) {
			t.Fatalf("catalog fetcher is missing %q", required)
		}
	}
	for _, forbidden := range []string{"oras pull", "--allow-path-traversal", "--disable-path-validation"} {
		if strings.Contains(script, forbidden) {
			t.Fatalf("catalog fetcher contains unsafe path behavior %q", forbidden)
		}
	}
	if strings.Contains(script, "--format json") {
		t.Fatal("catalog fetcher must validate the raw OCI manifest rather than ORAS formatted metadata")
	}
}

func TestWorkflowPreparesReviewSummaryBeforeProtectedPromotion(t *testing.T) {
	workflow := strings.ReplaceAll(readPublisherWorkflow(t), "\r\n", "\n")
	prepare := strings.Index(workflow, "  prepare-promotion-review:\n")
	manual := strings.Index(workflow, "  manual-promote:\n")
	if prepare < 0 || manual <= prepare {
		t.Fatalf("prepare review job must precede manual promotion: prepare=%d manual=%d", prepare, manual)
	}
	prepareJob := workflow[prepare:manual]
	if strings.Contains(prepareJob, "    environment:") {
		t.Fatal("review summary must be available before protected environment approval")
	}
	for _, required := range []string{
		"name: Prepare protected promotion review",
		"## EPAR prebuilt promotion review",
		"### Candidate identity",
		"### Human-reviewed acceptance evidence",
		"### Reviewer checklist",
		`[[ "$PROFILE" == act || "$PROFILE" == full ]]`,
		"playwright-docker.yml run ${amd64_playwright_run_id}",
		"dockerhub-private-pull.yml run ${amd64_dockerhub_run_id}",
		"playwright-docker.yml run ${arm64_playwright_run_id}",
		"dockerhub-private-pull.yml run ${arm64_dockerhub_run_id}",
		`go run ./cmd/epar-prebuilt-publisher verify-package`,
	} {
		if !strings.Contains(prepareJob, required) {
			t.Fatalf("promotion review summary is missing %q", required)
		}
	}
	manualJob := workflow[manual:]
	for _, required := range []string{
		"needs: prepare-promotion-review",
		"environment: epar-prebuilt-promotion",
	} {
		if !strings.Contains(manualJob, required) {
			t.Fatalf("protected promotion must depend on prepared review: missing %q", required)
		}
	}
}

func TestWorkflowBuildsAndPromotesFullWithoutPersistentNativeRunners(t *testing.T) {
	workflow := readPublisherWorkflow(t)
	for _, required := range []string{
		`- cron: '37 23 */7 * *'`,
		`- cron: '57 23 */7 * *'`,
		`'37 23 */7 * *') profile=full`,
		`'57 23 */7 * *') profile=act`,
		`act|full) ;;`,
		`if: needs.resolve.outputs.profile == 'full'`,
		`Full publication requires at least 40 GiB free`,
		`fallocate -l 8G`,
		`max-parallelism = 1`,
		`--arg profile "$PROFILE"`,
		`epar-prebuilt-${PROFILE}-${digest_prefix}-amd64`,
		`epar-prebuilt-${PROFILE}-${digest_prefix}-arm64`,
		`alias_tag="${PROFILE}-latest"`,
		`oras tag "${PACKAGE_REPOSITORY}@${CANDIDATE_DIGEST}" "$alias_tag"`,
	} {
		if !strings.Contains(workflow, required) {
			t.Fatalf("Full publication/acceptance contract is missing %q", required)
		}
	}
	for _, forbidden := range []string{"live-amd64:", "live-arm64:", "EPAR_PREBUILT_LIVE"} {
		if strings.Contains(workflow, forbidden) {
			t.Fatalf("Full publication reintroduced persistent native gate %q", forbidden)
		}
	}
}

func TestWorkflowRecordsRunnablePlatformManifestWithoutDuplicatePlatformEvidence(t *testing.T) {
	workflow := readPublisherWorkflow(t)
	for _, required := range []string{
		`platform_candidate_ref="${PACKAGE_REPOSITORY}@${CANDIDATE_DESCRIPTOR_DIGEST}"`,
		`docker buildx imagetools inspect "$platform_candidate_ref" --raw`,
		`provenance: false`,
		`sbom: false`,
		`application/vnd.oci.image.manifest.v1+json|application/vnd.docker.distribution.manifest.v2+json`,
		`package_manifest_digest="$CANDIDATE_DESCRIPTOR_DIGEST"`,
		`select(.platform.os == "linux" and .platform.architecture == $architecture)`,
		`if length == 1 then .[0].digest else error("expected exactly one runnable platform manifest") end`,
		`packageManifestDigest:$digest,candidateDescriptorDigest:$candidateDescriptorDigest`,
		`"${PACKAGE_REPOSITORY}@${amd64_digest}"`,
		`"${PACKAGE_REPOSITORY}@${arm64_digest}"`,
		`($manifests | length) == 2`,
	} {
		if !strings.Contains(workflow, required) {
			t.Fatalf("workflow no longer records an exact runnable platform manifest without duplicate platform evidence: missing %q", required)
		}
	}
	if strings.Contains(workflow, "provenance: mode=max") || strings.Contains(workflow, "sbom: true") {
		t.Fatal("platform builds must not recreate evidence already signed for the completed multi-platform index")
	}
	if got := strings.Count(workflow, "Generate signed index SLSA referrer"); got != 1 {
		t.Fatalf("completed package index must retain exactly one signed SLSA referrer step: got %d", got)
	}
	if got := strings.Count(workflow, "Generate signed index SPDX SBOM referrer"); got != 1 {
		t.Fatalf("completed package index must retain exactly one signed SPDX referrer step: got %d", got)
	}
}

func TestWorkflowUsesGitHubSupportedSLSAWorkflowBuildType(t *testing.T) {
	workflow := readPublisherWorkflow(t)
	for _, required := range []string{
		`buildType:"https://actions.github.io/buildtypes/workflow/v1"`,
		`[{uri:$gitUri,digest:{gitCommit:$revision}}]`,
		`workflow:{ref:$workflowRef,repository:$workflowRepository,path:".github/workflows/docker-sandboxes-images.yml"}`,
		`internalParameters:{github:{event_name:$eventName,repository_id:$repositoryId,repository_owner_id:$repositoryOwnerId,runner_environment:"github-hosted"}}`,
		`runDetails:{builder:{id:$builderId},metadata:{invocationId:$invocationId}}`,
	} {
		if !strings.Contains(workflow, required) {
			t.Fatalf("workflow must use GitHub's supported SLSA workflow provenance envelope: missing %q", required)
		}
	}
	if strings.Contains(workflow, `buildType:"https://solutionforest.dev/epar/docker-sandboxes/v1"`) {
		t.Fatal("workflow still uses the unsupported custom SLSA build type")
	}
}

func readPublisherWorkflow(t *testing.T) string {
	t.Helper()
	path := filepath.Join("..", "..", ".github", "workflows", "docker-sandboxes-images.yml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func readCatalogFetchScript(t *testing.T) string {
	t.Helper()
	path := filepath.Join("..", "..", ".github", "scripts", "fetch-prebuilt-catalog.sh")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
