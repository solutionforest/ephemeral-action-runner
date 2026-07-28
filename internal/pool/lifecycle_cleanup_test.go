package pool

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
	gh "github.com/solutionforest/ephemeral-action-runner/internal/github"
	poolstate "github.com/solutionforest/ephemeral-action-runner/internal/pool/state"
	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
)

func TestLifecycleCleanupRefusesRecreatedSameNameProviderInstance(t *testing.T) {
	store, err := poolstate.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const name = "epar-test-recreated"
	if _, err := store.Reserve(context.Background(), poolstate.CreateSpec{Name: name, ProviderType: "docker-container", GitHub: poolstate.GitHubIdentity{ExactName: name}}); err != nil {
		t.Fatal(err)
	}
	for _, transition := range []poolstate.Transition{
		{Action: poolstate.ActionCreateIntent},
		{Action: poolstate.ActionCreated, ProviderID: "docker:old-id", Receipt: poolstate.Receipt{Version: "v1", Payload: []byte(`{"providerId":"docker:old-id"}`)}},
		{Action: poolstate.ActionValidateIntent},
		{Action: poolstate.ActionValidated},
	} {
		if _, err := store.Transition(context.Background(), name, transition); err != nil {
			t.Fatal(err)
		}
	}
	fake := &fakeProvider{instances: []provider.Instance{{Name: name, ProviderID: "docker:new-id", State: "running"}}}
	manager := Manager{
		Config:         config.Config{Provider: config.ProviderConfig{Type: "docker-container"}, Pool: config.PoolConfig{NamePrefix: "epar-test"}, Logging: config.LoggingConfig{Directory: t.TempDir()}},
		Provider:       fake,
		Lifecycle:      provider.AdaptLegacy(fake),
		LifecycleState: store,
		ProjectRoot:    t.TempDir(),
	}
	err = manager.cleanupOwnedLifecycle(context.Background())
	if err == nil || !strings.Contains(err.Error(), "does not match recorded") {
		t.Fatalf("cleanup error = %v, want immutable identity mismatch", err)
	}
	if got := atomic.LoadInt32(&fake.deleteCalls); got != 0 {
		t.Fatalf("delete calls = %d, want 0", got)
	}
}

func TestLifecycleCleanupRefusesActiveLeaseBeforeSideEffects(t *testing.T) {
	store, err := poolstate.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const name = "epar-test-busy"
	if _, err := store.Reserve(context.Background(), poolstate.CreateSpec{Name: name, ProviderType: "docker-container", GitHub: poolstate.GitHubIdentity{ExactName: name}}); err != nil {
		t.Fatal(err)
	}
	for _, transition := range []poolstate.Transition{
		{Action: poolstate.ActionCreateIntent},
		{Action: poolstate.ActionCreated, ProviderID: "docker:busy-id", Receipt: poolstate.Receipt{Version: "v1", Payload: []byte(`{"providerId":"docker:busy-id"}`)}},
		{Action: poolstate.ActionValidateIntent},
		{Action: poolstate.ActionValidated},
	} {
		if _, err := store.Transition(context.Background(), name, transition); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.AcquireLease(context.Background(), name, poolstate.Lease{Purpose: "job", Holder: "controller-test", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	fake := &fakeProvider{instances: []provider.Instance{{Name: name, ProviderID: "docker:busy-id", State: "running"}}}
	manager := Manager{
		Config:         config.Config{Provider: config.ProviderConfig{Type: "docker-container"}, Pool: config.PoolConfig{NamePrefix: "epar-test"}, Logging: config.LoggingConfig{Directory: t.TempDir()}},
		Provider:       fake,
		Lifecycle:      provider.AdaptLegacy(fake),
		LifecycleState: store,
		ProjectRoot:    t.TempDir(),
	}
	err = manager.cleanupOwnedLifecycle(context.Background())
	if err == nil || !strings.Contains(err.Error(), "active job lease") {
		t.Fatalf("cleanup error = %v, want active lease protection", err)
	}
	if got := atomic.LoadInt32(&fake.deleteCalls); got != 0 {
		t.Fatalf("delete calls = %d, want 0", got)
	}
	record, err := store.Read(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	if record.Phase != poolstate.PhaseStandby {
		t.Fatalf("phase = %s, want %s", record.Phase, poolstate.PhaseStandby)
	}
}

func TestCleanupRecoversInterruptedProvisionLeaseAfterExclusiveLock(t *testing.T) {
	store, err := poolstate.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const name = "epar-test-interrupted"
	if _, err := store.Reserve(context.Background(), poolstate.CreateSpec{Name: name, ProviderType: "docker-container", GitHub: poolstate.GitHubIdentity{ExactName: name}}); err != nil {
		t.Fatal(err)
	}
	for _, transition := range []poolstate.Transition{
		{Action: poolstate.ActionCreateIntent},
		{Action: poolstate.ActionCreated, ProviderID: "docker:interrupted-id", Receipt: poolstate.Receipt{Version: "v1", Payload: []byte(`{"providerId":"docker:interrupted-id"}`)}},
		{Action: poolstate.ActionValidateIntent},
	} {
		if _, err := store.Transition(context.Background(), name, transition); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.AcquireLease(context.Background(), name, poolstate.Lease{Purpose: "provision", Holder: "controller", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	fake := &fakeProvider{instances: []provider.Instance{{Name: name, ProviderID: "docker:interrupted-id", State: "running"}}}
	projectRoot := t.TempDir()
	manager := Manager{
		Config:         config.Config{Provider: config.ProviderConfig{Type: "docker-container"}, Pool: config.PoolConfig{NamePrefix: "epar-test"}, Logging: config.LoggingConfig{Directory: t.TempDir()}},
		ConfigPath:     "interrupted.yml",
		Provider:       fake,
		Lifecycle:      provider.AdaptLegacy(fake),
		LifecycleState: store,
		ProjectRoot:    projectRoot,
	}

	if err := manager.Cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&fake.deleteCalls); got != 1 {
		t.Fatalf("delete calls = %d, want 1", got)
	}
	record, err := store.Read(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	if record.Phase != poolstate.PhaseTombstoned {
		t.Fatalf("phase = %s, want %s", record.Phase, poolstate.PhaseTombstoned)
	}
	if len(record.Leases) != 0 {
		t.Fatalf("leases = %+v, want none", record.Leases)
	}
}

func TestInterruptedProvisionRecoveryPreservesJobLease(t *testing.T) {
	manager, store, name := readyLifecycleManager(t)
	if _, err := store.Transition(context.Background(), name, poolstate.Transition{Action: poolstate.ActionJobStarted}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AcquireLease(context.Background(), name, poolstate.Lease{Purpose: "provision", Holder: "controller", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AcquireLease(context.Background(), name, poolstate.Lease{Purpose: "job", Holder: "github-42", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}

	if err := manager.recoverInterruptedProvisionLeases(context.Background()); err != nil {
		t.Fatal(err)
	}

	record, err := store.Read(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	if len(record.Leases) != 1 || record.Leases[0].Purpose != "job" || record.Leases[0].Holder != "github-42" {
		t.Fatalf("leases = %+v, want exact job lease only", record.Leases)
	}
}

func TestRemoteAbsenceReleasesExactJobLease(t *testing.T) {
	manager, store, name := readyLifecycleManager(t)
	if _, err := store.Transition(context.Background(), name, poolstate.Transition{Action: poolstate.ActionJobStarted}); err != nil {
		t.Fatal(err)
	}
	holder := "github-42"
	if _, err := store.AcquireLease(context.Background(), name, poolstate.Lease{Purpose: "job", Holder: holder, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}

	if err := manager.recordLifecycleRemoteAbsence(context.Background(), name); err != nil {
		t.Fatal(err)
	}

	record, err := store.Read(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	if record.Phase != poolstate.PhaseDraining {
		t.Fatalf("phase = %s, want %s", record.Phase, poolstate.PhaseDraining)
	}
	if lease, active := activeLifecycleLease(record.Leases, time.Now()); active {
		t.Fatalf("job lease remained active after exact remote absence: %+v", lease)
	}
}

func TestReconciliationPreservesStoppedBusyRunnerProtectedByJobLease(t *testing.T) {
	manager, store, name := readyLifecycleManager(t)
	if _, err := store.Transition(context.Background(), name, poolstate.Transition{Action: poolstate.ActionJobStarted}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AcquireLease(context.Background(), name, poolstate.Lease{Purpose: "job", Holder: "github-42", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	fake := &fakeProvider{instances: []provider.Instance{{Name: name, ProviderID: "docker:ready-id", State: "stopped"}}}
	github := &fakeGitHub{listRunners: []gh.Runner{{Name: name, ID: 42, Status: "offline", Busy: true}}}
	manager.Provider = fake
	manager.Lifecycle = provider.AdaptLegacy(fake)
	manager.GitHub = github

	active, err := manager.reconcilePhysicalPool(context.Background(), nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if active[name].Phase != LifecycleCleanupPending {
		t.Fatalf("phase = %s, want %s", active[name].Phase, LifecycleCleanupPending)
	}
	if got := atomic.LoadInt32(&fake.deleteCalls); got != 0 {
		t.Fatalf("provider delete calls = %d, want 0 while job lease is active", got)
	}
	if got := atomic.LoadInt32(&github.deleteCalls); got != 0 {
		t.Fatalf("GitHub delete calls = %d, want 0 while exact lifecycle cleanup is pending", got)
	}
}

func readyLifecycleManager(t *testing.T) (*Manager, *poolstate.Store, string) {
	t.Helper()
	store, err := poolstate.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const name = "epar-test-ready"
	if _, err := store.Reserve(context.Background(), poolstate.CreateSpec{Name: name, ProviderType: "docker-container", GitHub: poolstate.GitHubIdentity{ExactName: name}}); err != nil {
		t.Fatal(err)
	}
	for _, transition := range []poolstate.Transition{
		{Action: poolstate.ActionCreateIntent},
		{Action: poolstate.ActionCreated, ProviderID: "docker:ready-id", Receipt: poolstate.Receipt{Version: "v1", Payload: []byte(`{"providerId":"docker:ready-id"}`)}},
		{Action: poolstate.ActionValidateIntent},
		{Action: poolstate.ActionValidated},
		{Action: poolstate.ActionRegisterIntent},
		{Action: poolstate.ActionRegistered, RunnerID: 42},
	} {
		if _, err := store.Transition(context.Background(), name, transition); err != nil {
			t.Fatal(err)
		}
	}
	manager := &Manager{
		Config:         config.Config{Provider: config.ProviderConfig{Type: "docker-container"}, Pool: config.PoolConfig{NamePrefix: "epar-test"}, Logging: config.LoggingConfig{Directory: t.TempDir()}},
		LifecycleState: store,
		ProjectRoot:    t.TempDir(),
	}
	return manager, store, name
}
