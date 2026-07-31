package pool

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLifecycleStateUsesCanonicalConfigurationIdentity(t *testing.T) {
	project := t.TempDir()
	realConfig := filepath.Join(project, ".local", "config.yml")
	linkConfig := filepath.Join(project, ".local", "config-link.yml")
	if err := os.MkdirAll(filepath.Dir(realConfig), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(realConfig, []byte("provider: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realConfig, linkConfig); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	realStore, err := OpenLifecycleState(project, realConfig)
	if err != nil {
		t.Fatal(err)
	}
	linkStore, err := OpenLifecycleState(project, linkConfig)
	if err != nil {
		t.Fatal(err)
	}
	if realStore.Path() != linkStore.Path() {
		t.Fatalf("one configuration received split lifecycle state through a symlink: %q != %q", realStore.Path(), linkStore.Path())
	}
}

func TestLifecycleStateMigratesAncestorSymlinkNamespace(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	alias := filepath.Join(root, "project-alias")
	if err := os.MkdirAll(filepath.Join(project, ".local"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(project, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	configPath := filepath.Join(alias, ".local", "config.yml")
	if err := os.WriteFile(filepath.Join(project, ".local", "config.yml"), []byte("provider: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	legacyAbsolute, err := filepath.Abs(configPath)
	if err != nil {
		t.Fatal(err)
	}
	legacySum := sha256.Sum256([]byte(filepath.Clean(legacyAbsolute)))
	legacyDirectory := filepath.Join(project, ".local", "state", "pools", hex.EncodeToString(legacySum[:8]))
	if err := os.MkdirAll(legacyDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(legacyDirectory, "migration-marker")
	if err := os.WriteFile(marker, []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := OpenLifecycleState(project, configPath)
	if err != nil {
		t.Fatal(err)
	}
	if store.Path() == filepath.Join(legacyDirectory, "state-v1.json") {
		t.Fatal("ancestor-symlink lifecycle state remained in the legacy namespace")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(store.Path()), "migration-marker")); err != nil {
		t.Fatalf("legacy lifecycle state content was not migrated: %v", err)
	}
	if _, err := os.Stat(legacyDirectory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy lifecycle namespace still exists after migration: %v", err)
	}
}
