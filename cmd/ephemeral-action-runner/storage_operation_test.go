package main

import (
	"context"
	"testing"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
	imageartifact "github.com/solutionforest/ephemeral-action-runner/internal/image"
	"github.com/solutionforest/ephemeral-action-runner/internal/storage"
)

func TestStorageStatusTemplateBuildUsesPhaseAwareProviderRoles(t *testing.T) {
	oldResolver := initResolveDockerSandboxesSource
	initResolveDockerSandboxesSource = func(context.Context, string, string) (imageartifact.ResolvedDockerSource, error) {
		return imageartifact.ResolvedDockerSource{Reference: "source", Platform: "linux/amd64", CompressedLayerBytes: 2 * storage.GiB}, nil
	}
	t.Cleanup(func() { initResolveDockerSandboxesSource = oldResolver })
	cfg := config.Default()
	cfg.Provider.Type = "docker-sandboxes"
	plan, err := storageStatusOperationPlan(context.Background(), cfg, t.TempDir(), "template-build", storage.GiB)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Phases) != 2 || plan.Phases[0].ID != "build-export" || plan.Phases[1].ID != "import" {
		t.Fatalf("template phases = %#v", plan.Phases)
	}
	roles := make(map[storage.StorageRole]bool)
	for _, phase := range plan.Phases {
		for _, allocation := range phase.Allocations {
			roles[allocation.Role] = true
		}
	}
	for _, role := range []storage.StorageRole{storage.StorageRoleContainerdStore, storage.StorageRoleProject, storage.StorageRoleSandboxTemplateCache} {
		if !roles[role] {
			t.Fatalf("template plan roles = %#v, missing %q", roles, role)
		}
	}
	if roles[storage.StorageRoleSandboxRuntime] {
		t.Fatalf("template build allocated empty Sandbox staging/runtime: %#v", roles)
	}
}

func TestMergeStorageSurfacesReplacesReportOnlyDuplicate(t *testing.T) {
	existing := []storage.Surface{{ID: "cache", Advisory: true, Location: "report-only"}}
	measured := []storage.Surface{{ID: "cache", Role: storage.StorageRoleSandboxTemplateCache, AdmissionAuthoritative: true, Location: "measured"}}
	merged := mergeStorageSurfaces(existing, measured)
	if len(merged) != 1 || merged[0].Location != "measured" || !merged[0].AdmissionAuthoritative {
		t.Fatalf("merged surfaces = %#v", merged)
	}
}

func TestStoragePruneRejectsOperationFlag(t *testing.T) {
	err := runStorage([]string{"prune", "--operation", "template-build"})
	if err == nil {
		t.Fatal("storage prune accepted --operation")
	}
}
