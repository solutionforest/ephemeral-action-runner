package prebuilt

import "testing"

func TestEvidencePolicyBuildConfigURIIncludesRef(t *testing.T) {
	t.Parallel()
	policy := EvidencePolicy{
		Repository: "solutionforest/ephemeral-action-runner",
		Workflow:   ".github/workflows/docker-sandboxes-images.yml",
		Ref:        "refs/heads/feature/prebuilt_img",
	}
	want := "https://github.com/solutionforest/ephemeral-action-runner/.github/workflows/docker-sandboxes-images.yml@refs/heads/feature/prebuilt_img"
	if got := policy.buildConfigURI(); got != want {
		t.Fatalf("buildConfigURI() = %q, want %q", got, want)
	}
}
