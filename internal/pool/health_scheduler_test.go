package pool

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	gh "github.com/solutionforest/ephemeral-action-runner/internal/github"
	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
)

type healthTestLifecycle struct {
	provider.Lifecycle
	items                                    []provider.InventoryItem
	globalCalls, instanceCalls, processCalls int
	onCall                                   func(context.Context) error
	stopped                                  bool
}

func (l *healthTestLifecycle) call(ctx context.Context) error {
	if ctx.Err() != nil {
		panic("expired context passed to dependency")
	}
	if l.onCall != nil {
		return l.onCall(ctx)
	}
	return nil
}
func (l *healthTestLifecycle) Inventory(ctx context.Context) ([]provider.InventoryItem, error) {
	if ctx.Err() != nil {
		panic("expired inventory context")
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
	active, l := healthTestPool()
	// Every phase fits individually, while even one full check exceeds a tick.
	l.onCall = func(ctx context.Context) error {
		select {
		case <-time.After(5 * time.Millisecond):
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
	for tick := 0; tick < 100 && len(completed) < 3; tick++ {
		s.sync(active, 0, time.Now(), inactive)
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Millisecond)
		for visits := 0; visits < 10 && ctx.Err() == nil; visits++ {
			name, p := s.next()
			if p == nil {
				break
			}
			done, alive, _, _, err := m.visitRunnerHealth(ctx, &s, p, active[name])
			if err != nil {
				break
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
	}
	if len(completed) != 3 {
		t.Fatalf("starved identities: completed=%v", completed)
	}
	if l.globalCalls != 1 {
		t.Fatalf("global admission repeated %d times", l.globalCalls)
	}
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
	s := healthScheduler{globalAt: time.Now(), phaseDurations: map[string]time.Duration{"instance-admission": time.Minute}}
	p := &healthProgress{}
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
}

func TestHealthStagePreservesNonTrustTimeoutAndShorterParent(t *testing.T) {
	for _, bounded := range []bool{false, true} {
		t.Run(fmt.Sprint("bounded=", bounded), func(t *testing.T) {
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
			if bounded && (remaining <= 0 || remaining > 5*time.Second) {
				t.Fatalf("parent timeout not preserved: %s", remaining)
			}
			if !bounded && (remaining < 29*time.Second || remaining > 30*time.Second) {
				t.Fatalf("ordinary health timeout changed: %s", remaining)
			}
		})
	}
}

func TestHealthFastFailuresDoNotStallPeers(t *testing.T) {
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
	now := time.Now()
	m := &Manager{Lifecycle: l, GitHub: g, now: func() time.Time { return now }}
	s := healthScheduler{}
	inactive := map[string]int{}
	completed := 0
	for tick := 0; tick < 10; tick++ {
		s.sync(active, 0, now, inactive)
		ctx := healthClockContext{Context: context.Background(), now: &now, end: now.Add(hostTrustRefreshInterval / 2)}
		failed := map[string]bool{}
		for visits := 0; visits < 1+3*len(active) && ctx.Err() == nil; visits++ {
			previous := s.cursor
			name, p := s.next(failed)
			if p == nil {
				break
			}
			if !s.fits(ctx, p, now) {
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
		now = now.Add(15 * time.Second)
	}
	if completed != 10 {
		t.Fatalf("healthy peer completed %d/10 rounds", completed)
	}
	s.progress["e"].stage = 2
	inactive["e"] = 1
	s.sync(active, 0, now, inactive, "new-host-trust")
	if s.progress["e"].stage != 0 || inactive["e"] != 0 {
		t.Fatal("current host trust did not invalidate old VM evidence")
	}
}

// Deadline and the explicit scheduler clock use the same simulated timeline.
// The original caller's Err also checks that timeline after every phase.
type healthClockContext struct {
	context.Context
	now *time.Time
	end time.Time
}

func (c healthClockContext) Deadline() (time.Time, bool) {
	return c.end, true
}
func (c healthClockContext) Err() error {
	if !c.now.Before(c.end) {
		return context.DeadlineExceeded
	}
	return nil
}

func TestHealthSlowPoolCompletesAcrossGlobalRefresh(t *testing.T) {
	for _, failing := range []bool{false, true} {
		t.Run(fmt.Sprint("failing-advanced-peer=", failing), func(t *testing.T) {
			active, l := healthTestPool()
			for _, name := range []string{"d", "e"} {
				active[name] = ProvisionedInstance{Name: name, ProviderID: "id-" + name, ProviderOwned: true, RunnerID: 1}
				l.items = append(l.items, provider.InventoryItem{Instance: provider.Instance{Name: name, ProviderID: "id-" + name}})
			}
			now := time.Now()
			s := healthScheduler{}
			inactive := map[string]int{}
			completed := map[string]int{}
			stage := ""
			cost := map[string]time.Duration{"global-admission": 4 * time.Second, "instance-admission": 9 * time.Second, "github": 3 * time.Second, "process": 6 * time.Second}
			l.onCall = func(ctx context.Context) error { now = now.Add(cost[stage]); return ctx.Err() }
			m := &Manager{Lifecycle: l, now: func() time.Time { return now }, GitHub: &fakeGitHub{runnerByNameFunc: func(ctx context.Context, name string) (gh.Runner, bool, error) {
				now = now.Add(cost["github"])
				if failing && name == "a" {
					return gh.Runner{}, false, errors.New("remote unavailable")
				}
				return gh.Runner{ID: 1, Status: "online"}, true, ctx.Err()
			}}}
			for tick := 0; tick < 80; tick++ {
				start := now
				s.sync(active, 0, now, inactive)
				ctx := healthClockContext{Context: context.Background(), now: &now, end: start.Add(15 * time.Second)}
				failed := map[string]bool{}
				for visits := 0; visits < 1+3*len(active) && ctx.Err() == nil; visits++ {
					previous := s.cursor
					name, p := s.next(failed)
					if p == nil {
						break
					}
					if !s.fits(ctx, p, now) {
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
				if now.After(ctx.end) {
					t.Fatal("sweep budget exceeded")
				}
				now = start.Add(15 * time.Second)
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
	active, l := healthTestPool()
	now := time.Now().Add(time.Hour)
	ctx := healthClockContext{Context: context.Background(), now: &now, end: now.Add(15 * time.Second)}
	s := healthScheduler{globalAt: now}
	p := &healthProgress{}
	if !s.fits(ctx, p, now) {
		t.Fatal("fresh budget rejected")
	}
	now = now.Add(9 * time.Second)
	if s.fits(ctx, p, now) {
		t.Fatal("elapsed fake time did not consume the sweep budget")
	}
	s.globalAt = time.Time{}
	l.onCall = func(context.Context) error { now = now.Add(7 * time.Second); return nil }
	m := &Manager{Lifecycle: l, now: func() time.Time { return now }}
	done, _, _, _, err := m.visitRunnerHealth(ctx, &s, p, active["a"])
	if !errors.Is(err, context.DeadlineExceeded) || done || !s.globalAt.IsZero() || p.stage != 0 {
		t.Fatalf("late success advanced evidence: done=%t error=%v global=%v stage=%d", done, err, s.globalAt, p.stage)
	}
}
