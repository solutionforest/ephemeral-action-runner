package pool

import (
	"bytes"
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
	gh "github.com/solutionforest/ephemeral-action-runner/internal/github"
	"github.com/solutionforest/ephemeral-action-runner/internal/logging"
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

func TestLifecycleCleanupTombstonesIdentitylessQuarantineAfterExactAbsence(t *testing.T) {
	store, err := poolstate.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const name = "epar-test-identityless"
	if _, err := store.Reserve(context.Background(), poolstate.CreateSpec{Name: name, ProviderType: "docker-container", GitHub: poolstate.GitHubIdentity{ExactName: name}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Transition(context.Background(), name, poolstate.Transition{Action: poolstate.ActionQuarantine, Reason: "post-create identity was lost"}); err != nil {
		t.Fatal(err)
	}
	fake := &fakeProvider{}
	manager := Manager{
		Config:         config.Config{Provider: config.ProviderConfig{Type: "docker-container"}, Pool: config.PoolConfig{NamePrefix: "epar-test"}, Logging: config.LoggingConfig{Directory: t.TempDir()}},
		Provider:       fake,
		Lifecycle:      provider.AdaptLegacy(fake),
		LifecycleState: store,
		GitHub:         &fakeGitHub{},
		ProjectRoot:    t.TempDir(),
	}
	if err := manager.cleanupOwnedLifecycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	record, err := store.Read(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	if record.Phase != poolstate.PhaseTombstoned || record.Cleanup.RemoteAbsentAt == nil || record.Cleanup.LocalAbsentAt == nil {
		t.Fatalf("cleanup record = %#v, want exact absence tombstone", record)
	}
	if got := atomic.LoadInt32(&fake.deleteCalls); got != 0 {
		t.Fatalf("delete calls = %d, want no name-only deletion", got)
	}
}

func TestLifecycleCleanupKeepsIdentitylessQuarantineWhenSameNameExists(t *testing.T) {
	store, err := poolstate.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const name = "epar-test-identityless-present"
	if _, err := store.Reserve(context.Background(), poolstate.CreateSpec{Name: name, ProviderType: "docker-container", GitHub: poolstate.GitHubIdentity{ExactName: name}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Transition(context.Background(), name, poolstate.Transition{Action: poolstate.ActionQuarantine, Reason: "post-create identity was lost"}); err != nil {
		t.Fatal(err)
	}
	fake := &fakeProvider{instances: []provider.Instance{{Name: name, ProviderID: "docker:unknown-same-name", State: "running"}}}
	manager := Manager{
		Config:         config.Config{Provider: config.ProviderConfig{Type: "docker-container"}, Pool: config.PoolConfig{NamePrefix: "epar-test"}, Logging: config.LoggingConfig{Directory: t.TempDir()}},
		Provider:       fake,
		Lifecycle:      provider.AdaptLegacy(fake),
		LifecycleState: store,
		GitHub:         &fakeGitHub{},
		ProjectRoot:    t.TempDir(),
	}
	err = manager.cleanupOwnedLifecycle(context.Background())
	if err == nil || !strings.Contains(err.Error(), "same-name instance is quarantined") {
		t.Fatalf("cleanup error = %v, want same-name refusal", err)
	}
	record, readErr := store.Read(context.Background(), name)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if record.Phase != poolstate.PhaseQuarantined {
		t.Fatalf("phase = %s, want %s", record.Phase, poolstate.PhaseQuarantined)
	}
	if got := atomic.LoadInt32(&fake.deleteCalls); got != 0 {
		t.Fatalf("delete calls = %d, want no name-only deletion", got)
	}
}

func TestIdentitylessQuarantineAlwaysVerifiesGitHubAbsence(t *testing.T) {
	store, err := poolstate.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const name = "epar-test-identityless-remote"
	if _, err := store.Reserve(context.Background(), poolstate.CreateSpec{Name: name, ProviderType: "docker-container", GitHub: poolstate.GitHubIdentity{ExactName: name}}); err != nil {
		t.Fatal(err)
	}
	record, err := store.Transition(context.Background(), name, poolstate.Transition{Action: poolstate.ActionQuarantine, Reason: "post-create identity was lost"})
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeProvider{}
	manager := Manager{
		Config:         config.Config{Provider: config.ProviderConfig{Type: "docker-container"}, Pool: config.PoolConfig{NamePrefix: "epar-test"}, Logging: config.LoggingConfig{Directory: t.TempDir()}},
		Provider:       fake,
		Lifecycle:      provider.AdaptLegacy(fake),
		LifecycleState: store,
		GitHub:         &fakeGitHub{runner: gh.Runner{Name: name, ID: 9123}, found: true},
		ProjectRoot:    t.TempDir(),
	}
	err = manager.cleanupLifecycleRecordWithRemoteAbsence(context.Background(), record, nil, true)
	if err == nil || !strings.Contains(err.Error(), "same-name GitHub runner") {
		t.Fatalf("cleanup error = %v, want GitHub absence refusal", err)
	}
	record, readErr := store.Read(context.Background(), name)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if record.Phase != poolstate.PhaseQuarantined {
		t.Fatalf("phase = %s, want %s", record.Phase, poolstate.PhaseQuarantined)
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
	var console bytes.Buffer
	runtime, err := logging.NewRuntime(logging.Options{
		Directory:    t.TempDir(),
		ManagerSinks: logging.SinkConsole,
		Stdout:       &console,
		Stderr:       &console,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	manager.Logging = runtime
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
	if message := console.String(); !strings.Contains(message, "["+name+"] Job finished and GitHub released the ephemeral runner; GitHub Actions has the success or failure result.") {
		t.Fatalf("remote-absence lifecycle output omitted job completion: %q", message)
	}
}

func TestReconciliationLogsJobFinishBeforeExactCleanup(t *testing.T) {
	manager, store, name := readyLifecycleManager(t)
	if _, err := store.Transition(context.Background(), name, poolstate.Transition{Action: poolstate.ActionJobStarted}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AcquireLease(context.Background(), name, poolstate.Lease{Purpose: "job", Holder: "github-42", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	var console bytes.Buffer
	runtime, err := logging.NewRuntime(logging.Options{
		Directory:    t.TempDir(),
		ManagerSinks: logging.SinkConsole,
		Stdout:       &console,
		Stderr:       &console,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	fake := &fakeProvider{instances: []provider.Instance{{Name: name, ProviderID: "docker:ready-id", State: "running"}}}
	manager.Logging = runtime
	manager.Provider = fake
	manager.Lifecycle = provider.AdaptLegacy(fake)
	manager.GitHub = &fakeGitHub{}

	active, err := manager.reconcilePhysicalPool(context.Background(), nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 0 {
		t.Fatalf("active instances = %+v, want none after completed ephemeral runner cleanup", active)
	}
	output := console.String()
	finishedAt := strings.Index(output, "["+name+"] Job finished and GitHub released the ephemeral runner")
	cleanupAt := strings.Index(output, "cleanup: deleted exact owned instance "+name)
	if finishedAt < 0 || cleanupAt < 0 || finishedAt >= cleanupAt {
		t.Fatalf("job completion was not logged before exact cleanup: %q", output)
	}
}

func TestLifecycleJobObservationLogsStartAndFinishOnce(t *testing.T) {
	manager, store, name := readyLifecycleManager(t)
	var console bytes.Buffer
	runtime, err := logging.NewRuntime(logging.Options{
		Directory:    t.TempDir(),
		ManagerSinks: logging.SinkConsole,
		Stdout:       &console,
		Stderr:       &console,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	manager.Logging = runtime

	runner := gh.Runner{Name: name, ID: 42, Status: "online", Busy: true}
	if err := manager.recordLifecycleJobObservation(context.Background(), runner); err != nil {
		t.Fatal(err)
	}
	if err := manager.recordLifecycleJobObservation(context.Background(), runner); err != nil {
		t.Fatal(err)
	}
	runner.Busy = false
	if err := manager.recordLifecycleJobObservation(context.Background(), runner); err != nil {
		t.Fatal(err)
	}

	record, err := store.Read(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	if record.Phase != poolstate.PhaseDraining {
		t.Fatalf("phase = %s, want %s", record.Phase, poolstate.PhaseDraining)
	}
	output := console.String()
	if count := strings.Count(output, "["+name+"] Job started; GitHub assigned work to this runner."); count != 1 {
		t.Fatalf("job-start log count = %d, want 1: %q", count, output)
	}
	if count := strings.Count(output, "["+name+"] Job finished; GitHub Actions has the success or failure result."); count != 1 {
		t.Fatalf("job-finish log count = %d, want 1: %q", count, output)
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
