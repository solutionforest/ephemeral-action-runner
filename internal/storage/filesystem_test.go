package storage

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestSnapshotFilesystemTargetAndDrift(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "artifact.bin")
	if err := os.WriteFile(path, []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := SnapshotFilesystemTarget(path)
	if err != nil {
		t.Fatalf("SnapshotFilesystemTarget() error = %v", err)
	}
	if first.Match != MatchExact || first.Kind != TargetFile || first.Identity == "" || first.Fingerprint == "" {
		t.Fatalf("SnapshotFilesystemTarget() = %+v", first)
	}
	if err := os.WriteFile(path, []byte("different-size"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := SnapshotFilesystemTarget(path)
	if err != nil {
		t.Fatalf("second SnapshotFilesystemTarget() error = %v", err)
	}
	if first.Identity != second.Identity {
		t.Fatalf("same filesystem object identity changed: first=%s second=%s", first.Identity, second.Identity)
	}
	if first.Fingerprint == second.Fingerprint {
		t.Fatal("filesystem metadata drift did not change fingerprint")
	}
}

func TestSnapshotFilesystemTargetRejectsSymlinkOrReparse(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.WriteFile(target, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink creation unavailable on %s: %v", runtime.GOOS, err)
	}
	if _, err := SnapshotFilesystemTarget(link); err == nil {
		t.Fatal("SnapshotFilesystemTarget() accepted symlink or reparse point")
	}
}

func TestSnapshotFilesystemTargetRejectsRedirectedAncestor(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	realDirectory := filepath.Join(root, "real")
	if err := os.Mkdir(realDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(realDirectory, "artifact")
	if err := os.WriteFile(path, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "redirect")
	if err := os.Symlink(realDirectory, link); err != nil {
		t.Skipf("symlink creation unavailable on %s: %v", runtime.GOOS, err)
	}
	if _, err := SnapshotFilesystemTarget(filepath.Join(link, "artifact")); err == nil {
		t.Fatal("SnapshotFilesystemTarget() accepted redirected ancestor")
	}
}

func TestProbeFilesystemCapacity(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	capacity, err := ProbeFilesystemCapacity(t.TempDir(), now)
	if err != nil {
		t.Fatalf("ProbeFilesystemCapacity() error = %v", err)
	}
	if !capacity.Known || capacity.TotalBytes == 0 || capacity.AvailableBytes > capacity.TotalBytes || !capacity.ObservedAt.Equal(now) {
		t.Fatalf("ProbeFilesystemCapacity() = %+v", capacity)
	}
}

func TestProbeFilesystemCapacityDomainFollowsRedirectButCleanupSnapshotRejectsIt(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	realDirectory := filepath.Join(root, "real")
	if err := os.Mkdir(realDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	redirect := filepath.Join(root, "redirect")
	if err := os.Symlink(realDirectory, redirect); err != nil {
		t.Skipf("symlink creation unavailable on %s: %v", runtime.GOOS, err)
	}
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	realDomain, err := ProbeFilesystemCapacityDomain(realDirectory, now)
	if err != nil {
		t.Fatalf("ProbeFilesystemCapacityDomain(real) error = %v", err)
	}
	redirectDomain, err := ProbeFilesystemCapacityDomain(redirect, now)
	if err != nil {
		t.Fatalf("ProbeFilesystemCapacityDomain(redirect) error = %v", err)
	}
	if realDomain.ID == "" || realDomain.Identity == "" || realDomain.ID != redirectDomain.ID {
		t.Fatalf("capacity domains real=%+v redirect=%+v, want the same physical domain", realDomain, redirectDomain)
	}
	if _, err := SnapshotFilesystemTarget(redirect); err == nil {
		t.Fatal("SnapshotFilesystemTarget() accepted redirect used safely by the read-only probe")
	}
}
