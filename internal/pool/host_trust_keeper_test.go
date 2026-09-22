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
	"github.com/solutionforest/ephemeral-action-runner/internal/hosttrust"
	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
)

func keeperTestManager(t *testing.T, fake *fakeProvider, github *fakeGitHub) (*Manager, map[string]ProvisionedInstance) {
	t.Helper()
	oldInterval := hostTrustRefreshInterval
	hostTrustRefreshInterval = 5 * time.Millisecond
	t.Cleanup(func() { hostTrustRefreshInterval = oldInterval })
	m := newRegisteredTestManager(t, fake, github)
	m.Config.Image.HostTrustMode = config.HostTrustModeOverlay
	m.Config.Image.HostTrustScopes = []string{"system"}
	m.hostTrustResolver = func(context.Context) (hosttrust.Snapshot, error) {
		return hosttrust.Snapshot{Generation: "g1", HostOS: "linux", Scopes: []string{"system"}, Certificates: []hosttrust.Certificate{{Name: "root.crt", PEM: []byte("pem")}}, CollectedAt: time.Now().UTC()}, nil
	}
	return &m, map[string]ProvisionedInstance{"survivor": {Name: "survivor", ProviderID: "fake:survivor", RunnerID: 42, HostTrustGeneration: "g1", ProviderOwned: true, Phase: LifecycleReady}}
}

func waitKeeperSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for lease maintenance")
	}
}

func TestLeaseKeeperStopJoinsWriteWithoutCancelingOrFencing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		started, release, stopped := make(chan struct{}), make(chan struct{}), make(chan struct{})
		var writes atomic.Int32
		fake := &fakeProvider{execFunc: func(ctx context.Context, _ string, _ []string, opts provider.ExecOptions) (provider.ExecResult, error) {
			if strings.Contains(opts.Stdin, `"expiresAt"`) && writes.Add(1) == 1 {
				close(started)
				select {
				case <-ctx.Done():
					return provider.ExecResult{}, ctx.Err()
				case <-release:
				}
			}
			return provider.ExecResult{}, nil
		}}
		github := &fakeGitHub{runner: gh.Runner{Name: "survivor", ID: 42, Status: "online"}, found: true}
		m, active := keeperTestManager(t, fake, github)
		_, stop := m.startHostTrustLeaseKeeperForRunners(context.Background(), active, nil)
		defer stop(active)
		waitKeeperSignal(t, started)
		go func() { stop(active); close(stopped) }()
		// Let stop reach its join while the guest write is still blocked.
		synctest.Wait()
		select {
		case <-stopped:
			t.Fatal("stop returned before the in-flight lease write completed")
		default:
		}
		close(release)
		waitKeeperSignal(t, stopped)
		if atomic.LoadInt32(&github.deleteCalls) != 0 || active["survivor"].Phase != LifecycleReady {
			t.Fatal("normal keeper handoff fenced the survivor")
		}
	})
}

func TestLeaseKeeperPreservesBusyHandoffAcrossProvisioningPhases(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fake := &fakeProvider{}
		github := &fakeGitHub{runner: gh.Runner{Name: "survivor", ID: 42, Status: "online", Busy: true}, found: true}
		m, active := keeperTestManager(t, fake, github)
		handoffs := make(map[string]bool)
		for phase := 0; phase < 2; phase++ {
			observed := make(chan struct{}, 1)
			github.runnerByNameFunc = func(context.Context, string) (gh.Runner, bool, error) {
				select {
				case observed <- struct{}{}:
				default:
				}
				return github.runner, true, nil
			}
			_, stop := m.startHostTrustLeaseKeeperForRunners(context.Background(), active, handoffs)
			waitKeeperSignal(t, observed)
			stop(active)
		}
		if got := len(hostTrustLeaseInputs(fake)); got != 1 || !handoffs["survivor"] {
			t.Fatalf("busy lease writes=%d, handoffs=%v; want one handoff across both phases", got, handoffs)
		}
	})
}

func TestLeaseKeeperQuarantineIsStickyAndMergedByExactIdentity(t *testing.T) {
	for _, replaced := range []bool{false, true} {
		t.Run(map[bool]string{false: "same-identity", true: "new-identity"}[replaced], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				fenced := make(chan struct{}, 1)
				fake := &fakeProvider{execFunc: func(_ context.Context, _ string, _ []string, opts provider.ExecOptions) (provider.ExecResult, error) {
					if opts.Stdin != "" {
						return provider.ExecResult{}, errors.New("lease write unavailable")
					}
					return provider.ExecResult{}, nil
				}}
				github := &fakeGitHub{runner: gh.Runner{Name: "survivor", ID: 42}, found: true, deleteFunc: func(context.Context, int64) error {
					select {
					case fenced <- struct{}{}:
					default:
					}
					return nil
				}}
				m, active := keeperTestManager(t, fake, github)
				_, stop := m.startHostTrustLeaseKeeperForRunners(context.Background(), active, nil)
				defer stop(active)
				waitKeeperSignal(t, fenced)
				// Complete quarantine, then exercise several subsequent refreshes.
				synctest.Wait()
				time.Sleep(5 * hostTrustRefreshInterval)
				synctest.Wait()
				if replaced {
					instance := active["survivor"]
					instance.ProviderID = "fake:replacement"
					active["survivor"] = instance
				}
				stop(active)
				want := LifecycleQuarantined
				if replaced {
					want = LifecycleReady
				}
				if got := active["survivor"].Phase; got != want {
					t.Fatalf("phase=%s, want %s", got, want)
				}
				if got := len(hostTrustLeaseInputs(fake)); got != 1 {
					t.Fatalf("quarantined runner got %d writes; want one", got)
				}
			})
		})
	}
}

func TestReplacementProvisioningMaintainsSurvivorLeases(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Block the actual replacement creation path until multiple renewals occur.
		// The controller's synchronous monitor cannot perform these renewals itself.
		fake := &fakeProvider{}
		github := &fakeGitHub{runner: gh.Runner{Name: "survivor", ID: 42}, found: true}
		m, active := keeperTestManager(t, fake, github)
		var writes atomic.Int32
		renewed := make(chan struct{})
		fake.execFunc = func(_ context.Context, name string, _ []string, opts provider.ExecOptions) (provider.ExecResult, error) {
			if name == "survivor" && strings.Contains(opts.Stdin, `"expiresAt"`) && writes.Add(1) == 4 {
				close(renewed)
			}
			return provider.ExecResult{}, nil
		}
		wantErr := errors.New("end simulated slow replacement creation")
		m.Lifecycle = &partialCreateLifecycle{Lifecycle: provider.AdaptLegacy(fake, false), create: func() (provider.Instance, error) {
			select {
			case <-renewed:
				return provider.Instance{}, wantErr
			case <-time.After(3 * time.Second):
				return provider.Instance{}, errors.New("survivor lease maintenance stalled during replacement")
			}
		}}
		fake.instances = []provider.Instance{{Name: "survivor", ProviderID: "fake:survivor", State: "running"}}
		_, err := m.provisionWithHostTrustMaintenance(context.Background(), "candidate", true, true, active, make(map[string]bool))
		if !errors.Is(err, wantErr) {
			t.Fatalf("replacement returned %v, want controlled completion after renewals", err)
		}
		if writes.Load() < 4 || active["survivor"].Phase != LifecycleReady || atomic.LoadInt32(&github.deleteCalls) != 0 {
			t.Fatalf("survivor not maintained: writes=%d phase=%s remoteDeletes=%d", writes.Load(), active["survivor"].Phase, github.deleteCalls)
		}
		// Between initial/replacement attempts the controller may reconcile and
		// quarantine a survivor. A completed keeper must not revive it afterward.
		previousWrites := writes.Load()
		instance := active["survivor"]
		instance.Phase = LifecycleQuarantined
		active["survivor"] = instance
		m.Lifecycle = &partialCreateLifecycle{Lifecycle: provider.AdaptLegacy(fake, false), create: func() (provider.Instance, error) {
			time.Sleep(5 * hostTrustRefreshInterval)
			return provider.Instance{}, wantErr
		}}
		_, err = m.provisionWithHostTrustMaintenance(context.Background(), "candidate-2", true, true, active, make(map[string]bool))
		if !errors.Is(err, wantErr) || writes.Load() != previousWrites {
			t.Fatalf("quarantined survivor renewed in next provisioning phase: writes=%d->%d error=%v", previousWrites, writes.Load(), err)
		}
	})
}

func TestLeaseMaintenanceDeadlineStillQuarantinesAfterFailedWriteAndRevocation(t *testing.T) {
	for _, mode := range []string{"idle", "busy", "old-generation"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				oldTimeout := hostTrustWriteTimeout
				hostTrustWriteTimeout = 20 * time.Millisecond
				t.Cleanup(func() { hostTrustWriteTimeout = oldTimeout })
				fake := &fakeProvider{execFunc: func(ctx context.Context, _ string, _ []string, _ provider.ExecOptions) (provider.ExecResult, error) {
					<-ctx.Done()
					return provider.ExecResult{}, ctx.Err()
				}}
				github := &fakeGitHub{runner: gh.Runner{Name: "survivor", ID: 42, Busy: mode == "busy"}, found: true}
				m, active := keeperTestManager(t, fake, github)
				current, err := m.resolveHostTrust(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				if mode == "old-generation" {
					current.Generation = "g2"
				}
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
				defer cancel()
				m.reconcileHostTrustRunnersMode(ctx, active, current, make(map[string]bool), false)
				if active["survivor"].Phase != LifecycleQuarantined || atomic.LoadInt32(&github.deleteCalls) != 1 {
					t.Fatalf("expired maintenance deadline lost fencing: phase=%s deletes=%d", active["survivor"].Phase, github.deleteCalls)
				}
				if got := atomic.LoadInt32(&fake.execCalls); got != 2 {
					t.Fatalf("guest operations=%d, want failed write and failed revocation", got)
				}
			})
		})
	}
}

func TestExpiredLeaseSnapshotDefersWithoutFencingHealthyRunner(t *testing.T) {
	for _, mode := range []string{"idle", "busy", "old-generation"} {
		t.Run(mode, func(t *testing.T) {
			fake := &fakeProvider{}
			github := &fakeGitHub{runner: gh.Runner{Name: "survivor", ID: 42, Busy: mode == "busy"}, found: true}
			m, active := keeperTestManager(t, fake, github)
			current, err := m.resolveHostTrust(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			current.CollectedAt = time.Now().Add(-hostTrustMaximumAge - time.Second)
			if mode == "old-generation" {
				current.Generation = "g2"
			}
			m.reconcileHostTrustRunnersMode(context.Background(), active, current, make(map[string]bool), false)
			if active["survivor"].Phase != LifecycleReady || atomic.LoadInt32(&github.deleteCalls) != 0 || atomic.LoadInt32(&fake.execCalls) != 0 {
				t.Fatalf("expired local snapshot caused a guest mutation or fence: phase=%s deletes=%d execs=%d", active["survivor"].Phase, github.deleteCalls, fake.execCalls)
			}
		})
	}
}

func TestLeaseKeeperRefreshesSnapshotBetweenSlowSurvivors(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fake := &fakeProvider{}
		github := &fakeGitHub{found: true}
		m, active := keeperTestManager(t, fake, github)
		second := active["survivor"]
		second.Name, second.ProviderID = "second", "fake:second"
		active[second.Name] = second
		resolver := m.hostTrustResolver
		var collections, reads atomic.Int32
		m.hostTrustResolver = func(ctx context.Context) (hosttrust.Snapshot, error) {
			snapshot, err := resolver(ctx)
			if collections.Add(1) == 1 {
				snapshot.CollectedAt = time.Now().Add(-hostTrustMaximumAge + 10*time.Millisecond)
			}
			return snapshot, err
		}
		github.runnerByNameFunc = func(_ context.Context, name string) (gh.Runner, bool, error) {
			if reads.Add(1) == 1 {
				time.Sleep(25 * time.Millisecond)
			}
			return gh.Runner{Name: name, ID: 42}, true, nil
		}
		written := make(chan struct{}, 1)
		fake.execFunc = func(_ context.Context, _ string, _ []string, opts provider.ExecOptions) (provider.ExecResult, error) {
			if opts.Stdin != "" {
				select {
				case written <- struct{}{}:
				default:
				}
			}
			return provider.ExecResult{}, nil
		}
		_, stop := m.startHostTrustLeaseKeeperForRunners(context.Background(), active, nil)
		defer stop(active)
		waitKeeperSignal(t, written)
		stop(active)
		if collections.Load() < 2 {
			t.Fatal("expired snapshot was reused for the next survivor")
		}
		if atomic.LoadInt32(&github.deleteCalls) != 0 {
			t.Fatal("slow snapshot handling fenced a healthy survivor")
		}
	})
}

func TestLeaseKeeperRefreshesAcrossIndividuallyShortPhases(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		written := make(chan struct{}, 1)
		fake := &fakeProvider{execFunc: func(_ context.Context, _ string, _ []string, opts provider.ExecOptions) (provider.ExecResult, error) {
			if strings.Contains(opts.Stdin, `"expiresAt"`) {
				written <- struct{}{}
			}
			return provider.ExecResult{}, nil
		}}
		github := &fakeGitHub{runner: gh.Runner{Name: "survivor", ID: 42}, found: true}
		m, active := keeperTestManager(t, fake, github)
		hostTrustRefreshInterval = time.Second
		for phase := 0; phase < 4; phase++ {
			_, stop := m.startHostTrustLeaseKeeperForRunners(context.Background(), active, nil)
			// An immediate sweep must finish without advancing the fake clock.
			synctest.Wait()
			select {
			case <-written:
				stop(active)
			default:
				stop(active)
				t.Fatal("short phases keep postponing the first renewal")
			}
		}
		if got := len(hostTrustLeaseInputs(fake)); got != 4 {
			t.Fatalf("writes=%d, want one per short phase", got)
		}
	})
}
