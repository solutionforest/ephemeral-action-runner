package pool

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
	storagecatalog "github.com/solutionforest/ephemeral-action-runner/internal/storage/catalog"
)

func TestPoolControllerLockConflictsForSameConfig(t *testing.T) {
	setPoolControllerLockStateHome(t)
	manager := testPoolControllerManager(t, t.TempDir(), "config.yml", "docker-container", "epar-lock-same")
	first, err := manager.AcquirePoolControllerLock()
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if second, err := manager.AcquirePoolControllerLock(); err == nil {
		_ = second.Close()
		t.Fatal("second controller acquired the same configuration lock")
	}
}

func TestPoolControllerLockConflictsWhenSameConfigChangesProviderOrPrefix(t *testing.T) {
	setPoolControllerLockStateHome(t)
	projectRoot := t.TempDir()
	firstManager := testPoolControllerManager(t, projectRoot, "config.yml", "docker-container", "epar-lock-before")
	secondManager := testPoolControllerManager(t, projectRoot, "config.yml", "docker-sandboxes", "epar-lock-after")
	first, err := firstManager.AcquirePoolControllerLock()
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if second, err := secondManager.AcquirePoolControllerLock(); err == nil {
		_ = second.Close()
		t.Fatal("in-place configuration mutation bypassed the configuration lock")
	}
}

func TestPoolControllerLockConflictsForSamePrefixAcrossConfigProviderAndProject(t *testing.T) {
	setPoolControllerLockStateHome(t)
	firstManager := testPoolControllerManager(t, t.TempDir(), "first.yml", "docker-container", "epar-lock-shared-prefix")
	secondManager := testPoolControllerManager(t, t.TempDir(), "second.yml", "docker-sandboxes", "epar-lock-shared-prefix")
	first, err := firstManager.AcquirePoolControllerLock()
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if second, err := secondManager.AcquirePoolControllerLock(); err == nil {
		_ = second.Close()
		t.Fatal("second controller acquired a shared pool prefix across projects/providers")
	} else if !strings.Contains(err.Error(), "pool.namePrefix") || !strings.Contains(err.Error(), "owner config=") {
		t.Fatalf("prefix conflict error = %v, want owner diagnostic", err)
	}
}

func TestPoolControllerLockAllowsDistinctConfigAndPrefix(t *testing.T) {
	setPoolControllerLockStateHome(t)
	projectRoot := t.TempDir()
	firstManager := testPoolControllerManager(t, projectRoot, "first.yml", "docker-container", "epar-lock-first")
	secondManager := testPoolControllerManager(t, projectRoot, "second.yml", "docker-sandboxes", "epar-lock-second")
	first, err := firstManager.AcquirePoolControllerLock()
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := secondManager.AcquirePoolControllerLock()
	if err != nil {
		t.Fatalf("independent pool identity was blocked: %v", err)
	}
	defer second.Close()
}

func TestPoolControllerLockReleaseAllowsReacquire(t *testing.T) {
	setPoolControllerLockStateHome(t)
	manager := testPoolControllerManager(t, t.TempDir(), "config.yml", "docker-container", "epar-lock-release")
	first, err := manager.AcquirePoolControllerLock()
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := manager.AcquirePoolControllerLock()
	if err != nil {
		t.Fatalf("reacquire after release: %v", err)
	}
	defer second.Close()
}

func TestPoolControllerLockOverwritesStaleMetadataAfterBothLocksAreAcquired(t *testing.T) {
	stateHome := setPoolControllerLockStateHome(t)
	manager := testPoolControllerManager(t, t.TempDir(), "config.yml", "docker-container", "epar-lock-stale")
	canonicalConfig, err := manager.canonicalPoolControllerConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	lockRoot := filepath.Join(stateHome, poolControllerLockDirectory)
	if err := os.MkdirAll(lockRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	stale := poolControllerLockOwner{ConfigPath: "stale.yml", Provider: "tart", NamePrefix: "stale-prefix", PID: 1, StartedAt: time.Unix(1, 0).UTC()}
	for _, path := range []string{
		poolControllerLockPath(lockRoot, "config", canonicalConfig),
		poolControllerLockPath(lockRoot, "prefix", "epar-lock-stale"),
	} {
		if err := os.WriteFile(path, mustJSON(t, stale), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	lock, err := manager.AcquirePoolControllerLock()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	owner, err := readPoolControllerLockOwner(poolControllerLockPath(lockRoot, "prefix", "epar-lock-stale"))
	if err != nil {
		t.Fatal(err)
	}
	if owner.ConfigPath != canonicalConfig || owner.NamePrefix != "epar-lock-stale" || owner.PID != os.Getpid() {
		t.Fatalf("stale metadata was not replaced after lock acquisition: %#v", owner)
	}
}

func TestPoolControllerLockRaceAllowsOneOwner(t *testing.T) {
	setPoolControllerLockStateHome(t)
	manager := testPoolControllerManager(t, t.TempDir(), "config.yml", "docker-container", "epar-lock-race")
	const contenders = 8
	locks := make(chan io.Closer, contenders)
	errs := make(chan error, contenders)
	var start sync.WaitGroup
	start.Add(1)
	for range contenders {
		go func() {
			start.Wait()
			lock, err := manager.AcquirePoolControllerLock()
			if err != nil {
				errs <- err
				return
			}
			locks <- lock
		}()
	}
	start.Done()
	var winners []io.Closer
	for range contenders {
		select {
		case lock := <-locks:
			winners = append(winners, lock)
		case <-errs:
		}
	}
	for _, lock := range winners {
		defer lock.Close()
	}
	if len(winners) != 1 {
		t.Fatalf("concurrent lock winners = %d, want 1", len(winners))
	}
}

func TestPoolControllerLockMigratesLifecycleStateOnlyAfterExclusiveOwnership(t *testing.T) {
	setPoolControllerLockStateHome(t)
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
	legacyConfig, err := filepath.Abs(configPath)
	if err != nil {
		t.Fatal(err)
	}
	legacyDirectory := lifecycleStateDirectory(project, filepath.Clean(legacyConfig))
	canonicalConfig, err := storagecatalog.CanonicalPath(configPath)
	if err != nil {
		t.Fatal(err)
	}
	canonicalDirectory := lifecycleStateDirectory(project, canonicalConfig)
	if legacyDirectory == canonicalDirectory {
		t.Fatal("test requires distinct legacy and canonical lifecycle namespaces")
	}
	if err := os.MkdirAll(legacyDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(legacyDirectory, "migration-marker")
	if err := os.WriteFile(marker, []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}

	blocker := testPoolControllerManager(t, project, configPath, "docker-container", "epar-lock-migration")
	held, err := blocker.AcquirePoolControllerLock()
	if err != nil {
		t.Fatal(err)
	}
	candidate := testPoolControllerManager(t, project, configPath, "docker-container", "epar-lock-migration")
	candidate.LifecycleStateEnabled = true
	if lock, err := candidate.AcquirePoolControllerLock(); err == nil {
		_ = lock.Close()
		t.Fatal("candidate acquired controller locks while blocker was active")
	}
	if candidate.LifecycleState != nil {
		t.Fatal("lifecycle state opened before controller locks were acquired")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("failed lock attempt moved legacy lifecycle state: %v", err)
	}
	if _, err := os.Stat(canonicalDirectory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed lock attempt created canonical lifecycle state: %v", err)
	}
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}

	lock, err := candidate.AcquirePoolControllerLock()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if candidate.LifecycleState == nil {
		t.Fatal("lifecycle state was not opened after acquiring controller locks")
	}
	if filepath.Dir(candidate.LifecycleState.Path()) != canonicalDirectory {
		t.Fatalf("lifecycle state path = %q, want canonical directory %q", candidate.LifecycleState.Path(), canonicalDirectory)
	}
	if _, err := os.Stat(filepath.Join(canonicalDirectory, "migration-marker")); err != nil {
		t.Fatalf("legacy lifecycle content was not migrated under the controller locks: %v", err)
	}
	if _, err := os.Stat(legacyDirectory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy lifecycle namespace still exists after locked migration: %v", err)
	}
}

func TestVerifyRequiresPoolControllerLock(t *testing.T) {
	setPoolControllerLockStateHome(t)
	manager := Manager{ProjectRoot: t.TempDir(), ConfigPath: "verify.yml", Config: config.Config{Provider: config.ProviderConfig{Type: "docker-container", SourceImage: "image"}, Pool: config.PoolConfig{NamePrefix: "epar-lock-verify"}}, Provider: &fakeProvider{}}
	held, err := manager.AcquirePoolControllerLock()
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	err = manager.Verify(context.Background(), VerifyOptions{Instances: 1})
	if err == nil || !strings.Contains(err.Error(), "lock") {
		t.Fatalf("Verify() error = %v, want controller lock conflict", err)
	}
}

func TestCleanupRequiresPoolControllerLock(t *testing.T) {
	setPoolControllerLockStateHome(t)
	manager := Manager{ProjectRoot: t.TempDir(), ConfigPath: "cleanup.yml", Config: config.Config{Provider: config.ProviderConfig{Type: "docker-container"}, Pool: config.PoolConfig{NamePrefix: "epar-lock-cleanup"}}, Provider: &fakeProvider{}}
	held, err := manager.AcquirePoolControllerLock()
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	err = manager.Cleanup(context.Background())
	if err == nil || !strings.Contains(err.Error(), "lock") {
		t.Fatalf("Cleanup() error = %v, want controller lock conflict", err)
	}
}

func TestProvisionPoolRequiresPoolControllerLock(t *testing.T) {
	setPoolControllerLockStateHome(t)
	manager := Manager{ProjectRoot: t.TempDir(), ConfigPath: "provision.yml", Config: config.Config{Provider: config.ProviderConfig{Type: "docker-container", SourceImage: "image"}, Pool: config.PoolConfig{NamePrefix: "epar-lock-provision"}}, Provider: &fakeProvider{}}
	held, err := manager.AcquirePoolControllerLock()
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	_, err = manager.ProvisionPool(context.Background(), 1, false)
	if err == nil || !strings.Contains(err.Error(), "lock") {
		t.Fatalf("ProvisionPool() error = %v, want controller lock conflict", err)
	}
}

func setPoolControllerLockStateHome(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("EPAR_STATE_HOME", root)
	return root
}

func testPoolControllerManager(t *testing.T, projectRoot, configName, providerType, prefix string) Manager {
	t.Helper()
	manager := Manager{
		ProjectRoot: projectRoot,
		ConfigPath:  configName,
		Config: config.Config{
			Provider: config.ProviderConfig{Type: providerType},
			Pool:     config.PoolConfig{NamePrefix: prefix},
		},
	}
	if !filepath.IsAbs(configName) {
		manager.ConfigPath = filepath.Join(projectRoot, configName)
	}
	return manager
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	content, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return append(content, '\n')
}
