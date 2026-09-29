package pool

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
	gh "github.com/solutionforest/ephemeral-action-runner/internal/github"
	poolstate "github.com/solutionforest/ephemeral-action-runner/internal/pool/state"
	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
)

type deadlineIncidentLifecycle struct {
	*controlPlaneRecoveryLifecycle
	verifyErrs  []error
	verifyCalls []provider.Instance
}

func (lifecycle *deadlineIncidentLifecycle) VerifyControlPlaneIncident(ctx context.Context, instance provider.Instance) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	lifecycle.verifyCalls = append(lifecycle.verifyCalls, instance)
	if len(lifecycle.verifyErrs) == 0 {
		return nil
	}
	err := lifecycle.verifyErrs[0]
	lifecycle.verifyErrs = lifecycle.verifyErrs[1:]
	return err
}

type deadlineCleanupLifecycle struct {
	provider.Lifecycle
	stopCalls   int
	deleteCalls int
}

type leaseAwareIncidentLifecycle struct {
	provider.Lifecycle
	verify func(context.Context, provider.Instance) error
}

func (lifecycle *leaseAwareIncidentLifecycle) VerifyControlPlaneIncident(ctx context.Context, instance provider.Instance) error {
	return lifecycle.verify(ctx, instance)
}

func (lifecycle *deadlineCleanupLifecycle) Stop(ctx context.Context, _ provider.Instance) error {
	lifecycle.stopCalls++
	<-ctx.Done()
	return ctx.Err()
}

func (lifecycle *deadlineCleanupLifecycle) Delete(context.Context, provider.Instance) error {
	lifecycle.deleteCalls++
	return nil
}

func deadlineRecoveryTestManager(now *time.Time, lifecycle provider.Lifecycle) *Manager {
	cfg := config.Default()
	cfg.Provider.Type = "docker-sandboxes"
	cfg.Pool.NamePrefix = "epar-deadline"
	cfg.Pool.ReplacementRetryInitialSeconds = 15
	cfg.Pool.ReplacementRetryMaxSeconds = 1800
	cfg.Pool.ReplacementRetryMultiplier = 2
	cfg.Pool.ReplacementRetryJitterPercent = 0
	return &Manager{Config: cfg, Lifecycle: lifecycle, now: func() time.Time { return *now }}
}

func TestProviderCallerBudgetRequiresRepeatedObservationBeforeExactProbe(t *testing.T) {
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	lifecycle := &deadlineIncidentLifecycle{controlPlaneRecoveryLifecycle: &controlPlaneRecoveryLifecycle{}}
	manager := deadlineRecoveryTestManager(&now, lifecycle)
	state := &providerDeadlineRecoveryState{enabled: true}
	instance := provider.Instance{Name: "epar-deadline-one", ProviderID: "provider:one"}
	cause := newProviderCallerBudgetTimeout(instance, "stop exact provider instance")

	handled, err := manager.recoverProviderFailure(context.Background(), state, cause, nil, nil)
	if !handled || err != nil {
		t.Fatalf("first observation = handled %t error %v, want retained retry", handled, err)
	}
	if len(lifecycle.verifyCalls) != 0 {
		t.Fatalf("first observation ran %d independent probes, want zero", len(lifecycle.verifyCalls))
	}
	if errors.Is(cause, provider.ErrControlPlaneFailure) || errors.Is(cause, provider.ErrControlPlaneAdmissionFailure) {
		t.Fatalf("caller-owned deadline inherited provider recovery authorization: %v", cause)
	}

	now = now.Add(15 * time.Second)
	handled, err = manager.recoverProviderFailure(context.Background(), state, cause, nil, nil)
	if !handled || err != nil {
		t.Fatalf("second observation = handled %t error %v, want healthy exact-probe retry", handled, err)
	}
	if len(lifecycle.verifyCalls) != 1 || lifecycle.verifyCalls[0].Name != instance.Name || lifecycle.verifyCalls[0].ProviderID != instance.ProviderID {
		t.Fatalf("exact probes = %#v, want one immutable identity", lifecycle.verifyCalls)
	}
	if len(state.episodes) != 0 {
		t.Fatalf("healthy exact probe retained incident state: %#v", state.episodes)
	}
}

func TestProviderCallerBudgetInconclusiveProbeRetainsCapacityWithoutRecovery(t *testing.T) {
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	identityErr := errors.New("exact provider identity changed")
	base := &controlPlaneRecoveryLifecycle{}
	lifecycle := &deadlineIncidentLifecycle{controlPlaneRecoveryLifecycle: base, verifyErrs: []error{identityErr}}
	manager := deadlineRecoveryTestManager(&now, lifecycle)
	state := &providerDeadlineRecoveryState{enabled: true}
	instance := provider.Instance{Name: "epar-deadline-one", ProviderID: "provider:one"}
	cause := newProviderCallerBudgetTimeout(instance, "delete exact provider instance")

	if handled, err := manager.recoverProviderFailure(context.Background(), state, cause, nil, nil); !handled || err != nil {
		t.Fatalf("first observation = handled %t error %v", handled, err)
	}
	now = now.Add(15 * time.Second)
	if handled, err := manager.recoverProviderFailure(context.Background(), state, cause, nil, nil); !handled || err != nil {
		t.Fatalf("inconclusive observation = handled %t error %v", handled, err)
	}
	if len(lifecycle.verifyCalls) != 1 || base.recoverCalls != 0 {
		t.Fatalf("calls = probes %d recoveries %d, want one probe and no intervention", len(lifecycle.verifyCalls), base.recoverCalls)
	}
	episode := state.episodes[providerDeadlineIncidentKey{name: instance.Name, providerID: instance.ProviderID}]
	if episode == nil || episode.attempt != 2 || !episode.next.Equal(now.Add(30*time.Second)) {
		t.Fatalf("retained episode = %#v, want attempt 2 with capped retry progression", episode)
	}
}

func TestExactCommandPathFailureBypassesHealthyGlobalInventoryPrecheck(t *testing.T) {
	t.Setenv("EPAR_STATE_HOME", t.TempDir())
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	base := &controlPlaneRecoveryLifecycle{}
	lifecycle := &deadlineIncidentLifecycle{controlPlaneRecoveryLifecycle: base, verifyErrs: []error{provider.NewControlPlaneFailure("probe exact command path", errors.New("session unavailable"))}}
	manager := deadlineRecoveryTestManager(&now, lifecycle)
	manager.ProjectRoot = t.TempDir()
	state := &providerDeadlineRecoveryState{enabled: true}
	instance := provider.Instance{Name: "epar-deadline-one", ProviderID: "provider:one"}
	cause := newProviderCallerBudgetTimeout(instance, "delete exact provider instance")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	base.recoverHook = cancel

	if handled, err := manager.recoverProviderFailure(ctx, state, cause, nil, nil); !handled || err != nil {
		t.Fatalf("first observation = handled %t error %v", handled, err)
	}
	now = now.Add(15 * time.Second)
	handled, recoveryErr := manager.recoverProviderFailure(ctx, state, cause, nil, nil)
	if !handled || recoveryErr == nil || base.recoverCalls != 1 {
		t.Fatalf("confirmed exact incident = handled %t error %v recoveries %d, want one intervention before controlled cancellation", handled, recoveryErr, base.recoverCalls)
	}
	if base.inventoryOutsideCoordination != 0 {
		t.Fatalf("inventory outside recovery coordinator = %d, want no healthy-global-inventory short circuit", base.inventoryOutsideCoordination)
	}
}

func TestProviderCallerBudgetCancellationNeverProbesOrRecovers(t *testing.T) {
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	base := &controlPlaneRecoveryLifecycle{}
	lifecycle := &deadlineIncidentLifecycle{controlPlaneRecoveryLifecycle: base}
	manager := deadlineRecoveryTestManager(&now, lifecycle)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	handled, err := manager.recoverProviderFailure(ctx, &providerDeadlineRecoveryState{enabled: true}, newProviderCallerBudgetTimeout(provider.Instance{Name: "epar-deadline-one", ProviderID: "provider:one"}, "runner health process"), nil, nil)
	if handled || err != nil || len(lifecycle.verifyCalls) != 0 || base.recoverCalls != 0 {
		t.Fatalf("canceled recovery = handled %t error %v probes %d recoveries %d", handled, err, len(lifecycle.verifyCalls), base.recoverCalls)
	}
}

func TestProviderCallerBudgetRecoveryRequiresRegisteredReplacementSupervisor(t *testing.T) {
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	base := &controlPlaneRecoveryLifecycle{}
	lifecycle := &deadlineIncidentLifecycle{controlPlaneRecoveryLifecycle: base}
	manager := deadlineRecoveryTestManager(&now, lifecycle)
	handled, err := manager.recoverProviderFailure(context.Background(), &providerDeadlineRecoveryState{}, newProviderCallerBudgetTimeout(provider.Instance{Name: "epar-deadline-one", ProviderID: "provider:one"}, "stop exact provider instance"), nil, nil)
	if handled || err != nil || len(lifecycle.verifyCalls) != 0 || base.recoverCalls != 0 {
		t.Fatalf("disabled supervisor recovery = handled %t error %v probes %d recoveries %d", handled, err, len(lifecycle.verifyCalls), base.recoverCalls)
	}
}

func TestProviderIncidentProbeMaintainsHealthyPeerLease(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var writes atomic.Int32
		renewed := make(chan struct{})
		fake := &fakeProvider{execFunc: func(_ context.Context, name string, _ []string, opts provider.ExecOptions) (provider.ExecResult, error) {
			if name == "survivor" && strings.Contains(opts.Stdin, `"expiresAt"`) && writes.Add(1) == 4 {
				close(renewed)
			}
			return provider.ExecResult{}, nil
		}}
		github := &fakeGitHub{runner: gh.Runner{Name: "survivor", ID: 42, Status: "online"}, found: true}
		manager, active := keeperTestManager(t, fake, github)
		fake.instances = []provider.Instance{{Name: "survivor", ProviderID: "fake:survivor", State: "running"}}
		underlying := provider.AdaptLegacy(fake, false)
		manager.Lifecycle = &leaseAwareIncidentLifecycle{Lifecycle: underlying, verify: func(ctx context.Context, _ provider.Instance) error {
			select {
			case <-renewed:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}}
		manager.Config.Pool.ReplacementRetryInitialSeconds = 15
		manager.Config.Pool.ReplacementRetryJitterPercent = 0
		now := time.Now()
		manager.now = func() time.Time { return now }
		state := &providerDeadlineRecoveryState{enabled: true}
		failed := provider.Instance{Name: "failed", ProviderID: "fake:failed"}
		cause := newProviderCallerBudgetTimeout(failed, "stop exact provider instance")
		if handled, err := manager.recoverProviderFailure(context.Background(), state, cause, active, make(map[string]bool)); !handled || err != nil {
			t.Fatalf("first observation = handled %t error %v", handled, err)
		}
		now = now.Add(15 * time.Second)
		if handled, err := manager.recoverProviderFailure(context.Background(), state, cause, active, make(map[string]bool)); !handled || err != nil {
			t.Fatalf("verified observation = handled %t error %v", handled, err)
		}
		if writes.Load() < 4 || active["survivor"].Phase != LifecycleReady || atomic.LoadInt32(&github.deleteCalls) != 0 {
			t.Fatalf("peer lease maintenance = writes %d phase %s remoteDeletes %d", writes.Load(), active["survivor"].Phase, github.deleteCalls)
		}
	})
}

func TestExactCleanupStopsAfterCallerBudgetExpires(t *testing.T) {
	lifecycle := &deadlineCleanupLifecycle{}
	manager := &Manager{Lifecycle: lifecycle}
	instance := provider.Instance{Name: "epar-deadline-one", ProviderID: "provider:one"}
	record := poolstate.Record{Name: instance.Name, ProviderID: instance.ProviderID}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	err := manager.removeExactProviderInstance(ctx, record, []provider.InventoryItem{{Instance: instance}})
	failure, ok := asProviderCallerBudgetTimeout(err)
	if !ok || failure.instance.Name != instance.Name || failure.instance.ProviderID != instance.ProviderID {
		t.Fatalf("cleanup error = %T %v, want exact caller-budget outcome", err, err)
	}
	if lifecycle.stopCalls != 1 || lifecycle.deleteCalls != 0 {
		t.Fatalf("cleanup calls = stop %d delete %d, want stop only", lifecycle.stopCalls, lifecycle.deleteCalls)
	}
}
