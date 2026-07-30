package catalog

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMultipleConfigsShareResourceUntilLastReferenceIsRemoved(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	firstPath := filepath.Join(project, "first.yml")
	secondPath := filepath.Join(project, "second.yml")
	for _, path := range []string{firstPath, secondPath} {
		if err := os.WriteFile(path, []byte("provider: test\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Date(2026, 7, 29, 1, 2, 3, 0, time.UTC)
	store, err := Open(filepath.Join(root, "catalog"))
	if err != nil {
		t.Fatal(err)
	}
	value, err := store.WithLock(now, func(value *Catalog) error {
		first, err := RegisterConfig(value, project, firstPath, now)
		if err != nil {
			return err
		}
		second, err := RegisterConfig(value, project, secondPath, now)
		if err != nil {
			return err
		}
		resource := Resource{BackendID: "docker:test", Kind: "docker-image", Locator: "image:test", Identity: "sha256:abc", Custody: CustodyGenerated, State: StateCurrent, CreatedAt: now}
		resource.Key = ResourceKey(resource.BackendID, resource.Kind, resource.Identity)
		resource.References = []Reference{{ConfigID: first.ID}, {ConfigID: second.ID}}
		return UpsertResource(value, resource)
	})
	if err != nil {
		t.Fatal(err)
	}
	if value.Configs[0].InstallationID == "" || value.Configs[0].InstallationID != value.Configs[1].InstallationID {
		t.Fatalf("configs in one project did not share an installation identity: %#v", value.Configs)
	}
	key := value.Resources[0].Key
	firstID, _ := ConfigID(project, firstPath)
	ReplaceConfigReferences(&value, firstID, nil, now.Add(time.Minute))
	if got := len(value.Resources[0].References); got != 1 {
		t.Fatalf("references after first removal = %d, want 1", got)
	}
	secondID, _ := ConfigID(project, secondPath)
	ReplaceConfigReferences(&value, secondID, nil, now.Add(2*time.Minute))
	if value.Resources[0].State != StateSuperseded || value.Resources[0].SupersededAt == nil {
		t.Fatalf("resource %s was not superseded after its final reference was removed", key)
	}
}

func TestDifferentProjectRootsHaveDifferentInstallationIdentities(t *testing.T) {
	root := t.TempDir()
	now := time.Now().UTC()
	value := Catalog{InstallationID: "host-catalog"}
	var ids []string
	for _, name := range []string{"one", "two"} {
		project := filepath.Join(root, name)
		if err := os.MkdirAll(project, 0o755); err != nil {
			t.Fatal(err)
		}
		configPath := filepath.Join(project, "config.yml")
		if err := os.WriteFile(configPath, []byte("provider: test\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		record, err := RegisterConfig(&value, project, configPath, now)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, record.InstallationID)
	}
	if ids[0] == "" || ids[0] == ids[1] {
		t.Fatalf("different project roots share installation identity %q", ids[0])
	}
}

func TestBackendLocksAreSeparatedAndSerializeTheSameBackend(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.AcquireBackendLock(context.Background(), "docker:first")
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	secondBackend, err := store.AcquireBackendLock(context.Background(), "docker:second")
	if err != nil {
		t.Fatalf("different backend was unnecessarily blocked: %v", err)
	}
	if err := secondBackend.Close(); err != nil {
		t.Fatal(err)
	}
	waitContext, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if _, err := store.AcquireBackendLock(waitContext, "docker:first"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("same backend lock error = %v, want context deadline", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	reacquired, err := store.AcquireBackendLock(context.Background(), "docker:first")
	if err != nil {
		t.Fatalf("released backend lock could not be reacquired: %v", err)
	}
	if err := reacquired.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCompactDropsMissingConfigsResourcesAndCompletedJournals(t *testing.T) {
	root := t.TempDir()
	now := time.Now().UTC()
	value := Catalog{
		SchemaVersion: SchemaVersion,
		Configs:       []Config{{ID: "gone", Path: filepath.Join(root, "gone.yml")}},
		Resources: []Resource{{
			Key: "missing", BackendID: "docker:test", Kind: "docker-image", Locator: "x", Identity: "y",
			Custody: CustodyGenerated, State: StateCurrent, References: []Reference{{ConfigID: "gone"}},
		}},
		Journals: []Journal{{ID: "done", Phase: "complete"}, {ID: "pending", Phase: "pull"}},
	}
	warnings := Compact(&value, now, func(Resource) (bool, error) { return false, nil })
	if len(warnings) != 0 || len(value.Configs) != 0 || len(value.Resources) != 0 || len(value.Journals) != 1 || value.Journals[0].ID != "pending" {
		t.Fatalf("unexpected compact result: %#v warnings=%v", value, warnings)
	}
}

func TestCompactPreservesMissingConfigWhileControllerLeaseIsActive(t *testing.T) {
	root := t.TempDir()
	now := time.Now().UTC()
	value := Catalog{
		SchemaVersion: SchemaVersion,
		Configs: []Config{{
			ID: "active", Path: filepath.Join(root, "removed.yml"), ControllerLeaseUntil: timePointer(now.Add(time.Minute)),
		}},
		Resources: []Resource{{
			Key: "present", BackendID: "docker:test", Kind: "docker-image", Locator: "x", Identity: "y",
			Custody: CustodyGenerated, State: StateCurrent, References: []Reference{{ConfigID: "active"}},
		}},
	}
	Compact(&value, now, func(Resource) (bool, error) { return true, nil })
	if len(value.Configs) != 1 || len(value.Resources) != 1 || len(value.Resources[0].References) != 1 {
		t.Fatalf("active controller lease did not preserve missing config reference: %#v", value)
	}
	Compact(&value, now.Add(2*time.Minute), func(Resource) (bool, error) { return true, nil })
	if len(value.Configs) != 0 || len(value.Resources[0].References) != 0 || value.Resources[0].State != StateSuperseded {
		t.Fatalf("expired controller lease still protected missing config: %#v", value)
	}
}

func TestRegisterConfigPreservesControllerLease(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "config.yml")
	if err := os.WriteFile(configPath, []byte("provider: test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	value := Catalog{}
	record, err := RegisterConfig(&value, root, configPath, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := RefreshControllerLease(&value, record.ID, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := RegisterConfig(&value, root, configPath, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if value.Configs[0].ControllerLeaseUntil == nil || !value.Configs[0].ControllerLeaseUntil.Equal(now.Add(time.Minute)) {
		t.Fatalf("registered config lost controller lease: %#v", value.Configs[0])
	}
	ReleaseControllerLease(&value, record.ID)
	if value.Configs[0].ControllerLeaseUntil != nil {
		t.Fatalf("controller lease was not released: %#v", value.Configs[0])
	}
}

func TestDefaultRootHonorsExplicitOverride(t *testing.T) {
	override := filepath.Join(t.TempDir(), "state")
	t.Setenv("EPAR_STATE_HOME", override)
	got, err := DefaultRoot()
	if err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.Abs(override)
	if got != want {
		t.Fatalf("DefaultRoot = %q, want %q", got, want)
	}
}

func TestRegisterConfigPreservesRegisteredCacheLimit(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "config.yml")
	if err := os.WriteFile(configPath, []byte("provider: test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	value := Catalog{}
	record, err := RegisterConfig(&value, root, configPath, now)
	if err != nil {
		t.Fatal(err)
	}
	value.Configs[0].BuildCacheLimitBytes = 20 << 30
	if _, err := RegisterConfig(&value, root, configPath, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if value.Configs[0].ID != record.ID || value.Configs[0].BuildCacheLimitBytes != 20<<30 {
		t.Fatalf("registered config lost persisted cache policy: %#v", value.Configs[0])
	}
}

func timePointer(value time.Time) *time.Time {
	return &value
}
