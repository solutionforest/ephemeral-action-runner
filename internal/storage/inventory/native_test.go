package inventory

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/storage"
)

func TestCollectNativeRecognitionLeaseAndMalformedManifest(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	validKey := repeatedHex("4")
	writeNativeRevision(t, root, validKey, now.Add(-10*24*time.Hour), true)
	badKey := repeatedHex("5")
	badDirectory := filepath.Join(root, badKey)
	mustMkdirAll(t, badDirectory)
	mustWriteFile(t, filepath.Join(badDirectory, "ephemeral-action-runner"), []byte("binary"))
	mustWriteFile(t, filepath.Join(badDirectory, "controller-cache.manifest"), []byte("schemaVersion=1\ncacheKey="+badKey+"\ncacheKey="+badKey+"\nexecutable=ephemeral-action-runner\ncompletedAtUnix=1\n"))

	artifacts, warnings := collectNative(nativeOptions{Root: root})
	if len(artifacts) != 2 || len(warnings) != 1 {
		t.Fatalf("collectNative() artifacts=%+v warnings=%v", artifacts, warnings)
	}
	valid := findArtifact(t, artifacts, "native-controller:"+validKey)
	if valid.Ownership.Kind != storage.OwnershipExact || !hasProtection(valid, storage.ProtectionLease) {
		t.Fatalf("valid leased revision = %+v", valid)
	}
	bad := findArtifactByLocatorSuffix(t, artifacts, badKey)
	if bad.Ownership.Kind != storage.OwnershipUnknown || !hasProtection(bad, storage.ProtectionUncertain) {
		t.Fatalf("malformed revision = %+v", bad)
	}
}

func TestCollectNativeCurrentExecutableIdentity(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	oldKey := repeatedHex("6")
	currentKey := repeatedHex("7")
	writeNativeRevision(t, root, oldKey, now.Add(-20*24*time.Hour), false)
	currentExecutable := writeNativeRevision(t, root, currentKey, now.Add(-10*24*time.Hour), false)
	artifacts, warnings := collectNative(nativeOptions{Root: root, CurrentExecutable: currentExecutable})
	if len(warnings) != 0 {
		t.Fatalf("collectNative() warnings = %v", warnings)
	}
	current := findArtifact(t, artifacts, "native-controller:"+currentKey)
	old := findArtifact(t, artifacts, "native-controller:"+oldKey)
	if !current.Current || old.SupersededAt == nil || !old.SupersededAt.Equal(now.Add(-10*24*time.Hour)) {
		t.Fatalf("current=%+v old=%+v", current, old)
	}
}

func TestCollectNativeRecognizesStableControllerLayout(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	fingerprint := repeatedHex("d")
	toolchainID := repeatedHex("e")
	executableName := "ephemeral-action-runner"
	if runtime.GOOS == "windows" {
		executableName += ".exe"
	}
	executable := filepath.Join(root, executableName)
	mustWriteFile(t, executable, []byte("stable-binary"))
	completed := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	manifest := fmt.Sprintf("schemaVersion=2\nfingerprint=%s\nexecutable=%s\ntoolchainImageID=sha256:%s\nsourceRevision=dirty:sha256:%s\ncompletedAtUtc=%s\n", fingerprint, executableName, toolchainID, fingerprint, completed.Format(time.RFC3339Nano))
	mustWriteFile(t, filepath.Join(root, "ephemeral-action-runner.manifest"), []byte(manifest))
	mustWriteFile(t, filepath.Join(root, ".native-controller.lock"), nil)
	artifacts, warnings := collectNative(nativeOptions{Root: root, CurrentExecutable: executable})
	if len(warnings) != 0 || len(artifacts) != 1 {
		t.Fatalf("collectNative() artifacts=%+v warnings=%v", artifacts, warnings)
	}
	stable := findArtifact(t, artifacts, "native-controller-stable:"+fingerprint)
	if !stable.Current || stable.Ownership.Kind != storage.OwnershipExact || !hasProtection(stable, storage.ProtectionCurrent) || stable.Target.Locator != executable {
		t.Fatalf("stable native controller = %+v", stable)
	}
}

func TestCollectNativeRejectsSymlinkedRevision(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(root, repeatedHex("8"))
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink creation unavailable on %s: %v", runtime.GOOS, err)
	}
	artifacts, warnings := collectNative(nativeOptions{Root: root})
	if len(artifacts) != 1 || len(warnings) != 1 || artifacts[0].Ownership.Kind != storage.OwnershipUnknown || artifacts[0].Target.Match != storage.MatchUnknown {
		t.Fatalf("collectNative() symlink artifacts=%+v warnings=%v", artifacts, warnings)
	}
}

func TestParseManifestRejectsUnknownAndMissingFields(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "manifest")
	for name, content := range map[string]string{
		"unknown": "schemaVersion=1\ncacheKey=" + repeatedHex("9") + "\nexecutable=ephemeral-action-runner\ncompletedAtUnix=1\nother=value\n",
		"missing": "schemaVersion=1\ncacheKey=" + repeatedHex("9") + "\nexecutable=ephemeral-action-runner\n",
	} {
		t.Run(name, func(t *testing.T) {
			mustWriteFile(t, path, []byte(content))
			if _, err := parseManifest(path); err == nil {
				t.Fatalf("parseManifest() accepted %s fields", name)
			}
		})
	}
}

func TestInspectNativeRevisionAcceptsWindowsUTCManifest(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	key := repeatedHex("b")
	directory := filepath.Join(root, key)
	mustMkdirAll(t, directory)
	mustWriteFile(t, filepath.Join(directory, "ephemeral-action-runner.exe"), []byte("binary"))
	completed := time.Date(2026, 7, 27, 12, 0, 0, 123, time.UTC)
	manifest := fmt.Sprintf("schemaVersion=1\ncacheKey=%s\nexecutable=ephemeral-action-runner.exe\ncompletedAtUtc=%s\n", key, completed.Format(time.RFC3339Nano))
	mustWriteFile(t, filepath.Join(directory, "controller-cache.manifest"), []byte(manifest))
	revision, err := inspectNativeRevision(directory, key)
	if err != nil {
		t.Fatalf("inspectNativeRevision() error = %v", err)
	}
	if !revision.completed.Equal(completed) || revision.artifact.Ownership.Kind != storage.OwnershipExact {
		t.Fatalf("inspectNativeRevision() = %+v", revision)
	}
}

func TestNormalizeCurrentRevisionAcceptsInjectedSourceRevisionForms(t *testing.T) {
	t.Parallel()
	key := repeatedHex("c")
	for _, value := range []string{key, "sha256:" + key, "dirty:sha256:" + key} {
		got, warning := normalizeCurrentRevision(value)
		if got != key || warning != "" {
			t.Fatalf("normalizeCurrentRevision(%q) = %q, %q", value, got, warning)
		}
	}
}

func writeNativeRevision(t *testing.T, root, key string, completed time.Time, lease bool) string {
	t.Helper()
	directory := filepath.Join(root, key)
	mustMkdirAll(t, directory)
	executable := "ephemeral-action-runner"
	if runtime.GOOS == "windows" {
		executable += ".exe"
	}
	binary := filepath.Join(directory, executable)
	mustWriteFile(t, binary, []byte("binary:"+key))
	manifest := fmt.Sprintf("schemaVersion=1\ncacheKey=%s\nexecutable=%s\ncompletedAtUnix=%d\n", key, executable, completed.Unix())
	mustWriteFile(t, filepath.Join(directory, "controller-cache.manifest"), []byte(manifest))
	if lease {
		mustWriteFile(t, filepath.Join(directory, "lease.123.abcdef"), []byte("schemaVersion=1\n"))
	}
	return binary
}
