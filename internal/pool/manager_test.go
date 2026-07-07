package pool

import (
	"context"
	"errors"
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

type fakeProvider struct {
	execErr  error
	execErrs []error
	ip       string

	cloneCalls  int32
	execCalls   int32
	stopCalls   int32
	deleteCalls int32
}

func (p *fakeProvider) Clone(context.Context, string, string) error {
	atomic.AddInt32(&p.cloneCalls, 1)
	return nil
}

func (p *fakeProvider) Start(context.Context, string, provider.StartOptions) (*provider.RunningProcess, error) {
	return &provider.RunningProcess{}, nil
}

func (p *fakeProvider) Exec(context.Context, string, []string, provider.ExecOptions) (provider.ExecResult, error) {
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

func (p *fakeProvider) List(context.Context) ([]provider.Instance, error) {
	return nil, nil
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
