package pool

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/dependency"
)

type externalOutageRuntime struct {
	policy     dependency.Policy
	supervisor *dependency.Supervisor
}

func (m *Manager) ConfigureExternalOutageRetry(policy ExternalOutageRetryPolicy) error {
	if !policy.Valid() {
		return fmt.Errorf("invalid external outage retry policy")
	}
	m.externalOutageMu.Lock()
	defer m.externalOutageMu.Unlock()
	if m.externalOutage != nil && m.externalOutage.policy == policy {
		return nil
	}
	m.externalOutage = &externalOutageRuntime{policy: policy}
	return nil
}

func (m *Manager) externalOutageSupervisor() (*dependency.Supervisor, error) {
	m.externalOutageMu.Lock()
	defer m.externalOutageMu.Unlock()
	if m.externalOutage == nil || m.externalOutage.policy.IsOff() {
		return nil, nil
	}
	if m.externalOutage.supervisor != nil {
		return m.externalOutage.supervisor, nil
	}
	path, err := dependency.ExternalOutageStatePath(m.ProjectRoot, m.ConfigPath)
	if err != nil {
		return nil, fmt.Errorf("resolve external outage supervision state: %w", err)
	}
	initial, maximum, multiplier, jitter := m.replacementRetrySettings()
	settings := dependency.BackoffSettings{Initial: initial, Maximum: maximum, Multiplier: multiplier, Jitter: jitter, Minimum: time.Second, Random: m.randomValue}
	supervisor, err := dependency.NewSupervisor(dependency.SupervisorOptions{Policy: m.externalOutage.policy, Backoff: settings, Path: path, Now: m.currentTime, PID: os.Getpid()})
	if err != nil {
		return nil, err
	}
	m.externalOutage.supervisor = supervisor
	return supervisor, nil
}

func (m *Manager) externalOutageEnabled() bool {
	m.externalOutageMu.Lock()
	defer m.externalOutageMu.Unlock()
	return m.externalOutage != nil && !m.externalOutage.policy.IsOff()
}

func (m *Manager) RunExternalOutageStage(ctx context.Context, stage string, operation func(context.Context) error) error {
	if !m.externalOutageEnabled() {
		return operation(ctx)
	}
	supervisor, err := m.externalOutageSupervisor()
	if err != nil {
		return err
	}
	for {
		if err := supervisor.WaitUntilReady(ctx); err != nil {
			m.logExternalOutageExhaustion(err)
			return err
		}
		attemptCtx, cancelAttempt := supervisor.AttemptContext(ctx)
		err := operation(attemptCtx)
		attemptContextErr := attemptCtx.Err()
		cancelAttempt()
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if attemptContextErr != nil {
			if deadlineErr := supervisor.WaitUntilReady(ctx); deadlineErr != nil {
				m.logExternalOutageExhaustion(deadlineErr)
				return deadlineErr
			}
		}
		if !dependency.IsTypedRetryable(err) {
			return err
		}
		decision, scheduleErr := supervisor.Schedule(stage, err)
		if scheduleErr != nil {
			m.logExternalOutageExhaustion(scheduleErr)
			return scheduleErr
		}
		m.logExternalOutageRetry(decision, stage, err)
	}
}

func (m *Manager) deferExternalOutage(stage string, failure error) (bool, error) {
	if !m.externalOutageEnabled() {
		return false, failure
	}
	supervisor, err := m.externalOutageSupervisor()
	if err != nil {
		return true, err
	}
	if !dependency.IsTypedRetryable(failure) {
		if deadlineErr := supervisor.CheckDeadline(); deadlineErr != nil {
			m.logExternalOutageExhaustion(deadlineErr)
			return true, deadlineErr
		}
		return false, failure
	}
	decision, err := supervisor.Schedule(stage, failure)
	if err != nil {
		m.logExternalOutageExhaustion(err)
		return true, err
	}
	m.logExternalOutageRetry(decision, stage, failure)
	return true, nil
}

func (m *Manager) externalOutageCooldown(now time.Time) (time.Duration, bool, error) {
	if !m.externalOutageEnabled() {
		return 0, false, nil
	}
	supervisor, err := m.externalOutageSupervisor()
	if err != nil {
		return 0, false, err
	}
	remaining, active := supervisor.Cooldown(now)
	return remaining, active, nil
}

func (m *Manager) externalOutageReady(ctx context.Context) error {
	if !m.externalOutageEnabled() {
		return nil
	}
	supervisor, err := m.externalOutageSupervisor()
	if err != nil {
		return err
	}
	return supervisor.WaitUntilReady(ctx)
}

func (m *Manager) externalOutageAttemptContext(parent context.Context) (context.Context, context.CancelFunc, error) {
	if !m.externalOutageEnabled() {
		attemptCtx, cancel := context.WithCancel(parent)
		return attemptCtx, cancel, nil
	}
	supervisor, err := m.externalOutageSupervisor()
	if err != nil {
		return nil, nil, err
	}
	attemptCtx, cancel := supervisor.AttemptContext(parent)
	return attemptCtx, cancel, nil
}

func (m *Manager) markExternalOutageRecovered() error {
	var supervisor *dependency.Supervisor
	if m.externalOutageEnabled() {
		var err error
		supervisor, err = m.externalOutageSupervisor()
		if err != nil {
			return err
		}
	} else {
		path, err := dependency.ExternalOutageStatePath(m.ProjectRoot, m.ConfigPath)
		if err != nil {
			return err
		}
		state, err := dependency.ReadIncidentState(path)
		if err != nil {
			return err
		}
		if state.SchemaVersion == 0 || state.IncidentStartedAt.IsZero() || state.Outcome == "recovered" {
			return nil
		}
		supervisor, err = dependency.NewSupervisor(dependency.SupervisorOptions{Policy: dependency.NewPolicy(dependency.PolicyOff, 0), Backoff: dependency.DefaultBackoffSettings(), Path: path, Now: m.currentTime, PID: os.Getpid()})
		if err != nil {
			return err
		}
	}
	before := supervisor.State()
	if !before.Active {
		return nil
	}
	if err := supervisor.MarkRecovered(); err != nil {
		return err
	}
	duration := m.currentTime().UTC().Sub(before.IncidentStartedAt).Round(time.Second)
	m.infof("external outage recovered after %s; desired pool capacity is healthy\n", duration)
	return nil
}

func (m *Manager) logExternalOutageRetry(decision dependency.RetryDecision, stage string, failure error) {
	structured, _ := dependency.AsFailure(failure)
	safeFailure := dependency.SafeError(failure)
	service := "external dependency"
	requestID := ""
	if structured != nil {
		if structured.Service != "" {
			service = structured.Service
		}
		requestID = structured.RequestID
	}
	if decision.IncidentStarted {
		m.warnf("external outage incident started at stage %s for %s: %s\n", stage, service, safeFailure)
	}
	deadline := "continuous"
	if !decision.Deadline.IsZero() {
		deadline = decision.Deadline.In(time.Local).Format(time.RFC3339)
	}
	requestSuffix := ""
	if requestID != "" {
		requestSuffix = " requestId=" + requestID
	}
	m.warnf("external outage retry scheduled: stage=%s dependency=%s attempt=%d next=%s deadline=%s%s error=%s\n", stage, service, decision.Attempt, decision.NextRetryAt.In(time.Local).Format(time.RFC3339), deadline, requestSuffix, safeFailure)
}

func (m *Manager) logExternalOutageExhaustion(err error) {
	var exhausted *dependency.ExhaustedError
	if errors.As(err, &exhausted) {
		m.warnf("external outage retry exhausted after %s at stage %s: %s\n", m.currentTime().UTC().Sub(exhausted.State.IncidentStartedAt).Round(time.Second), exhausted.State.Stage, exhausted.State.LastSafeError)
	}
}

func (m *Manager) withOutageExhaustionCleanup(err error, active map[string]ProvisionedInstance, keep bool) error {
	var exhausted *dependency.ExhaustedError
	if !errors.As(err, &exhausted) {
		return err
	}
	return m.CleanupAfterExternalOutageExhaustion(err, keep)
}

func (m *Manager) cleanupAfterPoolFailure(err error, active map[string]ProvisionedInstance, keep bool) error {
	var exhausted *dependency.ExhaustedError
	if errors.As(err, &exhausted) {
		return m.CleanupAfterExternalOutageExhaustion(err, keep)
	}
	return errors.Join(err, m.cleanupAfterTerminalFailure(active, keep))
}

// CleanupAfterExternalOutageExhaustion applies the same exact, lease-aware
// cleanup used for a normal pool shutdown. The start command calls this while
// it still holds the canonical pool-controller lock when an early supervised
// preflight or required-image stage exhausts its bounded incident window.
func (m *Manager) CleanupAfterExternalOutageExhaustion(err error, keep bool) error {
	var exhausted *dependency.ExhaustedError
	if !errors.As(err, &exhausted) {
		return err
	}
	if keep {
		m.infof("Stopping EPAR pool after external outage retry exhaustion. --keep-on-exit is enabled, so owned runner resources will remain running.\n")
		return err
	}
	return errors.Join(err, m.cleanupPoolWithStatus("owned GitHub runner registrations and provider instances", m.cleanupWithFreshContext))
}

func (m *Manager) externalOutageStatus() string {
	path, err := dependency.ExternalOutageStatePath(m.ProjectRoot, m.ConfigPath)
	if err != nil {
		return fmt.Sprintf("External outage retry:\n  state=unavailable\terror=%s\n", err)
	}
	state, err := dependency.ReadIncidentState(path)
	if err != nil {
		return fmt.Sprintf("External outage retry:\n  state=unavailable\terror=%s\n", err)
	}
	if state.SchemaVersion == 0 {
		return "External outage retry:\n  state=none\n"
	}
	status := state.Outcome
	if state.Active {
		status = "active"
	}
	if status == "" {
		status = "inactive"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "External outage retry:\n  state=%s\tpolicy=%s", status, state.Policy)
	if state.Attempt > 0 {
		fmt.Fprintf(&b, "\tattempt=%d", state.Attempt)
	}
	if state.Stage != "" {
		fmt.Fprintf(&b, "\tstage=%s", state.Stage)
	}
	if state.Dependency != "" {
		fmt.Fprintf(&b, "\tdependency=%s", state.Dependency)
	}
	b.WriteString("\n")
	if !state.NextRetryAt.IsZero() {
		fmt.Fprintf(&b, "  next=%s\n", state.NextRetryAt.In(time.Local).Format("2006-01-02 15:04:05 MST"))
	}
	if !state.Deadline.IsZero() {
		fmt.Fprintf(&b, "  deadline=%s\n", state.Deadline.In(time.Local).Format("2006-01-02 15:04:05 MST"))
	}
	if state.PID > 0 || !state.UpdatedAt.IsZero() {
		ownerState := "stale"
		if processAlive(state.PID) {
			ownerState = "running"
		}
		fmt.Fprintf(&b, "  ownerPid=%d\townerState=%s\tupdated=%s\n", state.PID, ownerState, state.UpdatedAt.In(time.Local).Format("2006-01-02 15:04:05 MST"))
	}
	if state.RequestID != "" {
		fmt.Fprintf(&b, "  requestId=%s\n", state.RequestID)
	}
	if state.LastSafeError != "" {
		fmt.Fprintf(&b, "  last error: %s\n", state.LastSafeError)
	}
	if !state.RecoveredAt.IsZero() {
		fmt.Fprintf(&b, "  recovered=%s\n", state.RecoveredAt.In(time.Local).Format("2006-01-02 15:04:05 MST"))
	}
	if !state.ExhaustedAt.IsZero() {
		fmt.Fprintf(&b, "  exhausted=%s\n", state.ExhaustedAt.In(time.Local).Format("2006-01-02 15:04:05 MST"))
	}
	return b.String()
}
