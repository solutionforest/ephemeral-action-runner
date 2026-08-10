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
		`schemaVersion:2`,
		`runnerName:$amd64PlaywrightRunner`,
		`runnerName:$amd64DockerHubRunner`,
		`runnerName:$arm64PlaywrightRunner`,
		`runnerName:$arm64DockerHubRunner`,
		`catalog-v1 and the profile alias were not moved`,
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

func TestWorkflowAllowsAbsoluteCatalogPathsForORASPush(t *testing.T) {
	workflow := readPublisherWorkflow(t)
	const push = `oras push --disable-path-validation "$immutable_ref"`
	if got := strings.Count(workflow, push); got != 2 {
		t.Fatalf("automatic and manual catalog publication must allow their absolute runner-temp paths: got %d guarded pushes", got)
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
