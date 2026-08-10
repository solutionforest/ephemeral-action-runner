package pool

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
	"github.com/solutionforest/ephemeral-action-runner/internal/dependency"
	gh "github.com/solutionforest/ephemeral-action-runner/internal/github"
	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
)

func outageTestManager(t *testing.T) *Manager {
	t.Helper()
	root := t.TempDir()
	configPath := filepath.Join(root, ".local", "config.yml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte("test: config\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &Manager{
		ProjectRoot: root,
		ConfigPath:  configPath,
		Config: config.Config{
			Image: config.ImageConfig{UpdateFrequency: config.ImageUpdateFrequencyManual},
			Pool: config.PoolConfig{
				ReplacementRetryInitialSeconds: 1,
				ReplacementRetryMaxSeconds:     1,
				ReplacementRetryMultiplier:     2,
				ReplacementRetryJitterPercent:  0,
			},
		},
		randomFloat64: func() float64 { return 0.5 },
	}
}

func TestRunExternalOutageStageBoundedDeadlineIssuesNoExtraRequest(t *testing.T) {
	manager := outageTestManager(t)
	policy, err := ParseExternalOutageRetryPolicy("25ms")
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.ConfigureExternalOutageRetry(policy); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	err = manager.RunExternalOutageStage(context.Background(), "initial-reconciliation", func(context.Context) error {
		calls.Add(1)
		return dependency.NewHTTPFailure("github", "list runners", dependency.HTTPMetadata{StatusCode: http.StatusServiceUnavailable}, errors.New("unavailable"))
	})
	var exhausted *dependency.ExhaustedError
	if !errors.As(err, &exhausted) {
		t.Fatalf("error = %v, want bounded exhaustion", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("operation calls = %d, want exactly one before deadline", calls.Load())
	}
	statePath, err := dependency.ExternalOutageStatePath(manager.ProjectRoot, manager.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	state, err := dependency.ReadIncidentState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if state.Active || state.Outcome != "exhausted" || state.Attempt != 1 {
		t.Fatalf("state = %#v", state)
	}
}

func TestRunExternalOutageStageKeepsGlobalIncidentActiveUntilHealthyRecovery(t *testing.T) {
	manager := outageTestManager(t)
	policy, err := ParseExternalOutageRetryPolicy("2s")
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.ConfigureExternalOutageRetry(policy); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	err = manager.RunExternalOutageStage(context.Background(), "runner-group-security-preflight", func(context.Context) error {
		if calls.Add(1) == 1 {
			return dependency.NewHTTPFailure("github", "runner groups", dependency.HTTPMetadata{StatusCode: http.StatusServiceUnavailable}, errors.New("unavailable"))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	supervisor, err := manager.externalOutageSupervisor()
	if err != nil {
		t.Fatal(err)
	}
	if state := supervisor.State(); !state.Active || state.Attempt != 1 {
		t.Fatalf("incident reset after an intermediate stage succeeded: %#v", state)
	}
	if err := manager.markExternalOutageRecovered(); err != nil {
		t.Fatal(err)
	}
	if state := supervisor.State(); state.Active || state.Outcome != "recovered" {
		t.Fatalf("incident did not recover at healthy capacity: %#v", state)
	}
}

func TestRunExternalOutageStageCancelsAttemptAtOriginalIncidentDeadline(t *testing.T) {
	manager := outageTestManager(t)
	policy, err := ParseExternalOutageRetryPolicy("1200ms")
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.ConfigureExternalOutageRetry(policy); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	started := time.Now()
	err = manager.RunExternalOutageStage(context.Background(), "reconcile", func(attemptCtx context.Context) error {
		if calls.Add(1) == 1 {
			return dependency.NewTransient("list runners", errors.New("offline"))
		}
		<-attemptCtx.Done()
		return attemptCtx.Err()
	})
	var exhausted *dependency.ExhaustedError
	if !errors.As(err, &exhausted) {
		t.Fatalf("error = %v, want exhausted", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("operation calls = %d, want initial failure and one deadline-bounded retry", calls.Load())
	}
	if elapsed := time.Since(started); elapsed > 2500*time.Millisecond {
		t.Fatalf("bounded retry ran past the incident deadline: %s", elapsed)
	}
}

func TestRunExternalOutageStageDoesNotRetryTerminalAuthorization(t *testing.T) {
	manager := outageTestManager(t)
	policy, err := ParseExternalOutageRetryPolicy("continuous")
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.ConfigureExternalOutageRetry(policy); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	terminal := dependency.NewHTTPFailure("registry", "pull", dependency.HTTPMetadata{StatusCode: http.StatusForbidden}, errors.New("forbidden"))
	err = manager.RunExternalOutageStage(context.Background(), "required-image-assurance", func(context.Context) error {
		calls.Add(1)
		return terminal
	})
	if !errors.Is(err, terminal) || calls.Load() != 1 {
		t.Fatalf("error=%v calls=%d, want one terminal attempt", err, calls.Load())
	}
}

func TestRunExternalOutageStageRequiresTypedTransientFailure(t *testing.T) {
	manager := outageTestManager(t)
	policy, err := ParseExternalOutageRetryPolicy("continuous")
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.ConfigureExternalOutageRetry(policy); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	bare := &net.DNSError{Err: "no such host", Name: "api.example.test"}
	err = manager.RunExternalOutageStage(context.Background(), "preflight", func(context.Context) error {
		calls.Add(1)
		return bare
	})
	if !errors.Is(err, bare) || calls.Load() != 1 {
		t.Fatalf("error=%v calls=%d, want one unsupervised bare failure", err, calls.Load())
	}
}

func TestRunPoolSupervisesTransientRunnerGroupPreflightFailure(t *testing.T) {
	github := &fakeGitHub{policyErr: &gh.HTTPError{Method: http.MethodGet, Path: "/orgs/example/actions/runner-groups", StatusCode: http.StatusServiceUnavailable, Cause: errors.New("unavailable")}}
	manager := newRegisteredTestManager(t, &fakeProvider{}, github)
	manager.ConfigPath = filepath.Join(manager.ProjectRoot, "config.yml")
	if err := os.WriteFile(manager.ConfigPath, []byte("config\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	policy, err := ParseExternalOutageRetryPolicy("25ms")
	if err != nil {
		t.Fatal(err)
	}
	err = manager.RunPool(context.Background(), RunOptions{Instances: 1, Register: true, KeepOnExit: true, PoolLockHeld: true, ExternalOutageRetry: policy})
	var exhausted *dependency.ExhaustedError
	if !errors.As(err, &exhausted) {
		t.Fatalf("RunPool() error = %v, want supervised bounded exhaustion", err)
	}
	if got := atomic.LoadInt32(&github.policyCalls); got != 1 {
		t.Fatalf("runner-group policy calls = %d, want one request before bounded deadline", got)
	}
}

func TestRunPoolAcquiresControllerLockBeforeDurableSupervision(t *testing.T) {
	github := &fakeGitHub{}
	manager := newRegisteredTestManager(t, &fakeProvider{}, github)
	manager.ConfigPath = filepath.Join(manager.ProjectRoot, "config.yml")
	if err := os.WriteFile(manager.ConfigPath, []byte("config\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	probe, err := manager.AcquirePoolControllerLock()
	if err != nil {
		t.Fatalf("initial lock probe failed: %v", err)
	}
	if err := probe.Close(); err != nil {
		t.Fatal(err)
	}
	terminal := errors.New("stop after lock observation")
	lockObserved := false
	github.policyFunc = func(context.Context, string, config.RunnerGroupSecurityConfig) (gh.RunnerGroupPolicyResult, error) {
		duplicate, lockErr := manager.AcquirePoolControllerLock()
		if lockErr == nil {
			_ = duplicate.Close()
			return gh.RunnerGroupPolicyResult{}, errors.New("runner-group preflight ran before the pool-controller lock was held")
		}
		lockObserved = true
		return gh.RunnerGroupPolicyResult{}, terminal
	}
	policy, err := ParseExternalOutageRetryPolicy("continuous")
	if err != nil {
		t.Fatal(err)
	}
	err = manager.RunPool(context.Background(), RunOptions{Instances: 1, Register: true, KeepOnExit: true, ExternalOutageRetry: policy})
	if !errors.Is(err, terminal) {
		t.Fatalf("RunPool() error = %v, want terminal observation error", err)
	}
	if !lockObserved {
		t.Fatal("runner-group preflight did not observe the held pool-controller lock")
	}
}

func TestTerminalFailureDuringCooldownReturnsImmediately(t *testing.T) {
	manager := outageTestManager(t)
	policy, err := ParseExternalOutageRetryPolicy("continuous")
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.ConfigureExternalOutageRetry(policy); err != nil {
		t.Fatal(err)
	}
	supervisor, err := manager.externalOutageSupervisor()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := supervisor.Schedule("reconcile", dependency.NewTransient("list runners", errors.New("offline"))); err != nil {
		t.Fatal(err)
	}
	terminal := errors.New("invalid local configuration")
	started := time.Now()
	handled, got := manager.deferExternalOutage("provision", terminal)
	if handled || !errors.Is(got, terminal) {
		t.Fatalf("handled=%t error=%v, want immediate terminal error", handled, got)
	}
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Fatalf("terminal error waited for outage cooldown: %s", elapsed)
	}
}

func TestExternalOutageExhaustionPerformsExactCleanup(t *testing.T) {
	provider := &fakeProvider{instances: []provider.Instance{{Name: "epar-test-owned", State: "running"}, {Name: "other-pool", State: "running"}}}
	manager := newRegisteredTestManager(t, provider, &fakeGitHub{})
	exhausted := &dependency.ExhaustedError{State: dependency.IncidentState{SchemaVersion: dependency.IncidentStateSchemaVersion, Outcome: "exhausted", Stage: "initial-reconciliation"}}
	err := manager.CleanupAfterExternalOutageExhaustion(exhausted, false)
	var gotExhausted *dependency.ExhaustedError
	if !errors.As(err, &gotExhausted) {
		t.Fatalf("cleanup result = %v, want original exhaustion", err)
	}
	if got := atomic.LoadInt32(&provider.deleteCalls); got != 1 {
		t.Fatalf("provider delete calls = %d, want exact cleanup of only the owned prefix", got)
	}
}

func TestStatusReturnsLocalSupervisionStateBeforeProviderFailure(t *testing.T) {
	manager := outageTestManager(t)
	manager.Provider = &fakeProvider{listErr: errors.New("local provider inventory unavailable")}
	policy, err := ParseExternalOutageRetryPolicy("continuous")
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.ConfigureExternalOutageRetry(policy); err != nil {
		t.Fatal(err)
	}
	supervisor, err := manager.externalOutageSupervisor()
	if err != nil {
		t.Fatal(err)
	}
	_, err = supervisor.Schedule("initial-reconciliation", dependency.NewTransient("list runners", errors.New("github unavailable")))
	if err != nil {
		t.Fatal(err)
	}
	status, statusErr := manager.Status(context.Background())
	if statusErr == nil || !strings.Contains(statusErr.Error(), "local provider inventory unavailable") {
		t.Fatalf("Status() error = %v", statusErr)
	}
	if !strings.HasPrefix(status, "External outage retry:\n  state=active") || !strings.Contains(status, "stage=initial-reconciliation") {
		t.Fatalf("partial status omitted local supervision state:\n%s", status)
	}
}

func TestExternalOutageStatePathIsolatedByCanonicalConfig(t *testing.T) {
	root := t.TempDir()
	first := filepath.Join(root, ".local", "first.yml")
	second := filepath.Join(root, ".local", "second.yml")
	if err := os.MkdirAll(filepath.Dir(first), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{first, second} {
		if err := os.WriteFile(path, []byte("config\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	firstState, err := dependency.ExternalOutageStatePath(root, first)
	if err != nil {
		t.Fatal(err)
	}
	secondState, err := dependency.ExternalOutageStatePath(root, second)
	if err != nil {
		t.Fatal(err)
	}
	if firstState == secondState {
		t.Fatalf("distinct canonical configs shared state path %q", firstState)
	}
}

func TestStatusMarksDeadSupervisionOwnerAsStale(t *testing.T) {
	manager := outageTestManager(t)
	manager.Provider = &fakeProvider{}
	statePath, err := dependency.ExternalOutageStatePath(manager.ProjectRoot, manager.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	supervisor, err := dependency.NewSupervisor(dependency.SupervisorOptions{Policy: dependency.NewPolicy(dependency.PolicyContinuous, 0), Backoff: dependency.DefaultBackoffSettings(), Path: statePath, PID: 2147483000})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := supervisor.Schedule("reconcile", dependency.NewTransient("list runners", errors.New("offline"))); err != nil {
		t.Fatal(err)
	}
	status, err := manager.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(status, "ownerState=stale") {
		t.Fatalf("status did not mark dead owner stale:\n%s", status)
	}
}

func TestExternalOutageCooldownIsCancelable(t *testing.T) {
	manager := outageTestManager(t)
	policy, err := ParseExternalOutageRetryPolicy("continuous")
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.ConfigureExternalOutageRetry(policy); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	err = manager.RunExternalOutageStage(ctx, "preflight", func(context.Context) error {
		return dependency.NewTransient("preflight", errors.New("offline"))
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context cancellation", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("unexpected elapsed %s", elapsed)
	}
}

func TestExternalOutageCooldownSuppressesRemoteAllocationButKeepsLocalHousekeeping(t *testing.T) {
	p := &fakeProvider{instances: []provider.Instance{{Name: "epar-test-existing", State: "running"}}}
	g := &fakeGitHub{runner: gh.Runner{Name: "epar-test-existing", ID: 1, Status: "online"}, found: true}
	g.listFunc = func(context.Context) ([]gh.Runner, error) {
		if atomic.LoadInt32(&g.listCalls) == 1 {
			return []gh.Runner{{Name: "epar-test-existing", ID: 1, Status: "online"}}, nil
		}
		p.mu.Lock()
		if len(p.instances) > 0 {
			p.instances[0].State = "stopped"
		}
		p.mu.Unlock()
		return nil, &gh.HTTPError{Method: http.MethodGet, Path: "/orgs/example/actions/runners", StatusCode: http.StatusServiceUnavailable, Cause: errors.New("unavailable")}
	}
	manager := newRegisteredTestManager(t, p, g)
	manager.Config.Pool.ReplacementRetryInitialSeconds = 1
	manager.Config.Pool.ReplacementRetryMaxSeconds = 1
	manager.ConfigPath = filepath.Join(manager.ProjectRoot, "config.yml")
	if err := os.WriteFile(manager.ConfigPath, []byte("config\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	policy, err := ParseExternalOutageRetryPolicy("continuous")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if err := manager.RunPool(ctx, RunOptions{Instances: 1, Register: true, KeepOnExit: true, ReplaceCompleted: true, MonitorInterval: time.Millisecond, PoolLockHeld: true, ExternalOutageRetry: policy}); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&g.listCalls); got != 2 {
		t.Fatalf("GitHub ListRunners calls = %d, want initial success plus one failed attempt", got)
	}
	if got := atomic.LoadInt32(&p.listCalls); got <= 2 {
		t.Fatalf("local List calls = %d, want repeated local housekeeping", got)
	}
	if got := atomic.LoadInt32(&p.deleteCalls); got != 1 {
		t.Fatalf("local delete calls = %d, want exact stopped-instance cleanup", got)
	}
	if got := atomic.LoadInt32(&p.cloneCalls); got != 0 {
		t.Fatalf("clone calls = %d, want remote allocation suppressed", got)
	}
}

func TestInitialReconciliationRecoversInPlaceWithoutAllocation(t *testing.T) {
	p := &fakeProvider{instances: []provider.Instance{{Name: "epar-test-existing", State: "running"}}}
	g := &fakeGitHub{runner: gh.Runner{Name: "epar-test-existing", ID: 1, Status: "online"}, found: true}
	g.listFunc = func(context.Context) ([]gh.Runner, error) {
		if atomic.LoadInt32(&g.listCalls) == 1 {
			return nil, &gh.HTTPError{Method: http.MethodGet, Path: "/orgs/example/actions/runners", StatusCode: http.StatusServiceUnavailable, Cause: errors.New("unavailable")}
		}
		return []gh.Runner{{Name: "epar-test-existing", ID: 1, Status: "online"}}, nil
	}
	manager := newRegisteredTestManager(t, p, g)
	manager.Config.Pool.ReplacementRetryInitialSeconds = 1
	manager.Config.Pool.ReplacementRetryMaxSeconds = 1
	manager.ConfigPath = filepath.Join(manager.ProjectRoot, "config.yml")
	if err := os.WriteFile(manager.ConfigPath, []byte("config\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	policy, err := ParseExternalOutageRetryPolicy("3s")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1200*time.Millisecond)
	defer cancel()
	if err := manager.RunPool(ctx, RunOptions{Instances: 1, Register: true, KeepOnExit: true, ReplaceCompleted: true, MonitorInterval: 50 * time.Millisecond, PoolLockHeld: true, ExternalOutageRetry: policy}); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&g.listCalls); got < 2 {
		t.Fatalf("GitHub ListRunners calls = %d, want failed attempt and in-place retry", got)
	}
	if got := atomic.LoadInt32(&p.cloneCalls); got != 0 {
		t.Fatalf("clone calls = %d, want recovered existing capacity without allocation", got)
	}
	statePath, err := dependency.ExternalOutageStatePath(manager.ProjectRoot, manager.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	state, err := dependency.ReadIncidentState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if state.Active || state.Outcome != "recovered" {
		t.Fatalf("incident state = %#v, want recovered", state)
	}
}

func TestPartialInitialCapacitySurvivesRegistrationOutageWithoutCapacitySurge(t *testing.T) {
	p := &fakeProvider{}
	g := &fakeGitHub{}
	runners := func() []gh.Runner {
		p.mu.Lock()
		defer p.mu.Unlock()
		result := make([]gh.Runner, 0, len(p.instances))
		for index, instance := range p.instances {
			result = append(result, gh.Runner{Name: instance.Name, ID: int64(index + 1), Status: "online"})
		}
		return result
	}
	g.listFunc = func(context.Context) ([]gh.Runner, error) { return runners(), nil }
	g.runnerByNameFunc = func(_ context.Context, name string) (gh.Runner, bool, error) {
		for _, runner := range runners() {
			if runner.Name == name {
				return runner, true, nil
			}
		}
		return gh.Runner{}, false, nil
	}
	g.waitFunc = func(_ context.Context, name string, _ time.Duration) (gh.Runner, error) {
		return gh.Runner{Name: name, ID: int64(atomic.LoadInt32(&g.waitOnlineCalls)), Status: "online"}, nil
	}
	g.registrationFunc = func(context.Context) (gh.RegistrationToken, error) {
		if atomic.LoadInt32(&g.registrationCalls) == 2 {
			return gh.RegistrationToken{}, &gh.HTTPError{Method: http.MethodPost, Path: "/registration-token", StatusCode: http.StatusServiceUnavailable, Cause: errors.New("unavailable")}
		}
		return gh.RegistrationToken{Token: "token"}, nil
	}
	manager := newRegisteredTestManager(t, p, g)
	manager.Config.Pool.Instances = 2
	manager.Config.Pool.ReplacementRetryInitialSeconds = 1
	manager.Config.Pool.ReplacementRetryMaxSeconds = 1
	manager.ConfigPath = filepath.Join(manager.ProjectRoot, "config.yml")
	if err := os.WriteFile(manager.ConfigPath, []byte("config\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	policy, err := ParseExternalOutageRetryPolicy("4s")
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.ConfigureExternalOutageRetry(policy); err != nil {
		t.Fatal(err)
	}
	supervisor, err := manager.externalOutageSupervisor()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- manager.RunPool(ctx, RunOptions{Instances: 2, Register: true, KeepOnExit: true, ReplaceCompleted: true, MonitorInterval: 100 * time.Millisecond, PoolLockHeld: true, ExternalOutageRetry: policy})
	}()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	var state dependency.IncidentState
	for state.Active || state.Outcome != "recovered" || atomic.LoadInt32(&g.registrationCalls) < 3 {
		select {
		case runErr := <-done:
			if runErr != nil {
				t.Fatal(runErr)
			}
			t.Fatal("pool stopped before supervised registration recovery completed")
		case <-ctx.Done():
			t.Fatalf("timed out waiting for supervised registration recovery: %v", ctx.Err())
		case <-ticker.C:
			state = supervisor.State()
		}
	}
	cancel()
	select {
	case runErr := <-done:
		if runErr != nil {
			t.Fatal(runErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pool did not stop after acceptance condition was satisfied")
	}
	statePath, err := dependency.ExternalOutageStatePath(manager.ProjectRoot, manager.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	state, err = dependency.ReadIncidentState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&g.registrationCalls); got < 3 {
		t.Fatalf("registration token calls = %d, want success, outage, and supervised retry", got)
	}
	if got := atomic.LoadInt32(&p.maxInventory); got > 2 {
		t.Fatalf("maximum physical inventory = %d, exceeded pool.instances=2", got)
	}
	if state.Active || state.Outcome != "recovered" {
		t.Fatalf("incident state = %#v, want recovered after desired capacity", state)
	}
}
