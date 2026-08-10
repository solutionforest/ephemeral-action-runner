package inventory

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/solutionforest/ephemeral-action-runner/internal/storage"
)

func TestCollectLogsProtectsSubsystemOwnedRoot(t *testing.T) {
	t.Parallel()
	project := t.TempDir()
	root := filepath.Join(project, "work", "logs")
	mustWriteFile(t, filepath.Join(root, "instances", "runner.log"), []byte("runner"))
	artifacts, warnings := collectLogs(root, project)
	if len(warnings) != 0 || len(artifacts) != 1 {
		t.Fatalf("collectLogs() artifacts=%+v warnings=%v", artifacts, warnings)
	}
	if artifacts[0].Ownership.Kind != storage.OwnershipExact || artifacts[0].SizeBytes != uint64(len("runner")) || !hasProtection(artifacts[0], storage.ProtectionOperator) {
		t.Fatalf("logs artifact = %+v", artifacts[0])
	}
}

func TestCollectLogsRejectsRedirectedDescendant(t *testing.T) {
	t.Parallel()
	project := t.TempDir()
	root := filepath.Join(project, "work", "logs")
	mustMkdirAll(t, root)
	outside := t.TempDir()
	link := filepath.Join(root, "redirect")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink creation unavailable on %s: %v", runtime.GOOS, err)
	}
	artifacts, warnings := collectLogs(root, project)
	if len(warnings) != 1 || len(artifacts) != 1 || artifacts[0].Ownership.Kind != storage.OwnershipUnknown || !hasProtection(artifacts[0], storage.ProtectionUncertain) {
		t.Fatalf("collectLogs() artifacts=%+v warnings=%v", artifacts, warnings)
	}
}
