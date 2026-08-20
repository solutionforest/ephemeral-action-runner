package pool

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
)

type controlPlaneRecoveryLifecycle struct {
	provider.Lifecycle

	mu             sync.Mutex
	inventoryErrs  []error
	inventoryCalls int
	recoverCalls   int
	request        provider.ControlPlaneRecoveryRequest
	recoverErr     error
}

func (lifecycle *controlPlaneRecoveryLifecycle) Inventory(context.Context) ([]provider.InventoryItem, error) {
	lifecycle.mu.Lock()
	defer lifecycle.mu.Unlock()
	lifecycle.inventoryCalls++
	if len(lifecycle.inventoryErrs) == 0 {
		return nil, nil
	}
	err := lifecycle.inventoryErrs[0]
	lifecycle.inventoryErrs = lifecycle.inventoryErrs[1:]
	return nil, err
}

func (lifecycle *controlPlaneRecoveryLifecycle) RecoverControlPlane(_ context.Context, request provider.ControlPlaneRecoveryRequest) error {
	lifecycle.mu.Lock()
	defer lifecycle.mu.Unlock()
	lifecycle.recoverCalls++
	lifecycle.request = request
	return lifecycle.recoverErr
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
	if lifecycle.inventoryCalls != 4 {
		t.Fatalf("inventory calls = %d, want one recheck plus three stable probes", lifecycle.inventoryCalls)
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
