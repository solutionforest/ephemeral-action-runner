package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/projectlayout"
	"github.com/solutionforest/ephemeral-action-runner/internal/storage"
	storagecatalog "github.com/solutionforest/ephemeral-action-runner/internal/storage/catalog"
)

func TestStorageResetRemovesConfigStateAndReleasesSharedResource(t *testing.T) {
	stateRoot := filepath.Join(t.TempDir(), "host-state")
	t.Setenv("EPAR_STATE_HOME", stateRoot)
	projectRoot := t.TempDir()
	firstConfig := filepath.Join(projectRoot, ".local", "config.first.yml")
	secondConfig := filepath.Join(projectRoot, ".local", "config.second.yml")
	for _, path := range []string{firstConfig, secondConfig} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("provider:\n  type: docker-sandboxes\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Date(2026, time.August, 12, 3, 0, 0, 0, time.UTC)
	store, err := storagecatalog.Open(stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	var firstID, secondID string
	_, err = store.WithLock(now, func(value *storagecatalog.Catalog) error {
		first, err := storagecatalog.RegisterConfig(value, projectRoot, firstConfig, now)
		if err != nil {
			return err
		}
		second, err := storagecatalog.RegisterConfig(value, projectRoot, secondConfig, now)
		if err != nil {
			return err
		}
		firstID, secondID = first.ID, second.ID
		resource := storagecatalog.Resource{BackendID: "sandbox:test", Kind: "sandbox-template", Provider: "docker-sandboxes", Role: "runtime-template", Locator: "docker.io/library/epar-test:exact", Identity: "123456789abc", Fingerprint: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Custody: storagecatalog.CustodyGenerated, State: storagecatalog.StateCurrent, CreatedAt: now, LastSeenAt: now}
		resource.Key = storagecatalog.ResourceKey(resource.BackendID, resource.Kind, resource.Identity)
		resource.References = []storagecatalog.Reference{{ConfigID: first.ID, Role: "provider-artifact", UpdatedAt: now}, {ConfigID: second.ID, Role: "provider-artifact", UpdatedAt: now}}
		return storagecatalog.UpsertResource(value, resource)
	})
	if err != nil {
		t.Fatal(err)
	}
	cachePath := filepath.Join(projectlayout.CacheRoot(projectRoot), "image", firstID)
	if err := os.MkdirAll(cachePath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cachePath, "cache.bin"), []byte("cache"), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := buildStorageResetReport(projectRoot, firstConfig, now)
	if err != nil {
		t.Fatal(err)
	}
	if report.ApprovalHash == "" || report.Plan.RemovalCount != 1 || len(report.SharedResources) != 1 || report.SharedResources[0].Referenced[0] != secondID {
		t.Fatalf("reset report = %+v", report)
	}
	laterReport, err := buildStorageResetReport(projectRoot, firstConfig, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if laterReport.ApprovalHash != report.ApprovalHash {
		t.Fatalf("unchanged reset approval hash changed with wall clock: %s != %s", laterReport.ApprovalHash, report.ApprovalHash)
	}
	executor, err := newHostStorageExecutor(projectRoot)
	if err != nil {
		t.Fatal(err)
	}
	execution, err := storage.Execute(context.Background(), report.Plan, report.Plan.Hash, executor)
	if err != nil {
		t.Fatal(err)
	}
	if err := finalizeStorageResetCatalog(firstID, report.MissingResources, execution, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cachePath); !os.IsNotExist(err) {
		t.Fatalf("config cache remains after reset: %v", err)
	}
	value, err := store.Load(now.Add(2 * time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(value.Resources) != 1 || len(value.Resources[0].References) != 1 || value.Resources[0].References[0].ConfigID != secondID {
		t.Fatalf("shared catalog resource after reset = %+v", value.Resources)
	}
}

func TestStorageResetRejectsActiveControllerLease(t *testing.T) {
	stateRoot := filepath.Join(t.TempDir(), "host-state")
	t.Setenv("EPAR_STATE_HOME", stateRoot)
	projectRoot := t.TempDir()
	configPath := filepath.Join(projectRoot, ".local", "config.yml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte("provider:\n  type: docker-sandboxes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.August, 12, 3, 0, 0, 0, time.UTC)
	store, err := storagecatalog.Open(stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.WithLock(now, func(value *storagecatalog.Catalog) error {
		record, err := storagecatalog.RegisterConfig(value, projectRoot, configPath, now)
		if err != nil {
			return err
		}
		return storagecatalog.RefreshControllerLease(value, record.ID, now.Add(time.Minute))
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := buildStorageResetReport(projectRoot, configPath, now); err == nil {
		t.Fatal("storage reset accepted an active controller lease")
	}
}

func TestStorageResetForgetsAlreadyMissingExactResource(t *testing.T) {
	stateRoot := filepath.Join(t.TempDir(), "host-state")
	t.Setenv("EPAR_STATE_HOME", stateRoot)
	projectRoot := t.TempDir()
	configPath := filepath.Join(projectRoot, ".local", "config.yml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte("provider:\n  type: docker-sandboxes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.August, 12, 3, 0, 0, 0, time.UTC)
	store, err := storagecatalog.Open(stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	var configID string
	_, err = store.WithLock(now, func(value *storagecatalog.Catalog) error {
		record, err := storagecatalog.RegisterConfig(value, projectRoot, configPath, now)
		if err != nil {
			return err
		}
		configID = record.ID
		resource := storagecatalog.Resource{BackendID: "filesystem:test", Kind: "prebuilt-package-archive", Provider: "docker-sandboxes", Role: "verified-package-archive", Locator: filepath.Join(projectRoot, ".local", "cache", "missing"), Identity: "missing-directory-id", Fingerprint: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Custody: storagecatalog.CustodyAcquired, State: storagecatalog.StateCurrent, CreatedAt: now, LastSeenAt: now}
		resource.Key = storagecatalog.ResourceKey(resource.BackendID, resource.Kind, resource.Identity)
		resource.References = []storagecatalog.Reference{{ConfigID: record.ID, Role: "prebuilt-package", UpdatedAt: now}}
		return storagecatalog.UpsertResource(value, resource)
	})
	if err != nil {
		t.Fatal(err)
	}
	report, err := buildStorageResetReport(projectRoot, configPath, now)
	if err != nil {
		t.Fatal(err)
	}
	if report.Plan.RemovalCount != 0 || len(report.MissingResources) != 1 || report.ApprovalHash == "" {
		t.Fatalf("missing-resource reset report = %+v", report)
	}
	if err := finalizeStorageResetCatalog(configID, report.MissingResources, storage.ExecutionReport{PlanHash: report.Plan.Hash}, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	value, err := store.Load(now.Add(2 * time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(value.Resources) != 0 {
		t.Fatalf("already-missing resource remains cataloged: %+v", value.Resources)
	}
}
