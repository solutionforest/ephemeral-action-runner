package pool

import (
	"bytes"
	"context"
	"errors"
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

func TestLifecycleCleanupReconcilesJobLeaseAfterExactRemoteAbsence(t *testing.T) {
	manager, store, name := readyLifecycleManager(t)
	if _, err := store.Transition(context.Background(), name, poolstate.Transition{Action: poolstate.ActionJobStarted}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AcquireLease(context.Background(), name, poolstate.Lease{Purpose: "job", Holder: "github-42", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	fake := &fakeProvider{instances: []provider.Instance{{Name: name, ProviderID: "docker:ready-id", State: "running"}}}
	github := &fakeGitHub{}
	manager.Provider = fake
	manager.Lifecycle = provider.AdaptLegacy(fake)
	manager.GitHub = github

	if err := manager.cleanupOwnedLifecycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	record, err := store.Read(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	if record.Phase != poolstate.PhaseTombstoned {
		t.Fatalf("phase = %s, want %s", record.Phase, poolstate.PhaseTombstoned)
	}
	if lease, active := activeLifecycleLease(record.Leases, time.Now()); active {
		t.Fatalf("job lease remained active after exact remote absence: %+v", lease)
	}
	if got := atomic.LoadInt32(&github.deleteCalls); got != 0 {
		t.Fatalf("GitHub delete calls = %d, want 0 for an already-absent runner", got)
	}
	if got := atomic.LoadInt32(&fake.deleteCalls); got != 1 {
		t.Fatalf("provider delete calls = %d, want 1", got)
	}
}

func TestLifecycleCleanupReconcilesJobLeaseAfterExactRunnerBecomesIdle(t *testing.T) {
	manager, store, name := readyLifecycleManager(t)
	if _, err := store.Transition(context.Background(), name, poolstate.Transition{Action: poolstate.ActionJobStarted}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AcquireLease(context.Background(), name, poolstate.Lease{Purpose: "job", Holder: "github-42", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	fake := &fakeProvider{instances: []provider.Instance{{Name: name, ProviderID: "docker:ready-id", State: "running"}}}
	var deleted atomic.Bool
	github := &fakeGitHub{
		runnerByNameFunc: func(context.Context, string) (gh.Runner, bool, error) {
			if deleted.Load() {
				return gh.Runner{}, false, nil
			}
			return gh.Runner{Name: name, ID: 42, Status: "online", Busy: false}, true, nil
		},
		deleteFunc: func(context.Context, int64) error {
			deleted.Store(true)
			return nil
		},
	}
	manager.Provider = fake
	manager.Lifecycle = provider.AdaptLegacy(fake)
	manager.GitHub = github

	if err := manager.cleanupOwnedLifecycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	record, err := store.Read(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	if record.Phase != poolstate.PhaseTombstoned {
		t.Fatalf("phase = %s, want %s", record.Phase, poolstate.PhaseTombstoned)
	}
	if got := atomic.LoadInt32(&github.deleteCalls); got != 1 {
		t.Fatalf("GitHub delete calls = %d, want 1", got)
	}
	if got := atomic.LoadInt32(&fake.deleteCalls); got != 1 {
		t.Fatalf("provider delete calls = %d, want 1", got)
	}
}

func TestLifecycleCleanupPreservesJobLeaseWhileExactRunnerIsBusy(t *testing.T) {
	manager, store, name := readyLifecycleManager(t)
	if _, err := store.Transition(context.Background(), name, poolstate.Transition{Action: poolstate.ActionJobStarted}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AcquireLease(context.Background(), name, poolstate.Lease{Purpose: "job", Holder: "github-42", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	fake := &fakeProvider{instances: []provider.Instance{{Name: name, ProviderID: "docker:ready-id", State: "running"}}}
	github := &fakeGitHub{runner: gh.Runner{Name: name, ID: 42, Status: "online", Busy: true}, found: true}
	manager.Provider = fake
	manager.Lifecycle = provider.AdaptLegacy(fake)
	manager.GitHub = github

	err := manager.cleanupOwnedLifecycle(context.Background())
	if err == nil || !strings.Contains(err.Error(), "active job lease") {
		t.Fatalf("cleanup error = %v, want active lease protection", err)
	}
	record, readErr := store.Read(context.Background(), name)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if record.Phase != poolstate.PhaseBusy {
		t.Fatalf("phase = %s, want %s", record.Phase, poolstate.PhaseBusy)
	}
	if got := atomic.LoadInt32(&github.deleteCalls); got != 0 {
		t.Fatalf("GitHub delete calls = %d, want 0", got)
	}
	if got := atomic.LoadInt32(&fake.deleteCalls); got != 0 {
		t.Fatalf("provider delete calls = %d, want 0", got)
	}
}

func TestLifecycleCleanupRenewsExpiredJobLeaseWhileExactRunnerIsBusy(t *testing.T) {
	manager, store, name := readyLifecycleManager(t)
	observedAt := time.Now()
	if _, err := store.Transition(context.Background(), name, poolstate.Transition{Action: poolstate.ActionJobStarted}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AcquireLease(context.Background(), name, poolstate.Lease{Purpose: "job", Holder: "github-42", ExpiresAt: observedAt.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	fake := &fakeProvider{instances: []provider.Instance{{Name: name, ProviderID: "docker:ready-id", State: "running"}}}
	github := &fakeGitHub{runner: gh.Runner{Name: name, ID: 42, Status: "online", Busy: true}, found: true}
	manager.Provider = fake
	manager.Lifecycle = provider.AdaptLegacy(fake)
	manager.GitHub = github
	manager.now = func() time.Time { return observedAt.Add(2 * time.Minute) }

	err := manager.cleanupOwnedLifecycle(context.Background())
	if err == nil || !strings.Contains(err.Error(), "active job lease") {
		t.Fatalf("cleanup error = %v, want renewed active lease protection", err)
	}
	record, readErr := store.Read(context.Background(), name)
	if readErr != nil {
		t.Fatal(readErr)
	}
	lease, active := activeLifecycleLease(record.Leases, time.Now())
	if !active || lease.Purpose != "job" || lease.Holder != "github-42" {
		t.Fatalf("job lease = %+v active=%t, want renewed exact busy-runner lease", lease, active)
	}
	if got := atomic.LoadInt32(&github.deleteCalls); got != 0 {
		t.Fatalf("GitHub delete calls = %d, want 0", got)
	}
	if got := atomic.LoadInt32(&fake.deleteCalls); got != 0 {
		t.Fatalf("provider delete calls = %d, want 0", got)
	}
}

func TestLifecycleCleanupPreservesJobLeaseWhenGitHubStateIsUnavailable(t *testing.T) {
	manager, store, name := readyLifecycleManager(t)
	if _, err := store.Transition(context.Background(), name, poolstate.Transition{Action: poolstate.ActionJobStarted}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AcquireLease(context.Background(), name, poolstate.Lease{Purpose: "job", Holder: "github-42", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	fake := &fakeProvider{instances: []provider.Instance{{Name: name, ProviderID: "docker:ready-id", State: "running"}}}
	github := &fakeGitHub{runnerErr: context.DeadlineExceeded}
	manager.Provider = fake
	manager.Lifecycle = provider.AdaptLegacy(fake)
	manager.GitHub = github

	err := manager.cleanupOwnedLifecycle(context.Background())
	if err == nil || !strings.Contains(err.Error(), "verify job completion against exact GitHub runner") {
		t.Fatalf("cleanup error = %v, want exact GitHub verification failure", err)
	}
	if got := atomic.LoadInt32(&github.deleteCalls); got != 0 {
		t.Fatalf("GitHub delete calls = %d, want 0", got)
	}
	if got := atomic.LoadInt32(&fake.deleteCalls); got != 0 {
		t.Fatalf("provider delete calls = %d, want 0", got)
	}
}

func TestLifecycleCleanupPreservesJobLeaseOnGitHubIdentityDrift(t *testing.T) {
	manager, store, name := readyLifecycleManager(t)
	if _, err := store.Transition(context.Background(), name, poolstate.Transition{Action: poolstate.ActionJobStarted}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AcquireLease(context.Background(), name, poolstate.Lease{Purpose: "job", Holder: "github-42", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	fake := &fakeProvider{instances: []provider.Instance{{Name: name, ProviderID: "docker:ready-id", State: "running"}}}
	github := &fakeGitHub{runner: gh.Runner{Name: name, ID: 99, Status: "online"}, found: true}
	manager.Provider = fake
	manager.Lifecycle = provider.AdaptLegacy(fake)
	manager.GitHub = github

	err := manager.cleanupOwnedLifecycle(context.Background())
	if err == nil || !strings.Contains(err.Error(), "does not match recorded id=42") {
		t.Fatalf("cleanup error = %v, want exact GitHub identity mismatch", err)
	}
	if got := atomic.LoadInt32(&github.deleteCalls); got != 0 {
		t.Fatalf("GitHub delete calls = %d, want 0", got)
	}
	if got := atomic.LoadInt32(&fake.deleteCalls); got != 0 {
		t.Fatalf("provider delete calls = %d, want 0", got)
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
	busyRunner := gh.Runner{Name: name, ID: 42, Status: "offline", Busy: true}
	github := &fakeGitHub{runner: busyRunner, found: true, listRunners: []gh.Runner{busyRunner}}
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

func TestLifecycleCleanupRemovesPrefixOwnedOrphanAfterProviderProof(t *testing.T) {
	store, err := poolstate.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const name = "epar-test-orphan"
	item := provider.InventoryItem{
		Instance: provider.Instance{Name: name, ProviderID: "sandbox:orphan", State: "running", Source: "shell"},
		State:    "running",
		Source:   "shell",
		Workspaces: []string{
			"/tmp/epar-test-orphan",
		},
	}
	if _, err := store.ReportUnknown(context.Background(), poolstate.Discovery{
		ProviderType: "docker-sandboxes",
		ProviderID:   item.Instance.ProviderID,
		ExactName:    item.Instance.Name,
		Receipt:      poolstate.Receipt{Version: "v1", Payload: []byte(`{"state":"running"}`)},
	}); err != nil {
		t.Fatal(err)
	}
	lifecycle := &prefixOrphanLifecycle{items: []provider.InventoryItem{item}}
	var runnerDeleted atomic.Bool
	github := &fakeGitHub{
		runnerByNameFunc: func(context.Context, string) (gh.Runner, bool, error) {
			if runnerDeleted.Load() {
				return gh.Runner{}, false, nil
			}
			return gh.Runner{Name: name, ID: 73, Status: "offline"}, true, nil
		},
		deleteFunc: func(context.Context, int64) error {
			runnerDeleted.Store(true)
			return nil
		},
	}
	manager := Manager{
		Config: config.Config{
			Provider:        config.ProviderConfig{Type: "docker-sandboxes"},
			Pool:            config.PoolConfig{NamePrefix: "epar-test"},
			Logging:         config.LoggingConfig{Directory: t.TempDir()},
			DockerSandboxes: config.DockerSandboxesConfig{StagingRoot: ".local/cache/docker-sandboxes/staging"},
		},
		Lifecycle:      lifecycle,
		LifecycleState: store,
		GitHub:         github,
		ProjectRoot:    t.TempDir(),
	}

	if err := manager.cleanupOwnedLifecycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(lifecycle.items) != 0 {
		t.Fatalf("remaining inventory = %#v, want no orphan", lifecycle.items)
	}
	if lifecycle.deleteCalls != 1 || lifecycle.stopCalls != 1 {
		t.Fatalf("provider cleanup calls = stop %d delete %d, want one each", lifecycle.stopCalls, lifecycle.deleteCalls)
	}
	if got := atomic.LoadInt32(&github.deleteCalls); got != 1 {
		t.Fatalf("GitHub delete calls = %d, want 1", got)
	}
	discoveries, err := store.Discoveries(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(discoveries) != 0 {
		t.Fatalf("discoveries = %#v, want exact orphan discovery removed after readback", discoveries)
	}
	if lifecycle.preparedWorkspace == "" {
		t.Fatal("orphan cleanup did not request the configuration-derived workspace")
	}
}

func TestReconcileLocalInventoryCleansPrefixOwnedOrphanOnRestart(t *testing.T) {
	store, err := poolstate.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	item := provider.InventoryItem{
		Instance: provider.Instance{Name: "epar-test-restart-orphan", ProviderID: "sandbox:restart", State: "running", Source: "shell"},
		State:    "running",
		Source:   "shell",
	}
	lifecycle := &prefixOrphanLifecycle{items: []provider.InventoryItem{item}}
	manager := Manager{
		Config: config.Config{
			Provider:        config.ProviderConfig{Type: "docker-sandboxes"},
			Pool:            config.PoolConfig{NamePrefix: "epar-test"},
			Logging:         config.LoggingConfig{Directory: t.TempDir()},
			DockerSandboxes: config.DockerSandboxesConfig{StagingRoot: ".local/cache/docker-sandboxes/staging"},
		},
		Lifecycle:      lifecycle,
		LifecycleState: store,
		ProjectRoot:    t.TempDir(),
	}

	active, err := manager.reconcileLocalInventoryWithContext(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 0 {
		t.Fatalf("reconciled active instances = %#v, want orphan removed", active)
	}
	if len(lifecycle.items) != 0 || lifecycle.deleteCalls != 1 {
		t.Fatalf("restart orphan cleanup = remaining %#v, delete calls %d; want exact deletion", lifecycle.items, lifecycle.deleteCalls)
	}
}

func TestReconcileLocalInventoryDoesNotTreatIdentityMismatchAsPrefixOrphan(t *testing.T) {
	store, err := poolstate.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const name = "epar-test-rebound"
	if _, err := store.Reserve(context.Background(), poolstate.CreateSpec{
		Name:         name,
		ProviderType: "docker-sandboxes",
		GitHub:       poolstate.GitHubIdentity{ExactName: name},
	}); err != nil {
		t.Fatal(err)
	}
	item := provider.InventoryItem{
		Instance: provider.Instance{Name: name, ProviderID: "sandbox:new", State: "running", Source: "shell"},
		State:    "running",
		Source:   "shell",
	}
	lifecycle := &prefixOrphanLifecycle{items: []provider.InventoryItem{item}}
	manager := Manager{
		Config: config.Config{
			Provider:        config.ProviderConfig{Type: "docker-sandboxes"},
			Pool:            config.PoolConfig{NamePrefix: "epar-test"},
			Logging:         config.LoggingConfig{Directory: t.TempDir()},
			DockerSandboxes: config.DockerSandboxesConfig{StagingRoot: ".local/cache/docker-sandboxes/staging"},
		},
		Lifecycle:      lifecycle,
		LifecycleState: store,
		ProjectRoot:    t.TempDir(),
	}

	active, err := manager.reconcileLocalInventoryWithContext(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if lifecycle.deleteCalls != 0 {
		t.Fatalf("provider delete calls = %d, want 0 for same-name identity mismatch", lifecycle.deleteCalls)
	}
	if active[name].Phase != LifecycleQuarantined {
		t.Fatalf("phase = %s, want %s", active[name].Phase, LifecycleQuarantined)
	}
}

func TestLifecycleCleanupKeepsPrefixOrphanReportOnlyWithoutProviderProof(t *testing.T) {
	store, err := poolstate.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	item := provider.InventoryItem{
		Instance: provider.Instance{Name: "epar-test-unproven", ProviderID: "sandbox:unproven", State: "running", Source: "shell"},
		State:    "running",
		Source:   "shell",
	}
	lifecycle := &prefixOrphanLifecycle{items: []provider.InventoryItem{item}, prepareErr: errors.New("workspace proof unavailable")}
	manager := Manager{
		Config: config.Config{
			Provider:        config.ProviderConfig{Type: "docker-sandboxes"},
			Pool:            config.PoolConfig{NamePrefix: "epar-test"},
			Logging:         config.LoggingConfig{Directory: t.TempDir()},
			DockerSandboxes: config.DockerSandboxesConfig{StagingRoot: ".local/cache/docker-sandboxes/staging"},
		},
		Lifecycle:      lifecycle,
		LifecycleState: store,
		ProjectRoot:    t.TempDir(),
	}

	if err := manager.cleanupOwnedLifecycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if lifecycle.deleteCalls != 0 || len(lifecycle.items) != 1 {
		t.Fatalf("unproven orphan mutation = delete calls %d, remaining %d; want report-only", lifecycle.deleteCalls, len(lifecycle.items))
	}
	discoveries, err := store.Discoveries(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(discoveries) != 1 || discoveries[0].ExactName != item.Instance.Name {
		t.Fatalf("discoveries = %#v, want one retained discovery", discoveries)
	}
}

func TestResumePendingLifecycleCleanupTombstonesExactAbsentRecordOnRestart(t *testing.T) {
	manager, store, name := readyLifecycleManager(t)
	for _, transition := range []poolstate.Transition{
		{Action: poolstate.ActionFenceIntent},
		{Action: poolstate.ActionFenced},
		{Action: poolstate.ActionVerifyRemoteIntent},
		{Action: poolstate.ActionRemoteAbsent},
		{Action: poolstate.ActionRemoveLocalIntent},
		{Action: poolstate.ActionCleanupPending},
	} {
		if _, err := store.Transition(context.Background(), name, transition); err != nil {
			t.Fatal(err)
		}
	}
	lifecycle := &prefixOrphanLifecycle{}
	manager.Lifecycle = lifecycle

	if err := manager.resumePendingLifecycleCleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	record, err := store.Read(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	if record.Phase != poolstate.PhaseTombstoned {
		t.Fatalf("phase = %s, want %s after restart cleanup resumes exact absent record", record.Phase, poolstate.PhaseTombstoned)
	}
	if lifecycle.deleteCalls != 0 {
		t.Fatalf("provider delete calls = %d, want no provider mutation after exact inventory absence", lifecycle.deleteCalls)
	}
}

type prefixOrphanLifecycle struct {
	provider.Lifecycle
	items             []provider.InventoryItem
	prepareErr        error
	preparedWorkspace string
	stopCalls         int
	deleteCalls       int
}

func (lifecycle *prefixOrphanLifecycle) Inventory(context.Context) ([]provider.InventoryItem, error) {
	return append([]provider.InventoryItem(nil), lifecycle.items...), nil
}

func (lifecycle *prefixOrphanLifecycle) PrepareOrphanCleanup(_ context.Context, item provider.InventoryItem, expectedWorkspace string) (provider.Instance, error) {
	if lifecycle.prepareErr != nil {
		return provider.Instance{}, lifecycle.prepareErr
	}
	lifecycle.preparedWorkspace = expectedWorkspace
	instance := item.Instance
	instance.ReceiptVersion = "v1"
	instance.Receipt = []byte(`{"stagingPath":"/tmp/exact","stagingIdentity":"unix:1:2"}`)
	return instance, nil
}

func (lifecycle *prefixOrphanLifecycle) Stop(context.Context, provider.Instance) error {
	lifecycle.stopCalls++
	return nil
}

func (lifecycle *prefixOrphanLifecycle) Delete(_ context.Context, instance provider.Instance) error {
	lifecycle.deleteCalls++
	remaining := lifecycle.items[:0]
	for _, item := range lifecycle.items {
		if item.Instance.ProviderID != instance.ProviderID {
			remaining = append(remaining, item)
		}
	}
	lifecycle.items = remaining
	return nil
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
