package pool

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
	gh "github.com/solutionforest/ephemeral-action-runner/internal/github"
	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
)

type Manager struct {
	Config      config.Config
	Provider    provider.Provider
	GitHub      GitHubClient
	ProjectRoot string
	DryRun      bool
}

type GitHubClient interface {
	OrganizationURL() string
	RegistrationToken(ctx context.Context) (gh.RegistrationToken, error)
	ListRunners(ctx context.Context) ([]gh.Runner, error)
	RunnerByName(ctx context.Context, name string) (gh.Runner, bool, error)
	WaitRunnerOnlineIdle(ctx context.Context, name string, timeout time.Duration) (gh.Runner, error)
	DeleteRunnerIfExists(ctx context.Context, id int64) error
	DeleteRunnersByPrefix(ctx context.Context, prefix string) ([]gh.Runner, error)
}

type VerifyOptions struct {
	Instances    int
	RegisterOnly bool
	Cleanup      bool
}

type RunOptions struct {
	Instances        int
	Register         bool
	KeepOnExit       bool
	ReplaceCompleted bool
	MonitorInterval  time.Duration
}

type ProvisionedInstance struct {
	Name         string
	IP           string
	LogPath      string
	GuestLogPath string
	RunnerID     int64
}

func (m *Manager) Verify(ctx context.Context, opts VerifyOptions) error {
	if opts.Instances <= 0 {
		opts.Instances = m.Config.Pool.MinIdle
	}
	if opts.Instances > m.Config.Pool.MaxInstances {
		return fmt.Errorf("instances %d exceeds pool.maxInstances %d", opts.Instances, m.Config.Pool.MaxInstances)
	}
	names := RunnerNames(m.Config.Pool.NamePrefix, opts.Instances, time.Now())
	fmt.Printf("verifying %d instance(s): %s\n", opts.Instances, strings.Join(names, ", "))
	fmt.Printf("source image: %s\n", m.Config.Provider.SourceImage)
	if opts.RegisterOnly {
		fmt.Printf("registration: GitHub ephemeral runners for %s\n", m.Config.GitHub.Organization)
	} else {
		fmt.Printf("registration: skipped\n")
	}
	var (
		mu      sync.Mutex
		created []ProvisionedInstance
		wg      sync.WaitGroup
		errOnce error
	)
	for _, name := range names {
		name := name
		wg.Add(1)
		go func() {
			defer wg.Done()
			vm, err := m.provisionOne(ctx, name, opts.RegisterOnly)
			mu.Lock()
			defer mu.Unlock()
			if err != nil && errOnce == nil {
				errOnce = err
			}
			if vm.Name != "" {
				created = append(created, vm)
			}
		}()
	}
	wg.Wait()
	if opts.Cleanup {
		fmt.Printf("cleanup: removing instances and GitHub runners with prefix %q\n", m.Config.Pool.NamePrefix)
		if err := m.Cleanup(ctx); err != nil && errOnce == nil {
			errOnce = err
		}
	}
	if errOnce == nil {
		fmt.Printf("verify complete: %d instance(s) validated", len(created))
		if opts.RegisterOnly {
			fmt.Printf(" and registered online/idle")
		}
		if opts.Cleanup {
			fmt.Printf("; cleanup complete")
		}
		fmt.Printf("\n")
		for _, vm := range created {
			fmt.Printf("  %s ip=%s providerLog=%s guestLog=%s", vm.Name, emptyDash(vm.IP), vm.LogPath, vm.GuestLogPath)
			if vm.RunnerID != 0 {
				fmt.Printf(" runnerID=%d", vm.RunnerID)
			}
			fmt.Printf("\n")
		}
	}
	return errOnce
}

func (m *Manager) RunPool(ctx context.Context, opts RunOptions) error {
	if opts.Instances <= 0 {
		opts.Instances = m.Config.Pool.MinIdle
	}
	if opts.Instances > m.Config.Pool.MaxInstances {
		return fmt.Errorf("instances %d exceeds pool.maxInstances %d", opts.Instances, m.Config.Pool.MaxInstances)
	}
	if opts.MonitorInterval <= 0 {
		opts.MonitorInterval = 15 * time.Second
	}
	active := make(map[string]ProvisionedInstance)
	sequence := 1
	cleanup := func() error {
		if opts.KeepOnExit {
			return nil
		}
		cctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		return m.Cleanup(cctx)
	}
	for len(active) < opts.Instances {
		vm, err := m.provisionOne(ctx, RunnerName(m.Config.Pool.NamePrefix, sequence, time.Now()), opts.Register)
		sequence++
		if err != nil {
			_ = cleanup()
			return err
		}
		active[vm.Name] = vm
		fmt.Printf("%s online at %s providerLog=%s guestLog=%s\n", vm.Name, vm.IP, vm.LogPath, vm.GuestLogPath)
	}
	if !opts.Register || !opts.ReplaceCompleted {
		fmt.Println("pool is running; press Ctrl-C to stop")
		<-ctx.Done()
		return cleanup()
	}
	fmt.Printf("pool supervisor is running; monitoring every %s; press Ctrl-C to stop\n", opts.MonitorInterval)
	ticker := time.NewTicker(opts.MonitorInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return cleanup()
		case <-ticker.C:
			for name, vm := range active {
				alive, reason, err := m.runnerAlive(ctx, vm)
				if err != nil {
					fmt.Printf("[%s] liveness check warning: %v\n", name, err)
					continue
				}
				if alive {
					continue
				}
				fmt.Printf("[%s] runner is finished or unhealthy: %s\n", name, reason)
				if err := m.retireInstance(context.Background(), vm, reason); err != nil {
					fmt.Printf("[%s] retirement warning: %v\n", name, err)
					continue
				}
				delete(active, name)
			}
			for len(active) < opts.Instances {
				select {
				case <-ctx.Done():
					return cleanup()
				default:
				}
				name := RunnerName(m.Config.Pool.NamePrefix, sequence, time.Now())
				sequence++
				fmt.Printf("[%s] creating replacement runner\n", name)
				vm, err := m.provisionOne(ctx, name, opts.Register)
				if err != nil {
					fmt.Printf("[%s] replacement failed: %v\n", name, err)
					break
				}
				active[vm.Name] = vm
				fmt.Printf("%s online at %s providerLog=%s guestLog=%s\n", vm.Name, vm.IP, vm.LogPath, vm.GuestLogPath)
			}
		}
	}
}

func (m *Manager) ProvisionPool(ctx context.Context, instances int, register bool) ([]ProvisionedInstance, error) {
	if instances <= 0 {
		instances = m.Config.Pool.MinIdle
	}
	names := RunnerNames(m.Config.Pool.NamePrefix, instances, time.Now())
	out := make([]ProvisionedInstance, 0, len(names))
	for _, name := range names {
		vm, err := m.provisionOne(ctx, name, register)
		if err != nil {
			return out, err
		}
		out = append(out, vm)
	}
	return out, nil
}

func (m *Manager) Cleanup(ctx context.Context) error {
	var firstErr error
	vms, err := m.Provider.List(ctx)
	if err != nil {
		firstErr = err
	}
	for _, vm := range vms {
		if !HasPrefix(vm.Name, m.Config.Pool.NamePrefix) {
			continue
		}
		fmt.Printf("cleanup: deleting instance %s\n", vm.Name)
		stopCtx, stopCancel := context.WithTimeout(ctx, 60*time.Second)
		_ = m.Provider.Stop(stopCtx, vm.Name)
		stopCancel()
		deleteCtx, deleteCancel := context.WithTimeout(ctx, 60*time.Second)
		if err := m.Provider.Delete(deleteCtx, vm.Name); err != nil && firstErr == nil {
			firstErr = err
		}
		deleteCancel()
	}
	if m.GitHub != nil {
		deleteCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		deleted, err := m.GitHub.DeleteRunnersByPrefix(deleteCtx, m.Config.Pool.NamePrefix)
		if err != nil && firstErr == nil {
			firstErr = err
		}
		for _, runner := range deleted {
			fmt.Printf("cleanup: deleted GitHub runner %s id=%d\n", runner.Name, runner.ID)
		}
	}
	return firstErr
}

func (m *Manager) Status(ctx context.Context) (string, error) {
	var b strings.Builder
	vms, err := m.Provider.List(ctx)
	if err != nil {
		return "", err
	}
	b.WriteString("Instances:\n")
	for _, vm := range vms {
		if HasPrefix(vm.Name, m.Config.Pool.NamePrefix) {
			fmt.Fprintf(&b, "  %s\t%s\n", vm.Name, vm.State)
		}
	}
	if m.GitHub != nil {
		runners, err := m.GitHub.ListRunners(ctx)
		if err != nil {
			return b.String(), err
		}
		b.WriteString("GitHub runners:\n")
		for _, runner := range runners {
			if HasPrefix(runner.Name, m.Config.Pool.NamePrefix) {
				fmt.Fprintf(&b, "  %s\tstatus=%s\tbusy=%t\n", runner.Name, runner.Status, runner.Busy)
			}
		}
	}
	return b.String(), nil
}

func (m *Manager) provisionOne(ctx context.Context, name string, register bool) (ProvisionedInstance, error) {
	logPath := config.ProjectPath(m.ProjectRoot, m.Config.Pool.LogDir)
	logPath = filepath.Join(logPath, name+"."+m.Config.Provider.Type+".log")
	guestLogPath := filepath.Join(config.ProjectPath(m.ProjectRoot, m.Config.Pool.LogDir), name+".guest.log")
	vm := ProvisionedInstance{Name: name, LogPath: logPath, GuestLogPath: guestLogPath}
	if err := os.MkdirAll(filepath.Dir(logPath), 0755); err != nil {
		return vm, err
	}
	fmt.Printf("[%s] cloning %s\n", name, m.Config.Provider.SourceImage)
	if err := m.Provider.Clone(ctx, m.Config.Provider.SourceImage, name); err != nil {
		return vm, err
	}
	fmt.Printf("[%s] starting instance\n", name)
	if _, err := m.Provider.Start(ctx, name, provider.StartOptions{Network: m.Config.Provider.Network, LogPath: logPath}); err != nil {
		return vm, err
	}
	ip, err := m.Provider.IP(ctx, name, m.Config.Timeouts.BootSeconds)
	if err != nil {
		return vm, err
	}
	vm.IP = ip
	fmt.Printf("[%s] reachable at %s\n", name, ip)
	fmt.Printf("[%s] validating runner runtime\n", name)
	if err := m.validateRuntime(ctx, name); err != nil {
		return vm, err
	}
	fmt.Printf("[%s] runtime validation passed\n", name)
	if register {
		if m.GitHub == nil {
			if m.DryRun {
				fmt.Printf("[dry-run] would register GitHub runner %s with labels %s\n", name, strings.Join(m.Config.Runner.Labels, ","))
				return vm, nil
			}
			return vm, fmt.Errorf("github client is required for registration")
		}
		fmt.Printf("[%s] requesting GitHub registration token\n", name)
		token, err := m.GitHub.RegistrationToken(ctx)
		if err != nil {
			return vm, err
		}
		env := map[string]string{
			"RUNNER_URL":       m.GitHub.OrganizationURL(),
			"RUNNER_TOKEN":     token.Token,
			"RUNNER_NAME":      name,
			"RUNNER_LABELS":    strings.Join(m.Config.Runner.Labels, ","),
			"RUNNER_EPHEMERAL": fmt.Sprintf("%t", m.Config.Runner.Ephemeral),
		}
		if _, err := m.execGuest(ctx, name, []string{"sudo", "-E", "bash", "/opt/epar/configure-runner.sh"}, provider.ExecOptions{Env: env}); err != nil {
			return vm, err
		}
		fmt.Printf("[%s] starting runner service\n", name)
		if _, err := m.execGuest(ctx, name, []string{"sudo", "bash", "/opt/epar/run-runner.sh"}, provider.ExecOptions{}); err != nil {
			return vm, err
		}
		fmt.Printf("[%s] waiting for GitHub online/idle\n", name)
		runner, err := m.GitHub.WaitRunnerOnlineIdle(ctx, name, time.Duration(m.Config.Timeouts.GitHubOnlineSeconds)*time.Second)
		if err != nil {
			return vm, err
		}
		vm.RunnerID = runner.ID
		fmt.Printf("[%s] GitHub runner online/idle id=%d\n", name, runner.ID)
	}
	return vm, nil
}

func (m *Manager) runnerAlive(ctx context.Context, vm ProvisionedInstance) (bool, string, error) {
	if m.GitHub != nil {
		runner, found, err := m.GitHub.RunnerByName(ctx, vm.Name)
		if err != nil {
			return true, "", err
		}
		if !found {
			return false, "GitHub runner record is gone", nil
		}
		if runner.Busy {
			return true, "", nil
		}
		if runner.Status != "online" {
			return false, fmt.Sprintf("GitHub runner status is %q", runner.Status), nil
		}
	}
	checkCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	_, serviceErr := m.Provider.Exec(checkCtx, vm.Name, provider.ShellCommand("systemctl is-active --quiet actions-runner.service"), provider.ExecOptions{})
	if serviceErr != nil {
		return false, "actions-runner.service is no longer active", nil
	}
	return true, "", nil
}

func (m *Manager) retireInstance(ctx context.Context, vm ProvisionedInstance, reason string) error {
	fmt.Printf("[%s] retiring instance: %s\n", vm.Name, reason)
	var firstErr error
	if m.GitHub != nil && vm.RunnerID != 0 {
		deleteCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		if err := m.GitHub.DeleteRunnerIfExists(deleteCtx, vm.RunnerID); err != nil {
			cancel()
			return err
		}
		cancel()
	}
	stopCtx, stopCancel := context.WithTimeout(ctx, 60*time.Second)
	_ = m.Provider.Stop(stopCtx, vm.Name)
	stopCancel()
	deleteCtx, deleteCancel := context.WithTimeout(ctx, 60*time.Second)
	if err := m.Provider.Delete(deleteCtx, vm.Name); err != nil && firstErr == nil {
		firstErr = err
	}
	deleteCancel()
	return firstErr
}

func (m *Manager) validateRuntime(ctx context.Context, name string) error {
	_, err := m.execGuest(ctx, name, []string{"sudo", "bash", "/opt/epar/validate-runtime.sh"}, provider.ExecOptions{})
	return err
}

func (m *Manager) execGuest(ctx context.Context, name string, cmd []string, opts provider.ExecOptions) (provider.ExecResult, error) {
	timeout := time.Duration(m.Config.Timeouts.CommandSeconds) * time.Second
	if timeout <= 0 {
		timeout = 15 * time.Minute
	}
	if opts.LogPath == "" {
		opts.LogPath = filepath.Join(config.ProjectPath(m.ProjectRoot, m.Config.Pool.LogDir), name+".guest.log")
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return m.Provider.Exec(cctx, name, cmd, opts)
}

func emptyDash(value string) string {
	if value == "" {
		return "-"
	}
	return value
}
