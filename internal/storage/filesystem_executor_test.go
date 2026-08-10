package storage

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestFilesystemExecutorRemovesOnlyExactTargetBelowAllowedRoot(t *testing.T) {
	root := t.TempDir()
	targetPath := filepath.Join(root, "old", "archive.tar")
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(targetPath, []byte("archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	target, err := SnapshotFilesystemTarget(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	executor, err := NewFilesystemExecutor(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := executor.RemoveExact(context.Background(), Removal{ArtifactID: "archive", Target: target}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(targetPath); !os.IsNotExist(err) {
		t.Fatalf("target still exists: %v", err)
	}
}

func TestFilesystemExecutorRejectsAllowedRootAndDrift(t *testing.T) {
	root := t.TempDir()
	executor, err := NewFilesystemExecutor(root)
	if err != nil {
		t.Fatal(err)
	}
	rootTarget, err := SnapshotFilesystemTarget(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := executor.RemoveExact(context.Background(), Removal{ArtifactID: "root", Target: rootTarget}); err == nil {
		t.Fatal("RemoveExact() removed or accepted the allowed root")
	}

	path := filepath.Join(root, "candidate")
	if err := os.WriteFile(path, []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	target, err := SnapshotFilesystemTarget(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := executor.RemoveExact(context.Background(), Removal{ArtifactID: "candidate", Target: target}); err == nil {
		t.Fatal("RemoveExact() accepted a drifted target")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("drifted target was removed: %v", err)
	}
}

func TestFilesystemExecutorRejectsRedirectedDescendant(t *testing.T) {
	root := t.TempDir()
	targetPath := filepath.Join(root, "revision")
	if err := os.Mkdir(targetPath, 0o755); err != nil {
		t.Fatal(err)
	}
	linkPath := filepath.Join(targetPath, "redirect")
	if err := os.Symlink(t.TempDir(), linkPath); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	target, err := SnapshotFilesystemTarget(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	executor, err := NewFilesystemExecutor(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := executor.RemoveExact(context.Background(), Removal{ArtifactID: "revision", Target: target}); err == nil {
		t.Fatal("RemoveExact() accepted a redirected descendant")
	}
	if _, err := os.Stat(targetPath); err != nil {
		t.Fatalf("target directory was removed: %v", err)
	}
}
