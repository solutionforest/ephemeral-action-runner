package staging

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func TestCreateAndRemoveExactEmptyStagingDirectory(t *testing.T) {
	staging, err := Open(filepath.Join(t.TempDir(), "staging"))
	if err != nil {
		t.Fatal(err)
	}
	owned, err := staging.CreateOwned("epar-sbx-001")
	if err != nil {
		t.Fatal(err)
	}
	path := owned.Path
	if got, want := path, filepath.Join(staging.Root(), "epar-sbx-001"); got != want {
		t.Fatalf("path = %q, want %q", got, want)
	}
	if _, err := staging.VerifyOwnedEmpty("epar-sbx-001", owned.Identity); err != nil {
		t.Fatal(err)
	}
	if err := staging.RemoveEmptyOwned("epar-sbx-001", owned.Identity); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("removed staging directory still exists: %v", err)
	}
	if err := staging.RemoveEmptyOwned("epar-sbx-001", owned.Identity); err != nil {
		t.Fatalf("missing exact staging directory should be idempotent: %v", err)
	}
}

func TestRejectsUnsafeNamesAndAlternateDataStreams(t *testing.T) {
	staging, err := Open(filepath.Join(t.TempDir(), "staging"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"", ".", "..", "../escape", `..\\escape`, " leading", "trailing ", "name:stream", "name/subdir", "name\\subdir"} {
		t.Run(strings.ReplaceAll(name, "/", "_"), func(t *testing.T) {
			if _, err := staging.CreateOwned(name); err == nil {
				t.Fatalf("Create(%q) succeeded", name)
			}
		})
	}
}

func TestRejectsPreexistingOrNonemptyDirectory(t *testing.T) {
	staging, err := Open(filepath.Join(t.TempDir(), "staging"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(staging.Root(), "existing")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := staging.CreateOwned("existing"); err == nil {
		t.Fatal("pre-existing staging directory accepted")
	}
	if err := os.WriteFile(filepath.Join(path, "sentinel"), []byte("host data"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := staging.verifyEmpty("existing"); err == nil {
		t.Fatal("non-empty staging directory verified")
	}
	if _, err := os.Stat(filepath.Join(path, "sentinel")); err != nil {
		t.Fatalf("sentinel was changed: %v", err)
	}
}

func TestRejectsSymlinkOrJunctionRedirection(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := Open(link); err == nil {
		t.Fatal("redirected staging root accepted")
	}
}

func TestOpenDoesNotCreateThroughRedirectedMissingAncestor(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	redirect := filepath.Join(root, "redirect")
	if err := os.Symlink(target, redirect); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := Open(filepath.Join(redirect, "must-not-exist")); err == nil {
		t.Fatal("missing staging root beneath redirect was accepted")
	}
	if _, err := os.Lstat(filepath.Join(target, "must-not-exist")); !os.IsNotExist(err) {
		t.Fatalf("Open created content through redirected ancestor: %v", err)
	}
}

func TestRejectsWeakUnixPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits do not describe Windows ACL strength")
	}
	root := filepath.Join(t.TempDir(), "staging")
	if err := os.Mkdir(root, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(root); err == nil {
		t.Fatal("weak staging root permissions accepted")
	}
}

func TestCreatedStagingDirectoryHasStrongPlatformPermissions(t *testing.T) {
	staging, err := Open(filepath.Join(t.TempDir(), "staging"))
	if err != nil {
		t.Fatal(err)
	}
	owned, err := staging.CreateOwned("private")
	if err != nil {
		t.Fatal(err)
	}
	path := owned.Path
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := validatePlatformPermissions(path, info); err != nil {
		t.Fatalf("created staging permissions are weak: %v", err)
	}
}

func TestConcurrentCreateHasOneOwner(t *testing.T) {
	staging, err := Open(filepath.Join(t.TempDir(), "staging"))
	if err != nil {
		t.Fatal(err)
	}
	const callers = 16
	results := make(chan error, callers)
	var wait sync.WaitGroup
	wait.Add(callers)
	for index := 0; index < callers; index++ {
		go func() {
			defer wait.Done()
			_, err := staging.CreateOwned("one-owner")
			results <- err
		}()
	}
	wait.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("successful concurrent creates = %d, want 1", successes)
	}
}

func TestOwnedIdentityRejectsSamePathReplacement(t *testing.T) {
	staging, err := Open(filepath.Join(t.TempDir(), "staging"))
	if err != nil {
		t.Fatal(err)
	}
	owned, err := staging.CreateOwned("replace-me")
	if err != nil {
		t.Fatal(err)
	}
	previous := filepath.Join(staging.Root(), "previous-object")
	if err := os.Rename(owned.Path, previous); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(owned.Path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := restrictPlatformPermissions(owned.Path); err != nil {
		t.Fatal(err)
	}
	if _, err := staging.VerifyOwnedEmpty("replace-me", owned.Identity); err == nil || !strings.Contains(err.Error(), "replaced") {
		t.Fatalf("replacement identity error = %v", err)
	}
	if err := staging.RemoveEmptyOwned("replace-me", owned.Identity); err == nil {
		t.Fatal("same-path replacement was removed")
	}
	if _, err := os.Stat(owned.Path); err != nil {
		t.Fatalf("same-path replacement was not preserved: %v", err)
	}
}

func TestPurgeOwnedDoesNotFollowNestedSymlink(t *testing.T) {
	staging, err := Open(filepath.Join(t.TempDir(), "staging"))
	if err != nil {
		t.Fatal(err)
	}
	owned, err := staging.CreateOwned("symlink-tree")
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(target, "preserve")
	if err := os.WriteFile(sentinel, []byte("host data"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(owned.Path, "workflow-link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := staging.PurgeOwned("symlink-tree", owned.Identity); err != nil {
		t.Fatal(err)
	}
	if content, err := os.ReadFile(sentinel); err != nil || string(content) != "host data" {
		t.Fatalf("purge followed nested symlink: content=%q err=%v", content, err)
	}
}

func TestPurgeOwnedRecoversDeterministicQuarantine(t *testing.T) {
	staging, err := Open(filepath.Join(t.TempDir(), "staging"))
	if err != nil {
		t.Fatal(err)
	}
	owned, err := staging.CreateOwned("recover")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(owned.Path, "content"), []byte("job output"), 0600); err != nil {
		t.Fatal(err)
	}
	quarantine := filepath.Join(staging.Root(), "recover.deleting")
	if err := os.Rename(owned.Path, quarantine); err != nil {
		t.Fatal(err)
	}
	if err := staging.PurgeOwned("recover", owned.Identity); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(quarantine); !os.IsNotExist(err) {
		t.Fatalf("recovered quarantine remains: %v", err)
	}
}
