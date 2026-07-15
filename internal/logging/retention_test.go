package logging

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/filelock"
)

func TestRetentionAppliesAgeThenAggregateBudgetAndProtectsUnknownCurrent(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	old := now.Add(-72 * time.Hour)
	recentOldest := now.Add(-3 * time.Hour)
	recentNewest := now.Add(-2 * time.Hour)
	errorPath := ErrorPath(root, old)
	instancePath, _ := InstancePath(root, "runner-1", "guest")
	buildPath, _ := BuildPath(root, "ubuntu", "build")
	for _, file := range []struct {
		path string
		when time.Time
	}{
		{errorPath, old},
		{instancePath, recentOldest},
		{buildPath, recentNewest},
		{ManagerPath(root), recentNewest},
	} {
		writeSizedFile(t, file.path, 8, file.when)
	}
	unknown := filepath.Join(root, "instances", "runner-1.ready")
	writeSizedFile(t, unknown, 100, old)
	unknownLog := filepath.Join(root, "instances", "notes.user.log")
	writeSizedFile(t, unknownLog, 100, old)
	unknownFlatGuest := filepath.Join(root, "arbitrary.guest.log")
	writeSizedFile(t, unknownFlatGuest, 100, old)

	policy := RetentionPolicy{MaxTotalBytes: 10, ErrorMaxAge: 24 * time.Hour, Now: func() time.Time { return now }}
	report, err := ListRetention(root, policy)
	if err != nil {
		t.Fatalf("ListRetention: %v", err)
	}
	if report.WouldDelete != 3 || report.TotalBytesBefore != 32 || report.TotalBytesAfter != 8 {
		t.Fatalf("dry-run report = %#v", report)
	}
	assertRetentionReason(t, report, unknown, RetentionProtected, "unknown")
	assertRetentionReason(t, report, unknownLog, RetentionProtected, "unknown")
	assertRetentionReason(t, report, unknownFlatGuest, RetentionProtected, "unknown")
	assertRetentionReason(t, report, ManagerPath(root), RetentionProtected, "current")

	report, err = PruneRetention(root, policy, false)
	if err != nil {
		t.Fatalf("PruneRetention: %v", err)
	}
	if report.Deleted != 3 || report.ReclaimedBytes != 24 || report.TotalBytesAfter != 8 {
		t.Fatalf("prune report = %#v", report)
	}
	for _, path := range []string{errorPath, instancePath, buildPath} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("deleted path %s still exists: %v", path, err)
		}
	}
	for _, path := range []string{unknown, unknownLog, unknownFlatGuest, ManagerPath(root)} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("protected path %s: %v", path, err)
		}
	}
}

func TestRetentionProtectsActiveTranscriptThenDeletesAfterClose(t *testing.T) {
	root := t.TempDir()
	now := time.Now().UTC()
	runtime, err := NewRuntime(Options{Directory: root, TranscriptSinks: SinkFile})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	path, _ := InstancePath(root, "runner-active", "guest")
	transcript, err := runtime.OpenTranscript(TranscriptMetadata{Category: CategoryInstances, Instance: "runner-active", Component: "guest"}, path)
	if err != nil {
		t.Fatalf("OpenTranscript: %v", err)
	}
	_, _ = transcript.Stdout.Write([]byte("active\n"))
	old := now.Add(-48 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	backupPath := strings.TrimSuffix(path, ".log") + "-2026-07-14T01-02-03.004.log.gz"
	writeSizedFile(t, backupPath, 8, old)
	recognizedBackup, ok := recognizePath(mustCanonicalPath(t, root), mustCanonicalPath(t, backupPath))
	if !ok || !recognizedBackup.backup {
		t.Fatalf("backup not recognized: %#v %v", recognizedBackup, ok)
	}
	if got, want := mustCanonicalPath(t, activeLockPathForRecognized(mustCanonicalPath(t, backupPath), recognizedBackup)), mustCanonicalPath(t, path); got != want {
		t.Fatalf("backup active lock path = %s, want %s", got, want)
	}
	policy := RetentionPolicy{InstanceMaxAge: time.Hour, Now: func() time.Time { return now }}
	report, err := ListRetention(root, policy)
	if err != nil {
		t.Fatalf("ListRetention: %v", err)
	}
	assertRetentionReason(t, report, path, RetentionProtected, "active")
	assertRetentionReason(t, report, backupPath, RetentionProtected, "active")
	if err := transcript.Close(); err != nil {
		t.Fatalf("Close transcript: %v", err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatalf("Close runtime: %v", err)
	}
	report, err = PruneRetention(root, policy, false)
	if err != nil {
		t.Fatalf("PruneRetention: %v", err)
	}
	if report.Deleted != 2 {
		t.Fatalf("deleted = %d, report=%#v", report.Deleted, report)
	}
}

func TestRetentionRecoversStaleActiveMetadata(t *testing.T) {
	root, err := canonicalPath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path, _ := InstancePath(root, "runner-stale", "guest")
	canonical, _ := canonicalPath(path)
	hash := pathHash(canonical)
	activeDir := filepath.Join(root, controlDirectoryName, "active")
	if err := os.MkdirAll(activeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	metadataPath := filepath.Join(activeDir, hash+".json")
	data, err := json.Marshal(activeState{Version: 1, Path: canonical, PID: 999999})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metadataPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := ListRetention(root, RetentionPolicy{})
	if err != nil {
		t.Fatalf("ListRetention: %v", err)
	}
	if _, err := os.Stat(metadataPath); !os.IsNotExist(err) {
		t.Fatalf("stale metadata still exists: %v", err)
	}
	found := false
	for _, warning := range report.Warnings {
		found = found || strings.Contains(warning, "recovered stale")
	}
	if !found {
		t.Fatalf("warnings = %#v", report.Warnings)
	}
}

func TestRetentionWarnsWhenProtectedFilesAloneExceedBudget(t *testing.T) {
	root := t.TempDir()
	writeSizedFile(t, ManagerPath(root), 20, time.Now().UTC())
	report, err := ListRetention(root, RetentionPolicy{MaxTotalBytes: 10})
	if err != nil {
		t.Fatal(err)
	}
	if report.WouldDelete != 0 || report.TotalBytesAfter != 20 {
		t.Fatalf("report = %#v", report)
	}
	found := false
	for _, warning := range report.Warnings {
		found = found || strings.Contains(warning, "aggregate budget remains exceeded")
	}
	if !found {
		t.Fatalf("warnings = %#v", report.Warnings)
	}
}

func TestRetentionDryRunAndPruneSelectTheSameFiles(t *testing.T) {
	root := t.TempDir()
	now := time.Now().UTC()
	paths := []string{ErrorPath(root, now.Add(-48*time.Hour)), mustPath(InstancePath(root, "runner-old", "wsl")), mustPath(BuildPath(root, "image-old", "source"))}
	for index, path := range paths {
		writeSizedFile(t, path, index+1, now.Add(-time.Duration(48+index)*time.Hour))
	}
	policy := RetentionPolicy{InstanceMaxAge: time.Hour, BuildMaxAge: time.Hour, ErrorMaxAge: time.Hour, Now: func() time.Time { return now }}
	dryRun, err := ListRetention(root, policy)
	if err != nil {
		t.Fatal(err)
	}
	planned := retentionPathsWithAction(dryRun, RetentionWouldDelete)
	pruned, err := PruneRetention(root, policy, false)
	if err != nil {
		t.Fatal(err)
	}
	deleted := retentionPathsWithAction(pruned, RetentionDeleted)
	if !reflect.DeepEqual(planned, deleted) {
		t.Fatalf("planned=%v deleted=%v", planned, deleted)
	}
}

func TestRetentionSafeSkipsSymlinkEntry(t *testing.T) {
	root := t.TempDir()
	path, _ := InstancePath(root, "runner-link", "guest")
	report := RetentionReport{}
	entry := classifyRetentionEntry(root, path, fakeDirEntry{info: fakeFileInfo{name: filepath.Base(path), mode: os.ModeSymlink}}, map[string]struct{}{}, false, &report)
	if entry.Action != RetentionProtected || entry.Reason != "non-regular-or-reparse" {
		t.Fatalf("entry = %#v", entry)
	}
}

func TestRetentionProtectsRealSymlinkWhenAvailable(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(t.TempDir(), "target.log")
	writeSizedFile(t, target, 8, time.Now().Add(-48*time.Hour))
	link, _ := InstancePath(root, "runner-link", "guest")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}
	report, err := ListRetention(root, RetentionPolicy{InstanceMaxAge: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	assertRetentionReason(t, report, link, RetentionProtected, "non-regular-or-reparse")
}

func TestDeleteRetentionEntrySafeSkipsLockedPath(t *testing.T) {
	root, _ := canonicalPath(t.TempDir())
	path, _ := InstancePath(root, "runner-locked", "tart")
	writeSizedFile(t, path, 8, time.Now().Add(-48*time.Hour))
	directoryEntries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	report := RetentionReport{}
	entry := classifyRetentionEntry(root, path, directoryEntries[0], map[string]struct{}{}, false, &report)
	control := filepath.Join(root, controlDirectoryName)
	lockDir := filepath.Join(control, "locks")
	if err := ensureSafeDirectory(control, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := ensureSafeDirectory(lockDir, 0o700); err != nil {
		t.Fatal(err)
	}
	lock, err := filelock.Acquire(filepath.Join(lockDir, pathHash(mustCanonicalPath(t, path))+".lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	warning := deleteRetentionEntry(root, &entry)
	if !strings.Contains(warning, "acquire deletion lock") {
		t.Fatalf("warning = %q", warning)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("locked file was removed: %v", err)
	}
}

func retentionPathsWithAction(report RetentionReport, action RetentionAction) []string {
	var paths []string
	for _, entry := range report.Entries {
		if entry.Action == action {
			paths = append(paths, entry.Path)
		}
	}
	sort.Strings(paths)
	return paths
}

type fakeDirEntry struct{ info fakeFileInfo }

func (entry fakeDirEntry) Name() string               { return entry.info.Name() }
func (entry fakeDirEntry) IsDir() bool                { return entry.info.IsDir() }
func (entry fakeDirEntry) Type() os.FileMode          { return entry.info.Mode().Type() }
func (entry fakeDirEntry) Info() (os.FileInfo, error) { return entry.info, nil }

type fakeFileInfo struct {
	name string
	mode os.FileMode
	sys  any
}

func (info fakeFileInfo) Name() string       { return info.name }
func (info fakeFileInfo) Size() int64        { return 0 }
func (info fakeFileInfo) Mode() os.FileMode  { return info.mode }
func (info fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (info fakeFileInfo) IsDir() bool        { return info.mode.IsDir() }
func (info fakeFileInfo) Sys() any           { return info.sys }

func writeSizedFile(t *testing.T, path string, size int, timestamp time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("x", size)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, timestamp, timestamp); err != nil {
		t.Fatal(err)
	}
}

func assertRetentionReason(t *testing.T, report RetentionReport, path string, action RetentionAction, reason string) {
	t.Helper()
	want, _ := canonicalPath(path)
	for _, entry := range report.Entries {
		got, _ := canonicalPath(entry.Path)
		if got == want {
			if entry.Action != action || entry.Reason != reason {
				t.Fatalf("entry %s = action %s reason %s, want %s %s", path, entry.Action, entry.Reason, action, reason)
			}
			return
		}
	}
	t.Fatalf("entry %s not found", path)
}
