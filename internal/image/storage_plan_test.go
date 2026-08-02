package image

import (
	"math"
	"testing"

	"github.com/solutionforest/ephemeral-action-runner/internal/storage"
)

func TestEstimateSourceSizeUsesExactOrFiveTimesCompressed(t *testing.T) {
	exact, err := EstimateSourceSize(2*storage.GiB, 7*storage.GiB)
	if err != nil {
		t.Fatal(err)
	}
	if exact.ExpandedBytes != 7*storage.GiB || exact.Confidence != EstimateExact {
		t.Fatalf("exact estimate = %+v", exact)
	}
	fallback, err := EstimateSourceSize(2*storage.GiB, 0)
	if err != nil {
		t.Fatal(err)
	}
	if fallback.ExpandedBytes != 10*storage.GiB || fallback.Confidence != EstimateFallback {
		t.Fatalf("fallback estimate = %+v", fallback)
	}
	if _, err := EstimateSourceSize(math.MaxUint64, 0); err == nil {
		t.Fatal("EstimateSourceSize accepted overflowing fallback")
	}
}

func TestAutomaticDockerSandboxesRootTracksSourceSize(t *testing.T) {
	act, err := AutomaticDockerSandboxesRootBytes(5 * storage.GiB)
	if err != nil {
		t.Fatal(err)
	}
	full, err := AutomaticDockerSandboxesRootBytes(75 * storage.GiB)
	if err != nil {
		t.Fatal(err)
	}
	if act != 30*storage.GiB {
		t.Fatalf("act root = %d, want 30GiB", act)
	}
	if full != 100*storage.GiB {
		t.Fatalf("full root = %d, want 100GiB", full)
	}
	if full <= act {
		t.Fatalf("full root %d must exceed act root %d", full, act)
	}
}

func TestDockerSandboxesPlanDoesNotAddSparseLogicalLimitsToPhysicalPeak(t *testing.T) {
	source := SourceSizeEstimate{CompressedBytes: 16 * storage.GiB, ExpandedBytes: 75 * storage.GiB, Confidence: EstimateExact}
	plan, err := PlanArtifactStorage("docker-sandboxes", source, false, 50*storage.GiB)
	if err != nil {
		t.Fatal(err)
	}
	wantPhysical := 16*storage.GiB + 75*storage.GiB + 75*storage.GiB + CustomizationAllowanceBytes
	if plan.EstimatedIncrementalPeak != wantPhysical {
		t.Fatalf("physical peak = %d, want %d", plan.EstimatedIncrementalPeak, wantPhysical)
	}
	if plan.LogicalRootMaximumBytes != 100*storage.GiB || plan.LogicalDockerMaximumBytes != 50*storage.GiB || !plan.LogicalLimitsSparse {
		t.Fatalf("logical limits = %+v", plan)
	}
}

func TestVerifiedCachedArtifactHasZeroIncrementalPeak(t *testing.T) {
	source := SourceSizeEstimate{CompressedBytes: storage.GiB, ExpandedBytes: 5 * storage.GiB, Confidence: EstimateFallback}
	for _, providerType := range []string{"docker-container", "docker-sandboxes", "wsl"} {
		plan, err := PlanArtifactStorage(providerType, source, true, 50*storage.GiB)
		if err != nil {
			t.Fatal(err)
		}
		if plan.EstimatedIncrementalPeak != 0 {
			t.Fatalf("%s cached peak = %d, want zero", providerType, plan.EstimatedIncrementalPeak)
		}
	}
}
