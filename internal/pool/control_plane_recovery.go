package pool

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
)

const (
	providerRecoveryDaemonStop     = 2 * time.Minute
	providerRecoveryDaemonReadback = 30 * time.Second
	providerRecoveryProbeTimeout   = 45 * time.Second
	providerRecoveryProbeCount     = 3
	providerRecoveryProbeInterval  = 5 * time.Second
	providerRecoveryLockRetry      = 10 * time.Second
	providerRecoveryBackoffInitial = time.Minute
	providerRecoveryBackoffMaximum = 30 * time.Minute
	providerRecoverySafetyMargin   = time.Minute
)

// recoverProviderControlPlane performs the shared orchestration around a
// provider-owned recovery operation. The provider performs the exact daemon
// stop/start sequence; the pool owns policy, cross-process exclusion, retry
// backoff, and repeated post-recovery inventory verification.
//
// The handled result means the caller should retain its current lifecycle map
// and retry rather than clean up or terminate the pool. A provider that is not
// recovery-capable, an explicit observe mode, or a non-control-plane failure
// returns handled=false so existing fail-closed behavior remains unchanged.
func (m *Manager) recoverProviderControlPlane(ctx context.Context, cause error) (handled bool, err error) {
	admissionFailure := errors.Is(cause, provider.ErrControlPlaneAdmissionFailure)
	if (!admissionFailure && !errors.Is(cause, provider.ErrControlPlaneFailure)) || !m.dockerSandboxesExclusiveRecovery() {
		return false, nil
	}
	recoverer, ok := m.providerLifecycle().(provider.ControlPlaneRecoverer)
	if !ok {
		return false, nil
	}
	if !m.providerRecoveryWindowReady() {
		return true, nil
	}
	if permitted, admissionIncident := m.reserveProviderRecovery(admissionFailure); !permitted {
		next := m.scheduleProviderRecovery(providerRecoveryBackoffMaximum)
		if admissionFailure || admissionIncident {
			m.warnf("Docker Sandboxes create-admission recovery was already attempted; preserving exact capacity and retrying after %s\n", time.Until(next).Round(time.Second))
		} else {
			m.warnf("Docker Sandboxes inventory recovery is suppressed while a create-admission incident remains open; preserving exact capacity and retrying after %s\n", time.Until(next).Round(time.Second))
		}
		return true, nil
	}

	// The timeout may have been caused by a transient runtime stall that
	// cleared before recovery began. Recheck first so an already-healthy daemon
	// is never restarted unnecessarily.
	if !admissionFailure {
		if probeErr := m.probeProviderInventory(ctx); probeErr == nil {
			m.resetProviderRecovery()
			m.infof("Docker Sandboxes inventory recovered before daemon intervention; resuming pool reconciliation\n")
			return true, nil
		} else if ctx.Err() != nil {
			return true, ctx.Err()
		}
	}

	attempt := m.beginProviderRecoveryAttempt()
	recoveryCause := "inventory failure"
	if admissionFailure {
		recoveryCause = "create-admission failure"
	}
	m.warnf("Docker Sandboxes control-plane recovery attempt %d starting after %s: %v\n", attempt, recoveryCause, cause)
	quiescence := time.Duration(m.Config.DockerSandboxes.RecoveryQuiescenceSeconds) * time.Second
	if quiescence <= 0 {
		quiescence = config.DockerSandboxesDefaultRecoveryQuiescenceSeconds * time.Second
	}
	recoveryCtx, cancel := context.WithTimeout(ctx, providerRecoveryBudgetFor(quiescence))
	defer cancel()
	if recoveryErr := recoverer.RecoverControlPlane(recoveryCtx, provider.ControlPlaneRecoveryRequest{Quiescence: quiescence}); recoveryErr != nil {
		if errors.Is(recoveryErr, provider.ErrControlPlaneRecoveryBusy) {
			if admissionFailure {
				m.cancelProviderAdmissionRecovery()
			}
			m.cancelProviderRecoveryAttempt()
			next := m.scheduleProviderRecovery(providerRecoveryLockRetry)
			m.warnf("Docker Sandboxes control-plane recovery is already running on this host; preserving exact capacity and retrying after %s\n", time.Until(next).Round(time.Second))
			return true, nil
		}
		if ctx.Err() != nil {
			return true, ctx.Err()
		}
		attempt, next := m.recordProviderRecoveryFailure()
		m.warnf("Docker Sandboxes control-plane recovery attempt %d failed; preserving exact capacity and retrying after %s: %v\n", attempt, time.Until(next).Round(time.Second), recoveryErr)
		return true, nil
	}

	if verifyErr := m.verifyProviderInventoryAfterRecovery(recoveryCtx); verifyErr != nil {
		if ctx.Err() != nil {
			return true, ctx.Err()
		}
		attempt, next := m.recordProviderRecoveryFailure()
		m.warnf("Docker Sandboxes recovery attempt %d did not produce stable inventory; preserving exact capacity and retrying after %s: %v\n", attempt, time.Until(next).Round(time.Second), verifyErr)
		return true, nil
	}

	m.resetProviderRecovery()
	m.infof("Docker Sandboxes control-plane recovery succeeded; stable inventory verified and pool reconciliation will resume\n")
	return true, nil
}

func (m *Manager) dockerSandboxesExclusiveRecovery() bool {
	if strings.TrimSpace(strings.ToLower(m.Config.Provider.Type)) != "docker-sandboxes" {
		return false
	}
	mode := strings.TrimSpace(strings.ToLower(m.Config.DockerSandboxes.RecoveryMode))
	if mode == "" {
		mode = config.DockerSandboxesRecoveryModeExclusiveAuto
	}
	return mode == config.DockerSandboxesRecoveryModeExclusiveAuto
}

func (m *Manager) probeProviderInventory(parent context.Context) error {
	lifecycle := m.providerLifecycle()
	if lifecycle == nil {
		return errors.New("provider lifecycle is unavailable")
	}
	ctx, cancel := context.WithTimeout(parent, providerRecoveryProbeTimeout)
	defer cancel()
	_, err := lifecycle.Inventory(ctx)
	return err
}

func (m *Manager) verifyProviderInventoryAfterRecovery(parent context.Context) error {
	for attempt := 1; attempt <= providerRecoveryProbeCount; attempt++ {
		if err := m.probeProviderInventory(parent); err != nil {
			return fmt.Errorf("post-recovery inventory probe %d/%d failed: %w", attempt, providerRecoveryProbeCount, err)
		}
		if attempt < providerRecoveryProbeCount {
			if err := waitWithContext(parent, providerRecoveryProbeInterval); err != nil {
				return fmt.Errorf("wait between post-recovery inventory probes: %w", err)
			}
		}
	}
	return nil
}

func (m *Manager) waitForProviderRecoveryWindow(ctx context.Context) error {
	m.providerRecoveryMu.Lock()
	next := m.providerRecoveryNext
	m.providerRecoveryMu.Unlock()
	now := m.currentTime()
	if next.IsZero() || !now.Before(next) {
		return nil
	}
	return waitWithContext(ctx, next.Sub(now))
}

func (m *Manager) providerRecoveryWindowReady() bool {
	m.providerRecoveryMu.Lock()
	next := m.providerRecoveryNext
	m.providerRecoveryMu.Unlock()
	return next.IsZero() || !m.currentTime().Before(next)
}

func providerRecoveryBudgetFor(quiescence time.Duration) time.Duration {
	if quiescence <= 0 {
		quiescence = config.DockerSandboxesDefaultRecoveryQuiescenceSeconds * time.Second
	}
	return quiescence +
		(2 * providerRecoveryDaemonStop) +
		(3 * providerRecoveryDaemonReadback) +
		(time.Duration(providerRecoveryProbeCount) * providerRecoveryProbeTimeout) +
		(time.Duration(providerRecoveryProbeCount-1) * providerRecoveryProbeInterval) +
		providerRecoverySafetyMargin
}

func (m *Manager) beginProviderRecoveryAttempt() int {
	m.providerRecoveryMu.Lock()
	defer m.providerRecoveryMu.Unlock()
	m.providerRecoveryTries++
	m.providerRecoveryNext = time.Time{}
	return m.providerRecoveryTries
}

func (m *Manager) cancelProviderRecoveryAttempt() {
	m.providerRecoveryMu.Lock()
	defer m.providerRecoveryMu.Unlock()
	if m.providerRecoveryTries > 0 {
		m.providerRecoveryTries--
	}
	if m.providerRecoveryTries == 0 {
		m.providerRecoveryNext = time.Time{}
	}
}

func (m *Manager) recordProviderRecoveryFailure() (int, time.Time) {
	m.providerRecoveryMu.Lock()
	defer m.providerRecoveryMu.Unlock()
	delay := providerRecoveryBackoffInitial
	for i := 1; i < m.providerRecoveryTries; i++ {
		if delay >= providerRecoveryBackoffMaximum/2 {
			delay = providerRecoveryBackoffMaximum
			break
		}
		delay *= 2
	}
	if delay > providerRecoveryBackoffMaximum {
		delay = providerRecoveryBackoffMaximum
	}
	m.providerRecoveryNext = m.currentTime().Add(delay)
	return m.providerRecoveryTries, m.providerRecoveryNext
}

func (m *Manager) scheduleProviderRecovery(delay time.Duration) time.Time {
	m.providerRecoveryMu.Lock()
	defer m.providerRecoveryMu.Unlock()
	if delay <= 0 {
		delay = providerRecoveryLockRetry
	}
	m.providerRecoveryNext = m.currentTime().Add(delay)
	return m.providerRecoveryNext
}

func (m *Manager) resetProviderRecovery() {
	m.providerRecoveryMu.Lock()
	m.providerRecoveryTries = 0
	m.providerRecoveryNext = time.Time{}
	m.providerRecoveryMu.Unlock()
}

// reserveProviderRecovery atomically reserves the next recovery opportunity.
// Once an admission recovery has been attempted, ordinary inventory failures
// cannot restart the same daemon incident until a provider create succeeds.
func (m *Manager) reserveProviderRecovery(admissionFailure bool) (permitted, admissionIncident bool) {
	m.providerRecoveryMu.Lock()
	defer m.providerRecoveryMu.Unlock()
	if m.providerAdmissionRecoveryAttempted {
		return false, true
	}
	if admissionFailure {
		m.providerAdmissionRecoveryAttempted = true
	}
	return true, false
}

func (m *Manager) cancelProviderAdmissionRecovery() {
	m.providerRecoveryMu.Lock()
	m.providerAdmissionRecoveryAttempted = false
	m.providerRecoveryMu.Unlock()
}

func (m *Manager) resetProviderAdmissionRecovery() {
	m.providerRecoveryMu.Lock()
	m.providerAdmissionRecoveryAttempted = false
	m.providerRecoveryMu.Unlock()
}

func waitWithContext(ctx context.Context, duration time.Duration) error {
	if duration <= 0 {
		return nil
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
