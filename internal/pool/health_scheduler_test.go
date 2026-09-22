package pool

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	gh "github.com/solutionforest/ephemeral-action-runner/internal/github"
	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
)

type healthTestLifecycle struct {
	provider.Lifecycle
	items                                    []provider.InventoryItem
	globalCalls, instanceCalls, processCalls int
	inventoryCalls                           int
	onCall                                   func(context.Context) error
	stopped                                  bool
}

func (l *healthTestLifecycle) call(ctx context.Context) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if l.onCall != nil {
		return l.onCall(ctx)
	}
	return nil
}
func (l *healthTestLifecycle) Inventory(ctx context.Context) ([]provider.InventoryItem, error) {
	l.inventoryCalls++
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return l.items, nil
}
func (l *healthTestLifecycle) VerifyAdmission(ctx context.Context) error {
	l.globalCalls++
	return l.call(ctx)
}
func (l *healthTestLifecycle) VerifyInstanceAdmission(ctx context.Context, _ provider.Instance) error {
	l.instanceCalls++
	return l.call(ctx)
}
func (l *healthTestLifecycle) Exec(ctx context.Context, _ provider.Instance, _ []string, _ provider.ExecOptions) (provider.ExecResult, error) {
	l.processCalls++
	if err := l.call(ctx); err != nil {
		return provider.ExecResult{}, err
	}
	stdout := runnerProcessRunningSentinel
	if l.stopped {
		stdout = runnerProcessStoppedSentinel
	}
	return provider.ExecResult{Stdout: stdout}, nil
}

func healthTestPool() (map[string]ProvisionedInstance, *healthTestLifecycle) {
	active := map[string]ProvisionedInstance{}
	l := &healthTestLifecycle{}
	for _, name := range []string{"a", "b", "c"} {
		active[name] = ProvisionedInstance{Name: name, ProviderID: "id-" + name, ProviderOwned: true, RunnerID: 1}
		l.items = append(l.items, provider.InventoryItem{Instance: provider.Instance{Name: name, ProviderID: "id-" + name}})
	}
	return active, l
}

func TestHealthSchedulerProgressBeyondAggregateBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		active, l := healthTestPool()
		// Each phase fits a fresh production sweep budget; a full check including
		// global admission does not. All clocks and context timers share the bubble.
		l.onCall = func(ctx context.Context) error {
			select {
			case <-time.After(4 * time.Second):
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		m := &Manager{Lifecycle: l, GitHub: &fakeGitHub{runnerByNameFunc: func(ctx context.Context, name string) (gh.Runner, bool, error) {
			if err := l.call(ctx); err != nil {
				return gh.Runner{}, false, err
			}
			return gh.Runner{ID: 1, Status: "online"}, true, nil
		}}}
		s := healthScheduler{}
		completed := map[string]int{}
		inactive := map[string]int{}
		deferred := ""
		deferrals := 0
		for tick := 0; tick < 10 && len(completed) < len(active); tick++ {
			start := time.Now()
			s.sync(active, 0, start, inactive)
			ctx, cancel := context.WithTimeout(context.Background(), hostTrustRefreshInterval/2)
			for visits := 0; visits < 1+3*len(active) && ctx.Err() == nil; visits++ {
				previous := s.cursor
				name, p := s.next()
				if p == nil {
					break
				}
				if deferred != "" {
					if name != deferred {
						t.Fatalf("deferred %s lost its turn to %s", deferred, name)
					}
					deferred = ""
				}
				if !s.fits(ctx, p) {
					s.cursor = previous
					deferred = name
					deferrals++
					break
				}
				done, alive, _, stage, err := m.visitRunnerHealth(ctx, &s, p, active[name])
				if err != nil {
					t.Fatalf("tick %d %s %s: %v", tick, name, stage, err)
				}
				if done {
					if !alive {
						t.Fatal("healthy runner reported dead")
					}
					p.done = true
					completed[name]++
				}
			}
			cancel()
			time.Sleep(start.Add(hostTrustRefreshInterval / 2).Sub(time.Now()))
		}
		if len(completed) != len(active) || deferrals == 0 {
			t.Fatalf("missing bounded progress: completed=%v deferrals=%d", completed, deferrals)
		}
		if l.globalCalls != 1 {
			t.Fatalf("successful global admission repeated %d times", l.globalCalls)
		}
	})
}

func TestHealthSchedulerCompletedPeerRestartsAndFairness(t *testing.T) {
	active, _ := healthTestPool()
	s := healthScheduler{}
	inactive := map[string]int{}
	s.sync(active, 0, time.Now(), inactive)
	for _, want := range []string{"a", "b", "c", "a"} {
		got, _ := s.next()
		if got != want {
			t.Fatalf("visit=%s want=%s", got, want)
		}
	}
	s.progress["a"].done = true
	s.progress["a"].stage = 2
	s.progress["b"].stage = 1 // a failing peer has not finished
	s.progress["b"].admittedAt = time.Now()
	s.sync(active, 0, time.Now(), inactive)
	if s.progress["a"].done || s.progress["a"].stage != 0 || s.progress["b"].stage != 1 {
		t.Fatal("completed peer did not restart independently")
	}
}

func TestHealthSchedulerInvalidatesEvidence(t *testing.T) {
	for _, kind := range []string{"provider", "runner", "trust", "recovery", "freshness", "uncertain"} {
		t.Run(kind, func(t *testing.T) {
			active, _ := healthTestPool()
			s := healthScheduler{}
			now := time.Now()
			inactive := map[string]int{}
			s.sync(active, 0, now, inactive)
			s.globalAt = now
			s.progress["a"].stage = 2
			s.progress["a"].admittedAt = now
			inactive["a"] = 1
			vm := active["a"]
			gen := uint64(0)
			switch kind {
			case "provider":
				vm.ProviderID = "new"
			case "runner":
				vm.RunnerID = 2
			case "trust":
				vm.HostTrustGeneration = "new"
			case "recovery":
				gen = 1
			case "freshness":
				now = now.Add(healthEvidenceLifetime)
			case "uncertain":
				vm.RecoveryInventoryUncertain = true
			}
			active["a"] = vm
			s.sync(active, gen, now, inactive)
			if p := s.progress["a"]; p != nil && p.stage != 0 {
				t.Fatal("stale phase retained")
			}
			if kind == "freshness" && inactive["a"] != 1 {
				t.Fatal("TTL discarded valid inactive confirmation")
			}
			if kind != "freshness" && inactive["a"] != 0 {
				t.Fatal("stale inactive confirmation retained")
			}
		})
	}
}

func TestHealthSchedulerDeadlineAndFreshNegative(t *testing.T) {
	active, l := healthTestPool()
	vm := active["a"]
	l.stopped = true
	g := &fakeGitHub{runner: gh.Runner{ID: 1, Status: "online", Busy: true}, found: true}
	m := &Manager{Lifecycle: l, GitHub: g}
	s := healthScheduler{globalAt: time.Now()}
	p := &healthProgress{stage: 2, admittedAt: time.Now()}
	done, alive, _, _, err := m.visitRunnerHealth(context.Background(), &s, p, vm)
	if err != nil || !done || !alive {
		t.Fatalf("new busy runner not protected: %v %v %v", done, alive, err)
	}
	g.runner.Busy = false
	g.runner.ID = 2
	done, _, _, _, err = m.visitRunnerHealth(context.Background(), &s, p, vm)
	if err == nil || done {
		t.Fatal("changed identity accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	before := l.processCalls
	done, _, _, _, err = m.visitRunnerHealth(ctx, &s, p, vm)
	if !errors.Is(err, context.Canceled) || done || l.processCalls != before {
		t.Fatal("expired context executed a probe")
	}
	ctx, cancel = context.WithCancel(context.Background())
	l.onCall = func(context.Context) error { cancel(); return nil }
	done, _, _, _, err = m.visitRunnerHealth(ctx, &s, p, vm)
	if !errors.Is(err, context.Canceled) || done {
		t.Fatal("late negative result accepted")
	}
}

func TestHealthWarningEpisodesAndFlapping(t *testing.T) {
	s := healthScheduler{}
	now := time.Now()
	id := healthIdentity{name: "a", providerID: "exact", runnerID: 1}
	if emit, count, _ := s.unknown(id, now); !emit || count != 1 {
		t.Fatal("first warning missing")
	}
	if emit, count, elapsed := s.unknown(id, now.Add(time.Minute)); emit || count != 2 || elapsed != time.Minute {
		t.Fatal("repeat not suppressed/count wrong")
	}
	if emit, count, _ := s.unknown(id, now.Add(healthWarningInterval)); !emit || count != 3 {
		t.Fatal("summary missing")
	}
	m := &Manager{now: func() time.Time { return now.Add(healthWarningInterval + time.Second) }}
	m.reportRecoveredHealth(&s, ProvisionedInstance{Name: "a", ProviderID: "exact", RunnerID: 1})
	if len(s.episodes) != 0 {
		t.Fatal("episode not recovered")
	}
	if emit, _, _ := s.unknown(id, now.Add(healthWarningInterval+2*time.Second)); emit {
		t.Fatal("flapping bypassed cooldown")
	}
	id.providerID = "replacement"
	if emit, _, _ := s.unknown(id, now); !emit {
		t.Fatal("replacement warning suppressed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	m.reportUnknownHealth(ctx, ctx, &s, ProvisionedInstance{Name: "shutdown"}, "process", context.Canceled)
	if len(s.episodes) != 2 {
		t.Fatal("shutdown created warning episode")
	}
}

func TestHealthBudgetDeferralAndOversizedEstimate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := healthScheduler{globalAt: time.Now()}
		p := &healthProgress{}
		s.observeDuration(p.identity, "instance-admission", time.Minute, true)
		short, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if s.fits(short, p) {
			t.Fatal("expensive work started with exhausted budget")
		}
		fresh, cancelFresh := context.WithTimeout(context.Background(), hostTrustRefreshInterval/2)
		defer cancelFresh()
		if !s.fits(fresh, p) {
			t.Fatal("slow estimate can never retry with a fresh budget")
		}
	})
}

func TestHealthDurationIsolationAndGlobalTiming(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		active, lifecycle := healthTestPool()
		now := time.Now()
		s := healthScheduler{}
		s.sync(active, 0, now, map[string]int{})
		s.globalAt = now
		cost := 10 * time.Second
		lifecycle.onCall = func(context.Context) error { time.Sleep(cost); return nil }
		m := &Manager{Lifecycle: lifecycle}
		for _, name := range []string{"a", "b"} {
			p := s.progress[name]
			if _, _, _, _, err := m.visitRunnerHealth(context.Background(), &s, p, active[name]); err != nil {
				t.Fatal(err)
			}
			p.stage = 0
			cost = time.Second
		}
		if got := s.estimatedDuration(s.progress["a"]); got != 10*time.Second {
			t.Fatalf("fast peer replaced slow estimate: %s", got)
		}
		if got := s.estimatedDuration(s.progress["b"]); got != time.Second {
			t.Fatalf("peer did not retain own estimate: %s", got)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if s.fits(ctx, s.progress["a"]) || !s.fits(ctx, s.progress["b"]) {
			t.Fatal("budget admission ignored individual duration/headroom")
		}
		s.globalAt = time.Time{}
		s.observeDuration(s.progress["a"].identity, "global-admission", 8*time.Second, true)
		s.observeDuration(s.progress["b"].identity, "global-admission", time.Second, true)
		for _, p := range s.progress {
			if got := s.estimatedDuration(p); got != 8*time.Second-7*time.Second/8 {
				t.Fatalf("global timing was not shared and conservative: %s", got)
			}
		}
	})
}

func TestHealthDurationConservativeUpdate(t *testing.T) {
	s := healthScheduler{globalAt: time.Now()}
	p := &healthProgress{identity: healthIdentity{name: "a", providerID: "exact"}}
	for _, phase := range []string{"instance-admission", "github", "process", "global-admission"} {
		t.Run(phase, func(t *testing.T) {
			read := func() time.Duration {
				if phase == "global-admission" {
					return s.globalDuration
				}
				return s.instanceDurations[p.identity][phase]
			}
			s.observeDuration(p.identity, phase, 10*time.Second, true)
			s.observeDuration(p.identity, phase, 2*time.Second, true)
			if got := read(); got != 9*time.Second {
				t.Fatalf("fast success did not decay conservatively: %s", got)
			}
			s.observeDuration(p.identity, phase, time.Second, false)
			s.observeDuration(p.identity, phase, 0, true)
			if got := read(); got != 9*time.Second {
				t.Fatalf("partial/zero sample reduced estimate: %s", got)
			}
			s.observeDuration(p.identity, phase, 12*time.Second, false)
			if got := read(); got != 12*time.Second {
				t.Fatalf("slow failure did not raise estimate: %s", got)
			}
			for i := 0; i < 100; i++ {
				s.observeDuration(p.identity, phase, 2*time.Second, true)
			}
			if got := read(); got < 2*time.Second || got > 2100*time.Millisecond {
				t.Fatalf("estimate failed to converge toward sustained faster success: %s", got)
			}
		})
	}
}

func TestHealthDurationIdentityReset(t *testing.T) {
	for _, change := range []string{"provider", "runner", "trust", "removed", "uncertain"} {
		t.Run(change, func(t *testing.T) {
			active, _ := healthTestPool()
			now := time.Now()
			s := healthScheduler{}
			inactive := map[string]int{}
			s.sync(active, 0, now, inactive)
			old := s.progress["a"].identity
			s.observeDuration(old, "instance-admission", 12*time.Second, true)
			s.observeDuration(s.progress["b"].identity, "instance-admission", time.Second, true)
			s.observeDuration(old, "global-admission", 2*time.Second, true)
			vm := active["a"]
			switch change {
			case "provider":
				vm.ProviderID = "replacement"
			case "runner":
				vm.RunnerID++
			case "trust":
				vm.HostTrustGeneration = "replacement"
			case "uncertain":
				vm.RecoveryInventoryUncertain = true
			}
			active["a"] = vm
			if change == "removed" {
				delete(active, "a")
			}
			s.sync(active, 0, now, inactive)
			s.globalAt = now
			if _, ok := s.instanceDurations[old]; ok {
				t.Fatal("obsolete identity retained duration history")
			}
			if p := s.progress["a"]; p != nil && s.estimatedDuration(p) != 9*time.Second {
				t.Fatal("replacement inherited old timing")
			}
			if s.estimatedDuration(s.progress["b"]) != time.Second || s.globalDuration != 2*time.Second {
				t.Fatal("identity change discarded peer or global timing")
			}
		})
	}
}

func TestHealthStagePreservesNonTrustTimeoutAndShorterParent(t *testing.T) {
	for _, bounded := range []bool{false, true} {
		t.Run(fmt.Sprint("bounded=", bounded), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				active, lifecycle := healthTestPool()
				manager := &Manager{Lifecycle: lifecycle}
				var remaining time.Duration
				lifecycle.onCall = func(ctx context.Context) error {
					deadline, ok := ctx.Deadline()
					if !ok {
						t.Fatal("health stage has no timeout")
					}
					remaining = time.Until(deadline)
					return nil
				}
				ctx := context.Background()
				if bounded {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, 5*time.Second)
					defer cancel()
				}
				_, _, _, _, err := manager.visitRunnerHealth(ctx, &healthScheduler{}, &healthProgress{}, active["a"])
				if err != nil {
					t.Fatal(err)
				}
				if bounded && remaining != 5*time.Second {
					t.Fatalf("parent timeout not preserved: %s", remaining)
				}
				if !bounded && remaining != 30*time.Second {
					t.Fatalf("ordinary health timeout changed: %s", remaining)
				}
			})
		})
	}
}

func TestHealthFastFailuresDoNotStallPeers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		active, l := healthTestPool()
		active["d"] = ProvisionedInstance{Name: "d", ProviderID: "id-d", ProviderOwned: true, RunnerID: 1}
		active["e"] = ProvisionedInstance{Name: "e", ProviderID: "id-e", ProviderOwned: true, RunnerID: 1}
		for _, name := range []string{"d", "e"} {
			l.items = append(l.items, provider.InventoryItem{Instance: provider.Instance{Name: name, ProviderID: "id-" + name}})
		}
		g := &fakeGitHub{runnerByNameFunc: func(ctx context.Context, name string) (gh.Runner, bool, error) {
			if name != "e" {
				return gh.Runner{}, false, errors.New("unavailable")
			}
			return gh.Runner{ID: 1, Status: "online"}, true, nil
		}}
		m := &Manager{Lifecycle: l, GitHub: g}
		s := healthScheduler{}
		inactive := map[string]int{}
		completed := 0
		for tick := 0; tick < 10; tick++ {
			s.sync(active, 0, time.Now(), inactive)
			ctx, cancel := context.WithTimeout(context.Background(), hostTrustRefreshInterval/2)
			failed := map[string]bool{}
			for visits := 0; visits < 1+3*len(active) && ctx.Err() == nil; visits++ {
				previous := s.cursor
				name, p := s.next(failed)
				if p == nil {
					break
				}
				if !s.fits(ctx, p) {
					s.cursor = previous
					break
				}
				done, alive, _, stage, err := m.visitRunnerHealth(ctx, &s, p, active[name])
				if err != nil {
					if stage == "global-admission" {
						break
					}
					failed[name] = true
					continue
				}
				if done {
					p.done = true
					if name == "e" && alive {
						completed++
					}
				}
			}
			cancel()
			time.Sleep(15 * time.Second)
		}
		if completed != 10 {
			t.Fatalf("healthy peer completed %d/10 rounds", completed)
		}
		s.progress["e"].stage = 2
		inactive["e"] = 1
		s.sync(active, 0, time.Now(), inactive, "new-host-trust")
		if s.progress["e"].stage != 0 || inactive["e"] != 0 {
			t.Fatal("current host trust did not invalidate old VM evidence")
		}
	})
}

func TestHealthSlowPoolCompletesAcrossGlobalRefresh(t *testing.T) {
	for _, failing := range []bool{false, true} {
		t.Run(fmt.Sprint("failing-advanced-peer=", failing), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				active, l := healthTestPool()
				for _, name := range []string{"d", "e"} {
					active[name] = ProvisionedInstance{Name: name, ProviderID: "id-" + name, ProviderOwned: true, RunnerID: 1}
					l.items = append(l.items, provider.InventoryItem{Instance: provider.Instance{Name: name, ProviderID: "id-" + name}})
				}
				s := healthScheduler{}
				inactive := map[string]int{}
				completed := map[string]int{}
				stage := ""
				cost := map[string]time.Duration{"global-admission": 4 * time.Second, "instance-admission": 9 * time.Second, "github": 3 * time.Second, "process": 6 * time.Second}
				l.onCall = func(ctx context.Context) error { time.Sleep(cost[stage]); return ctx.Err() }
				m := &Manager{Lifecycle: l, GitHub: &fakeGitHub{runnerByNameFunc: func(ctx context.Context, name string) (gh.Runner, bool, error) {
					time.Sleep(cost["github"])
					if failing && name == "a" {
						return gh.Runner{}, false, errors.New("remote unavailable")
					}
					return gh.Runner{ID: 1, Status: "online"}, true, ctx.Err()
				}}}
				for tick := 0; tick < 80; tick++ {
					start := time.Now()
					s.sync(active, 0, start, inactive)
					ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
					failed := map[string]bool{}
					for visits := 0; visits < 1+3*len(active) && ctx.Err() == nil; visits++ {
						previous := s.cursor
						name, p := s.next(failed)
						if p == nil {
							break
						}
						if !s.fits(ctx, p) {
							s.cursor = previous
							break
						}
						stage = s.phase(p)
						done, alive, _, _, err := m.visitRunnerHealth(ctx, &s, p, active[name])
						if err != nil {
							if !(failing && name == "a" && stage == "github") {
								t.Fatalf("tick %d %s %s: %v", tick, name, stage, err)
							}
							failed[name] = true
							continue
						}
						if done {
							if !alive {
								t.Fatal("healthy runner marked inactive")
							}
							p.done = true
							completed[name]++
						}
					}
					if time.Since(start) > 15*time.Second {
						t.Fatal("sweep budget exceeded")
					}
					cancel()
					time.Sleep(time.Until(start.Add(15 * time.Second)))
				}
				for name := range active {
					if failing && name == "a" {
						continue
					}
					if completed[name] < 3 {
						t.Fatalf("runner %s made insufficient progress: %v", name, completed)
					}
				}
				if l.globalCalls < 3 {
					t.Fatal("global admission was not renewed")
				}
			})
		})
	}
}

func TestHealthIndependentEvidenceAndRetirementFreshness(t *testing.T) {
	active, _ := healthTestPool()
	now := time.Now()
	s := healthScheduler{}
	inactive := map[string]int{}
	s.sync(active, 0, now, inactive, "trusted")
	s.globalAt = now.Add(-healthEvidenceLifetime)
	p := s.progress["a"]
	p.stage = 2
	p.admittedAt = now.Add(-10 * time.Second)
	s.resume = "a"
	s.sync(active, 0, now, inactive, "trusted")
	if !s.globalAt.IsZero() || p.stage != 2 || s.resume != "a" {
		t.Fatal("global refresh erased pending instance progress")
	}
	s.globalAt = now
	p.admittedAt = now.Add(-59 * time.Second)
	if !s.evidenceFresh(p, now) {
		t.Fatal("fresh verdict rejected")
	}
	if s.evidenceFresh(p, now.Add(2*time.Second)) {
		t.Fatal("diagnostics allowed expired evidence to authorize retirement")
	}
	p.admittedAt = now
	s.sync(active, 0, now, inactive, "")
	if p.stage != 2 || s.trustGeneration != "trusted" {
		t.Fatal("unknown trust snapshot treated as observed generation change")
	}
}

func TestHealthFakeBudgetConsumesTimeAndRejectsLateSuccess(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		active, l := healthTestPool()
		now := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		s := healthScheduler{globalAt: now}
		p := &healthProgress{}
		if !s.fits(ctx, p) {
			t.Fatal("fresh budget rejected")
		}
		time.Sleep(9 * time.Second)
		if s.fits(ctx, p) {
			t.Fatal("elapsed fake time did not consume the sweep budget")
		}
		s.globalAt = time.Time{}
		l.onCall = func(ctx context.Context) error { <-ctx.Done(); return nil }
		m := &Manager{Lifecycle: l}
		done, _, _, _, err := m.visitRunnerHealth(ctx, &s, p, active["a"])
		if !errors.Is(err, context.DeadlineExceeded) || done || !s.globalAt.IsZero() || p.stage != 0 {
			t.Fatalf("late success advanced evidence: done=%t error=%v global=%v stage=%d", done, err, s.globalAt, p.stage)
		}
	})
}

func TestHealthStageCancellationAndRetry(t *testing.T) {
	for _, phase := range []string{"global-admission", "instance-admission", "github", "process"} {
		for _, mode := range []string{"already-canceled", "canceled-in-flight", "parent-deadline", "stage-deadline"} {
			t.Run(phase+"/"+mode, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					active, l := healthTestPool()
					s := healthScheduler{}
					s.sync(active, 0, time.Now(), map[string]int{})
					p := s.progress["a"]
					if phase != "global-admission" {
						s.globalAt = time.Now()
					}
					switch phase {
					case "github":
						p.stage = 1
					case "process":
						p.stage = 2
					}
					if p.stage > 0 {
						p.admittedAt = time.Now()
					}
					initialStage, initialAdmission := p.stage, p.admittedAt
					s.resume = "a"
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					if mode == "parent-deadline" {
						var deadlineCancel context.CancelFunc
						ctx, deadlineCancel = context.WithTimeout(ctx, 5*time.Second)
						defer deadlineCancel()
					}
					calls := 0
					l.onCall = func(stageCtx context.Context) error {
						calls++
						if mode == "canceled-in-flight" {
							cancel()
						}
						// A dependency may return success after cancellation. Done and
						// Err must agree, and that success must not become evidence.
						<-stageCtx.Done()
						if stageCtx.Err() == nil {
							t.Fatal("Done closed without a context error")
						}
						return nil
					}
					m := &Manager{Lifecycle: l, GitHub: &fakeGitHub{runnerByNameFunc: func(ctx context.Context, _ string) (gh.Runner, bool, error) {
						err := l.call(ctx)
						return gh.Runner{ID: 1, Status: "online"}, true, err
					}}}
					if mode == "already-canceled" {
						cancel()
					}
					start := time.Now()
					done, _, _, gotPhase, err := m.visitRunnerHealth(ctx, &s, p, active["a"])
					wantErr := context.Canceled
					wantElapsed := time.Duration(0)
					if mode == "parent-deadline" {
						wantErr, wantElapsed = context.DeadlineExceeded, 5*time.Second
					} else if mode == "stage-deadline" {
						wantErr, wantElapsed = context.DeadlineExceeded, 30*time.Second
					}
					if !errors.Is(err, wantErr) || done || gotPhase != phase || time.Since(start) != wantElapsed {
						t.Fatalf("canceled phase: done=%t phase=%s err=%v elapsed=%s", done, gotPhase, err, time.Since(start))
					}
					wantCalls := 1
					if mode == "already-canceled" {
						wantCalls = 0
					}
					if calls != wantCalls || p.stage != initialStage || p.admittedAt != initialAdmission {
						t.Fatalf("cancellation advanced work: calls=%d stage=%d admittedAt=%v", calls, p.stage, p.admittedAt)
					}
					if mode == "already-canceled" {
						githubCalls := atomic.LoadInt32(&m.GitHub.(*fakeGitHub).runnerByNameCalls)
						if l.globalCalls+l.instanceCalls+l.processCalls+l.inventoryCalls != 0 || githubCalls != 0 {
							t.Fatalf("already-canceled visit entered dependencies: global=%d instance=%d process=%d inventory=%d github=%d", l.globalCalls, l.instanceCalls, l.processCalls, l.inventoryCalls, githubCalls)
						}
					}
					if phase == "global-admission" && !s.globalAt.IsZero() {
						t.Fatal("late global success was cached")
					}
					if mode != "already-canceled" && s.resume != "" {
						t.Fatal("failed phase retained resume preference")
					}
					// A fresh attempt succeeds and only that attempt advances evidence.
					l.onCall = nil
					done, alive, _, _, err := m.visitRunnerHealth(context.Background(), &s, p, active["a"])
					if err != nil || (phase == "process" && (!done || !alive)) {
						t.Fatalf("retry failed: done=%t alive=%t err=%v", done, alive, err)
					}
					switch phase {
					case "global-admission":
						if s.globalAt != time.Now() || p.stage != 0 {
							t.Fatal("successful retry did not cache global evidence")
						}
						attempts := l.globalCalls
						_, _, _, _, err = m.visitRunnerHealth(context.Background(), &s, s.progress["b"], active["b"])
						if err != nil || l.globalCalls != attempts || s.progress["b"].stage != 1 {
							t.Fatal("peer did not reuse successful global admission")
						}
					case "instance-admission":
						if p.stage != 1 || p.admittedAt != time.Now() {
							t.Fatal("successful retry did not cache instance evidence")
						}
					case "github":
						if p.stage != 2 {
							t.Fatal("successful retry did not advance to process")
						}
					}
				})
			})
		}
	}
}
