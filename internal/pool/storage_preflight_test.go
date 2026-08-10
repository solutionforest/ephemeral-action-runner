package pool

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
	"github.com/solutionforest/ephemeral-action-runner/internal/logging"
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
			Role:     storage.StorageRoleSandboxRuntime,
			Kind:     storage.SurfaceHostFilesystem,
			DomainID: "provider-domain",
			Location: "test-backing",
			Path:     "test-backing",
			Capacity: storage.Capacity{Known: true, TotalBytes: 100 * storage.GiB, AvailableBytes: 15 * storage.GiB},
		}},
		Domains: []storage.CapacityDomain{{
			ID:       "provider-domain",
			Kind:     storage.SurfaceHostFilesystem,
			Path:     "test-backing",
			Capacity: storage.Capacity{Known: true, TotalBytes: 100 * storage.GiB, AvailableBytes: 15 * storage.GiB},
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
		ConfigPath:     "config.sbx.yml",
	}

	err = manager.RunPool(context.Background(), RunOptions{Instances: 1, PoolLockHeld: true, HostTrustLockHeld: true})
	if err == nil || !strings.Contains(err.Error(), "not enough disk space to initialize the runner") {
		t.Fatalf("RunPool() error = %v, want capacity rejection", err)
	}
	for _, want := range []string{
		"Storage location: test-backing",
		"Available: 15.00 GiB",
		"Estimated operation growth: 10.00 GiB",
		"Free-space reserve: 20.00 GiB",
		"Required before starting: 30.00 GiB",
		"Additional space needed: 15.00 GiB",
		"./start storage prune --provider docker-sandboxes",
		"--config config.sbx.yml",
		"--project-root " + manager.ProjectRoot,
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

type failedStorageDiscovery struct{}

func (failedStorageDiscovery) StorageSnapshot(context.Context, provider.StorageRequest) (provider.StorageSnapshot, error) {
	return provider.StorageSnapshot{}, errors.New("discovery failed")
}

func TestStorageMeasurementHintPreservesConfigAndProjectRoot(t *testing.T) {
	t.Setenv("EPAR_INVOCATION", "start")
	manager := Manager{
		Config:                   config.Config{Provider: config.ProviderConfig{Type: "docker-sandboxes"}, Storage: config.StorageConfig{MinimumFree: "1GiB"}},
		Storage:                  failedStorageDiscovery{},
		ProjectRoot:              t.TempDir(),
		ConfigPath:               "config.sbx.yml",
		AllowInsufficientStorage: true,
	}
	err := manager.preflightStorage(manager.instanceCreateOperationPlan())
	if err == nil {
		t.Fatal("storage discovery failure was accepted")
	}
	want := manager.storageStatusCommand("instance-create")
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("measurement error = %q, want exact hint %q", err, want)
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
	if err := manager.preflightStorage(manager.instanceCreateOperationPlan()); err != nil {
		t.Fatalf("storage override rejected storage-only admission: %v", err)
	}
}

type fixedStorageSnapshot struct {
	snapshot provider.StorageSnapshot
}

func (fixed fixedStorageSnapshot) StorageSnapshot(context.Context, provider.StorageRequest) (provider.StorageSnapshot, error) {
	return fixed.snapshot, nil
}

func TestUnknownStorageContinuesWithoutOverrideAndWarnsWithCause(t *testing.T) {
	var console bytes.Buffer
	runtime, err := logging.NewRuntime(logging.Options{Directory: t.TempDir(), ManagerSinks: logging.SinkConsole, Stdout: &console, Stderr: &console})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	manager := Manager{
		Config: config.Config{Provider: config.ProviderConfig{Type: "docker-sandboxes"}, Storage: config.StorageConfig{MinimumFree: "1GiB"}},
		Storage: fixedStorageSnapshot{snapshot: provider.StorageSnapshot{
			Surfaces: []storage.Surface{{ID: "runtime", Provider: "docker-sandboxes", Role: storage.StorageRoleSandboxRuntime, Kind: storage.SurfaceDockerEngine, DomainID: "unknown-domain", Path: "/var/lib/docker", Capacity: storage.Capacity{}}},
			Domains:  []storage.CapacityDomain{{ID: "unknown-domain", Kind: storage.SurfaceDockerEngine, Path: "/var/lib/docker", CapacityUnavailableReason: "stat /var/lib/docker: no such file or directory", Capacity: storage.Capacity{}}},
		}},
		ProjectRoot: t.TempDir(),
		ConfigPath:  "config.sbx.yml",
		Logging:     runtime,
	}
	if err := manager.preflightStorage(manager.instanceCreateOperationPlan()); err != nil {
		t.Fatalf("unknown capacity rejected without override: %v", err)
	}
	output := console.String()
	for _, want := range []string{"STORAGE CAPACITY UNKNOWN", "operation=instance-create", "domain=unknown-domain", "roles=sandbox-runtime", "estimatedBytes=10737418240", "requiredBytes=", "stat /var/lib/docker", manager.storageStatusCommand("instance-create")} {
		if !strings.Contains(output, want) {
			t.Errorf("warning = %q, want %q", output, want)
		}
	}
}

func TestProjectProbeFailureWarnsWithOperationEstimate(t *testing.T) {
	var console bytes.Buffer
	runtime, err := logging.NewRuntime(logging.Options{Directory: t.TempDir(), ManagerSinks: logging.SinkConsole, Stdout: &console, Stderr: &console})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	manager := Manager{
		Config:      config.Config{Provider: config.ProviderConfig{Type: "docker-sandboxes"}, Storage: config.StorageConfig{MinimumFree: "1GiB"}},
		ProjectRoot: filepath.Join(t.TempDir(), "missing-project-root"),
		ConfigPath:  "config.sbx.yml",
		Logging:     runtime,
	}
	if err := manager.preflightStorage(manager.instanceCreateOperationPlan()); err != nil {
		t.Fatalf("project capacity probe failure rejected: %v", err)
	}
	output := console.String()
	for _, want := range []string{"STORAGE CAPACITY UNKNOWN", "operation=instance-create", "domain=project-domain", "roles=project", "estimatedBytes=10737418240", "requiredBytes=11811160064", "missing-project-root"} {
		if !strings.Contains(output, want) {
			t.Errorf("warning = %q, want %q", output, want)
		}
	}
}

func TestUnknownAndInsufficientStorageStillBlocksInsufficientDomain(t *testing.T) {
	plan := storage.OperationPlan{ID: "mixed", MinimumFreeBytes: storage.GiB, Phases: []storage.OperationPhase{{ID: "phase", Allocations: []storage.Allocation{
		{ID: "engine", Role: storage.StorageRoleDockerEngine, Bytes: storage.GiB},
		{ID: "runtime", Role: storage.StorageRoleSandboxRuntime, Bytes: 10 * storage.GiB},
	}}}}
	manager := Manager{
		Config: config.Config{Provider: config.ProviderConfig{Type: "docker-sandboxes"}, Storage: config.StorageConfig{MinimumFree: "1GiB"}},
		Storage: fixedStorageSnapshot{snapshot: provider.StorageSnapshot{
			Surfaces: []storage.Surface{
				{ID: "engine", Provider: "docker-sandboxes", Role: storage.StorageRoleDockerEngine, Kind: storage.SurfaceDockerEngine, DomainID: "unknown-domain", Path: "/var/lib/docker", Capacity: storage.Capacity{}},
				{ID: "runtime", Provider: "docker-sandboxes", Role: storage.StorageRoleSandboxRuntime, Kind: storage.SurfaceSandboxCache, DomainID: "small-domain", Path: "/runtime", Capacity: storage.Capacity{Known: true, TotalBytes: 20 * storage.GiB, AvailableBytes: 5 * storage.GiB}},
			},
			Domains: []storage.CapacityDomain{
				{ID: "unknown-domain", Kind: storage.SurfaceDockerEngine, Path: "/var/lib/docker", CapacityUnavailableReason: "guest path is not host-visible", Capacity: storage.Capacity{}},
				{ID: "small-domain", Kind: storage.SurfaceSandboxCache, Path: "/runtime", Capacity: storage.Capacity{Known: true, TotalBytes: 20 * storage.GiB, AvailableBytes: 5 * storage.GiB}},
			},
		}},
		ProjectRoot: t.TempDir(),
	}
	err := manager.preflightStorage(plan)
	if err == nil || !strings.Contains(err.Error(), "not enough disk space") || !strings.Contains(err.Error(), "/runtime") {
		t.Fatalf("mixed capacity error = %v", err)
	}
}
