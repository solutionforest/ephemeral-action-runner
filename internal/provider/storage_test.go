package provider

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/storage"
)

func TestFilesystemStoragePublishesRoleAndCapacityDomain(t *testing.T) {
	root := t.TempDir()
	contribution := NewMultiFilesystemStorage("example", []StorageRoot{{ID: "project", Role: storage.StorageRoleProject, Path: root}})

	snapshot, err := contribution.StorageSnapshot(context.Background(), StorageRequest{
		OperationPlan: storage.OperationPlan{ID: "instance-create"},
		Now:           time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(snapshot.Surfaces), 1; got != want {
		t.Fatalf("surface count = %d, want %d", got, want)
	}
	if got, want := len(snapshot.Domains), 1; got != want {
		t.Fatalf("domain count = %d, want %d", got, want)
	}
	surface := snapshot.Surfaces[0]
	if surface.Role != storage.StorageRoleProject {
		t.Fatalf("surface role = %q, want %q", surface.Role, storage.StorageRoleProject)
	}
	if surface.DomainID == "" || surface.DomainID != snapshot.Domains[0].ID {
		t.Fatalf("surface domain = %q, domains = %#v", surface.DomainID, snapshot.Domains)
	}
	if surface.Path != root || surface.Location != root {
		t.Fatalf("surface paths = logical %q capacity %q, want %q", surface.Path, surface.Location, root)
	}
}

func TestFilesystemStorageConsolidatesSameCapacityDomain(t *testing.T) {
	root := t.TempDir()
	contribution := NewMultiFilesystemStorage("example", []StorageRoot{
		{ID: "runtime", Role: storage.StorageRoleSandboxRuntime, Path: filepath.Join(root, "runtime")},
		{ID: "template", Role: storage.StorageRoleSandboxTemplateCache, Path: filepath.Join(root, "template")},
	})
	snapshot, err := contribution.StorageSnapshot(context.Background(), StorageRequest{OperationPlan: storage.OperationPlan{ID: "instance-create", Phases: []storage.OperationPhase{{ID: "create", Allocations: []storage.Allocation{{ID: "runtime", Role: storage.StorageRoleSandboxRuntime}}}}}, Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Surfaces) != 2 || len(snapshot.Domains) != 1 {
		t.Fatalf("surfaces=%d domains=%d, want 2 surfaces sharing 1 domain", len(snapshot.Surfaces), len(snapshot.Domains))
	}
	if snapshot.Surfaces[0].DomainID != snapshot.Surfaces[1].DomainID {
		t.Fatalf("same-volume roots got domains %q and %q", snapshot.Surfaces[0].DomainID, snapshot.Surfaces[1].DomainID)
	}
}

func TestFilesystemStorageSkipsUnallocatedProviderDiscovery(t *testing.T) {
	root := t.TempDir()
	discoveryCalled := false
	contribution := NewFilesystemStorageWithDiscovery("example", []StorageRoot{{ID: "project", Role: storage.StorageRoleProject, Path: root}}, func(context.Context, StorageRequest) ([]StorageRoot, error) {
		discoveryCalled = true
		return nil, context.Canceled
	})
	plan := storage.OperationPlan{ID: "source-update", Phases: []storage.OperationPhase{{ID: "source-update", Allocations: []storage.Allocation{{ID: "project", Role: storage.StorageRoleProject, Bytes: storage.GiB}}}}}
	if _, err := contribution.StorageSnapshot(context.Background(), StorageRequest{OperationPlan: plan, Now: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if discoveryCalled {
		t.Fatal("project-only operation invoked unrelated provider discovery")
	}
}

func TestFilesystemStorageKeepsDifferentCapacityDomainsSeparate(t *testing.T) {
	first := t.TempDir()
	second := t.TempDir()
	contribution := NewMultiFilesystemStorage("example", []StorageRoot{
		{ID: "first", Path: first},
		{ID: "second", Path: second},
	}).(*filesystemStorage)
	contribution.domainProbe = func(path string, now time.Time) (storage.CapacityDomain, error) {
		switch path {
		case first:
			return storage.CapacityDomain{ID: "volume:first", Identity: "volume:first", Path: path, Capacity: storage.Capacity{Known: true, ObservedAt: now}}, nil
		case second:
			return storage.CapacityDomain{ID: "volume:second", Identity: "volume:second", Path: path, Capacity: storage.Capacity{Known: true, ObservedAt: now}}, nil
		default:
			t.Fatalf("unexpected capacity path %q", path)
			return storage.CapacityDomain{}, nil
		}
	}
	snapshot, err := contribution.StorageSnapshot(context.Background(), StorageRequest{Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Domains) != 2 {
		t.Fatalf("domain count = %d, want 2", len(snapshot.Domains))
	}
	if snapshot.Surfaces[0].DomainID == snapshot.Surfaces[1].DomainID {
		t.Fatalf("different-volume roots share domain %q", snapshot.Surfaces[0].DomainID)
	}
}

func TestFilesystemStorageIncludesDiscoveredEvidenceAndWarnings(t *testing.T) {
	root := t.TempDir()
	contribution := NewFilesystemStorageWithDiscovery("example", nil, func(context.Context, StorageRequest) ([]StorageRoot, error) {
		return []StorageRoot{{
			ID:           "runtime",
			Role:         storage.StorageRoleSandboxRuntime,
			Kind:         storage.SurfaceSandboxCache,
			Path:         filepath.Join(root, "future", "runtime"),
			CapacityPath: root,
			Provenance:   "documented-default-assumed",
			Confidence:   "assumed",
			Warnings:     []string{"using nearest existing ancestor"},
		}}, nil
	})
	plan := storage.OperationPlan{ID: "instance-create", Phases: []storage.OperationPhase{{ID: "create", Allocations: []storage.Allocation{{ID: "runtime", Role: storage.StorageRoleSandboxRuntime}}}}}
	snapshot, err := contribution.StorageSnapshot(context.Background(), StorageRequest{OperationPlan: plan, Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	surface := snapshot.Surfaces[0]
	if surface.Path == surface.Location || surface.Location != root {
		t.Fatalf("logical path %q capacity path %q", surface.Path, surface.Location)
	}
	if surface.Provenance != "documented-default-assumed" || surface.Confidence != "assumed" {
		t.Fatalf("surface evidence = %#v", surface)
	}
	if len(snapshot.Warnings) != 1 || snapshot.Warnings[0] != "using nearest existing ancestor" {
		t.Fatalf("warnings = %v", snapshot.Warnings)
	}
}

func TestFilesystemStorageMarksReportOnlyRootNonAuthoritative(t *testing.T) {
	root := t.TempDir()
	contribution := NewMultiFilesystemStorage("example", []StorageRoot{{ID: "config", Path: root, ReportOnly: true}})
	snapshot, err := contribution.StorageSnapshot(context.Background(), StorageRequest{Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Surfaces[0].AdmissionAuthoritative {
		t.Fatal("report-only storage root is admission-authoritative")
	}
}

func TestFilesystemStorageRejectsAmbiguousRoleMapping(t *testing.T) {
	root := t.TempDir()
	contribution := NewMultiFilesystemStorage("example", []StorageRoot{
		{ID: "first", Role: storage.StorageRoleProject, Path: root},
		{ID: "second", Role: storage.StorageRoleProject, Path: root},
	})
	_, err := contribution.StorageSnapshot(context.Background(), StorageRequest{Now: time.Now()})
	if err == nil {
		t.Fatal("duplicate storage role mapping was accepted")
	}
}
