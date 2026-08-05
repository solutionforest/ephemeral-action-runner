package pool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
	gh "github.com/solutionforest/ephemeral-action-runner/internal/github"
	"github.com/solutionforest/ephemeral-action-runner/internal/hosttrust"
	"github.com/solutionforest/ephemeral-action-runner/internal/logging"
	poolstate "github.com/solutionforest/ephemeral-action-runner/internal/pool/state"
	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
)

type Manager struct {
	Config                   config.Config
	Provider                 provider.Provider
	Lifecycle                provider.Lifecycle
	PolicyManager            provider.PolicyManager
	Storage                  provider.StorageContribution
	LifecycleState           *poolstate.Store
	LifecycleStateEnabled    bool
	GitHub                   GitHubClient
	ProjectRoot              string
	ConfigPath               string
	DryRun                   bool
	Logging                  *logging.Runtime
	AllowInsufficientStorage bool
	StorageOverrideCommand   string
	AutomaticImageLifecycle  bool
	// AcknowledgeFailedDiagnostics permits the explicit cleanup command to
	// dispose an exact retained sandbox after the operator has captured the
	// durable failed-diagnostics evidence. Normal startup and automatic cleanup
	// never set this override.
	AcknowledgeFailedDiagnostics bool
	startupTiming                *startupTiming
	transcriptMu                 sync.Mutex
	transcripts                  map[string]*logging.Transcript

	hostTrustResolver     func(context.Context) (hosttrust.Snapshot, error)
	buildTrustResolver    func(context.Context) (hosttrust.Snapshot, error)
	hostTrustImageEnsurer func(context.Context) error
	hostTrustImageMu      sync.Mutex
	imageEnsureMu         sync.Mutex
	imageEnsured          bool
	now                   func() time.Time
	randomFloat64         func() float64
}

func (m *Manager) ConfigureStorageAdmissionOverride(allow bool, command string) {
	m.AllowInsufficientStorage = allow
	m.StorageOverrideCommand = command
}

type GitHubClient interface {
	OrganizationURL() string
	EvaluateRunnerGroupPolicy(ctx context.Context, configuredGroup string, policy config.RunnerGroupSecurityConfig) (gh.RunnerGroupPolicyResult, error)
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
	PoolLockHeld      bool
}

type LifecyclePhase string

const (
	LifecycleProvisioning   LifecyclePhase = "provisioning"
	LifecycleReady          LifecyclePhase = "ready"
	LifecycleDraining       LifecyclePhase = "draining"
	LifecycleQuarantined    LifecyclePhase = "quarantined"
	LifecycleCleanupPending LifecyclePhase = "cleanup-pending"
)

const (
	runnerProcessRunningSentinel      = "EPAR_RUNNER_PROCESS=running"
	runnerProcessStoppedSentinel      = "EPAR_RUNNER_PROCESS=stopped"
	runnerProcessInactiveReason       = "actions runner process is confirmed inactive"
	runnerConfirmedInactiveCheckLimit = 2
)

type ProvisionedInstance struct {
	Name                string
	IP                  string
	LogPath             string
	GuestLogPath        string
	RunnerID            int64
	ProviderID          string
	HostTrustGeneration string
	Phase               LifecyclePhase
	ProviderOwned       bool
}

var runtimeValidationRetryDelay = 5 * time.Second
var runnerReadinessHealthCheckInterval = 2 * time.Second
var dependencyHTTPStatusPattern = regexp.MustCompile(`(?i)(?:status code does not indicate success|http response code)\s*:\s*([0-9]{3})`)

const (
	cleanupTimeout                    = 5 * time.Minute
	runnerReadinessDiagnosticsTimeout = 30 * time.Second
	runnerReadinessProbeFailureLimit  = 3
)

func (m *Manager) Verify(ctx context.Context, opts VerifyOptions) error {
	if opts.RegisterOnly {
		if err := m.PreflightRunnerGroup(ctx); err != nil {
			return err
		}
	}
	poolLock, err := m.AcquirePoolControllerLock()
	if err != nil {
		return err
	}
	defer poolLock.Close()
	if err := m.recoverInterruptedProvisionLeases(ctx); err != nil {
		return err
	}
	controllerLock, err := m.acquireHostTrustControllerLock()
	if err != nil {
		return err
	}
	if controllerLock != nil {
		defer controllerLock.Close()
	}
	stopStorageLease, err := m.startStorageCatalogControllerLease()
	if err != nil {
		return err
	}
	defer stopStorageLease()
	if m.AutomaticImageLifecycle {
		if err := m.EnsureImage(ctx); err != nil {
			return fmt.Errorf("ensure current provider artifact before verification: %w", err)
		}
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
	if opts.Register {
		if err := m.PreflightRunnerGroup(ctx); err != nil {
			return err
		}
	}
	if !opts.PoolLockHeld {
		controllerLock, err := m.AcquirePoolControllerLock()
		if err != nil {
			return err
		}
		defer controllerLock.Close()
	}
	if err := m.recoverInterruptedProvisionLeases(ctx); err != nil {
		return err
	}
	if !opts.HostTrustLockHeld {
		controllerLock, err := m.AcquireHostTrustControllerLock()
		if err != nil {
			return err
		}
		if controllerLock != nil {
			defer controllerLock.Close()
		}
	}
	stopStorageLease, err := m.startStorageCatalogControllerLease()
	if err != nil {
		return err
	}
	defer stopStorageLease()
	if m.AutomaticImageLifecycle {
		if err := m.EnsureImage(ctx); err != nil {
			return fmt.Errorf("ensure current provider artifact before pool startup: %w", err)
		}
	}
	opts.Instances = m.requestedInstances(opts.Instances)
	if opts.MonitorInterval <= 0 {
		opts.MonitorInterval = 15 * time.Second
	}
	if ctx.Err() != nil {
		if opts.KeepOnExit {
			m.infof("Stopping EPAR pool. --keep-on-exit is enabled, so owned runner resources will remain running.\n")
			return nil
		}
		return m.cleanupPoolWithStatus("owned GitHub runner registrations and provider instances", m.cleanupWithFreshContext)
	}
	active, err := m.reconcilePhysicalPool(ctx, nil, opts.Register)
	if err != nil {
		return fmt.Errorf("initial pool reconciliation: %w", err)
	}
	active, err = m.reduceOverCapacity(ctx, active, opts.Instances, opts.Register)
	if err != nil {
		return fmt.Errorf("initial over-capacity reconciliation: %w", err)
	}
	sequence := 1
	poolTrustGeneration := ""
	cleanup := func() error {
		if opts.KeepOnExit {
			m.infof("Stopping EPAR pool. --keep-on-exit is enabled, so owned runner resources will remain running.\n")
			return nil
		}
		return m.cleanupPoolWithStatus("owned GitHub runner registrations and provider instances", m.cleanupWithFreshContext)
	}
	leaseAdd, stopLeaseKeeper := m.startHostTrustLeaseKeeper(ctx)
	for len(active) < opts.Instances {
		active, err = m.reconcilePhysicalPool(ctx, active, opts.Register)
		if err != nil {
			stopLeaseKeeper()
			return fmt.Errorf("initial pool reconciliation before allocation: %w", err)
		}
		if len(active) >= opts.Instances {
			break
		}
		vm, err := m.provisionOne(ctx, RunnerName(m.Config.Pool.NamePrefix, sequence, time.Now()), opts.Register, opts.Register && opts.ReplaceCompleted)
		sequence++
		if isPhysicalPhase(vm.Phase) {
			active[vm.Name] = vm
		}
		if err != nil {
			stopLeaseKeeper()
			if ctx.Err() != nil {
				return cleanup()
			}
			return errors.Join(err, m.cleanupAfterTerminalFailure(active, opts.KeepOnExit))
		}
		leaseAdd(vm)
		if vm.HostTrustGeneration != "" {
			poolTrustGeneration = vm.HostTrustGeneration
		}
		m.infof("%s online at %s providerLog=%s guestLog=%s\n", vm.Name, vm.IP, vm.LogPath, vm.GuestLogPath)
	}
	stopLeaseKeeper()
	if !opts.Register || (!opts.ReplaceCompleted && !m.hostTrustEnabled()) {
		m.logPoolRunning("EPAR pool")
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
	m.infof("Pool supervisor is monitoring every %s.\n", opts.MonitorInterval)
	m.logPoolRunning("EPAR pool")
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
	hostTrustBusyHandoff := make(map[string]bool)
	confirmedInactiveChecks := make(map[string]int)
	imageMaintenanceIdleChecks := make(map[string]int)
	retry := replacementRetryState{}
	imageMaintenancePending := false
	for {
		select {
		case <-ctx.Done():
			return cleanup()
		case <-ticker.C:
			now := m.currentTime()
			dependencyCooldown := retry.active(now)
			if m.Config.Logging.RetentionEnabled && !time.Now().Before(nextRetention) {
				m.pruneLogsBestEffort()
				nextRetention = time.Now().Add(time.Duration(m.Config.Logging.RetentionIntervalMinutes) * time.Minute)
			}
			if m.AutomaticImageLifecycle && !imageMaintenancePending && m.Config.Image.UpdateFrequency != config.ImageUpdateFrequencyManual {
				check, checkErr := m.CheckRemoteImageUpdate(ctx, now)
				if checkErr != nil {
					m.warnf("scheduled image update check failed; the current verified pool remains available: %v\n", checkErr)
				} else if check.Changed {
					if !m.Config.Runner.Ephemeral {
						_ = m.DeferPendingImageUpdate("runner.ephemeral=false; the update will be applied on the next EPAR startup")
						m.warnf("image update is available but this pool uses persistent runners; restart EPAR to apply it without an assignment race\n")
					} else {
						imageMaintenancePending = true
						m.infof("image or Actions runner update is available; draining the ephemeral pool before artifact provisioning\n")
					}
				}
			}
			if imageMaintenancePending {
				remaining, drainErr := m.drainPoolForImageUpdate(ctx, active, imageMaintenanceIdleChecks)
				if drainErr != nil {
					m.warnf("scheduled image maintenance drain warning; retrying without creating replacements: %v\n", drainErr)
					continue
				}
				if remaining > 0 {
					continue
				}
				m.infof("scheduled image maintenance drain complete; building and activating the verified replacement artifact\n")
				if updateErr := m.ApplyPendingImageUpdate(ctx, now); updateErr != nil {
					m.warnf("scheduled image update failed; restoring pool capacity with the previous verified generation: %v\n", updateErr)
				} else {
					m.infof("scheduled image update activated; restoring pool capacity\n")
				}
				imageMaintenancePending = false
				clear(imageMaintenanceIdleChecks)
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
							if !dependencyCooldown {
								trustRetired += m.reconcileHostTrustRunners(ctx, active, current, hostTrustBusyHandoff)
							}
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
				if currentHostTrust.Generation != "" && !dependencyCooldown {
					trustRetired += m.reconcileHostTrustRunners(ctx, active, currentHostTrust, hostTrustBusyHandoff)
				}
			}
			if dependencyCooldown {
				var localErr error
				active, localErr = m.reconcileLocalInventory(active)
				if localErr != nil {
					m.warnf("local pool housekeeping warning during replacement cooldown: %v\n", localErr)
				}
				continue
			}
			if opts.ReplaceCompleted && !time.Now().Before(nextLivenessCheck) {
				nextLivenessCheck = time.Now().Add(opts.MonitorInterval)
				for name, vm := range active {
					alive, reason, err := m.runnerAlive(ctx, vm)
					if err != nil {
						recordRunnerLiveness(confirmedInactiveChecks, name, alive, reason, err)
						m.warnf("[%s] runner health is temporarily unknown; keeping the runner and retrying: %v\n", name, err)
						continue
					}
					if alive {
						recordRunnerLiveness(confirmedInactiveChecks, name, alive, reason, nil)
						continue
					}
					confirmedCount, retire := recordRunnerLiveness(confirmedInactiveChecks, name, alive, reason, nil)
					if !retire {
						m.warnf("[%s] runner process is confirmed inactive (%d/%d); EPAR will verify once more before cleanup\n", name, confirmedCount, runnerConfirmedInactiveCheckLimit)
						continue
					}
					if reason == runnerProcessInactiveReason {
						m.captureRunnerReadinessDiagnostics(name, vm.GuestLogPath)
					}
					m.infof("[%s] runner is finished or unhealthy: %s\n", name, reason)
					if err := m.retireInstance(context.Background(), vm, reason); err != nil {
						m.warnf("[%s] retirement warning: %v\n", name, err)
						continue
					}
					delete(active, name)
					delete(confirmedInactiveChecks, name)
				}
			}
			var reconcileErr error
			beforeReconcile := active
			active, reconcileErr = m.reconcilePhysicalPool(ctx, active, opts.Register)
			if reconcileErr != nil {
				if ctx.Err() != nil {
					return cleanup()
				}
				if !isTransientDependencyError(reconcileErr) {
					return errors.Join(fmt.Errorf("pool reconciliation: %w", reconcileErr), m.cleanupAfterTerminalFailure(active, opts.KeepOnExit))
				}
				retry.schedule(m, now, reconcileErr)
				m.warnf("pool reconciliation deferred during transient dependency failure; retrying after %s: %v\n", retry.remaining(now), reconcileErr)
				continue
			}
			retry.resetAfterAdoption(beforeReconcile, active)
			active, reconcileErr = m.reduceOverCapacity(ctx, active, opts.Instances, opts.Register)
			if reconcileErr != nil {
				if !isTransientDependencyError(reconcileErr) {
					return errors.Join(fmt.Errorf("reduce over-capacity pool: %w", reconcileErr), m.cleanupAfterTerminalFailure(active, opts.KeepOnExit))
				}
				retry.schedule(m, now, reconcileErr)
				continue
			}
			if m.hostTrustEnabled() && currentHostTrust.Generation != "" && currentHostTrust.Generation != poolTrustGeneration {
				trustCapacityReady = false
			}
			replacementCapacity := len(active)
			needsTrustCapacity := false
			if m.hostTrustEnabled() && currentHostTrust.Generation != "" {
				needsTrustCapacity = currentHostTrustCapacity(active, currentHostTrust.Generation) < opts.Instances
			}
			if !trustCapacityReady || (!opts.ReplaceCompleted && trustRetired == 0 && !needsTrustCapacity) {
				continue
			}
			if retry.active(now) {
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
				active, err = m.reconcilePhysicalPool(ctx, active, opts.Register)
				if err != nil {
					if !isTransientDependencyError(err) {
						return errors.Join(fmt.Errorf("pool reconciliation before replacement allocation: %w", err), m.cleanupAfterTerminalFailure(active, opts.KeepOnExit))
					}
					retry.schedule(m, m.currentTime(), err)
					m.warnf("[%s] replacement deferred during transient reconciliation failure: %v\n", name, err)
					break
				}
				replacementCapacity = len(active)
				if replacementCapacity >= opts.Instances {
					break
				}
				m.infof("[%s] creating replacement runner\n", name)
				vm, err := m.provisionOne(ctx, name, opts.Register, true)
				if isPhysicalPhase(vm.Phase) {
					active[vm.Name] = vm
				}
				if err != nil {
					if ctx.Err() != nil {
						return cleanup()
					}
					m.warnf("[%s] replacement failed: %v\n", name, err)
					if !isTransientDependencyError(err) {
						return errors.Join(err, m.cleanupAfterTerminalFailure(active, opts.KeepOnExit))
					}
					retry.schedule(m, m.currentTime(), err)
					break
				}
				active[vm.Name] = vm
				retry.reset()
				replacementCapacity++
				if vm.HostTrustGeneration != "" {
					poolTrustGeneration = vm.HostTrustGeneration
				}
				m.infof("%s online at %s providerLog=%s guestLog=%s\n", vm.Name, vm.IP, vm.LogPath, vm.GuestLogPath)
				m.logReplacementReady("EPAR pool", vm.Name)
			}
		}
	}
}

func (m *Manager) drainPoolForImageUpdate(ctx context.Context, active map[string]ProvisionedInstance, confirmedIdle map[string]int) (int, error) {
	for name, vm := range active {
		if m.GitHub != nil {
			runner, found, err := m.GitHub.RunnerByName(ctx, name)
			if err != nil {
				delete(confirmedIdle, name)
				return len(active), fmt.Errorf("inspect GitHub runner %s before maintenance: %w", name, err)
			}
			if found {
				vm.RunnerID = runner.ID
				active[name] = vm
				if err := m.recordLifecycleJobObservation(ctx, runner); err != nil {
					delete(confirmedIdle, name)
					return len(active), fmt.Errorf("record GitHub job state for %s before maintenance: %w", name, err)
				}
			}
			if found && runner.Busy {
				delete(confirmedIdle, name)
				m.infof("[%s] scheduled image maintenance is waiting for the active job to finish\n", name)
				continue
			}
			if found {
				confirmedIdle[name]++
				if confirmedIdle[name] < 2 {
					m.infof("[%s] scheduled image maintenance observed the runner idle; confirming it remains unassigned before retirement\n", name)
					continue
				}
			}
		}
		m.infof("[%s] retiring idle runner for scheduled image maintenance\n", name)
		if err := m.retireInstance(context.Background(), vm, "scheduled image and Actions runner update"); err != nil {
			return len(active), err
		}
		delete(active, name)
		delete(confirmedIdle, name)
	}
	return len(active), nil
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

func isPhysicalPhase(phase LifecyclePhase) bool {
	return phase == LifecycleProvisioning || phase == LifecycleReady || phase == LifecycleDraining || phase == LifecycleQuarantined || phase == LifecycleCleanupPending
}

func adoptedReadyInstance(before, after map[string]ProvisionedInstance) bool {
	for name, instance := range after {
		previous, found := before[name]
		if instance.Phase == LifecycleReady && (!found || previous.Phase != LifecycleReady) {
			return true
		}
	}
	return false
}

func (m *Manager) reconcilePhysicalPool(ctx context.Context, known map[string]ProvisionedInstance, register bool) (map[string]ProvisionedInstance, error) {
	reconciled, err := m.reconcileLocalInventoryWithContext(ctx, known)
	if err != nil {
		return known, err
	}
	if !register {
		for name, vm := range reconciled {
			if vm.ProviderOwned && vm.Phase != LifecycleCleanupPending {
				vm.Phase = LifecycleReady
				reconciled[name] = vm
			}
		}
		return reconciled, nil
	}
	if m.GitHub == nil {
		return reconciled, fmt.Errorf("github client is required for registered pool reconciliation")
	}
	remoteByName := make(map[string]gh.Runner)
	runners, err := m.GitHub.ListRunners(ctx)
	if err != nil {
		for name, vm := range reconciled {
			if vm.Phase != LifecycleCleanupPending {
				vm.Phase = LifecycleQuarantined
				reconciled[name] = vm
			}
		}
		return reconciled, err
	}
	for _, runner := range runners {
		if HasPrefix(runner.Name, m.Config.Pool.NamePrefix) {
			remoteByName[runner.Name] = runner
		}
	}
	for name, vm := range reconciled {
		if !vm.ProviderOwned {
			delete(remoteByName, name)
			vm.Phase = LifecycleQuarantined
			reconciled[name] = vm
			continue
		}
		if vm.Phase == LifecycleCleanupPending {
			// The local cleanup path already matched this exact lifecycle
			// record. Keep its remote identity attached to the protected
			// record instead of treating it as an orphan below.
			delete(remoteByName, name)
			continue
		}
		runner, found := remoteByName[name]
		if !found && isPhysicalPhase(vm.Phase) {
			var lookupErr error
			runner, found, lookupErr = m.GitHub.RunnerByName(ctx, name)
			if lookupErr != nil {
				vm.Phase = LifecycleQuarantined
				reconciled[name] = vm
				return reconciled, lookupErr
			}
		}
		if !found {
			if err := m.recordLifecycleRemoteAbsence(ctx, name); err != nil {
				vm.Phase = LifecycleQuarantined
				reconciled[name] = vm
				return reconciled, fmt.Errorf("record GitHub runner absence for %s: %w", name, err)
			}
			if err := m.deleteLocalInstance(context.Background(), vm); err != nil {
				vm.Phase = LifecycleCleanupPending
				reconciled[name] = vm
				m.warnf("[%s] unregistered-instance cleanup pending: %v\n", name, err)
			} else {
				delete(reconciled, name)
			}
			continue
		}
		delete(remoteByName, name)
		vm.RunnerID = runner.ID
		if err := m.recordLifecycleJobObservation(ctx, runner); err != nil {
			vm.Phase = LifecycleQuarantined
			reconciled[name] = vm
			return reconciled, fmt.Errorf("record GitHub job phase for %s: %w", name, err)
		}
		if runner.Status == "online" {
			vm.Phase = LifecycleReady
			reconciled[name] = vm
			continue
		}
		if runner.Busy {
			vm.Phase = LifecycleQuarantined
			reconciled[name] = vm
			continue
		}
		if vm.Phase == LifecycleQuarantined {
			if err := m.retireInstance(context.Background(), vm, "GitHub recovered but quarantined runner remained offline"); err != nil {
				vm.Phase = LifecycleCleanupPending
				reconciled[name] = vm
				m.warnf("[%s] recovered-offline retirement pending: %v\n", name, err)
			} else {
				delete(reconciled, name)
			}
			continue
		}
		alive, _, processErr := m.runnerProcessAlive(ctx, vm)
		if processErr != nil {
			vm.Phase = LifecycleQuarantined
			reconciled[name] = vm
			m.warnf("[%s] reconciliation could not verify the Actions runner process; preserving the exact instance in quarantine: %v\n", name, processErr)
			continue
		}
		if alive {
			vm.Phase = LifecycleQuarantined
			reconciled[name] = vm
			continue
		}
		if err := m.retireInstance(context.Background(), vm, "reconciliation found offline runner with inactive listener"); err != nil {
			vm.Phase = LifecycleCleanupPending
			reconciled[name] = vm
			m.warnf("[%s] inactive-instance cleanup pending: %v\n", name, err)
		} else {
			delete(reconciled, name)
		}
	}
	for _, runner := range remoteByName {
		owned, ownershipErr := m.lifecycleOwnsRunner(ctx, runner.Name, runner.ID)
		if ownershipErr != nil {
			return reconciled, ownershipErr
		}
		if !owned {
			m.warnf("reconciliation: quarantined unowned GitHub runner %s id=%d; prefix-only resources are report-only\n", runner.Name, runner.ID)
			continue
		}
		if err := m.deleteRemoteRunner(context.Background(), runner); err != nil {
			return reconciled, err
		}
		m.infof("reconciliation: deleted stale GitHub runner %s id=%d\n", runner.Name, runner.ID)
	}
	return reconciled, nil
}

func (m *Manager) reduceOverCapacity(ctx context.Context, active map[string]ProvisionedInstance, target int, register bool) (map[string]ProvisionedInstance, error) {
	if len(active) <= target || !register || m.GitHub == nil {
		return active, nil
	}
	names := make([]string, 0, len(active))
	for name := range active {
		names = append(names, name)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	for _, name := range names {
		if len(active) <= target {
			break
		}
		vm := active[name]
		if vm.Phase != LifecycleReady {
			continue
		}
		runner, found, err := m.GitHub.RunnerByName(ctx, name)
		if err != nil {
			return active, err
		}
		if !found || runner.Busy {
			continue
		}
		vm.RunnerID = runner.ID
		if err := m.retireInstance(context.Background(), vm, "reconciling legacy physical inventory above pool.instances"); err != nil {
			vm.Phase = LifecycleCleanupPending
			active[name] = vm
			continue
		}
		delete(active, name)
	}
	return active, nil
}

func (m *Manager) reconcileLocalInventory(known map[string]ProvisionedInstance) (map[string]ProvisionedInstance, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	return m.reconcileLocalInventoryWithContext(ctx, known)
}

func (m *Manager) reconcileLocalInventoryWithContext(ctx context.Context, known map[string]ProvisionedInstance) (map[string]ProvisionedInstance, error) {
	locals, err := m.inventoryProvider(ctx)
	if err != nil {
		return known, err
	}
	reconciled := make(map[string]ProvisionedInstance)
	for _, item := range locals {
		local := item.Instance
		if !HasPrefix(local.Name, m.Config.Pool.NamePrefix) {
			continue
		}
		vm := m.reconciledInstance(known, local.Name)
		vm.ProviderID = local.ProviderID
		owned, ownershipErr := m.lifecycleOwns(ctx, local.Name, local.ProviderID)
		if ownershipErr != nil {
			return known, fmt.Errorf("verify lifecycle ownership for %s: %w", local.Name, ownershipErr)
		}
		vm.ProviderOwned = owned
		if !owned {
			providerID := local.ProviderID
			if providerID == "" {
				providerID = "unidentified:" + local.Name
			}
			if reportErr := m.reportUnknownLifecycle(ctx, local.Name, providerID, item.Source, item.State); reportErr != nil {
				return known, fmt.Errorf("quarantine unowned provider instance %s: %w", local.Name, reportErr)
			}
			vm.Phase = LifecycleQuarantined
			reconciled[local.Name] = vm
			continue
		}
		if !localInstanceStopped(item.State) {
			reconciled[local.Name] = vm
			continue
		}
		if err := m.deleteLocalInstance(context.Background(), vm); err != nil {
			vm.Phase = LifecycleCleanupPending
			reconciled[local.Name] = vm
			m.warnf("[%s] stopped-instance cleanup pending: %v\n", local.Name, err)
		}
	}
	return reconciled, nil
}

func (m *Manager) reconciledInstance(known map[string]ProvisionedInstance, name string) ProvisionedInstance {
	if vm, found := known[name]; found {
		return vm
	}
	return ProvisionedInstance{
		Name:          name,
		LogPath:       m.instanceLogPath(name, "."+m.Config.Provider.Type+".log"),
		GuestLogPath:  m.instanceLogPath(name, ".guest.log"),
		Phase:         LifecycleQuarantined,
		ProviderOwned: m.LifecycleState == nil,
	}
}

func localInstanceStopped(state string) bool {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "stopped", "exited", "dead", "failed":
		return true
	default:
		return false
	}
}

func (m *Manager) deleteLocalInstance(ctx context.Context, vm ProvisionedInstance) error {
	if m.LifecycleState != nil {
		record, err := m.LifecycleState.Read(ctx, vm.Name)
		if err != nil {
			return err
		}
		inventory, err := m.inventoryProvider(ctx)
		if err != nil {
			return err
		}
		return m.cleanupLifecycleRecord(ctx, record, inventoryByName(inventory)[vm.Name])
	}
	cleanupCtx, cancel := context.WithTimeout(ctx, cleanupTimeout)
	defer cancel()
	if vm.ProviderID == "" && m.Lifecycle == nil && m.Provider != nil {
		stopCtx, stopCancel := context.WithTimeout(cleanupCtx, 60*time.Second)
		_ = m.Provider.Stop(stopCtx, vm.Name)
		stopCancel()
		deleteCtx, deleteCancel := context.WithTimeout(cleanupCtx, 60*time.Second)
		err := m.Provider.Delete(deleteCtx, vm.Name)
		deleteCancel()
		return err
	}
	instance, err := m.providerInstance(cleanupCtx, vm.Name)
	if err != nil {
		return err
	}
	if vm.ProviderID != "" && instance.ProviderID != vm.ProviderID {
		return fmt.Errorf("same-name provider instance id=%s does not match expected id=%s; refusing deletion", instance.ProviderID, vm.ProviderID)
	}
	stopCtx, stopCancel := context.WithTimeout(cleanupCtx, 60*time.Second)
	_ = m.stopProviderInstance(stopCtx, instance)
	stopCancel()
	deleteCtx, deleteCancel := context.WithTimeout(cleanupCtx, 60*time.Second)
	err = m.deleteProviderInstance(deleteCtx, instance)
	deleteCancel()
	if err != nil {
		return err
	}
	if releaseErr := m.releaseInstanceTranscripts(vm); releaseErr != nil {
		m.logger().Warn("instance transcript close failed after local deletion", "provider", m.Config.Provider.Type, "instance", vm.Name, "operation", "reconcile", "error", releaseErr)
	}
	return nil
}

func (m *Manager) deleteRemoteRunner(ctx context.Context, runner gh.Runner) error {
	deleteCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	return m.GitHub.DeleteRunnerIfExists(deleteCtx, runner.ID)
}

type replacementRetryState struct {
	attempt int
	next    time.Time
}

func (s *replacementRetryState) active(now time.Time) bool {
	return !s.next.IsZero() && now.Before(s.next)
}

func (s *replacementRetryState) remaining(now time.Time) time.Duration {
	if !s.active(now) {
		return 0
	}
	return s.next.Sub(now).Round(time.Second)
}

func (s *replacementRetryState) schedule(m *Manager, now time.Time, err error) {
	initial, maximum, multiplier, jitter := m.replacementRetrySettings()
	nominal := float64(initial) * math.Pow(multiplier, float64(s.attempt))
	if nominal > float64(maximum) {
		nominal = float64(maximum)
	}
	factor := 1.0
	if jitter > 0 {
		factor += ((m.randomValue() * 2) - 1) * jitter
	}
	delay := time.Duration(nominal * factor)
	if delay < time.Second {
		delay = time.Second
	}
	if delay > maximum {
		delay = maximum
	}
	if retryAfter := retryAfterDuration(err); retryAfter > delay {
		delay = retryAfter
	}
	s.attempt++
	s.next = now.Add(delay)
}

func (s *replacementRetryState) reset() {
	s.attempt = 0
	s.next = time.Time{}
}

func (s *replacementRetryState) resetAfterAdoption(before, after map[string]ProvisionedInstance) {
	if adoptedReadyInstance(before, after) {
		s.reset()
	}
}

func (m *Manager) replacementRetrySettings() (time.Duration, time.Duration, float64, float64) {
	initialSeconds := m.Config.Pool.ReplacementRetryInitialSeconds
	if initialSeconds <= 0 {
		initialSeconds = 15
	}
	maximumSeconds := m.Config.Pool.ReplacementRetryMaxSeconds
	if maximumSeconds <= 0 {
		maximumSeconds = 1800
	}
	multiplier := m.Config.Pool.ReplacementRetryMultiplier
	if multiplier < 1 {
		multiplier = 2
	}
	jitterPercent := m.Config.Pool.ReplacementRetryJitterPercent
	if jitterPercent < 0 {
		jitterPercent = 0
	}
	return time.Duration(initialSeconds) * time.Second, time.Duration(maximumSeconds) * time.Second, multiplier, float64(jitterPercent) / 100
}

func (m *Manager) currentTime() time.Time {
	if m.now != nil {
		return m.now()
	}
	return time.Now()
}

func (m *Manager) randomValue() float64 {
	if m.randomFloat64 != nil {
		return m.randomFloat64()
	}
	return rand.Float64()
}

func retryAfterDuration(err error) time.Duration {
	var httpErr *gh.HTTPError
	if errors.As(err, &httpErr) {
		return httpErr.RetryAfter
	}
	return 0
}

func isTransientDependencyError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var httpErr *gh.HTTPError
	if errors.As(err, &httpErr) {
		return httpErr.StatusCode == http.StatusTooManyRequests || httpErr.StatusCode >= http.StatusInternalServerError
	}
	var networkErr net.Error
	if errors.As(err, &networkErr) {
		return true
	}
	text := strings.ToLower(err.Error())
	for _, match := range dependencyHTTPStatusPattern.FindAllStringSubmatch(text, -1) {
		statusCode, parseErr := strconv.Atoi(match[1])
		if parseErr == nil && (statusCode == http.StatusTooManyRequests || statusCode >= http.StatusInternalServerError) {
			return true
		}
	}
	for _, marker := range []string{
		"badgateway", "gatewaytimeout", "internalservererror", "serviceunavailable",
		"bad gateway", "gateway timeout", "internal server error", "service unavailable",
		"connection reset", "connection refused", "temporary failure",
		"tls handshake timeout", "i/o timeout", "no such host",
	} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

func (m *Manager) cleanupWithFreshContext() error {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	return m.cleanupUnlocked(cleanupCtx)
}

func (m *Manager) cleanupAfterTerminalFailure(active map[string]ProvisionedInstance, keep bool) error {
	if keep {
		return nil
	}
	var firstErr error
	for _, vm := range active {
		var err error
		switch vm.Phase {
		case LifecycleReady:
			err = m.retireInstance(context.Background(), vm, "controller stopped after terminal pool failure")
		case LifecycleCleanupPending, LifecycleProvisioning:
			err = m.deleteLocalInstance(context.Background(), vm)
		case LifecycleDraining, LifecycleQuarantined:
			// These instances may already be running a job. They stay counted and
			// are reconciled by the next controller instead of being killed merely
			// because GitHub readiness or status became uncertain.
			continue
		}
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (m *Manager) ProvisionPool(ctx context.Context, instances int, register bool) ([]ProvisionedInstance, error) {
	poolLock, err := m.AcquirePoolControllerLock()
	if err != nil {
		return nil, err
	}
	defer poolLock.Close()
	if err := m.recoverInterruptedProvisionLeases(ctx); err != nil {
		return nil, err
	}
	hostTrustLock, err := m.AcquireHostTrustControllerLock()
	if err != nil {
		return nil, err
	}
	if hostTrustLock != nil {
		defer hostTrustLock.Close()
	}
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
	poolLock, err := m.AcquirePoolControllerLock()
	if err != nil {
		return err
	}
	defer poolLock.Close()
	if err := m.recoverInterruptedProvisionLeases(ctx); err != nil {
		return err
	}
	return m.cleanupUnlocked(ctx)
}

func (m *Manager) cleanupUnlocked(ctx context.Context) error {
	if m.LifecycleState != nil {
		return m.cleanupOwnedLifecycle(ctx)
	}
	if m.Lifecycle == nil && m.Provider != nil {
		return m.cleanupLegacyTestProvider(ctx)
	}
	var firstErr error
	items, err := m.inventoryProvider(ctx)
	if err != nil {
		firstErr = err
	}
	for _, item := range items {
		vm := item.Instance
		if !HasPrefix(vm.Name, m.Config.Pool.NamePrefix) {
			continue
		}
		if vm.ProviderID == "" {
			m.warnf("cleanup: provider instance %s has no immutable identity; leaving it report-only\n", vm.Name)
			continue
		}
		if m.DryRun {
			m.infof("[dry-run] cleanup would delete exact provider instance %s id=%s\n", vm.Name, vm.ProviderID)
			continue
		}
		m.infof("cleanup: deleting instance %s\n", vm.Name)
		stopCtx, stopCancel := context.WithTimeout(ctx, 60*time.Second)
		_ = m.stopProviderInstance(stopCtx, vm)
		stopCancel()
		deleteCtx, deleteCancel := context.WithTimeout(ctx, 60*time.Second)
		deleteErr := m.deleteProviderInstance(deleteCtx, vm)
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

// cleanupLegacyTestProvider keeps the old in-memory test seam isolated from
// production. Every registry-constructed manager has Lifecycle and durable
// state, so real cleanup always uses immutable provider and GitHub identities.
func (m *Manager) cleanupLegacyTestProvider(ctx context.Context) error {
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
		deleteCancel()
		if deleteErr != nil && firstErr == nil {
			firstErr = deleteErr
		}
	}
	if m.GitHub != nil {
		deleteCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		_, err := m.GitHub.DeleteRunnersByPrefix(deleteCtx, m.Config.Pool.NamePrefix)
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (m *Manager) Status(ctx context.Context) (string, error) {
	var b strings.Builder
	updateStatus, updateErr := m.ImageUpdatePolicyStatus()
	if updateErr != nil {
		if m.Config.Image.UpdateFrequency == config.ImageUpdateFrequencyManual {
			fmt.Fprintf(&b, "Image updates:\n  policy=manual\tstate=unavailable\terror=%s\n", updateErr)
		} else {
			fmt.Fprintf(&b, "Image updates:\n  policy=%s at %s local\tstate=unavailable\terror=%s\n", m.Config.Image.UpdateFrequency, m.Config.Image.UpdateTime, updateErr)
		}
	} else {
		if updateStatus.Frequency == config.ImageUpdateFrequencyManual {
			fmt.Fprintf(&b, "Image updates:\n  policy=manual")
		} else {
			fmt.Fprintf(&b, "Image updates:\n  policy=%s at %s local", updateStatus.Frequency, updateStatus.UpdateTime)
		}
		if !updateStatus.LastSuccessfulCheckAt.IsZero() {
			fmt.Fprintf(&b, "\tlast=%s", updateStatus.LastSuccessfulCheckAt.In(time.Local).Format("2006-01-02 15:04 MST"))
		}
		if !updateStatus.NextEligibleAt.IsZero() {
			fmt.Fprintf(&b, "\tnext=%s", updateStatus.NextEligibleAt.In(time.Local).Format("2006-01-02 15:04 MST"))
		}
		if !updateStatus.NextRetryAt.IsZero() {
			fmt.Fprintf(&b, "\tretry=%s", updateStatus.NextRetryAt.In(time.Local).Format("2006-01-02 15:04 MST"))
		}
		if updateStatus.Pending {
			fmt.Fprintf(&b, "\tpending=%s", updateStatus.PendingIdentity)
		}
		b.WriteString("\n")
		if updateStatus.DeferredReason != "" {
			fmt.Fprintf(&b, "  deferred: %s\n", updateStatus.DeferredReason)
		}
		if updateStatus.LastError != "" {
			fmt.Fprintf(&b, "  last error: %s\n", updateStatus.LastError)
		}
	}
	items, err := m.inventoryProvider(ctx)
	if err != nil {
		return "", err
	}
	b.WriteString("Instances:\n")
	for _, item := range items {
		vm := item.Instance
		if HasPrefix(vm.Name, m.Config.Pool.NamePrefix) {
			fmt.Fprintf(&b, "  %s\t%s\tid=%s\n", vm.Name, item.State, emptyDash(vm.ProviderID))
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
	return m.provisionOneAttempt(ctx, name, register, allowBusy)
}

func (m *Manager) provisionOneAttempt(ctx context.Context, name string, register, allowBusy bool) (vm ProvisionedInstance, err error) {
	logPath := m.instanceLogPath(name, "."+m.Config.Provider.Type+".log")
	guestLogPath := m.instanceLogPath(name, ".guest.log")
	vm = ProvisionedInstance{Name: name, LogPath: logPath, GuestLogPath: guestLogPath, ProviderOwned: true}
	if register && m.GitHub != nil && !m.DryRun && m.LifecycleState != nil {
		if runner, found, lookupErr := m.GitHub.RunnerByName(ctx, name); lookupErr != nil {
			return vm, fmt.Errorf("verify exact GitHub runner name is unallocated: %w", lookupErr)
		} else if found {
			return vm, fmt.Errorf("GitHub runner name %q is already allocated to id=%d", name, runner.ID)
		}
	}
	if err := m.preflightStorage(m.instanceCreateOperationPlan()); err != nil {
		return vm, err
	}
	if err := m.reserveLifecycle(ctx, name); err != nil {
		return vm, err
	}
	vm.Phase = LifecycleProvisioning
	if err := m.acquireLifecycleLease(ctx, name, "provision", "controller", 2*time.Hour); err != nil {
		return vm, fmt.Errorf("acquire provisioning lifecycle lease: %w", err)
	}
	configureAttempted := false
	listenerMayBeRunning := false
	defer func() {
		m.releaseLifecycleLease(context.Background(), name, "provision", "controller")
		if err == nil {
			vm.Phase = LifecycleReady
			return
		}
		remoteKnownAbsent := !configureAttempted
		if configureAttempted && m.GitHub != nil {
			runner, found, lookupErr := m.GitHub.RunnerByName(context.Background(), name)
			if lookupErr != nil {
				m.quarantineLifecycle(context.Background(), name, fmt.Errorf("%w; exact GitHub registration lookup failed: %v", err, lookupErr))
				vm.Phase = LifecycleQuarantined
				return
			}
			remoteKnownAbsent = !found
			if found {
				vm.RunnerID = runner.ID
				if recordErr := m.recordLifecycleRegistered(context.Background(), name, runner.ID); recordErr != nil {
					m.quarantineLifecycle(context.Background(), name, fmt.Errorf("%w; exact GitHub runner id=%d could not be recorded: %v", err, runner.ID, recordErr))
					vm.Phase = LifecycleQuarantined
					return
				}
			}
		}
		if listenerMayBeRunning {
			m.quarantineLifecycle(context.Background(), name, err)
			vm.Phase = LifecycleQuarantined
			return
		}
		if m.LifecycleState == nil {
			if vm.RunnerID != 0 && m.GitHub != nil {
				if deleteErr := m.GitHub.DeleteRunnerIfExists(context.Background(), vm.RunnerID); deleteErr != nil {
					vm.Phase = LifecycleCleanupPending
					err = errors.Join(err, fmt.Errorf("rollback exact GitHub runner id=%d: %w", vm.RunnerID, deleteErr))
					return
				}
			}
			cleanupErr := m.deleteLocalInstance(context.Background(), vm)
			if cleanupErr != nil {
				vm.Phase = LifecycleCleanupPending
				err = errors.Join(err, fmt.Errorf("rollback local instance %s: %w", name, cleanupErr))
			} else {
				vm.Phase = ""
			}
			return
		}
		record, recordErr := m.LifecycleState.Read(context.Background(), name)
		if recordErr != nil {
			vm.Phase = LifecycleCleanupPending
			err = errors.Join(err, fmt.Errorf("read lifecycle for rollback: %w", recordErr))
			return
		}
		inventory, inventoryErr := m.inventoryProvider(context.Background())
		if inventoryErr != nil {
			m.quarantineLifecycle(context.Background(), name, errors.Join(err, inventoryErr))
			vm.Phase = LifecycleQuarantined
			return
		}
		cleanupErr := m.cleanupLifecycleRecordWithRemoteAbsence(context.Background(), record, inventoryByName(inventory)[name], remoteKnownAbsent)
		if cleanupErr != nil {
			vm.Phase = LifecycleCleanupPending
			err = errors.Join(err, fmt.Errorf("rollback local instance %s: %w", name, cleanupErr))
			return
		}
		vm.Phase = ""
	}()
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
	creationMessage := "cloning instance"
	creationOperation := "clone"
	if m.Config.Provider.Type == "docker-sandboxes" {
		creationMessage = "creating Docker Sandboxes instance"
		creationOperation = "create"
	}
	m.logger().Info(creationMessage, "provider", m.Config.Provider.Type, "instance", name, "operation", creationOperation, "sourceImage", m.Config.Provider.SourceImage, "logPath", logPath)
	var created provider.Instance
	createStage := "instance_container_create"
	if m.Config.Provider.Type == "docker-sandboxes" {
		createStage = "sandbox_create_and_initial_identity_verification"
	}
	createStageErr := m.timeFirstInstanceStage(name, createStage, func() error {
		return m.runDockerSandboxesCreateProgress(name, func() error {
			var createErr error
			created, createErr = m.createProviderInstance(ctx, name)
			return createErr
		})
	})
	if created.Name != "" || created.ProviderID != "" || created.ReceiptVersion != "" || len(created.Receipt) != 0 {
		if created.Name != name || created.ProviderID == "" {
			identityErr := fmt.Errorf("provider create returned an incomplete immutable identity for %q", name)
			if createStageErr != nil {
				return vm, errors.Join(createStageErr, identityErr)
			}
			return vm, identityErr
		}
		if created.ReceiptVersion == "" || len(created.Receipt) == 0 {
			receiptErr := fmt.Errorf("provider create returned an incomplete versioned receipt for %q", name)
			if createStageErr != nil {
				return vm, errors.Join(createStageErr, receiptErr)
			}
			return vm, receiptErr
		}
		var providerReceipt map[string]any
		if json.Unmarshal(created.Receipt, &providerReceipt) != nil || providerReceipt == nil {
			receiptErr := fmt.Errorf("provider create returned an invalid versioned receipt for %q", name)
			if createStageErr != nil {
				return vm, errors.Join(createStageErr, receiptErr)
			}
			return vm, receiptErr
		}
		vm.ProviderID = created.ProviderID
		if recordErr := m.recordLifecycleCreated(context.WithoutCancel(ctx), created); recordErr != nil {
			if createStageErr != nil {
				return vm, errors.Join(createStageErr, recordErr)
			}
			return vm, recordErr
		}
	}
	if createStageErr != nil {
		return vm, createStageErr
	}
	if created.Name != name || created.ProviderID == "" {
		return vm, fmt.Errorf("provider create returned no immutable identity for %q", name)
	}
	if err := m.recordLifecycleValidationIntent(ctx, name); err != nil {
		return vm, fmt.Errorf("record runtime validation intent: %w", err)
	}
	applyNetworkPolicy := func() error {
		return m.applyProviderNetworkPolicy(ctx, created)
	}
	var networkPolicyErr error
	if m.Config.Provider.Type == "docker-sandboxes" {
		networkPolicyErr = m.timeFirstInstanceStage(name, "sandbox_network_policy_apply_and_readback", func() error {
			return m.runDockerSandboxesPostCreateStage(name, "sandbox-network-policy", "Docker Sandboxes network policy application and readback", applyNetworkPolicy)
		})
	} else {
		networkPolicyErr = applyNetworkPolicy()
	}
	if networkPolicyErr != nil {
		return vm, fmt.Errorf("apply provider network policy: %w", networkPolicyErr)
	}
	verifyAdmission := func() error {
		return m.verifyProviderAdmission(ctx, created)
	}
	var admissionErr error
	if m.Config.Provider.Type == "docker-sandboxes" {
		admissionErr = m.timeFirstInstanceStage(name, "sandbox_post_create_admission", func() error {
			return m.runDockerSandboxesPostCreateStage(name, "sandbox-post-create-admission", "Docker Sandboxes post-create admission verification", verifyAdmission)
		})
	} else {
		admissionErr = verifyAdmission()
	}
	if admissionErr != nil {
		return vm, admissionErr
	}
	m.logger().Info("starting instance", "provider", m.Config.Provider.Type, "instance", name, "operation", "start", "logPath", logPath)
	if err := m.timeFirstInstanceStage(name, m.startupInstanceStartStage(), func() error {
		startOptions, startOptionsErr := m.startOptions(logPath, name)
		if startOptionsErr != nil {
			return startOptionsErr
		}
		_, err := m.startProviderInstance(ctx, created, startOptions)
		return err
	}); err != nil {
		return vm, err
	}
	ip, available, err := m.providerAddress(ctx, created, m.Config.Timeouts.BootSeconds)
	if err != nil {
		return vm, err
	}
	vm.IP = ip
	if available {
		m.logger().Info("instance reachable", "provider", m.Config.Provider.Type, "instance", name, "operation", "wait-reachable", "address", ip)
	} else {
		m.logger().Info("instance uses delegated provider execution", "provider", m.Config.Provider.Type, "instance", name, "operation", "wait-reachable")
	}
	if m.hostTrustEnabled() {
		trustSnapshot, err = m.resolveHostTrust(ctx)
		if err != nil {
			return vm, fmt.Errorf("refresh host trust before runtime installation: %w", err)
		}
		if err := m.installHostTrustRuntime(ctx, name, trustSnapshot); err != nil {
			return vm, err
		}
		vm.HostTrustGeneration = trustSnapshot.Generation
	}
	m.logger().Info("validating runner runtime", "provider", m.Config.Provider.Type, "instance", name, "operation", "validate-runtime", "stage", "start")
	if err := m.timeFirstInstanceStage(name, "runtime_validation", func() error {
		if err := m.configureDockerRegistryMirrors(ctx, name); err != nil {
			return err
		}
		return m.verifyProviderRuntimeWithRetry(ctx, created, guestLogPath)
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
			if installErr := m.installHostTrustRuntime(ctx, name, currentTrust); installErr != nil {
				return vm, fmt.Errorf("%w: %v; runtime refresh failed: %v", errHostTrustImageMismatch, err, installErr)
			}
			marker, err = m.readInstanceHostTrustMarker(ctx, name)
			if err != nil {
				return vm, fmt.Errorf("%w: read refreshed marker: %v", errHostTrustImageMismatch, err)
			}
			if err := validateHostTrustMarkerAgainstSnapshot(marker, currentTrust); err != nil {
				return vm, fmt.Errorf("%w: refreshed runtime marker: %v", errHostTrustImageMismatch, err)
			}
		}
		// Track the immutable generation read from the cloned image, not merely
		// the pre-clone snapshot. This prevents a trust-store change racing image
		// cloning from making the supervisor believe a stale image is current.
		vm.HostTrustGeneration = marker.Generation
		trustSnapshot = currentTrust
	}
	if err := m.recordLifecycleValidated(ctx, name); err != nil {
		return vm, fmt.Errorf("record validated runtime: %w", err)
	}
	if register {
		if err := m.recordLifecycleRegistrationIntent(ctx, name); err != nil {
			return vm, fmt.Errorf("record GitHub registration intent: %w", err)
		}
		if err := m.issueHostTrustLease(ctx, name, trustSnapshot); err != nil {
			return vm, fmt.Errorf("issue host trust lease: %w", err)
		}
		if err := m.verifyProviderAdmission(ctx, created); err != nil {
			return vm, err
		}
		if m.GitHub == nil {
			if m.DryRun {
				m.infof("[dry-run] would register GitHub runner %s with labels %s\n", name, strings.Join(m.Config.Runner.Labels, ","))
				return vm, nil
			}
			return vm, fmt.Errorf("github client is required for registration")
		}
		if err := m.PreflightRunnerGroup(ctx); err != nil {
			return vm, err
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
				"RUNNER_NAME":              name,
				"RUNNER_LABELS":            strings.Join(m.Config.Runner.Labels, ","),
				"RUNNER_EPHEMERAL":         fmt.Sprintf("%t", m.Config.Runner.Ephemeral),
				"RUNNER_GROUP":             m.Config.Runner.Group,
				"RUNNER_NO_DEFAULT_LABELS": fmt.Sprintf("%t", m.Config.Runner.NoDefaultLabels),
			}
			configureAttempted = true
			if _, err := m.execGuest(ctx, name, []string{"sudo", "-E", "bash", "/opt/epar/configure-runner.sh"}, provider.ExecOptions{Env: env, Stdin: token.Token + "\n", SensitiveValues: []string{token.Token}}); err != nil {
				return provider.RedactError(err, token.Token)
			}
			m.infof("[%s] starting runner service\n", name)
			listenerMayBeRunning = true
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
		if err := m.recordLifecycleRegistered(ctx, name, runner.ID); err != nil {
			return vm, fmt.Errorf("record exact GitHub runner identity: %w", err)
		}
		m.infof("[%s] GitHub runner %s id=%d busy=%t\n", name, readiness, runner.ID, runner.Busy)
		m.finishFirstRunnerReady(name)
	} else {
		m.finishFirstRunnerReady(name)
	}
	return vm, nil
}

func (m *Manager) PreflightRunnerGroup(ctx context.Context) error {
	if m.DryRun {
		m.infof("[dry-run] would verify GitHub runner-group security policy before registration\n")
		return nil
	}
	if m.GitHub == nil {
		return fmt.Errorf("runner-group security preflight requires a GitHub client")
	}
	policy := m.Config.Security.RunnerGroup
	result, err := m.GitHub.EvaluateRunnerGroupPolicy(ctx, m.Config.Runner.Group, policy)
	if err != nil {
		message := fmt.Sprintf("runner-group security preflight could not read GitHub policy: %v", err)
		if policy.Enforcement == config.RunnerGroupEnforcementWarn {
			m.warnf("warning: %s; continuing because security.runnerGroup.enforcement is warn\n", message)
			return nil
		}
		return fmt.Errorf("%s", message)
	}
	for _, advisory := range result.Advisories {
		m.warnf("warning: runner-group security advisory: %s\n", advisory)
	}
	if len(result.Violations) > 0 {
		message := strings.Join(result.Violations, "; ")
		if policy.Enforcement == config.RunnerGroupEnforcementWarn {
			m.warnf("warning: runner-group security policy violation: %s; continuing because security.runnerGroup.enforcement is warn\n", message)
			return nil
		}
		return fmt.Errorf("runner-group security preflight failed: %s", message)
	}
	if result.Resolved {
		m.infof("runner-group security preflight passed for %q\n", result.Group.Name)
	}
	return nil
}

func (m *Manager) startupInstanceStartStage() string {
	if m.Config.Provider.Type == "docker-container" {
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
			instance, instanceErr := m.providerInstance(waitCtx, vm.Name)
			if instanceErr != nil {
				cancel()
				return gh.Runner{}, instanceErr
			}
			if err := m.verifyProviderAdmission(waitCtx, instance); err != nil {
				cancel()
				return gh.Runner{}, err
			}
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
	if _, globalAdmission := m.Lifecycle.(provider.AdmissionVerifier); globalAdmission {
		instance, err := m.providerInstance(ctx, vm.Name)
		if err != nil {
			return false, "provider instance identity is unavailable", err
		}
		if err := m.verifyProviderAdmission(ctx, instance); err != nil {
			return false, "provider admission changed", err
		}
	}
	if m.GitHub != nil {
		runner, found, err := m.GitHub.RunnerByName(ctx, vm.Name)
		if err != nil {
			if !isTransientGitHubLivenessError(err) {
				return true, "", err
			}
		} else {
			if !found {
				if err := m.recordLifecycleRemoteAbsence(ctx, vm.Name); err != nil {
					return false, "GitHub runner record is gone", fmt.Errorf("record GitHub runner absence: %w", err)
				}
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

func recordRunnerLiveness(confirmedInactive map[string]int, name string, alive bool, reason string, err error) (int, bool) {
	if err != nil || alive {
		delete(confirmedInactive, name)
		return 0, false
	}
	if reason != runnerProcessInactiveReason {
		delete(confirmedInactive, name)
		return 0, true
	}
	confirmedInactive[name]++
	return confirmedInactive[name], confirmedInactive[name] >= runnerConfirmedInactiveCheckLimit
}

func (m *Manager) runnerProcessAlive(ctx context.Context, vm ProvisionedInstance) (bool, string, error) {
	running, err := m.probeRunnerProcess(ctx, vm.Name)
	if err != nil {
		return true, "runner process health could not be measured", err
	}
	if !running {
		return false, runnerProcessInactiveReason, nil
	}
	return true, "", nil
}

func (m *Manager) checkRunnerProcess(ctx context.Context, name string) error {
	running, err := m.probeRunnerProcess(ctx, name)
	if err != nil {
		return err
	}
	if !running {
		return fmt.Errorf(runnerProcessInactiveReason)
	}
	return nil
}

func (m *Manager) probeRunnerProcess(ctx context.Context, name string) (bool, error) {
	checkCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	script := fmt.Sprintf("if test -x /opt/epar/check-runner.sh; then if sudo bash /opt/epar/check-runner.sh; then printf '%%s\\n' %s; else printf '%%s\\n' %s; fi; elif systemctl is-active --quiet actions-runner.service; then printf '%%s\\n' %s; else printf '%%s\\n' %s; fi", shellQuote(runnerProcessRunningSentinel), shellQuote(runnerProcessStoppedSentinel), shellQuote(runnerProcessRunningSentinel), shellQuote(runnerProcessStoppedSentinel))
	result, err := m.execGuest(checkCtx, name, provider.ShellCommand(script), provider.ExecOptions{SuppressTranscript: true})
	if err != nil {
		return false, fmt.Errorf("execute runner process health probe: %w", err)
	}
	switch strings.TrimSpace(result.Stdout) {
	case runnerProcessRunningSentinel:
		return true, nil
	case runnerProcessStoppedSentinel:
		return false, nil
	default:
		return false, fmt.Errorf("runner process health probe returned an unsupported response")
	}
}

func isTransientGitHubLivenessError(err error) bool {
	var httpErr *gh.HTTPError
	return errors.As(err, &httpErr) && (httpErr.StatusCode == http.StatusTooManyRequests || httpErr.StatusCode >= http.StatusInternalServerError)
}

func (m *Manager) retireInstance(ctx context.Context, vm ProvisionedInstance, reason string) error {
	if m.LifecycleState != nil && !vm.ProviderOwned {
		return fmt.Errorf("refusing to retire unowned provider instance %q; prefix-only resources are report-only", vm.Name)
	}
	m.infof("[%s] retiring instance: %s\n", vm.Name, reason)
	if m.LifecycleState != nil {
		record, err := m.LifecycleState.Read(ctx, vm.Name)
		if err != nil {
			return err
		}
		inventory, err := m.inventoryProvider(ctx)
		if err != nil {
			return err
		}
		return m.cleanupLifecycleRecord(ctx, record, inventoryByName(inventory)[vm.Name])
	}
	var firstErr error
	if m.GitHub != nil && vm.RunnerID != 0 {
		deleteCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		if err := m.GitHub.DeleteRunnerIfExists(deleteCtx, vm.RunnerID); err != nil {
			cancel()
			return err
		}
		cancel()
	}
	if vm.ProviderID == "" && m.Lifecycle == nil && m.Provider != nil {
		stopCtx, stopCancel := context.WithTimeout(ctx, 60*time.Second)
		_ = m.Provider.Stop(stopCtx, vm.Name)
		stopCancel()
		deleteCtx, deleteCancel := context.WithTimeout(ctx, 60*time.Second)
		deleteErr := m.Provider.Delete(deleteCtx, vm.Name)
		deleteCancel()
		if deleteErr == nil {
			if releaseErr := m.releaseInstanceTranscripts(vm); releaseErr != nil {
				m.logger().Warn("instance transcript close failed after retirement", "provider", m.Config.Provider.Type, "instance", vm.Name, "operation", "retire", "error", releaseErr)
			}
		}
		return deleteErr
	}
	instance, err := m.providerInstance(ctx, vm.Name)
	if err != nil {
		return err
	}
	if vm.ProviderID != "" && instance.ProviderID != vm.ProviderID {
		return fmt.Errorf("same-name provider instance id=%s does not match expected id=%s; refusing retirement", instance.ProviderID, vm.ProviderID)
	}
	stopCtx, stopCancel := context.WithTimeout(ctx, 60*time.Second)
	_ = m.stopProviderInstance(stopCtx, instance)
	stopCancel()
	deleteCtx, deleteCancel := context.WithTimeout(ctx, 60*time.Second)
	deleteErr := m.deleteProviderInstance(deleteCtx, instance)
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

func (m *Manager) verifyProviderRuntimeWithRetry(ctx context.Context, instance provider.Instance, guestLogPath string) error {
	const attempts = 2
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		err := m.verifyProviderRuntime(ctx, instance)
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
		m.warnf("[%s] runtime validation attempt %d/%d failed: %v\n", instance.Name, attempt, attempts, err)
		m.infof("[%s] retrying runtime validation in %s; guest log: %s\n", instance.Name, runtimeValidationRetryDelay, guestLogPath)
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
	if err := m.copyTextGuest(ctx, name, "/opt/epar/configure-docker-daemon.sh", "0755", guestText(content), false); err != nil {
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
	if !opts.SuppressTranscript {
		if opts.LogPath == "" {
			opts.LogPath = m.instanceLogPath(name, ".guest.log")
		}
		transcript, err := m.transcript(opts.LogPath, name, transcriptComponent(opts.LogPath))
		if err != nil {
			return provider.ExecResult{}, err
		}
		opts.Stdout = transcript.Stdout
		opts.Stderr = transcript.Stderr
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if m.Lifecycle != nil {
		instance, err := m.providerInstance(cctx, name)
		if err != nil {
			return provider.ExecResult{}, err
		}
		providerOpts := opts
		providerOpts.LogPath = ""
		providerOpts.Env = nil
		return m.Lifecycle.Exec(cctx, instance, provider.EnvCommand(opts.Env, cmd), providerOpts)
	}
	if m.Provider == nil {
		return provider.ExecResult{}, fmt.Errorf("provider lifecycle is required")
	}
	return m.Provider.Exec(cctx, name, cmd, opts)
}

func emptyDash(value string) string {
	if value == "" {
		return "-"
	}
	return value
}
