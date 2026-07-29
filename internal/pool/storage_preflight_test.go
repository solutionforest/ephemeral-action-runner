package pool

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
	poolstate "github.com/solutionforest/ephemeral-action-runner/internal/pool/state"
	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
	"github.com/solutionforest/ephemeral-action-runner/internal/storage"
)

type insufficientStorage struct{}

func (insufficientStorage) StorageSnapshot(context.Context, provider.StorageRequest) (provider.StorageSnapshot, error) {
	return provider.StorageSnapshot{
		Surfaces: []storage.Surface{{
			ID:       "provider-backing",
			Provider: "docker-sandboxes",
			Kind:     storage.SurfaceHostFilesystem,
			Location: "test-backing",
			Capacity: storage.Capacity{Known: true, TotalBytes: 100 * storage.GiB, AvailableBytes: 30 * storage.GiB},
		}},
		Requirements: []storage.Requirement{{
			ID:               "instance-create-provider-backing",
			Provider:         "docker-sandboxes",
			SurfaceID:        "provider-backing",
			PeakBytes:        20 * storage.GiB,
			MinimumFreeBytes: 20 * storage.GiB,
		}},
	}, nil
}

func TestRunPoolCapacityRejectionDoesNotCleanupUncreatedInstance(t *testing.T) {
	t.Setenv("EPAR_INVOCATION", "start")
	state, err := poolstate.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeProvider{}
	manager := Manager{
		Config: config.Config{
			Provider: config.ProviderConfig{Type: "docker-sandboxes"},
			Pool:     config.PoolConfig{Instances: 1, NamePrefix: "epar-capacity"},
			Logging:  config.LoggingConfig{Directory: t.TempDir()},
			Storage:  config.StorageConfig{MinimumFree: "20GiB"},
		},
		Provider:       fake,
		Lifecycle:      provider.AdaptLegacy(fake),
		LifecycleState: state,
		Storage:        insufficientStorage{},
		ProjectRoot:    t.TempDir(),
	}

	err = manager.RunPool(context.Background(), RunOptions{Instances: 1, PoolLockHeld: true, HostTrustLockHeld: true})
	if err == nil || !strings.Contains(err.Error(), "not enough disk space to initialize the runner") {
		t.Fatalf("RunPool() error = %v, want capacity rejection", err)
	}
	for _, want := range []string{
		"Storage location: test-backing",
		"Available: 30.00 GiB",
		"Estimated operation growth: 20.00 GiB",
		"Free-space reserve: 20.00 GiB",
		"Required before starting: 40.00 GiB",
		"Additional space needed: 10.00 GiB",
		"./start storage prune --provider docker-sandboxes",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("RunPool() error = %q, want %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "32212254720") {
		t.Fatalf("RunPool() error contains raw byte count: %v", err)
	}
	if strings.Contains(err.Error(), "lifecycle record not found") {
		t.Fatalf("RunPool() appended cleanup error for an uncreated instance: %v", err)
	}
	if got := atomic.LoadInt32(&fake.stopCalls); got != 0 {
		t.Fatalf("provider stop calls = %d, want 0", got)
	}
	if got := atomic.LoadInt32(&fake.deleteCalls); got != 0 {
		t.Fatalf("provider delete calls = %d, want 0", got)
	}
	records, err := state.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 0 {
		t.Fatalf("lifecycle records = %+v, want none before capacity admission", records)
	}
}

func TestStorageOverrideContinuesOnlyStorageAdmission(t *testing.T) {
	manager := Manager{
		Config: config.Config{
			Provider: config.ProviderConfig{Type: "docker-sandboxes"},
			Storage:  config.StorageConfig{MinimumFree: "1GiB"},
		},
		Storage:                  insufficientStorage{},
		AllowInsufficientStorage: true,
		StorageOverrideCommand:   "./start --allow-insufficient-storage",
		ProjectRoot:              t.TempDir(),
	}
	if err := manager.preflightStorage("instance-create", 20*storage.GiB); err != nil {
		t.Fatalf("storage override rejected storage-only admission: %v", err)
	}
}
