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
	if plan.LogicalRootMaximumBytes != 100*storage.GiB || plan.LogicalDockerMaximumBytes != 50*storage.GiB || !plan.LogicalLimitsSparse {
		t.Fatalf("logical limits = %+v", plan)
	}
	build := phaseRoleAllocations(t, plan.OperationPlan, "build-export")
	if got, want := build[storage.StorageRoleContainerdStore], 16*storage.GiB+75*storage.GiB+CustomizationAllowanceBytes; got != want {
		t.Fatalf("build Docker Engine allocation = %d, want %d", got, want)
	}
	if got, want := build[storage.StorageRoleProject], 75*storage.GiB+CustomizationAllowanceBytes+projectWorkspaceAllowanceBytes; got != want {
		t.Fatalf("build project allocation = %d, want %d", got, want)
	}
	importPhase := phaseRoleAllocations(t, plan.OperationPlan, "import")
	if got, want := importPhase[storage.StorageRoleSandboxTemplateCache], 75*storage.GiB+CustomizationAllowanceBytes; got != want {
		t.Fatalf("import Sandbox cache allocation = %d, want %d", got, want)
	}
	if importPhase[storage.StorageRoleContainerdStore] != build[storage.StorageRoleContainerdStore] || importPhase[storage.StorageRoleProject] != build[storage.StorageRoleProject] {
		t.Fatalf("import phase did not retain build/export overlap: build=%v import=%v", build, importPhase)
	}
	for _, phase := range plan.OperationPlan.Phases {
		for _, allocation := range phase.Allocations {
			if allocation.SurfaceID != "" {
				t.Fatalf("allocation %+v used a provider-specific surface instead of a role", allocation)
			}
		}
	}
	evaluation, err := storage.EvaluateOperationPlan(plan.OperationPlan,
		[]storage.Surface{
			{ID: "project", Role: storage.StorageRoleProject, Kind: storage.SurfaceHostFilesystem, DomainID: "host"},
			{ID: "image-store", Role: storage.StorageRoleContainerdStore, Kind: storage.SurfaceDockerEngine, DomainID: "host"},
			{ID: "sandbox-cache", Role: storage.StorageRoleSandboxTemplateCache, Kind: storage.SurfaceSandboxCache, DomainID: "sandbox-cache"},
		},
		[]storage.CapacityDomain{
			{ID: "host", Kind: storage.SurfaceHostFilesystem, Capacity: storage.Capacity{Known: true, AvailableBytes: 500 * storage.GiB}},
			{ID: "sandbox-cache", Kind: storage.SurfaceSandboxCache, Capacity: storage.Capacity{Known: true, AvailableBytes: 500 * storage.GiB}},
		},
	)
	if err != nil {
		t.Fatalf("evaluate role-based phase overlap: %v", err)
	}
	if len(evaluation.ResolvedOperationPlan.Requirements) != 2 {
		t.Fatalf("domain requirements = %+v, want host and Sandbox cache", evaluation.ResolvedOperationPlan.Requirements)
	}
	for _, requirement := range evaluation.ResolvedOperationPlan.Requirements {
		switch requirement.DomainID {
		case "host":
			if want := build[storage.StorageRoleContainerdStore] + build[storage.StorageRoleProject]; requirement.PeakBytes != want {
				t.Fatalf("host overlapping phase peak = %d, want %d", requirement.PeakBytes, want)
			}
		case "sandbox-cache":
			if want := importPhase[storage.StorageRoleSandboxTemplateCache]; requirement.PeakBytes != want {
				t.Fatalf("Sandbox cache phase peak = %d, want %d", requirement.PeakBytes, want)
			}
		default:
			t.Fatalf("unexpected capacity domain requirement %+v", requirement)
		}
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
		if len(plan.OperationPlan.Phases) != 1 || plan.OperationPlan.Phases[0].ID != "build-export" || len(plan.OperationPlan.Phases[0].Allocations) != 0 {
			t.Fatalf("%s cached operation plan = %+v, want named zero-growth phase", providerType, plan.OperationPlan)
		}
	}
}

func TestDockerContainerAndWSLPlansAssignTheirOverlappingRoles(t *testing.T) {
	source := SourceSizeEstimate{CompressedBytes: 2 * storage.GiB, ExpandedBytes: 10 * storage.GiB, Confidence: EstimateExact}
	container, err := PlanArtifactStorage("docker-container", source, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	containerAllocations := phaseRoleAllocations(t, container.OperationPlan, "build-export")
	if len(containerAllocations) != 1 || containerAllocations[storage.StorageRoleContainerdStore] != 17*storage.GiB {
		t.Fatalf("Docker Container allocations = %v, want Docker Engine only", containerAllocations)
	}
	wsl, err := PlanArtifactStorage("wsl", source, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	wslAllocations := phaseRoleAllocations(t, wsl.OperationPlan, "build-export")
	for role, want := range map[storage.StorageRole]uint64{
		storage.StorageRoleContainerdStore: 17 * storage.GiB,
		storage.StorageRoleProject:         20 * storage.GiB,
		storage.StorageRoleWSLDistribution: 15 * storage.GiB,
	} {
		if got := wslAllocations[role]; got != want {
			t.Fatalf("WSL %s allocation = %d, want %d", role, got, want)
		}
	}
}

func TestTartAndSourceUpdatePlansUseTheirDedicatedRoles(t *testing.T) {
	tart, err := PlanArtifactStorage("tart", SourceSizeEstimate{ExpandedBytes: 10 * storage.GiB, Confidence: EstimateDerived}, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	tartAllocations := phaseRoleAllocations(t, tart.OperationPlan, "build-clone")
	if len(tartAllocations) != 1 || tartAllocations[storage.StorageRoleTartStore] != 15*storage.GiB {
		t.Fatalf("Tart allocations = %v, want Tart store build/clone allocation", tartAllocations)
	}
	sourceUpdate := phaseRoleAllocations(t, sourceUpdateOperationPlan(), "source-update")
	if len(sourceUpdate) != 1 || sourceUpdate[storage.StorageRoleProject] != sourceUpdateExpansionBytes {
		t.Fatalf("source update allocations = %v, want project-only allocation", sourceUpdate)
	}
}

func TestDockerSandboxesImportPlanUsesVerifiedArchiveDerivedMaximum(t *testing.T) {
	source := SourceSizeEstimate{CompressedBytes: 2 * storage.GiB, ExpandedBytes: 10 * storage.GiB, Confidence: EstimateExact}
	plan, err := PlanDockerSandboxesImportStorage(source, 20*storage.GiB)
	if err != nil {
		t.Fatal(err)
	}
	allocations := phaseRoleAllocations(t, plan.OperationPlan, "import-only")
	if len(allocations) != 1 || allocations[storage.StorageRoleSandboxTemplateCache] != 25*storage.GiB {
		t.Fatalf("archive-derived import allocations = %v, want cache max of 25GiB", allocations)
	}
	plan, err = PlanDockerSandboxesImportStorage(source, storage.GiB)
	if err != nil {
		t.Fatal(err)
	}
	allocations = phaseRoleAllocations(t, plan.OperationPlan, "import-only")
	if allocations[storage.StorageRoleSandboxTemplateCache] != 15*storage.GiB {
		t.Fatalf("planned import cache allocation = %d, want 15GiB", allocations[storage.StorageRoleSandboxTemplateCache])
	}
}

func TestArtifactStoragePlansRejectOverflow(t *testing.T) {
	if _, err := PlanArtifactStorage("docker-container", SourceSizeEstimate{CompressedBytes: storage.GiB, ExpandedBytes: math.MaxUint64, Confidence: EstimateExact}, false, 0); err == nil {
		t.Fatal("Docker Container plan accepted overflowing allocation")
	}
	if _, err := PlanDockerSandboxesImportStorage(SourceSizeEstimate{ExpandedBytes: storage.GiB, Confidence: EstimateExact}, math.MaxUint64); err == nil {
		t.Fatal("Docker Sandboxes import plan accepted overflowing verified archive estimate")
	}
}

func phaseRoleAllocations(t *testing.T, plan storage.OperationPlan, phaseID string) map[storage.StorageRole]uint64 {
	t.Helper()
	for _, phase := range plan.Phases {
		if phase.ID != phaseID {
			continue
		}
		allocations := make(map[storage.StorageRole]uint64, len(phase.Allocations))
		for _, allocation := range phase.Allocations {
			allocations[allocation.Role] += allocation.Bytes
		}
		return allocations
	}
	t.Fatalf("operation plan %q has no %q phase: %+v", plan.ID, phaseID, plan.Phases)
	return nil
}
