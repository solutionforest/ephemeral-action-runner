package pool

import (
	"context"
	"strings"
	"testing"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
)

func TestPoolControllerLockConflictsForSameProviderAndPrefix(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	manager := Manager{ConfigPath: "config.yml", Config: config.Config{Provider: config.ProviderConfig{Type: "docker-dind"}, Pool: config.PoolConfig{NamePrefix: "epar-lock-same"}}}
	first, err := manager.AcquirePoolControllerLock()
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if second, err := manager.AcquirePoolControllerLock(); err == nil {
		_ = second.Close()
		t.Fatal("second controller acquired the same provider/prefix lock")
	}
}

func TestVerifyRequiresPoolControllerLock(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	manager := Manager{ProjectRoot: t.TempDir(), ConfigPath: "verify.yml", Config: config.Config{Provider: config.ProviderConfig{Type: "docker-dind", SourceImage: "image"}, Pool: config.PoolConfig{NamePrefix: "epar-lock-verify"}}, Provider: &fakeProvider{}}
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
	t.Setenv("LOCALAPPDATA", t.TempDir())
	manager := Manager{ProjectRoot: t.TempDir(), ConfigPath: "cleanup.yml", Config: config.Config{Provider: config.ProviderConfig{Type: "docker-dind"}, Pool: config.PoolConfig{NamePrefix: "epar-lock-cleanup"}}, Provider: &fakeProvider{}}
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
	t.Setenv("LOCALAPPDATA", t.TempDir())
	manager := Manager{ProjectRoot: t.TempDir(), ConfigPath: "provision.yml", Config: config.Config{Provider: config.ProviderConfig{Type: "docker-dind", SourceImage: "image"}, Pool: config.PoolConfig{NamePrefix: "epar-lock-provision"}}, Provider: &fakeProvider{}}
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

func TestPoolControllerLockIsIndependentAcrossPoolIdentity(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	firstManager := Manager{ConfigPath: "first.yml", Config: config.Config{Provider: config.ProviderConfig{Type: "docker-dind"}, Pool: config.PoolConfig{NamePrefix: "epar-lock-first"}}}
	secondManager := Manager{ConfigPath: "second.yml", Config: config.Config{Provider: config.ProviderConfig{Type: "docker-dind"}, Pool: config.PoolConfig{NamePrefix: "epar-lock-second"}}}
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

func TestPoolControllerLockIncludesCanonicalConfigIdentity(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	firstManager := Manager{ConfigPath: "first.yml", Config: config.Config{Provider: config.ProviderConfig{Type: "docker-dind"}, Pool: config.PoolConfig{NamePrefix: "epar-lock-shared-prefix"}}}
	secondManager := Manager{ConfigPath: "second.yml", Config: config.Config{Provider: config.ProviderConfig{Type: "docker-dind"}, Pool: config.PoolConfig{NamePrefix: "epar-lock-shared-prefix"}}}
	first, err := firstManager.AcquirePoolControllerLock()
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := secondManager.AcquirePoolControllerLock()
	if err != nil {
		t.Fatalf("distinct canonical config identity was blocked: %v", err)
	}
	defer second.Close()
}
