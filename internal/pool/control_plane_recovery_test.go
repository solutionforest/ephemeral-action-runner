package pool

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
)

type controlPlaneRecoveryLifecycle struct {
	provider.Lifecycle

	mu                           sync.Mutex
	inventoryErrs                []error
	inventoryItems               []provider.InventoryItem
	inventorySets                [][]provider.InventoryItem
	inventoryIndex               int
	inventoryCalls               int
	recoverCalls                 int
	request                      provider.ControlPlaneRecoveryRequest
	recoverErr                   error
	coordinateCalls              int
	coordinateErr                error
	coordinateEntered            chan struct{}
	coordinateRelease            <-chan struct{}
	coordinationActive           bool
	inventoryOutsideCoordination int
	recoveryOutsideCoordination  int
	coordinateExitInventoryCalls int
	coordinateExitRecoveryCalls  int
	recoverHook                  func()
	absenceResults               map[string]bool
	absenceErrors                map[string]error
	absenceCalls                 []provider.Instance
	absenceDeadlines             []time.Time
	absenceOutsideCoordination   int
	absenceWithoutLeaseMarker    int
}

type uncoordinatedControlPlaneRecoveryLifecycle struct {
	provider.Lifecycle
	recoverCalls int
}

func (lifecycle *uncoordinatedControlPlaneRecoveryLifecycle) RecoverControlPlane(context.Context, provider.ControlPlaneRecoveryRequest) error {
	lifecycle.recoverCalls++
	return nil
}

func (lifecycle *controlPlaneRecoveryLifecycle) Inventory(context.Context) ([]provider.InventoryItem, error) {
	lifecycle.mu.Lock()
	defer lifecycle.mu.Unlock()
	lifecycle.inventoryCalls++
	if !lifecycle.coordinationActive {
		lifecycle.inventoryOutsideCoordination++
	}
	if len(lifecycle.inventoryErrs) != 0 {
		err := lifecycle.inventoryErrs[0]
		lifecycle.inventoryErrs = lifecycle.inventoryErrs[1:]
		if err != nil {
			return nil, err
		}
	}
	if len(lifecycle.inventorySets) != 0 {
		index := lifecycle.inventoryIndex
		if index >= len(lifecycle.inventorySets) {
			index = len(lifecycle.inventorySets) - 1
		}
		lifecycle.inventoryIndex++
		return append([]provider.InventoryItem(nil), lifecycle.inventorySets[index]...), nil
	}
	return append([]provider.InventoryItem(nil), lifecycle.inventoryItems...), nil
}

func (lifecycle *controlPlaneRecoveryLifecycle) RecoverControlPlane(_ context.Context, request provider.ControlPlaneRecoveryRequest) error {
	lifecycle.mu.Lock()
	lifecycle.recoverCalls++
	if !lifecycle.coordinationActive {
		lifecycle.recoveryOutsideCoordination++
	}
	lifecycle.request = request
	err := lifecycle.recoverErr
	hook := lifecycle.recoverHook
	lifecycle.mu.Unlock()
	if hook != nil {
		hook()
	}
	return err
}

func (lifecycle *controlPlaneRecoveryLifecycle) VerifyControlPlaneIdentityAbsent(ctx context.Context, instance provider.Instance) (bool, error) {
	lifecycle.mu.Lock()
	defer lifecycle.mu.Unlock()
	instance.Receipt = append([]byte(nil), instance.Receipt...)
	lifecycle.absenceCalls = append(lifecycle.absenceCalls, instance)
	deadline, _ := ctx.Deadline()
	lifecycle.absenceDeadlines = append(lifecycle.absenceDeadlines, deadline)
	if !lifecycle.coordinationActive {
		lifecycle.absenceOutsideCoordination++
	}
	if !provider.ControlPlaneRecoveryCoordinatorHeld(ctx) {
		lifecycle.absenceWithoutLeaseMarker++
	}
	return lifecycle.absenceResults[instance.Name], lifecycle.absenceErrors[instance.Name]
}

func (lifecycle *controlPlaneRecoveryLifecycle) CoordinateControlPlaneRecovery(ctx context.Context, operation func(context.Context) error) error {
	lifecycle.mu.Lock()
	lifecycle.coordinateCalls++
	coordinateErr := lifecycle.coordinateErr
	entered := lifecycle.coordinateEntered
	release := lifecycle.coordinateRelease
	lifecycle.mu.Unlock()
	if coordinateErr != nil {
		return coordinateErr
	}
	if entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
	}
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	lifecycle.mu.Lock()
	lifecycle.coordinationActive = true
	lifecycle.mu.Unlock()
	err := operation(provider.WithControlPlaneRecoveryCoordinator(ctx))
	lifecycle.mu.Lock()
	lifecycle.coordinationActive = false
	lifecycle.coordinateExitInventoryCalls = lifecycle.inventoryCalls
	lifecycle.coordinateExitRecoveryCalls = lifecycle.recoverCalls
	lifecycle.mu.Unlock()
	return err
}

func TestControlPlaneRecoveryDefaultsToExclusiveAuto(t *testing.T) {
	t.Setenv("EPAR_STATE_HOME", t.TempDir())
	lifecycle := &controlPlaneRecoveryLifecycle{
		inventoryErrs: []error{provider.NewControlPlaneFailure("inventory Docker Sandboxes", errors.New("wedged")), nil, nil, nil},
	}
	cfg := config.Default()
	cfg.Provider.Type = "docker-sandboxes"
	manager := Manager{Config: cfg, Lifecycle: lifecycle, ProjectRoot: t.TempDir()}

	handled, err := manager.recoverProviderControlPlane(context.Background(), provider.ErrControlPlaneFailure)
	if err != nil || !handled {
		t.Fatalf("recoverProviderControlPlane() = handled %t, error %v; want handled success", handled, err)
	}
	lifecycle.mu.Lock()
	defer lifecycle.mu.Unlock()
	if lifecycle.recoverCalls != 1 {
		t.Fatalf("recovery calls = %d, want 1", lifecycle.recoverCalls)
	}
	if lifecycle.request.Quiescence != time.Duration(config.DockerSandboxesDefaultRecoveryQuiescenceSeconds)*time.Second {
		t.Fatalf("recovery quiescence = %s, want %ds", lifecycle.request.Quiescence, config.DockerSandboxesDefaultRecoveryQuiescenceSeconds)
	}
	if lifecycle.inventoryCalls != 5 {
		t.Fatalf("inventory calls = %d, want one recheck, one host-wide census, plus three stable probes", lifecycle.inventoryCalls)
	}
}

func TestControlPlaneRecoveryFailsClosedWithoutProviderCoordinator(t *testing.T) {
	lifecycle := &uncoordinatedControlPlaneRecoveryLifecycle{}
	cfg := config.Default()
	cfg.Provider.Type = "docker-sandboxes"
	manager := Manager{Config: cfg, Lifecycle: lifecycle}

	handled, err := manager.recoverProviderControlPlane(context.Background(), provider.ErrControlPlaneAdmissionFailure)
	if !handled || err == nil || !strings.Contains(err.Error(), "does not implement host-wide recovery coordination") {
		t.Fatalf("uncoordinated recovery = handled %t error %v; want fail-closed coordinator error", handled, err)
	}
	if lifecycle.recoverCalls != 0 {
		t.Fatalf("uncoordinated provider recovery calls = %d, want zero", lifecycle.recoverCalls)
	}
}

func TestControlPlaneRecoveryCoordinatorLeaseCoversCensusReservationInterventionAndStableVerification(t *testing.T) {
	t.Setenv("EPAR_STATE_HOME", t.TempDir())
	ledger, err := provider.OpenControlPlaneRecoveryLedger()
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	type interventionObservation struct {
		state provider.ControlPlaneRecoveryState
		err   error
	}
	observed := make(chan interventionObservation, 1)
	lifecycle := &controlPlaneRecoveryLifecycle{
		coordinateEntered: entered,
		coordinateRelease: release,
	}
	lifecycle.recoverHook = func() {
		state, stateErr := ledger.State(context.Background())
		observed <- interventionObservation{state: state, err: stateErr}
	}
	cfg := config.Default()
	cfg.Provider.Type = "docker-sandboxes"
	manager := Manager{Config: cfg, Lifecycle: lifecycle, providerRecoveryLedger: ledger}
	type recoveryResult struct {
		handled bool
		err     error
	}
	done := make(chan recoveryResult, 1)
	go func() {
		handled, recoveryErr := manager.recoverProviderControlPlane(context.Background(), provider.ErrControlPlaneAdmissionFailure)
		done <- recoveryResult{handled: handled, err: recoveryErr}
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("recovery did not reach the provider coordinator")
	}
	lifecycle.mu.Lock()
	inventoryBeforeLease := lifecycle.inventoryCalls
	recoveryBeforeLease := lifecycle.recoverCalls
	lifecycle.mu.Unlock()
	if inventoryBeforeLease != 0 || recoveryBeforeLease != 0 {
		t.Fatalf("provider work before coordinator callback = inventory %d recovery %d, want zero", inventoryBeforeLease, recoveryBeforeLease)
	}
	before, err := ledger.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if before.RecoveryReservationToken != 0 || before.ExpectedIdentities != nil {
		t.Fatalf("recovery state before coordinator callback = %#v, want no census or reservation", before)
	}
	close(release)
	observation := <-observed
	if observation.err != nil {
		t.Fatal(observation.err)
	}
	if observation.state.RecoveryReservationPhase != provider.RecoveryReservationIntervening || observation.state.RecoveryReservationToken == 0 || observation.state.ExpectedIdentities == nil {
		t.Fatalf("durable state at provider intervention = %#v, want intervening reservation with published census", observation.state)
	}
	result := <-done
	if !result.handled || result.err != nil {
		t.Fatalf("coordinated recovery = handled %t error %v; want success", result.handled, result.err)
	}
	lifecycle.mu.Lock()
	defer lifecycle.mu.Unlock()
	if lifecycle.coordinateCalls != 1 || lifecycle.recoverCalls != 1 || lifecycle.inventoryCalls != providerRecoveryProbeCount+1 {
		t.Fatalf("coordinated calls = coordinator %d inventory %d recovery %d; want one lease, one census, one intervention, and %d stable probes", lifecycle.coordinateCalls, lifecycle.inventoryCalls, lifecycle.recoverCalls, providerRecoveryProbeCount)
	}
	if lifecycle.inventoryOutsideCoordination != 0 || lifecycle.recoveryOutsideCoordination != 0 {
		t.Fatalf("provider work outside coordinator = inventory %d recovery %d; want zero", lifecycle.inventoryOutsideCoordination, lifecycle.recoveryOutsideCoordination)
	}
	if lifecycle.coordinateExitInventoryCalls != lifecycle.inventoryCalls || lifecycle.coordinateExitRecoveryCalls != lifecycle.recoverCalls {
		t.Fatalf("coordinator exited before provider work completed: exit inventory/recovery=%d/%d final=%d/%d", lifecycle.coordinateExitInventoryCalls, lifecycle.coordinateExitRecoveryCalls, lifecycle.inventoryCalls, lifecycle.recoverCalls)
	}
}

func TestControlPlaneRecoveryBusyCoordinatorBacksOffWithoutCensusOrReservation(t *testing.T) {
	t.Setenv("EPAR_STATE_HOME", t.TempDir())
	ledger, err := provider.OpenControlPlaneRecoveryLedger()
	if err != nil {
		t.Fatal(err)
	}
	lifecycle := &controlPlaneRecoveryLifecycle{coordinateErr: provider.ErrControlPlaneRecoveryBusy}
	cfg := config.Default()
	cfg.Provider.Type = "docker-sandboxes"
	manager := Manager{Config: cfg, Lifecycle: lifecycle, providerRecoveryLedger: ledger}

	handled, recoveryErr := manager.recoverProviderControlPlane(context.Background(), provider.ErrControlPlaneAdmissionFailure)
	if !handled || recoveryErr != nil {
		t.Fatalf("busy coordinated recovery = handled %t error %v; want handled backoff", handled, recoveryErr)
	}
	state, err := ledger.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if state.RecoveryReservationToken != 0 || state.Attempts != 0 || len(state.ExpectedIdentities) != 0 || state.NextAttemptAt.IsZero() {
		t.Fatalf("busy coordinator state = %#v, want backoff without census or reservation", state)
	}
	lifecycle.mu.Lock()
	defer lifecycle.mu.Unlock()
	if lifecycle.coordinateCalls != 1 || lifecycle.inventoryCalls != 0 || lifecycle.recoverCalls != 0 {
		t.Fatalf("busy coordinator calls = coordinator %d inventory %d recovery %d; want one lock attempt and no callback work", lifecycle.coordinateCalls, lifecycle.inventoryCalls, lifecycle.recoverCalls)
	}
}

func TestControlPlaneAdmissionRecoveryBypassesHealthyInventoryProbe(t *testing.T) {
	t.Setenv("EPAR_STATE_HOME", t.TempDir())
	lifecycle := &controlPlaneRecoveryLifecycle{}
	cfg := config.Default()
	cfg.Provider.Type = "docker-sandboxes"
	manager := Manager{Config: cfg, Lifecycle: lifecycle}

	handled, err := manager.recoverProviderControlPlane(context.Background(), provider.ErrControlPlaneAdmissionFailure)
	if err != nil || !handled {
		t.Fatalf("recoverProviderControlPlane() = handled %t, error %v; want handled success", handled, err)
	}
	lifecycle.mu.Lock()
	defer lifecycle.mu.Unlock()
	if lifecycle.recoverCalls != 1 {
		t.Fatalf("recovery calls = %d, want 1", lifecycle.recoverCalls)
	}
	if lifecycle.inventoryCalls != providerRecoveryProbeCount+1 {
		t.Fatalf("inventory calls = %d, want one host-wide census plus %d post-recovery probes", lifecycle.inventoryCalls, providerRecoveryProbeCount)
	}
}

func TestControlPlaneAdmissionRecoveryObserveModeDoesNotMutate(t *testing.T) {
	lifecycle := &controlPlaneRecoveryLifecycle{}
	cfg := config.Default()
	cfg.Provider.Type = "docker-sandboxes"
	cfg.DockerSandboxes.RecoveryMode = config.DockerSandboxesRecoveryModeObserve
	manager := Manager{Config: cfg, Lifecycle: lifecycle}

	handled, err := manager.recoverProviderControlPlane(context.Background(), provider.ErrControlPlaneAdmissionFailure)
	if err != nil || handled {
		t.Fatalf("observe admission recovery = handled %t, error %v; want no automatic handling", handled, err)
	}
	lifecycle.mu.Lock()
	defer lifecycle.mu.Unlock()
	if lifecycle.recoverCalls != 0 || lifecycle.inventoryCalls != 0 {
		t.Fatalf("observe mode invoked recovery=%d inventory=%d; want both zero", lifecycle.recoverCalls, lifecycle.inventoryCalls)
	}
}

func TestControlPlaneAdmissionRecoveryRunsAtMostOnceUntilCreateSucceeds(t *testing.T) {
	t.Setenv("EPAR_STATE_HOME", t.TempDir())
	lifecycle := &controlPlaneRecoveryLifecycle{}
	cfg := config.Default()
	cfg.Provider.Type = "docker-sandboxes"
	manager := Manager{Config: cfg, Lifecycle: lifecycle}

	handled, err := manager.recoverProviderControlPlane(context.Background(), provider.ErrControlPlaneAdmissionFailure)
	if err != nil || !handled {
		t.Fatalf("first admission recovery = handled %t, error %v; want handled success", handled, err)
	}
	admissionToken := manager.providerAdmissionRecoveryToken
	handled, err = manager.recoverProviderControlPlane(context.Background(), provider.ErrControlPlaneAdmissionFailure)
	if err != nil || !handled {
		t.Fatalf("second admission recovery = handled %t, error %v; want handled backoff", handled, err)
	}
	lifecycle.mu.Lock()
	if lifecycle.recoverCalls != 1 {
		t.Fatalf("recovery calls after repeated admission failure = %d, want 1", lifecycle.recoverCalls)
	}
	if lifecycle.inventoryCalls != providerRecoveryProbeCount+1 {
		t.Fatalf("inventory calls after repeated admission failure = %d, want the first census and stable verification only", lifecycle.inventoryCalls)
	}
	lifecycle.mu.Unlock()

	if err := manager.resetProviderAdmissionRecoveryWithContext(context.Background(), admissionToken); err != nil {
		t.Fatalf("durable admission recovery rearm = %v", err)
	}
	manager.providerRecoveryNext = time.Time{}
	handled, err = manager.recoverProviderControlPlane(context.Background(), provider.ErrControlPlaneAdmissionFailure)
	if err != nil || !handled {
		t.Fatalf("re-armed admission recovery = handled %t, error %v; want handled success", handled, err)
	}
	lifecycle.mu.Lock()
	defer lifecycle.mu.Unlock()
	if lifecycle.recoverCalls != 2 {
		t.Fatalf("recovery calls after successful-create rearm = %d, want 2", lifecycle.recoverCalls)
	}
}

func TestControlPlaneAdmissionRecoveryBlocksInventoryRestartUntilCreateSucceeds(t *testing.T) {
	t.Setenv("EPAR_STATE_HOME", t.TempDir())
	lifecycle := &controlPlaneRecoveryLifecycle{}
	cfg := config.Default()
	cfg.Provider.Type = "docker-sandboxes"
	manager := Manager{Config: cfg, Lifecycle: lifecycle}

	if handled, err := manager.recoverProviderControlPlane(context.Background(), provider.ErrControlPlaneAdmissionFailure); err != nil || !handled {
		t.Fatalf("admission recovery = handled %t, error %v; want handled success", handled, err)
	}
	manager.providerRecoveryNext = time.Time{}
	if handled, err := manager.recoverProviderControlPlane(context.Background(), provider.ErrControlPlaneFailure); err != nil || !handled {
		t.Fatalf("inventory recovery during admission incident = handled %t, error %v; want handled backoff", handled, err)
	}
	lifecycle.mu.Lock()
	defer lifecycle.mu.Unlock()
	if lifecycle.recoverCalls != 1 {
		t.Fatalf("recovery calls after admission-to-inventory transition = %d, want 1", lifecycle.recoverCalls)
	}
}

func TestControlPlaneRecoveryObserveModeDoesNotRestart(t *testing.T) {
	lifecycle := &controlPlaneRecoveryLifecycle{}
	cfg := config.Default()
	cfg.Provider.Type = "docker-sandboxes"
	cfg.DockerSandboxes.RecoveryMode = config.DockerSandboxesRecoveryModeObserve
	manager := Manager{Config: cfg, Lifecycle: lifecycle}

	handled, err := manager.recoverProviderControlPlane(context.Background(), provider.ErrControlPlaneFailure)
	if err != nil || handled {
		t.Fatalf("observe recovery = handled %t, error %v; want no automatic handling", handled, err)
	}
	lifecycle.mu.Lock()
	defer lifecycle.mu.Unlock()
	if lifecycle.recoverCalls != 0 || lifecycle.inventoryCalls != 0 {
		t.Fatalf("observe mode invoked recovery=%d inventory=%d; want both zero", lifecycle.recoverCalls, lifecycle.inventoryCalls)
	}
}

func TestControlPlaneRecoveryBacksOffAfterFailedRecovery(t *testing.T) {
	t.Setenv("EPAR_STATE_HOME", t.TempDir())
	lifecycle := &controlPlaneRecoveryLifecycle{
		inventoryErrs: []error{provider.NewControlPlaneFailure("inventory Docker Sandboxes", errors.New("wedged"))},
		recoverErr:    errors.New("daemon stop could not confirm stopped"),
	}
	cfg := config.Default()
	cfg.Provider.Type = "docker-sandboxes"
	manager := Manager{Config: cfg, Lifecycle: lifecycle}

	handled, err := manager.recoverProviderControlPlane(context.Background(), provider.ErrControlPlaneFailure)
	if err != nil || !handled {
		t.Fatalf("recoverProviderControlPlane() = handled %t, error %v; want handled retry", handled, err)
	}
	if !manager.providerRecoveryNext.After(time.Now()) {
		t.Fatalf("provider recovery next attempt = %s, want future backoff", manager.providerRecoveryNext)
	}
	if manager.providerRecoveryTries != 1 {
		t.Fatalf("provider recovery tries = %d, want 1", manager.providerRecoveryTries)
	}
}

func TestControlPlaneRecoveryProviderCommandDeadlineRetainsVerificationReservation(t *testing.T) {
	t.Setenv("EPAR_STATE_HOME", t.TempDir())
	ledger, err := provider.OpenControlPlaneRecoveryLedger()
	if err != nil {
		t.Fatal(err)
	}
	lifecycle := &controlPlaneRecoveryLifecycle{
		inventoryErrs: []error{provider.NewControlPlaneFailure("inventory Docker Sandboxes", errors.New("wedged"))},
		recoverErr:    context.DeadlineExceeded,
	}
	cfg := config.Default()
	cfg.Provider.Type = "docker-sandboxes"
	manager := Manager{Config: cfg, Lifecycle: lifecycle, providerRecoveryLedger: ledger}

	handled, recoveryErr := manager.recoverProviderControlPlane(context.Background(), provider.ErrControlPlaneFailure)
	if !handled || recoveryErr != nil {
		t.Fatalf("provider-owned recovery deadline = handled %t error %v; want handled fail-closed retry", handled, recoveryErr)
	}
	state, err := ledger.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if state.RecoveryReservationPhase != provider.RecoveryReservationVerifying || state.RecoveryReservationToken == 0 || state.NextAttemptAt.IsZero() || state.ExpectedIdentities == nil {
		t.Fatalf("provider-owned recovery deadline state = %#v, want retained census and verification-only reservation", state)
	}
	lifecycle.mu.Lock()
	defer lifecycle.mu.Unlock()
	if lifecycle.recoverCalls != 1 || lifecycle.inventoryCalls != 2 {
		t.Fatalf("provider-owned recovery deadline calls = intervention %d inventory %d; want one recheck, one census, and one intervention", lifecycle.recoverCalls, lifecycle.inventoryCalls)
	}
}

func TestControlPlaneRecoveryCooldownDoesNotBlockSupervisor(t *testing.T) {
	now := time.Date(2026, 8, 20, 1, 0, 0, 0, time.UTC)
	lifecycle := &controlPlaneRecoveryLifecycle{}
	cfg := config.Default()
	cfg.Provider.Type = "docker-sandboxes"
	manager := Manager{Config: cfg, Lifecycle: lifecycle, now: func() time.Time { return now }}
	manager.providerRecoveryNext = now.Add(time.Hour)

	started := time.Now()
	handled, err := manager.recoverProviderControlPlane(context.Background(), provider.ErrControlPlaneFailure)
	if err != nil || !handled {
		t.Fatalf("cooldown recovery = handled %t, error %v; want handled without error", handled, err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("cooldown blocked supervisor for %s", elapsed)
	}
	lifecycle.mu.Lock()
	defer lifecycle.mu.Unlock()
	if lifecycle.inventoryCalls != 0 || lifecycle.recoverCalls != 0 {
		t.Fatalf("cooldown invoked inventory=%d recovery=%d; want both zero", lifecycle.inventoryCalls, lifecycle.recoverCalls)
	}
}

func TestProviderRecoveryBudgetCoversMaximumQuiescence(t *testing.T) {
	quiescence := 5 * time.Minute
	want := quiescence +
		(2 * providerRecoveryDaemonStop) +
		(3 * providerRecoveryDaemonReadback) +
		(time.Duration(providerRecoveryProbeCount) * providerRecoveryProbeTimeout) +
		(time.Duration(providerRecoveryProbeCount-1) * providerRecoveryProbeInterval) +
		providerRecoverySafetyMargin
	if got := providerRecoveryBudgetFor(quiescence); got != want || got <= quiescence {
		t.Fatalf("provider recovery budget = %s, want %s and greater than quiescence", got, want)
	}
}
