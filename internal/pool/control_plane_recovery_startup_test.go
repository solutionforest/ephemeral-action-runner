package pool

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
	gh "github.com/solutionforest/ephemeral-action-runner/internal/github"
	"github.com/solutionforest/ephemeral-action-runner/internal/logging"
	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
)

type startupRecoveryLifecycle struct {
	*controlPlaneRecoveryLifecycle
}

func (lifecycle *startupRecoveryLifecycle) Inventory(ctx context.Context) ([]provider.InventoryItem, error) {
	if _, err := lifecycle.controlPlaneRecoveryLifecycle.Inventory(ctx); err != nil {
		return nil, err
	}
	return lifecycle.Lifecycle.Inventory(ctx)
}

func TestRunPoolStartsDuringUnreservedRecoveryCooldown(t *testing.T) {
	for _, failures := range []int{0, 2, 3} {
		name := "already healthy"
		if failures != 0 {
			name = "recovers between retries"
		}
		if failures == 3 {
			name = "cancelled while retrying"
		}
		t.Run(name, func(t *testing.T) {
			t.Setenv("EPAR_STATE_HOME", t.TempDir())
			synctest.Test(t, func(t *testing.T) {
				started := time.Now()
				deadline := started.Add(30 * time.Minute)
				ledger, err := provider.OpenControlPlaneRecoveryLedger()
				if err != nil {
					t.Fatal(err)
				}
				if _, err := ledger.Update(context.Background(), func(state *provider.ControlPlaneRecoveryState) error {
					state.NextAttemptAt = deadline
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				p := &fakeProvider{ip: "127.0.0.1"}
				lifecycle := &startupRecoveryLifecycle{&controlPlaneRecoveryLifecycle{Lifecycle: provider.AdaptLegacy(p)}}
				for range failures {
					lifecycle.inventoryErrs = append(lifecycle.inventoryErrs, provider.NewControlPlaneFailure("inventory Docker Sandboxes", errors.New("not signed in; run sbx login")))
				}
				var onlineAfter time.Duration
				github := &fakeGitHub{waitFunc: func(_ context.Context, name string, _ time.Duration) (gh.Runner, error) {
					onlineAfter = time.Since(started)
					return gh.Runner{Name: name, ID: 123, Status: "online"}, nil
				}}
				manager := newRegisteredTestManager(t, p, github)
				manager.Config.Provider.Type = "docker-sandboxes"
				manager.Config.DockerSandboxes.RecoveryMode = config.DockerSandboxesRecoveryModeExclusiveAuto
				manager.Lifecycle = lifecycle
				manager.providerRecoveryLedger = ledger
				var console bytes.Buffer
				runtime, err := logging.NewRuntime(logging.Options{Directory: manager.Config.Logging.Directory, ManagerSinks: logging.SinkConsole, Stdout: &console, Stderr: &console})
				if err != nil {
					t.Fatal(err)
				}
				defer runtime.Close()
				manager.Logging = runtime
				ctx, cancel := context.WithTimeout(context.Background(), 31*time.Second)
				defer cancel()
				if err := manager.RunPool(ctx, RunOptions{Instances: 1, Register: true, KeepOnExit: true, PoolLockHeld: true, MonitorInterval: 15 * time.Second}); err != nil {
					t.Fatal(err)
				}
				wantRunners := int32(1)
				if failures == 3 {
					wantRunners = 0
				}
				if got := atomic.LoadInt32(&github.waitOnlineIdleCalls); got != wantRunners {
					t.Fatalf("online/idle checks = %d, want %d before the cooldown expires; console: %s", got, wantRunners, &console)
				}
				if want := time.Duration(failures) * 15 * time.Second; wantRunners != 0 && onlineAfter != want {
					t.Fatalf("runner online after %s, want %s (immediate first check and paced retries)", onlineAfter, want)
				}
				if got := atomic.LoadInt32(&p.cloneCalls); got != wantRunners {
					t.Fatalf("created runners = %d, want %d", got, wantRunners)
				}
				if lifecycle.recoverCalls != 0 {
					t.Fatalf("daemon recovery calls = %d, want cooldown preserved", lifecycle.recoverCalls)
				}
				state, err := ledger.State(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				if !state.NextAttemptAt.Equal(deadline) || state.RecoveryReservationToken != 0 || state.Attempts != 0 {
					t.Fatalf("startup changed the intervention cooldown: %#v", state)
				}
				if failures != 0 && (!strings.Contains(console.String(), "retryIn=15s") || !strings.Contains(console.String(), "inventory Docker Sandboxes: provider control plane failure")) {
					t.Fatalf("startup retry did not report its interval and cause: %s", &console)
				}
			})
		})
	}
}

func TestStartupWaitsForActiveRecoveryReservation(t *testing.T) {
	for _, phase := range []provider.RecoveryReservationPhase{provider.RecoveryReservationReserved, provider.RecoveryReservationIntervening, provider.RecoveryReservationVerifying} {
		t.Run(string(phase), func(t *testing.T) {
			t.Setenv("EPAR_STATE_HOME", t.TempDir())
			synctest.Test(t, func(t *testing.T) {
				deadline := time.Now().Add(30 * time.Minute)
				ledger, err := provider.OpenControlPlaneRecoveryLedger()
				if err != nil {
					t.Fatal(err)
				}
				if _, err := ledger.Update(context.Background(), func(state *provider.ControlPlaneRecoveryState) error {
					state.Attempts = 1
					state.NextAttemptAt = deadline
					state.RecoveryReservationToken = 17
					state.RecoveryReservationPhase = phase
					state.RecoveryReservationExpiresAt = deadline
					state.ExpectedIdentities = map[string]string{}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				var console bytes.Buffer
				runtime, err := logging.NewRuntime(logging.Options{Directory: t.TempDir(), ManagerSinks: logging.SinkConsole, Stdout: &console, Stderr: &console})
				if err != nil {
					t.Fatal(err)
				}
				defer runtime.Close()
				lifecycle := &controlPlaneRecoveryLifecycle{}
				manager := Manager{Config: configForRecoveryHardeningTest(), Lifecycle: lifecycle, Logging: runtime, providerRecoveryLedger: ledger}
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				if err := manager.waitForProviderRecoveryWindow(ctx); !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("startup wait = %v, want cancellable reservation wait", err)
				}
				if lifecycle.inventoryCalls != 0 || lifecycle.recoverCalls != 0 {
					t.Fatalf("active reservation was bypassed: inventory=%d recovery=%d", lifecycle.inventoryCalls, lifecycle.recoverCalls)
				}
				if !strings.Contains(console.String(), string(phase)) || !strings.Contains(console.String(), deadline.In(time.Local).Format(time.RFC3339)) {
					t.Fatalf("reservation wait did not report its phase and deadline: %s", &console)
				}
			})
		})
	}
}
