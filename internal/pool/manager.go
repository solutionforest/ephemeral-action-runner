package pool

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
	gh "github.com/solutionforest/ephemeral-action-runner/internal/github"
	"github.com/solutionforest/ephemeral-action-runner/internal/hosttrust"
	"github.com/solutionforest/ephemeral-action-runner/internal/logging"
	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
)

type Manager struct {
	Config        config.Config
	Provider      provider.Provider
	GitHub        GitHubClient
	ProjectRoot   string
	ConfigPath    string
	DryRun        bool
	Logging       *logging.Runtime
	startupTiming *startupTiming
	transcriptMu  sync.Mutex
	transcripts   map[string]*logging.Transcript

	hostTrustResolver     func(context.Context) (hosttrust.Snapshot, error)
	hostTrustImageEnsurer func(context.Context) error
	hostTrustImageMu      sync.Mutex
}

type GitHubClient interface {
	OrganizationURL() string
	RegistrationToken(ctx context.Context) (gh.RegistrationToken, error)
	ListRunners(ctx context.Context) ([]gh.Runner, error)
	RunnerByName(ctx context.Context, name string) (gh.Runner, bool, error)
	WaitRunnerOnline(ctx context.Context, name string, timeout time.Duration) (gh.Runner, error)
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
	Instances         int
	Register          bool
	KeepOnExit        bool
	ReplaceCompleted  bool
	MonitorInterval   time.Duration
	HostTrustLockHeld bool
}

type ProvisionedInstance struct {
	Name                string
	IP                  string
	LogPath             string
	GuestLogPath        string
	RunnerID            int64
	HostTrustGeneration string
}

var runtimeValidationRetryDelay = 5 * time.Second
var runnerReadinessHealthCheckInterval = 2 * time.Second

const (
	cleanupTimeout                    = 5 * time.Minute
	runnerReadinessDiagnosticsTimeout = 30 * time.Second
	runnerReadinessProbeFailureLimit  = 3
)

func (m *Manager) Verify(ctx context.Context, opts VerifyOptions) error {
	controllerLock, err := m.acquireHostTrustControllerLock()
	if err != nil {
		return err
	}
	if controllerLock != nil {
		defer controllerLock.Close()
	}
	opts.Instances = m.requestedInstances(opts.Instances)
	names := RunnerNames(m.Config.Pool.NamePrefix, opts.Instances, time.Now())
	m.logger().Info("verifying instances", "provider", m.Config.Provider.Type, "operation", "verify", "instances", opts.Instances, "instanceNames", strings.Join(names, ", "), "sourceImage", m.Config.Provider.SourceImage)
	if opts.RegisterOnly {
		m.infof("registration: GitHub ephemeral runners for %s\n", m.Config.GitHub.Organization)
	} else {
		m.infof("registration: skipped\n")
	}
	var (
		mu      sync.Mutex
		created []ProvisionedInstance
		wg      sync.WaitGroup
		errOnce error
	)
	leaseAdd, stopLeaseKeeper := m.startHostTrustLeaseKeeper(ctx)
	for _, name := range names {
		name := name
		wg.Add(1)
		go func() {
			defer wg.Done()
			vm, err := m.provisionOne(ctx, name, opts.RegisterOnly, false)
			mu.Lock()
			defer mu.Unlock()
			if err != nil && errOnce == nil {
				errOnce = err
			}
			if vm.Name != "" {
				created = append(created, vm)
				leaseAdd(vm)
			}
		}()
	}
	wg.Wait()
	stopLeaseKeeper()
	if opts.Cleanup {
		m.infof("cleanup: removing instances and GitHub runners with prefix %q\n", m.Config.Pool.NamePrefix)
		if err := m.cleanupWithFreshContext(); err != nil && errOnce == nil {
			errOnce = err
		}
	}
	if errOnce == nil {
		m.infof("verify complete: %d instance(s) validated", len(created))
		if opts.RegisterOnly {
			m.infof(" and registered online/idle")
		}
		if opts.Cleanup {
			m.infof("; cleanup complete")
		}
		m.infof("\n")
		for _, vm := range created {
			m.infof("  %s ip=%s providerLog=%s guestLog=%s", vm.Name, emptyDash(vm.IP), vm.LogPath, vm.GuestLogPath)
			if vm.RunnerID != 0 {
				m.infof(" runnerID=%d", vm.RunnerID)
			}
			m.infof("\n")
		}
	}
	return errOnce
}

func (m *Manager) RunPool(ctx context.Context, opts RunOptions) error {
	if !opts.HostTrustLockHeld {
		controllerLock, err := m.AcquireHostTrustControllerLock()
		if err != nil {
			return err
		}
		if controllerLock != nil {
			defer controllerLock.Close()
		}
	}
	opts.Instances = m.requestedInstances(opts.Instances)
	if opts.MonitorInterval <= 0 {
		opts.MonitorInterval = 15 * time.Second
	}
	active := make(map[string]ProvisionedInstance)
	sequence := 1
	poolTrustGeneration := ""
	cleanup := func() error {
		if opts.KeepOnExit {
			return nil
		}
		return m.cleanupWithFreshContext()
	}
	leaseAdd, stopLeaseKeeper := m.startHostTrustLeaseKeeper(ctx)
	for len(active) < opts.Instances {
		vm, err := m.provisionOne(ctx, RunnerName(m.Config.Pool.NamePrefix, sequence, time.Now()), opts.Register, opts.Register && opts.ReplaceCompleted)
		sequence++
		if err != nil {
			stopLeaseKeeper()
			_ = cleanup()
			return err
		}
		active[vm.Name] = vm
		leaseAdd(vm)
		if vm.HostTrustGeneration != "" {
			poolTrustGeneration = vm.HostTrustGeneration
		}
		m.infof("%s online at %s providerLog=%s guestLog=%s\n", vm.Name, vm.IP, vm.LogPath, vm.GuestLogPath)
	}
	stopLeaseKeeper()
	if !opts.Register || (!opts.ReplaceCompleted && !m.hostTrustEnabled()) {
		m.infof("pool is running; press Ctrl-C to stop")
		if !m.Config.Logging.RetentionEnabled {
			<-ctx.Done()
			return cleanup()
		}
		retentionTicker := time.NewTicker(time.Duration(m.Config.Logging.RetentionIntervalMinutes) * time.Minute)
		defer retentionTicker.Stop()
		for {
			select {
			case <-ctx.Done():
				return cleanup()
			case <-retentionTicker.C:
				m.pruneLogsBestEffort()
			}
		}
	}
	m.infof("pool supervisor is running; monitoring every %s; press Ctrl-C to stop\n", opts.MonitorInterval)
	tickInterval := opts.MonitorInterval
	if m.hostTrustEnabled() && tickInterval > hostTrustRefreshInterval {
		tickInterval = hostTrustRefreshInterval
	}
	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()
	nextLivenessCheck := time.Now().Add(opts.MonitorInterval)
	nextRetention := time.Now().Add(time.Duration(m.Config.Logging.RetentionIntervalMinutes) * time.Minute)
	nextHostTrustCollection := time.Time{}
	var currentHostTrust hosttrust.Snapshot
	for {
		select {
		case <-ctx.Done():
			return cleanup()
		case <-ticker.C:
			if m.Config.Logging.RetentionEnabled && !time.Now().Before(nextRetention) {
				m.pruneLogsBestEffort()
				nextRetention = time.Now().Add(time.Duration(m.Config.Logging.RetentionIntervalMinutes) * time.Minute)
			}
			trustRetired := 0
			trustCapacityReady := true
			if m.hostTrustEnabled() {
				now := time.Now()
				if currentHostTrust.Generation == "" || !now.Before(nextHostTrustCollection) {
					current, err := m.resolveHostTrust(ctx)
					nextHostTrustCollection = now.Add(m.hostTrustCollectionInterval())
					if err != nil {
						currentHostTrust = hosttrust.Snapshot{}
						m.warnf("host trust refresh warning; existing leases will expire closed: %v\n", err)
					} else {
						ready := true
						if poolTrustGeneration != current.Generation {
							// Stop old-generation assignment before the replacement build.
							// Idle runners are removed now; busy runners keep running but
							// receive a mismatching lease so no subsequent job can start.
							currentHostTrust = current
							trustRetired += m.reconcileHostTrustRunners(ctx, active, current)
							m.infof("host trust generation changed (%s -> %s); building replacement image\n", emptyDash(poolTrustGeneration), current.Generation)
							ready = false
							for attempt := 1; attempt <= 3; attempt++ {
								generationBeforeEnsure := current.Generation
								if err := m.ensureHostTrustImage(ctx); err != nil {
									m.warnf("host trust replacement image warning: %v\n", err)
									nextHostTrustCollection = time.Now()
									break
								}
								current, err = m.resolveHostTrust(ctx)
								if err != nil {
									m.warnf("host trust post-build refresh warning: %v\n", err)
									nextHostTrustCollection = time.Now()
									break
								}
								if current.Generation == generationBeforeEnsure {
									poolTrustGeneration = current.Generation
									ready = true
									break
								}
								if attempt < 3 {
									m.infof("host trust changed again during replacement image publication (%s -> %s); retrying %d/3\n", generationBeforeEnsure, current.Generation, attempt+1)
								} else {
									m.warnf("host trust did not stabilize across 3 replacement image attempts (%s -> %s)\n", generationBeforeEnsure, current.Generation)
								}
							}
							trustCapacityReady = ready
						}
						if ready {
							currentHostTrust = current
						}
					}
				}
				if currentHostTrust.Generation != "" {
					trustRetired += m.reconcileHostTrustRunners(ctx, active, currentHostTrust)
				}
			}
			if opts.ReplaceCompleted && !time.Now().Before(nextLivenessCheck) {
				nextLivenessCheck = time.Now().Add(opts.MonitorInterval)
				for name, vm := range active {
					alive, reason, err := m.runnerAlive(ctx, vm)
					if err != nil {
						m.logger().Warn("liveness check failed", "provider", m.Config.Provider.Type, "instance", name, "operation", "liveness-check", "error", err)
						continue
					}
					if alive {
						continue
					}
					m.infof("[%s] runner is finished or unhealthy: %s\n", name, reason)
					if err := m.retireInstance(context.Background(), vm, reason); err != nil {
						m.warnf("[%s] retirement warning: %v\n", name, err)
						continue
					}
					delete(active, name)
				}
			}
			if m.hostTrustEnabled() && currentHostTrust.Generation != "" && currentHostTrust.Generation != poolTrustGeneration {
				trustCapacityReady = false
			}
			replacementCapacity := len(active)
			needsTrustCapacity := false
			if m.hostTrustEnabled() && currentHostTrust.Generation != "" {
				replacementCapacity = currentHostTrustCapacity(active, currentHostTrust.Generation)
				needsTrustCapacity = replacementCapacity < opts.Instances
			}
			if !trustCapacityReady || (!opts.ReplaceCompleted && trustRetired == 0 && !needsTrustCapacity) {
				continue
			}
			for replacementCapacity < opts.Instances {
				select {
				case <-ctx.Done():
					return cleanup()
				default:
				}
				name := RunnerName(m.Config.Pool.NamePrefix, sequence, time.Now())
				sequence++
				m.infof("[%s] creating replacement runner\n", name)
				vm, err := m.provisionOne(ctx, name, opts.Register, true)
				if err != nil {
					m.warnf("[%s] replacement failed: %v\n", name, err)
					break
				}
				active[vm.Name] = vm
				replacementCapacity++
				if vm.HostTrustGeneration != "" {
					poolTrustGeneration = vm.HostTrustGeneration
				}
				m.infof("%s online at %s providerLog=%s guestLog=%s\n", vm.Name, vm.IP, vm.LogPath, vm.GuestLogPath)
			}
		}
	}
}

func currentHostTrustCapacity(active map[string]ProvisionedInstance, generation string) int {
	capacity := 0
	for _, instance := range active {
		if instance.HostTrustGeneration == generation {
			capacity++
		}
	}
	return capacity
}

func (m *Manager) cleanupWithFreshContext() error {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	return m.Cleanup(cleanupCtx)
}

func (m *Manager) ProvisionPool(ctx context.Context, instances int, register bool) ([]ProvisionedInstance, error) {
	instances = m.requestedInstances(instances)
	names := RunnerNames(m.Config.Pool.NamePrefix, instances, time.Now())
	out := make([]ProvisionedInstance, 0, len(names))
	for _, name := range names {
		vm, err := m.provisionOne(ctx, name, register, false)
		if err != nil {
			return out, err
		}
		out = append(out, vm)
	}
	return out, nil
}

func (m *Manager) requestedInstances(override int) int {
	if override > 0 {
		return override
	}
	if m.Config.Pool.Instances > 0 {
		return m.Config.Pool.Instances
	}
	return 1
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
		m.infof("cleanup: deleting instance %s\n", vm.Name)
		stopCtx, stopCancel := context.WithTimeout(ctx, 60*time.Second)
		_ = m.Provider.Stop(stopCtx, vm.Name)
		stopCancel()
		deleteCtx, deleteCancel := context.WithTimeout(ctx, 60*time.Second)
		deleteErr := m.Provider.Delete(deleteCtx, vm.Name)
		if deleteErr != nil && firstErr == nil {
			firstErr = deleteErr
		}
		if deleteErr == nil {
			paths := ProvisionedInstance{
				Name:         vm.Name,
				LogPath:      m.instanceLogPath(vm.Name, "."+m.Config.Provider.Type+".log"),
				GuestLogPath: m.instanceLogPath(vm.Name, ".guest.log"),
			}
			if releaseErr := m.releaseInstanceTranscripts(paths); releaseErr != nil {
				m.logger().Warn("instance transcript close failed after cleanup", "provider", m.Config.Provider.Type, "instance", vm.Name, "operation", "cleanup", "error", releaseErr)
			}
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
			m.infof("cleanup: deleted GitHub runner %s id=%d\n", runner.Name, runner.ID)
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

var errHostTrustImageMismatch = errors.New("runner image host trust generation does not match current host trust")

func (m *Manager) provisionOne(ctx context.Context, name string, register, allowBusy bool) (ProvisionedInstance, error) {
	const attempts = 3
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		vm, err := m.provisionOneAttempt(ctx, name, register, allowBusy)
		if err == nil || !errors.Is(err, errHostTrustImageMismatch) {
			return vm, err
		}
		lastErr = err
		if vm.Name != "" {
			_ = m.retireInstance(context.Background(), vm, "discarding stale host-trust image generation")
		}
		if attempt == attempts {
			break
		}
		m.infof("[%s] host trust changed before runner publication; rebuilding image (attempt %d/%d)\n", name, attempt+1, attempts)
		if err := m.ensureHostTrustImage(ctx); err != nil {
			return vm, fmt.Errorf("rebuild image after host trust changed during provisioning: %w", err)
		}
	}
	return ProvisionedInstance{Name: name}, fmt.Errorf("provision runner after %d host trust image stabilization attempts: %w", attempts, lastErr)
}

func (m *Manager) provisionOneAttempt(ctx context.Context, name string, register, allowBusy bool) (ProvisionedInstance, error) {
	logPath := m.instanceLogPath(name, "."+m.Config.Provider.Type+".log")
	guestLogPath := m.instanceLogPath(name, ".guest.log")
	vm := ProvisionedInstance{Name: name, LogPath: logPath, GuestLogPath: guestLogPath}
	var trustSnapshot hosttrust.Snapshot
	if m.hostTrustEnabled() {
		var err error
		trustSnapshot, err = m.resolveHostTrust(ctx)
		if err != nil {
			return vm, fmt.Errorf("resolve host trust before provisioning: %w", err)
		}
		vm.HostTrustGeneration = trustSnapshot.Generation
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0755); err != nil {
		return vm, err
	}
	m.logger().Info("cloning instance", "provider", m.Config.Provider.Type, "instance", name, "operation", "clone", "sourceImage", m.Config.Provider.SourceImage, "logPath", logPath)
	if err := m.timeFirstInstanceStage(name, "instance_container_create", func() error {
		return m.Provider.Clone(ctx, m.Config.Provider.SourceImage, name)
	}); err != nil {
		return vm, err
	}
	m.logger().Info("starting instance", "provider", m.Config.Provider.Type, "instance", name, "operation", "start", "logPath", logPath)
	if err := m.timeFirstInstanceStage(name, m.startupInstanceStartStage(), func() error {
		startOptions, startOptionsErr := m.startOptions(logPath, name)
		if startOptionsErr != nil {
			return startOptionsErr
		}
		_, err := m.Provider.Start(ctx, name, startOptions)
		return err
	}); err != nil {
		return vm, err
	}
	ip, err := m.Provider.IP(ctx, name, m.Config.Timeouts.BootSeconds)
	if err != nil {
		return vm, err
	}
	vm.IP = ip
	m.logger().Info("instance reachable", "provider", m.Config.Provider.Type, "instance", name, "operation", "wait-reachable", "address", ip)
	m.logger().Info("validating runner runtime", "provider", m.Config.Provider.Type, "instance", name, "operation", "validate-runtime", "stage", "start")
	if err := m.timeFirstInstanceStage(name, "runtime_validation", func() error {
		if err := m.configureDockerRegistryMirrors(ctx, name); err != nil {
			return err
		}
		return m.validateRuntimeWithRetry(ctx, name, guestLogPath)
	}); err != nil {
		return vm, err
	}
	m.infof("[%s] runtime validation passed\n", name)
	if m.hostTrustEnabled() {
		marker, err := m.readInstanceHostTrustMarker(ctx, name)
		if err != nil {
			return vm, fmt.Errorf("%w: %v", errHostTrustImageMismatch, err)
		}
		currentTrust, err := m.resolveHostTrust(ctx)
		if err != nil {
			return vm, fmt.Errorf("%w: refresh host trust after runtime validation: %v", errHostTrustImageMismatch, err)
		}
		if err := validateHostTrustMarkerAgainstSnapshot(marker, currentTrust); err != nil {
			return vm, fmt.Errorf("%w: %v", errHostTrustImageMismatch, err)
		}
		// Track the immutable generation read from the cloned image, not merely
		// the pre-clone snapshot. This prevents a trust-store change racing image
		// cloning from making the supervisor believe a stale image is current.
		vm.HostTrustGeneration = marker.Generation
		trustSnapshot = currentTrust
	}
	if register {
		if err := m.issueHostTrustLease(ctx, name, trustSnapshot); err != nil {
			return vm, fmt.Errorf("issue host trust lease: %w", err)
		}
		if m.GitHub == nil {
			if m.DryRun {
				m.infof("[dry-run] would register GitHub runner %s with labels %s\n", name, strings.Join(m.Config.Runner.Labels, ","))
				return vm, nil
			}
			return vm, fmt.Errorf("github client is required for registration")
		}
		var (
			token     gh.RegistrationToken
			runner    gh.Runner
			readiness = "online/idle"
		)
		if err := m.timeFirstInstanceStage(name, "github_registration_and_online_idle", func() error {
			m.infof("[%s] requesting GitHub registration token\n", name)
			var err error
			token, err = m.GitHub.RegistrationToken(ctx)
			if err != nil {
				return err
			}
			env := map[string]string{
				"RUNNER_URL":               m.GitHub.OrganizationURL(),
				"RUNNER_TOKEN":             token.Token,
				"RUNNER_NAME":              name,
				"RUNNER_LABELS":            strings.Join(m.Config.Runner.Labels, ","),
				"RUNNER_EPHEMERAL":         fmt.Sprintf("%t", m.Config.Runner.Ephemeral),
				"RUNNER_GROUP":             m.Config.Runner.Group,
				"RUNNER_NO_DEFAULT_LABELS": fmt.Sprintf("%t", m.Config.Runner.NoDefaultLabels),
			}
			if _, err := m.execGuest(ctx, name, []string{"sudo", "-E", "bash", "/opt/epar/configure-runner.sh"}, provider.ExecOptions{Env: env}); err != nil {
				return err
			}
			m.infof("[%s] starting runner service\n", name)
			if _, err := m.execGuest(ctx, name, []string{"sudo", "bash", "/opt/epar/run-runner.sh"}, provider.ExecOptions{}); err != nil {
				return err
			}
			if allowBusy {
				readiness = "online"
			}
			m.infof("[%s] waiting for GitHub %s\n", name, readiness)
			runner, err = m.waitRunnerReadyAndHealthy(ctx, vm, time.Duration(m.Config.Timeouts.GitHubOnlineSeconds)*time.Second, allowBusy)
			return err
		}); err != nil {
			m.captureRunnerReadinessDiagnostics(name, guestLogPath)
			return vm, err
		}
		vm.RunnerID = runner.ID
		m.infof("[%s] GitHub runner %s id=%d busy=%t\n", name, readiness, runner.ID, runner.Busy)
		m.finishFirstRunnerReady(name)
	} else {
		m.finishFirstRunnerReady(name)
	}
	return vm, nil
}

func (m *Manager) startupInstanceStartStage() string {
	if m.Config.Provider.Type == "docker-dind" {
		return "instance_start_and_inner_docker_ready"
	}
	return "instance_start_and_provider_ready"
}

type runnerReadinessResult struct {
	runner gh.Runner
	err    error
}

func (m *Manager) waitRunnerReadyAndHealthy(ctx context.Context, vm ProvisionedInstance, timeout time.Duration, allowBusy bool) (gh.Runner, error) {
	waitCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	resultCh := make(chan runnerReadinessResult, 1)
	go func() {
		var runner gh.Runner
		var err error
		if allowBusy {
			runner, err = m.GitHub.WaitRunnerOnline(waitCtx, vm.Name, timeout)
		} else {
			runner, err = m.GitHub.WaitRunnerOnlineIdle(waitCtx, vm.Name, timeout)
		}
		resultCh <- runnerReadinessResult{runner: runner, err: err}
	}()

	interval := runnerReadinessHealthCheckInterval
	if interval <= 0 {
		interval = 2 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	consecutiveProbeFailures := 0
	var lastProbeErr error
	nextLeaseRefresh := time.Now().Add(hostTrustRefreshInterval)

	for {
		select {
		case result := <-resultCh:
			return result.runner, result.err
		case <-ticker.C:
			if m.hostTrustEnabled() && !time.Now().Before(nextLeaseRefresh) {
				current, err := m.resolveHostTrust(waitCtx)
				if err != nil {
					cancel()
					return gh.Runner{}, fmt.Errorf("refresh host trust while waiting for runner readiness: %w", err)
				}
				if current.Generation != vm.HostTrustGeneration {
					if revokeErr := m.issueHostTrustLease(waitCtx, vm.Name, current); revokeErr != nil {
						m.warnf("[%s] host trust readiness revocation warning: %v\n", vm.Name, revokeErr)
					}
					cancel()
					return gh.Runner{}, fmt.Errorf("host trust changed while runner %s was registering (%s -> %s)", vm.Name, vm.HostTrustGeneration, current.Generation)
				}
				if err := m.issueHostTrustLease(waitCtx, vm.Name, current); err != nil {
					cancel()
					return gh.Runner{}, fmt.Errorf("refresh host trust lease while waiting for runner readiness: %w", err)
				}
				nextLeaseRefresh = time.Now().Add(hostTrustRefreshInterval)
			}
			err := m.checkRunnerProcess(waitCtx, vm.Name)
			if err == nil {
				consecutiveProbeFailures = 0
				lastProbeErr = nil
				continue
			}
			if ctx.Err() != nil {
				cancel()
				return gh.Runner{}, ctx.Err()
			}
			consecutiveProbeFailures++
			lastProbeErr = err
			if consecutiveProbeFailures < runnerReadinessProbeFailureLimit {
				m.warnf("[%s] runner readiness process check failed (%d/%d): %v\n", vm.Name, consecutiveProbeFailures, runnerReadinessProbeFailureLimit, err)
				continue
			}
			cancel()
			readiness := "online/idle"
			if allowBusy {
				readiness = "online"
			}
			return gh.Runner{}, fmt.Errorf("actions runner process failed %d consecutive checks while waiting for GitHub %s: %w", runnerReadinessProbeFailureLimit, readiness, lastProbeErr)
		case <-ctx.Done():
			cancel()
			return gh.Runner{}, ctx.Err()
		}
	}
}

func (m *Manager) captureRunnerReadinessDiagnostics(name, guestLogPath string) {
	diagnosticCtx, cancel := context.WithTimeout(context.Background(), runnerReadinessDiagnosticsTimeout)
	defer cancel()
	_, err := m.execGuest(
		diagnosticCtx,
		name,
		[]string{"sudo", "bash", "/opt/epar/collect-runner-diagnostics.sh"},
		provider.ExecOptions{LogPath: guestLogPath},
	)
	if err != nil {
		m.warnf("[%s] runner readiness diagnostic collection warning: %v\n", name, err)
	}
}

func (m *Manager) runnerAlive(ctx context.Context, vm ProvisionedInstance) (bool, string, error) {
	if m.GitHub != nil {
		runner, found, err := m.GitHub.RunnerByName(ctx, vm.Name)
		if err != nil {
			if !isTransientGitHubLivenessError(err) {
				return true, "", err
			}
		} else {
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
	}
	return m.runnerProcessAlive(ctx, vm)
}

func (m *Manager) runnerProcessAlive(ctx context.Context, vm ProvisionedInstance) (bool, string, error) {
	if err := m.checkRunnerProcess(ctx, vm.Name); err != nil {
		return false, "actions runner process is no longer active", nil
	}
	return true, "", nil
}

func (m *Manager) checkRunnerProcess(ctx context.Context, name string) error {
	checkCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	_, err := m.execGuest(checkCtx, name, provider.ShellCommand("if test -x /opt/epar/check-runner.sh; then sudo bash /opt/epar/check-runner.sh; else systemctl is-active --quiet actions-runner.service; fi"), provider.ExecOptions{})
	return err
}

func isTransientGitHubLivenessError(err error) bool {
	var httpErr *gh.HTTPError
	return errors.As(err, &httpErr) && httpErr.StatusCode >= http.StatusInternalServerError
}

func (m *Manager) retireInstance(ctx context.Context, vm ProvisionedInstance, reason string) error {
	m.infof("[%s] retiring instance: %s\n", vm.Name, reason)
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
	deleteErr := m.Provider.Delete(deleteCtx, vm.Name)
	if deleteErr != nil && firstErr == nil {
		firstErr = deleteErr
	}
	if deleteErr == nil {
		if releaseErr := m.releaseInstanceTranscripts(vm); releaseErr != nil {
			m.logger().Warn("instance transcript close failed after retirement", "provider", m.Config.Provider.Type, "instance", vm.Name, "operation", "retire", "error", releaseErr)
		}
	}
	deleteCancel()
	return firstErr
}

func (m *Manager) validateRuntime(ctx context.Context, name string) error {
	_, err := m.execGuest(ctx, name, []string{"sudo", "bash", "/opt/epar/validate-runtime.sh"}, provider.ExecOptions{})
	return err
}

func (m *Manager) validateRuntimeWithRetry(ctx context.Context, name, guestLogPath string) error {
	const attempts = 2
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		err := m.validateRuntime(ctx, name)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		lastErr = err
		if attempt == attempts {
			break
		}
		m.warnf("[%s] runtime validation attempt %d/%d failed: %v\n", name, attempt, attempts, err)
		m.infof("[%s] retrying runtime validation in %s; guest log: %s\n", name, runtimeValidationRetryDelay, guestLogPath)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(runtimeValidationRetryDelay):
		}
	}
	return fmt.Errorf("runtime validation failed after %d attempts; guest log: %s: %w", attempts, guestLogPath, lastErr)
}

func (m *Manager) configureDockerRegistryMirrors(ctx context.Context, name string) error {
	if len(m.Config.Docker.RegistryMirrors) == 0 {
		return nil
	}
	m.infof("[%s] configuring Docker registry mirror(s): %s\n", name, strings.Join(m.Config.Docker.RegistryMirrors, ", "))
	hostPath := filepath.Join(m.ProjectRoot, "scripts", "guest", "ubuntu", "configure-docker-daemon.sh")
	content, err := os.ReadFile(hostPath)
	if err != nil {
		return fmt.Errorf("read Docker daemon configuration script %s: %w", hostPath, err)
	}
	if err := provider.CopyText(ctx, m.Provider, name, "/opt/epar/configure-docker-daemon.sh", "0755", guestText(content)); err != nil {
		return err
	}
	_, err = m.execGuest(ctx, name, []string{"sudo", "-E", "bash", "/opt/epar/configure-docker-daemon.sh"}, provider.ExecOptions{
		Env: map[string]string{
			"EPAR_DOCKER_REGISTRY_MIRRORS": strings.Join(m.Config.Docker.RegistryMirrors, "\n"),
		},
	})
	return err
}

func (m *Manager) execGuest(ctx context.Context, name string, cmd []string, opts provider.ExecOptions) (provider.ExecResult, error) {
	timeout := time.Duration(m.Config.Timeouts.CommandSeconds) * time.Second
	if timeout <= 0 {
		timeout = 15 * time.Minute
	}
	if opts.LogPath == "" {
		opts.LogPath = m.instanceLogPath(name, ".guest.log")
	}
	transcript, err := m.transcript(opts.LogPath, name, transcriptComponent(opts.LogPath))
	if err != nil {
		return provider.ExecResult{}, err
	}
	opts.Stdout = transcript.Stdout
	opts.Stderr = transcript.Stderr
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
