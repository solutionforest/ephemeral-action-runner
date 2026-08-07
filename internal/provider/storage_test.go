package provider

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/storage"
)

func TestFilesystemStoragePublishesRoleAndCapacityDomain(t *testing.T) {
	root := t.TempDir()
	capacityRoot, err := nearestExistingDirectory(root)
	if err != nil {
		t.Fatal(err)
	}
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
	if surface.Path != root || surface.Location != capacityRoot {
		t.Fatalf("surface paths = logical %q capacity %q, want logical %q capacity %q", surface.Path, surface.Location, root, capacityRoot)
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
	firstCapacityRoot, err := nearestExistingDirectory(first)
	if err != nil {
		t.Fatal(err)
	}
	secondCapacityRoot, err := nearestExistingDirectory(second)
	if err != nil {
		t.Fatal(err)
	}
	contribution := NewMultiFilesystemStorage("example", []StorageRoot{
		{ID: "first", Path: first},
		{ID: "second", Path: second},
	}).(*filesystemStorage)
	contribution.domainProbe = func(path string, now time.Time) (storage.CapacityDomain, error) {
		switch path {
		case firstCapacityRoot:
			return storage.CapacityDomain{ID: "volume:first", Identity: "volume:first", Path: path, Capacity: storage.Capacity{Known: true, ObservedAt: now}}, nil
		case secondCapacityRoot:
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
	capacityRoot, err := nearestExistingDirectory(root)
	if err != nil {
		t.Fatal(err)
	}
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
	if surface.Path == surface.Location || surface.Location != capacityRoot {
		t.Fatalf("logical path %q capacity path %q, want %q", surface.Path, surface.Location, capacityRoot)
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

func TestFilesystemStoragePublishesUnknownCapacityAndGroupsOnlyMatchingLocators(t *testing.T) {
	now := time.Now()
	contribution := NewMultiFilesystemStorage("example", []StorageRoot{
		{ID: "engine", Role: storage.StorageRoleDockerEngine, Path: "/guest/docker", CapacityPath: "/guest/store/../store", CapacityUnavailableReason: "host path is not visible"},
		{ID: "containerd", Role: storage.StorageRoleContainerdStore, Path: "/guest/containerd", CapacityPath: "/guest/store", CapacityUnavailableReason: "host path is not visible"},
		{ID: "runtime", Role: storage.StorageRoleSandboxRuntime, Path: "/guest/runtime", CapacityUnavailableReason: "runtime store is not visible"},
	})
	snapshot, err := contribution.StorageSnapshot(context.Background(), StorageRequest{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Domains) != 2 {
		t.Fatalf("domains = %#v, want two normalized unknown locators", snapshot.Domains)
	}
	byRole := make(map[storage.StorageRole]storage.Surface)
	for _, surface := range snapshot.Surfaces {
		byRole[surface.Role] = surface
		if surface.Capacity.Known {
			t.Fatalf("unknown surface has known capacity: %#v", surface)
		}
	}
	if byRole[storage.StorageRoleDockerEngine].DomainID != byRole[storage.StorageRoleContainerdStore].DomainID {
		t.Fatalf("matching normalized locators were not grouped: %#v", snapshot.Surfaces)
	}
	if byRole[storage.StorageRoleDockerEngine].DomainID == byRole[storage.StorageRoleSandboxRuntime].DomainID {
		t.Fatalf("different unknown locators were grouped: %#v", snapshot.Surfaces)
	}
	for _, domain := range snapshot.Domains {
		if domain.Capacity.Known || domain.CapacityUnavailableReason == "" {
			t.Fatalf("unknown domain = %#v", domain)
		}
	}
	for _, domain := range snapshot.Domains {
		if domain.ID == byRole[storage.StorageRoleDockerEngine].DomainID && !strings.Contains(domain.CapacityUnavailableReason, "host path is not visible") {
			t.Fatalf("aliased unknown reasons = %q", domain.CapacityUnavailableReason)
		}
	}
	if len(snapshot.Warnings) != 3 || !strings.Contains(strings.Join(snapshot.Warnings, "\n"), "host path is not visible") {
		t.Fatalf("warnings = %v", snapshot.Warnings)
	}
}

func TestFilesystemStorageRetainsKnownDomainWhenAnotherProbeFails(t *testing.T) {
	knownRoot := t.TempDir()
	unknownRoot := t.TempDir()
	contribution := NewMultiFilesystemStorage("example", []StorageRoot{
		{ID: "known", Role: storage.StorageRoleProject, Path: knownRoot},
		{ID: "unknown", Role: storage.StorageRoleDockerEngine, Path: unknownRoot},
	}).(*filesystemStorage)
	contribution.domainProbe = func(path string, now time.Time) (storage.CapacityDomain, error) {
		if path == unknownRoot {
			return storage.CapacityDomain{}, errors.New("statfs denied")
		}
		return storage.CapacityDomain{ID: "known-domain", Kind: storage.SurfaceHostFilesystem, Identity: "known-domain", Path: path, Capacity: storage.Capacity{Known: true, TotalBytes: 100 * storage.GiB, AvailableBytes: 80 * storage.GiB, ObservedAt: now}}, nil
	}
	plan := storage.OperationPlan{ID: "mixed", MinimumFreeBytes: storage.GiB, Phases: []storage.OperationPhase{{ID: "phase", Allocations: []storage.Allocation{
		{ID: "known-allocation", Role: storage.StorageRoleProject, Bytes: storage.GiB},
		{ID: "unknown-allocation", Role: storage.StorageRoleDockerEngine, Bytes: storage.GiB},
	}}}}
	snapshot, err := contribution.StorageSnapshot(context.Background(), StorageRequest{OperationPlan: plan, Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Domains) != 2 {
		t.Fatalf("domains = %#v, want known and unknown", snapshot.Domains)
	}
	evaluation, err := storage.EvaluateOperationPlan(plan, snapshot.Surfaces, snapshot.Domains)
	if err != nil {
		t.Fatal(err)
	}
	statuses := make(map[storage.CapacityStatus]int)
	for _, check := range evaluation.CapacityChecks {
		statuses[check.Status]++
		if check.Status == storage.CapacityUnknown && !strings.Contains(check.Reason, "statfs denied") {
			t.Fatalf("unknown check reason = %q", check.Reason)
		}
	}
	if statuses[storage.CapacityReady] != 1 || statuses[storage.CapacityUnknown] != 1 {
		t.Fatalf("capacity statuses = %#v", statuses)
	}
}

func TestFilesystemStorageRejectsInvalidProbeObservation(t *testing.T) {
	root := t.TempDir()
	contribution := NewMultiFilesystemStorage("example", []StorageRoot{{ID: "project", Role: storage.StorageRoleProject, Path: root}}).(*filesystemStorage)
	contribution.domainProbe = func(string, time.Time) (storage.CapacityDomain, error) {
		return storage.CapacityDomain{ID: "missing-identity", Capacity: storage.Capacity{Known: true}}, nil
	}
	if _, err := contribution.StorageSnapshot(context.Background(), StorageRequest{Now: time.Now()}); err == nil || !strings.Contains(err.Error(), "incomplete capacity domain") {
		t.Fatalf("invalid observation error = %v", err)
	}
}

func TestFilesystemStorageRejectsDuplicateDomainIDForDifferentIdentities(t *testing.T) {
	first := t.TempDir()
	second := t.TempDir()
	contribution := NewMultiFilesystemStorage("example", []StorageRoot{{ID: "first", Path: first}, {ID: "second", Path: second}}).(*filesystemStorage)
	contribution.domainProbe = func(path string, now time.Time) (storage.CapacityDomain, error) {
		return storage.CapacityDomain{ID: "duplicate-domain", Identity: "identity:" + path, Kind: storage.SurfaceHostFilesystem, Path: path, Capacity: storage.Capacity{Known: true, ObservedAt: now}}, nil
	}
	if _, err := contribution.StorageSnapshot(context.Background(), StorageRequest{Now: time.Now()}); err == nil || !strings.Contains(err.Error(), "conflicting identities") {
		t.Fatalf("duplicate domain ID error = %v", err)
	}
}
