package pool

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
	gh "github.com/solutionforest/ephemeral-action-runner/internal/github"
	"github.com/solutionforest/ephemeral-action-runner/internal/hosttrust"
	poolstate "github.com/solutionforest/ephemeral-action-runner/internal/pool/state"
	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
)

func TestReadinessHostTrustLeaseFailureClassification(t *testing.T) {
	permanent := errors.New("permission denied writing host trust lease")
	for _, test := range []struct {
		name         string
		writeErr     error
		revokeErr    error
		cancelParent bool
		recoverable  bool
	}{
		{name: "write timeout", writeErr: context.DeadlineExceeded, recoverable: true},
		{name: "write and revocation timeout", writeErr: context.DeadlineExceeded, revokeErr: context.DeadlineExceeded, recoverable: true},
		{name: "permanent write and revocation timeout", writeErr: permanent, revokeErr: context.DeadlineExceeded},
		{name: "permanent write", writeErr: permanent},
		{name: "parent cancellation", writeErr: context.Canceled, cancelParent: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				var revoked bool
				fake := &fakeProvider{instances: []provider.Instance{{Name: "candidate", ProviderID: "fake:candidate", State: "running"}}}
				fake.execFunc = func(_ context.Context, _ string, command []string, opts provider.ExecOptions) (provider.ExecResult, error) {
					if strings.Contains(opts.Stdin, `"expiresAt"`) {
						if test.cancelParent {
							cancel()
						}
						return provider.ExecResult{}, test.writeErr
					}
					if strings.Contains(strings.Join(command, " "), hostTrustLeaseGuest) {
						revoked = true
						return provider.ExecResult{}, test.revokeErr
					}
					return provider.ExecResult{Stdout: runnerProcessRunningSentinel}, nil
				}
				github := &fakeGitHub{waitFunc: func(ctx context.Context, _ string, _ time.Duration) (gh.Runner, error) {
					<-ctx.Done()
					return gh.Runner{}, ctx.Err()
				}}
				manager, _ := keeperTestManager(t, fake, github)
				_, err := manager.waitRunnerReadyAndHealthy(ctx, ProvisionedInstance{Name: "candidate", HostTrustGeneration: "g1"}, time.Minute, true)
				candidate, recoverable := recoverableCandidateFailure(err)
				if recoverable != test.recoverable || !errors.Is(err, test.writeErr) || !revoked {
					t.Fatalf("readiness error=%v recoverable=%t revoked=%t, want recoverable=%t preserving write failure and attempting revocation", err, recoverable, revoked, test.recoverable)
				}
				if recoverable && candidate.outcome != candidateHostTrustLeaseTimedOut {
					t.Fatalf("recovery outcome=%s, want host-trust lease timeout", candidate.outcome)
				}
			})
		})
	}
}

func TestRunPoolRecoversReadinessHostTrustLeaseTimeout(t *testing.T) {
	oldRefresh := hostTrustRefreshInterval
	hostTrustRefreshInterval = time.Second
	t.Cleanup(func() { hostTrustRefreshInterval = oldRefresh })
	for _, test := range []struct {
		name         string
		replacement  bool
		blockCleanup bool
		outagePolicy string
	}{
		{name: "initial", outagePolicy: "off"},
		{name: "replacement", replacement: true, outagePolicy: "off"},
		{name: "continuous external outage policy", replacement: true, outagePolicy: "continuous"},
		{name: "unresolved cleanup retains capacity", blockCleanup: true, outagePolicy: "off"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("EPAR_STATE_HOME", t.TempDir())
			synctest.Test(t, func(t *testing.T) {
				fake := &fakeProvider{ip: "127.0.0.1"}
				github := &fakeGitHub{}
				var mu sync.Mutex
				runners := make(map[string]gh.Runner)
				var firstName, survivorName, failedName string
				var waits, revocations, peerRenewals atomic.Int32
				snapshot := hosttrust.Snapshot{Generation: "g1", HostOS: "linux", Scopes: []string{"system"}, Certificates: []hosttrust.Certificate{{Name: "root.crt", PEM: []byte("pem")}}}
				marker, err := hostTrustMarkerJSON(snapshot)
				if err != nil {
					t.Fatal(err)
				}
				fake.execFunc = func(ctx context.Context, name string, command []string, opts provider.ExecOptions) (provider.ExecResult, error) {
					text := strings.Join(command, " ")
					if text == "cat "+hostTrustMarkerGuest {
						return provider.ExecResult{Stdout: string(marker)}, nil
					}
					mu.Lock()
					failed, peer := name == failedName, name == survivorName
					mu.Unlock()
					if strings.Contains(text, hostTrustLeaseGuest) && failed {
						if opts.Stdin == "" {
							revocations.Add(1)
						}
						<-ctx.Done()
						return provider.ExecResult{}, ctx.Err()
					}
					if peer && strings.Contains(opts.Stdin, `"expiresAt"`) {
						peerRenewals.Add(1)
					}
					return provider.ExecResult{Stdout: runnerProcessRunningSentinel}, nil
				}
				failureAttempt := int32(1)
				instances := 1
				if test.replacement {
					failureAttempt, instances = 3, 2
				}
				github.waitFunc = func(ctx context.Context, name string, _ time.Duration) (gh.Runner, error) {
					call := waits.Add(1)
					runner := gh.Runner{Name: name, ID: int64(call), Status: "online"}
					mu.Lock()
					runners[name] = runner
					if test.replacement && call == 1 {
						firstName = name
					}
					if test.replacement && call == 2 {
						survivorName = name
						delete(runners, firstName)
					}
					if call == failureAttempt {
						failedName = name
					}
					mu.Unlock()
					if call == failureAttempt {
						<-ctx.Done()
						return gh.Runner{}, ctx.Err()
					}
					return runner, nil
				}
				github.listFunc = func(context.Context) ([]gh.Runner, error) {
					mu.Lock()
					defer mu.Unlock()
					var result []gh.Runner
					for _, runner := range runners {
						result = append(result, runner)
					}
					return result, nil
				}
				github.runnerByNameFunc = func(_ context.Context, name string) (gh.Runner, bool, error) {
					mu.Lock()
					defer mu.Unlock()
					runner, found := runners[name]
					return runner, found, nil
				}
				github.deleteFunc = func(_ context.Context, id int64) error {
					mu.Lock()
					defer mu.Unlock()
					for name, runner := range runners {
						if runner.ID == id {
							delete(runners, name)
						}
					}
					return nil
				}
				if test.blockCleanup {
					fake.deleteErr = errors.New("exact provider cleanup unavailable")
				}
				manager := newRegisteredTestManager(t, fake, github)
				if test.replacement {
					manager.Config.Provider.Type = "docker-sandboxes"
				}
				manager.Config.Image.HostTrustMode = config.HostTrustModeOverlay
				manager.Config.Image.HostTrustScopes = []string{"system"}
				manager.Config.Image.UpdateFrequency = config.ImageUpdateFrequencyManual
				manager.Config.Timeouts.CommandSeconds = 900
				manager.Config.Pool.Instances = instances
				manager.Config.Pool.ReplacementRetryInitialSeconds = 1
				manager.Config.Pool.ReplacementRetryMaxSeconds = 1
				manager.Config.Pool.ReplacementRetryJitterPercent = 0
				manager.hostTrustImageEnsurer = func(context.Context) error { return nil }
				manager.hostTrustResolver = func(context.Context) (hosttrust.Snapshot, error) {
					fresh := snapshot
					fresh.CollectedAt = time.Now().UTC()
					return fresh, nil
				}
				manager.LifecycleState, err = poolstate.Open(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				manager.ConfigPath = filepath.Join(manager.ProjectRoot, "config.yml")
				if err := os.WriteFile(manager.ConfigPath, []byte("test: config\n"), 0600); err != nil {
					t.Fatal(err)
				}
				policy, err := ParseExternalOutageRetryPolicy(test.outagePolicy)
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
				defer cancel()
				err = manager.RunPool(ctx, RunOptions{Instances: instances, Register: true, ReplaceCompleted: true, KeepOnExit: true, MonitorInterval: time.Second, PoolLockHeld: true, HostTrustLockHeld: true, ExternalOutageRetry: policy})
				if err != nil || ctx.Err() == nil {
					t.Fatalf("supervisor exited before cancellation: %v", err)
				}
				wantAttempts := failureAttempt + 1
				if test.blockCleanup {
					wantAttempts--
				}
				if waits.Load() != wantAttempts || atomic.LoadInt32(&fake.maxInventory) > int32(instances) || revocations.Load() == 0 {
					t.Fatalf("attempts=%d want=%d maxInventory=%d capacity=%d revocations=%d", waits.Load(), wantAttempts, fake.maxInventory, instances, revocations.Load())
				}
				if test.replacement && peerRenewals.Load() < 2 {
					t.Fatalf("peer lease renewals=%d, want continued maintenance", peerRenewals.Load())
				}
				if !slices.Contains(github.deletedIDs, int64(failureAttempt)) || slices.Contains(fake.deletedNames, survivorName) {
					t.Fatalf("cleanup remote=%v local=%v, want exact candidate and preserved peer", github.deletedIDs, fake.deletedNames)
				}
				if test.blockCleanup {
					record, err := manager.LifecycleState.Read(context.Background(), failedName)
					if err != nil || record.Phase != poolstate.PhaseCleanupPending {
						t.Fatalf("unresolved candidate phase=%s error=%v, want durable cleanup-pending", record.Phase, err)
					}
				}
			})
		})
	}
}
