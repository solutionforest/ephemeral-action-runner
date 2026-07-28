package provider

import (
	"context"
	"testing"
	"time"
)

func TestFilesystemStorageAppliesProviderOperationMinimumExpansion(t *testing.T) {
	contribution := NewMultiFilesystemStorageWithMinimumExpansions(
		"example",
		[]StorageRoot{{ID: "project", Location: t.TempDir()}},
		map[string]uint64{"instance-create": 42},
	)

	snapshot, err := contribution.StorageSnapshot(context.Background(), StorageRequest{
		Operation:        "instance-create",
		Now:              time.Now(),
		PeakBytes:        10,
		MinimumFreeBytes: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(snapshot.Requirements), 1; got != want {
		t.Fatalf("requirement count = %d, want %d", got, want)
	}
	if got, want := snapshot.Requirements[0].PeakBytes, uint64(42); got != want {
		t.Fatalf("peak bytes = %d, want %d", got, want)
	}
}

func TestFilesystemStorageKeepsLargerCommonExpansion(t *testing.T) {
	contribution := NewMultiFilesystemStorageWithMinimumExpansions(
		"example",
		[]StorageRoot{{ID: "project", Location: t.TempDir()}},
		map[string]uint64{"instance-create": 42},
	)

	snapshot, err := contribution.StorageSnapshot(context.Background(), StorageRequest{
		Operation: "instance-create",
		Now:       time.Now(),
		PeakBytes: 84,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := snapshot.Requirements[0].PeakBytes, uint64(84); got != want {
		t.Fatalf("peak bytes = %d, want %d", got, want)
	}
}

func TestFilesystemStorageRoutesOperationRequirementsToExactSurfaces(t *testing.T) {
	root := t.TempDir()
	contribution := NewMultiFilesystemStorage(
		"example",
		[]StorageRoot{
			{ID: "project", Location: root},
			{ID: "engine", Location: root, MinimumExpansions: map[string]uint64{"image-pull": 20}},
			{ID: "instance-store", Location: root, MinimumExpansions: map[string]uint64{"instance-create": 42}},
		},
	)

	snapshot, err := contribution.StorageSnapshot(context.Background(), StorageRequest{
		Operation:        "instance-create",
		Now:              time.Now(),
		PeakBytes:        10,
		MinimumFreeBytes: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(snapshot.Requirements), 2; got != want {
		t.Fatalf("requirement count = %d, want %d", got, want)
	}
	if got, want := len(snapshot.Surfaces), 3; got != want {
		t.Fatalf("surface count = %d, want %d", got, want)
	}
	got := map[string]uint64{}
	for _, requirement := range snapshot.Requirements {
		got[requirement.SurfaceID] = requirement.PeakBytes
	}
	if got["project"] != 10 || got["instance-store"] != 42 {
		t.Fatalf("instance-create requirements = %v, want project=10 and instance-store=42", got)
	}
	if _, found := got["engine"]; found {
		t.Fatalf("instance-create incorrectly required engine capacity: %v", got)
	}
}
