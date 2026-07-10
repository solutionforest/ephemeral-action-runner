package pool

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
	gh "github.com/solutionforest/ephemeral-action-runner/internal/github"
	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
)

func TestRunnerAliveKeepsBusyGitHubRunnerWithoutServiceCheck(t *testing.T) {
	provider := &fakeProvider{execErr: errors.New("inactive")}
	github := &fakeGitHub{
		runner: gh.Runner{Name: "epar-test-1", Status: "online", Busy: true},
		found:  true,
	}
	manager := Manager{Provider: provider, GitHub: github}

	alive, reason, err := manager.runnerAlive(context.Background(), ProvisionedInstance{Name: "epar-test-1"})
	if err != nil {
		t.Fatal(err)
	}
	if !alive {
		t.Fatalf("runnerAlive() alive = false, reason = %q", reason)
	}
	if got := atomic.LoadInt32(&provider.execCalls); got != 0 {
		t.Fatalf("service check ran %d time(s), want 0", got)
	}
}

func TestRunnerAliveRetiresIdleRunnerWhenServiceIsInactive(t *testing.T) {
	provider := &fakeProvider{execErr: errors.New("inactive")}
	github := &fakeGitHub{
		runner: gh.Runner{Name: "epar-test-1", Status: "online", Busy: false},
		found:  true,
	}
	manager := Manager{Provider: provider, GitHub: github}

	alive, reason, err := manager.runnerAlive(context.Background(), ProvisionedInstance{Name: "epar-test-1"})
	if err != nil {
		t.Fatal(err)
	}
	if alive {
		t.Fatal("runnerAlive() alive = true, want false")
	}
	if reason != "actions runner process is no longer active" {
		t.Fatalf("reason = %q", reason)
	}
	if got := atomic.LoadInt32(&provider.execCalls); got != 1 {
		t.Fatalf("service check ran %d time(s), want 1", got)
	}
}

func TestRunnerAliveFallsBackToServiceCheckWhenGitHubLivenessHasServerError(t *testing.T) {
	provider := &fakeProvider{}
	github := &fakeGitHub{
		runnerErr: &gh.HTTPError{StatusCode: 500},
	}
	manager := Manager{Provider: provider, GitHub: github}

	alive, reason, err := manager.runnerAlive(context.Background(), ProvisionedInstance{Name: "epar-test-1"})
	if err != nil {
		t.Fatal(err)
	}
	if !alive {
		t.Fatalf("runnerAlive() alive = false, reason = %q", reason)
	}
	if got := atomic.LoadInt32(&provider.execCalls); got != 1 {
		t.Fatalf("service check ran %d time(s), want 1", got)
	}
}

func TestRunnerAliveReturnsNonTransientGitHubLivenessError(t *testing.T) {
	provider := &fakeProvider{}
	github := &fakeGitHub{
		runnerErr: &gh.HTTPError{StatusCode: 403},
	}
	manager := Manager{Provider: provider, GitHub: github}

	alive, reason, err := manager.runnerAlive(context.Background(), ProvisionedInstance{Name: "epar-test-1"})
	if err == nil {
		t.Fatal("runnerAlive() error = nil, want GitHub error")
	}
	if !alive {
		t.Fatalf("runnerAlive() alive = false, reason = %q", reason)
	}
	if got := atomic.LoadInt32(&provider.execCalls); got != 0 {
		t.Fatalf("service check ran %d time(s), want 0", got)
	}
}

func TestRetireInstanceDefersLocalDeleteWhenGitHubDeleteFails(t *testing.T) {
	provider := &fakeProvider{}
	github := &fakeGitHub{deleteErr: errors.New("github runner is currently running a job")}
	manager := Manager{Provider: provider, GitHub: github}

	err := manager.retireInstance(context.Background(), ProvisionedInstance{Name: "epar-test-1", RunnerID: 123}, "done")
	if err == nil {
		t.Fatal("retireInstance() error = nil, want GitHub delete error")
	}
	if got := atomic.LoadInt32(&provider.stopCalls); got != 0 {
		t.Fatalf("Stop called %d time(s), want 0", got)
	}
	if got := atomic.LoadInt32(&provider.deleteCalls); got != 0 {
		t.Fatalf("Delete called %d time(s), want 0", got)
	}
}

func TestRunPoolDoesNotReplaceWhenRetirementIsDeferred(t *testing.T) {
	provider := &fakeProvider{ip: "127.0.0.1"}
	github := &fakeGitHub{
		runner:     gh.Runner{Name: "epar-test-1", ID: 123, Status: "offline"},
		found:      true,
		waitRunner: gh.Runner{Name: "epar-test-1", ID: 123, Status: "online"},
		deleteErr:  errors.New("github runner is currently running a job"),
	}
	manager := Manager{
		Config: config.Config{
			Provider: config.ProviderConfig{SourceImage: "image"},
			Pool:     config.PoolConfig{Instances: 1, NamePrefix: "epar-test", LogDir: t.TempDir()},
			Runner:   config.RunnerConfig{Labels: []string{"self-hosted"}},
		},
		Provider:    provider,
		GitHub:      github,
		ProjectRoot: t.TempDir(),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()

	if err := manager.RunPool(ctx, RunOptions{
		Instances:        1,
		Register:         true,
		KeepOnExit:       true,
		ReplaceCompleted: true,
		MonitorInterval:  5 * time.Millisecond,
	}); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&provider.cloneCalls); got != 1 {
		t.Fatalf("Clone called %d time(s), want 1; deferred retirement should not create replacements", got)
	}
}

func TestRunPoolUsesConfiguredInstancesWhenNoOverride(t *testing.T) {
	provider := &fakeProvider{ip: "127.0.0.1"}
	manager := Manager{
		Config: config.Config{
			Provider: config.ProviderConfig{SourceImage: "image"},
			Pool:     config.PoolConfig{Instances: 2, NamePrefix: "epar-test", LogDir: t.TempDir()},
		},
		Provider:    provider,
		ProjectRoot: t.TempDir(),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	if err := manager.RunPool(ctx, RunOptions{
		Instances:        0,
		Register:         false,
		KeepOnExit:       true,
		ReplaceCompleted: false,
	}); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&provider.cloneCalls); got != 2 {
		t.Fatalf("Clone called %d time(s), want configured instances 2", got)
	}
}

func TestProvisionOneRetriesTransientRuntimeValidationFailure(t *testing.T) {
	oldDelay := runtimeValidationRetryDelay
	runtimeValidationRetryDelay = 0
	t.Cleanup(func() { runtimeValidationRetryDelay = oldDelay })

	provider := &fakeProvider{
		ip:       "127.0.0.1",
		execErrs: []error{errors.New("transient validation failure"), nil},
	}
	manager := Manager{
		Config: config.Config{
			Provider: config.ProviderConfig{SourceImage: "image", Type: "docker-dind"},
			Pool:     config.PoolConfig{Instances: 1, NamePrefix: "epar-test", LogDir: t.TempDir()},
			Timeouts: config.TimeoutConfig{CommandSeconds: 5},
		},
		Provider:    provider,
		ProjectRoot: t.TempDir(),
	}

	if _, err := manager.provisionOne(context.Background(), "epar-test-1", false); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&provider.execCalls); got != 2 {
		t.Fatalf("runtime validation attempts = %d, want 2", got)
	}
}

func TestVerifyCleanupUsesFreshContextAfterCancellation(t *testing.T) {
	provider := &fakeProvider{
		instances: []provider.Instance{
			{Name: "epar-test-1"},
			{Name: "epar-test-unrelated"},
			{Name: "epar-testing-1"},
		},
	}
	manager := Manager{
		Config: config.Config{
			Provider: config.ProviderConfig{SourceImage: "image", Type: "docker-dind"},
			Pool:     config.PoolConfig{Instances: 1, NamePrefix: "epar-test", LogDir: t.TempDir()},
			Timeouts: config.TimeoutConfig{CommandSeconds: 5},
		},
		Provider:    provider,
		ProjectRoot: t.TempDir(),
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := manager.Verify(ctx, VerifyOptions{Instances: 1, Cleanup: true}); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&provider.canceledListCalls); got != 0 {
		t.Fatalf("cleanup List received a canceled context %d time(s)", got)
	}
	if got := atomic.LoadInt32(&provider.stopCalls); got != 2 {
		t.Fatalf("Stop called %d time(s), want 2 matching prefix-boundary instances", got)
	}
	if got := atomic.LoadInt32(&provider.deleteCalls); got != 2 {
		t.Fatalf("Delete called %d time(s), want 2 matching prefix-boundary instances", got)
	}
}

func TestRunPoolCleanupUsesFreshContextAfterCancellation(t *testing.T) {
	provider := &fakeProvider{
		instances: []provider.Instance{{Name: "epar-test-1"}},
	}
	manager := Manager{
		Config: config.Config{
			Provider: config.ProviderConfig{SourceImage: "image", Type: "docker-dind"},
			Pool:     config.PoolConfig{Instances: 1, NamePrefix: "epar-test", LogDir: t.TempDir()},
			Timeouts: config.TimeoutConfig{CommandSeconds: 5},
		},
		Provider:    provider,
		ProjectRoot: t.TempDir(),
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := manager.RunPool(ctx, RunOptions{Instances: 1}); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&provider.canceledListCalls); got != 0 {
		t.Fatalf("cleanup List received a canceled context %d time(s)", got)
	}
	if got := atomic.LoadInt32(&provider.stopCalls); got != 1 {
		t.Fatalf("Stop called %d time(s), want 1", got)
	}
	if got := atomic.LoadInt32(&provider.deleteCalls); got != 1 {
		t.Fatalf("Delete called %d time(s), want 1", got)
	}
}

func TestProvisionOnePassesRunnerRegistrationControlsWithoutPrivateKey(t *testing.T) {
	provider := &fakeProvider{ip: "127.0.0.1"}
	github := &fakeGitHub{
		waitRunner: gh.Runner{Name: "epar-test-1", ID: 123, Status: "online"},
	}
	manager := Manager{
		Config: config.Config{
			GitHub: config.GitHubConfig{PrivateKeyPath: "/secret/app.pem"},
			Provider: config.ProviderConfig{
				SourceImage: "image",
				Type:        "docker-dind",
			},
			Pool: config.PoolConfig{
				Instances:  1,
				NamePrefix: "epar-test",
				LogDir:     t.TempDir(),
			},
			Runner: config.RunnerConfig{
				Labels:          []string{"epar-core-test"},
				Ephemeral:       true,
				Group:           "epar-ci-canary",
				NoDefaultLabels: true,
			},
			Timeouts: config.TimeoutConfig{
				CommandSeconds:      5,
				GitHubOnlineSeconds: 5,
			},
		},
		Provider:    provider,
		GitHub:      github,
		ProjectRoot: t.TempDir(),
	}

	if _, err := manager.provisionOne(context.Background(), "epar-test-1", true); err != nil {
		t.Fatal(err)
	}
	provider.mu.Lock()
	env := provider.configureEnv
	provider.mu.Unlock()
	if env == nil {
		t.Fatal("configure-runner invocation was not captured")
	}
	for key, want := range map[string]string{
		"RUNNER_GROUP":             "epar-ci-canary",
		"RUNNER_NO_DEFAULT_LABELS": "true",
		"RUNNER_LABELS":            "epar-core-test",
		"RUNNER_EPHEMERAL":         "true",
		"RUNNER_TOKEN":             "token",
	} {
		if got := env[key]; got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	for key, value := range env {
		if strings.Contains(strings.ToLower(key), "private") || value == "/secret/app.pem" {
			t.Fatalf("guest registration environment exposes private key through %s", key)
		}
	}
}

type fakeProvider struct {
	execErr  error
	execErrs []error
	ip       string
	mu       sync.Mutex

	configureEnv map[string]string
	instances    []provider.Instance

	cloneCalls        int32
	execCalls         int32
	stopCalls         int32
	deleteCalls       int32
	canceledListCalls int32
}

func (p *fakeProvider) Clone(context.Context, string, string) error {
	atomic.AddInt32(&p.cloneCalls, 1)
	return nil
}

func (p *fakeProvider) Start(context.Context, string, provider.StartOptions) (*provider.RunningProcess, error) {
	return &provider.RunningProcess{}, nil
}

func (p *fakeProvider) Exec(_ context.Context, _ string, command []string, opts provider.ExecOptions) (provider.ExecResult, error) {
	if strings.Contains(strings.Join(command, " "), "configure-runner.sh") {
		p.mu.Lock()
		p.configureEnv = make(map[string]string, len(opts.Env))
		for key, value := range opts.Env {
			p.configureEnv[key] = value
		}
		p.mu.Unlock()
	}
	call := atomic.AddInt32(&p.execCalls, 1)
	if int(call) <= len(p.execErrs) {
		return provider.ExecResult{}, p.execErrs[call-1]
	}
	return provider.ExecResult{}, p.execErr
}

func (p *fakeProvider) IP(context.Context, string, int) (string, error) {
	if p.ip == "" {
		return "127.0.0.1", nil
	}
	return p.ip, nil
}

func (p *fakeProvider) Stop(context.Context, string) error {
	atomic.AddInt32(&p.stopCalls, 1)
	return nil
}

func (p *fakeProvider) Delete(context.Context, string) error {
	atomic.AddInt32(&p.deleteCalls, 1)
	return nil
}

func (p *fakeProvider) List(ctx context.Context) ([]provider.Instance, error) {
	if ctx.Err() != nil {
		atomic.AddInt32(&p.canceledListCalls, 1)
		return nil, ctx.Err()
	}
	return append([]provider.Instance(nil), p.instances...), nil
}

type fakeGitHub struct {
	runner     gh.Runner
	waitRunner gh.Runner
	found      bool
	runnerErr  error
	deleteErr  error
}

func (g *fakeGitHub) OrganizationURL() string {
	return "https://github.test/example"
}

func (g *fakeGitHub) RegistrationToken(context.Context) (gh.RegistrationToken, error) {
	return gh.RegistrationToken{Token: "token"}, nil
}

func (g *fakeGitHub) ListRunners(context.Context) ([]gh.Runner, error) {
	if !g.found {
		return nil, nil
	}
	return []gh.Runner{g.runner}, nil
}

func (g *fakeGitHub) RunnerByName(context.Context, string) (gh.Runner, bool, error) {
	return g.runner, g.found, g.runnerErr
}

func (g *fakeGitHub) WaitRunnerOnlineIdle(context.Context, string, time.Duration) (gh.Runner, error) {
	if g.waitRunner.ID != 0 {
		return g.waitRunner, nil
	}
	return g.runner, nil
}

func (g *fakeGitHub) DeleteRunnerIfExists(context.Context, int64) error {
	return g.deleteErr
}

func (g *fakeGitHub) DeleteRunnersByPrefix(context.Context, string) ([]gh.Runner, error) {
	return nil, nil
}
