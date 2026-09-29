package pool

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
)

// providerCallerBudgetTimeout records that an exact provider operation was
// still unresolved when the pool's caller-owned maintenance allowance ended.
// It deliberately unwraps only to DeadlineExceeded: a caller deadline is not
// provider-failure evidence and must never inherit a nested typed failure that
// could authorize intervention in a shared control plane.
type providerCallerBudgetTimeout struct {
	instance  provider.Instance
	operation string
}

func (failure *providerCallerBudgetTimeout) Error() string {
	if failure == nil {
		return context.DeadlineExceeded.Error()
	}
	identity := strings.TrimSpace(failure.instance.Name)
	if providerID := strings.TrimSpace(failure.instance.ProviderID); providerID != "" {
		identity = fmt.Sprintf("%s id=%s", identity, providerID)
	}
	if identity == "" {
		identity = "unknown instance"
	}
	operation := strings.TrimSpace(failure.operation)
	if operation == "" {
		operation = "provider operation"
	}
	return fmt.Sprintf("%s exceeded the caller maintenance budget for %s: %v", operation, identity, context.DeadlineExceeded)
}

func (*providerCallerBudgetTimeout) Unwrap() error { return context.DeadlineExceeded }

func newProviderCallerBudgetTimeout(instance provider.Instance, operation string) error {
	return &providerCallerBudgetTimeout{instance: instance, operation: operation}
}

func asProviderCallerBudgetTimeout(err error) (*providerCallerBudgetTimeout, bool) {
	var failure *providerCallerBudgetTimeout
	ok := errors.As(err, &failure)
	return failure, ok && failure != nil
}

func isProviderCallerBudgetTimeout(err error) bool {
	_, ok := asProviderCallerBudgetTimeout(err)
	return ok
}

func isProviderRecoverySignal(err error) bool {
	return isProviderCallerBudgetTimeout(err) || errors.Is(err, provider.ErrControlPlaneFailure) || errors.Is(err, provider.ErrControlPlaneAdmissionFailure)
}

func classifyProviderOperationError(parent, operationCtx context.Context, instance provider.Instance, operation string, err error) error {
	if err == nil {
		return nil
	}
	if parentErr := parent.Err(); parentErr != nil {
		if errors.Is(parentErr, context.DeadlineExceeded) {
			return newProviderCallerBudgetTimeout(instance, operation)
		}
		return parentErr
	}
	if operationErr := operationCtx.Err(); errors.Is(operationErr, context.DeadlineExceeded) {
		return newProviderCallerBudgetTimeout(instance, operation)
	} else if operationErr != nil {
		return operationErr
	}
	return err
}

type providerDeadlineIncidentKey struct {
	name       string
	providerID string
}

type providerDeadlineEpisode struct {
	replacementRetryState
	first     time.Time
	operation string
}

// providerDeadlineRecoveryState is owned by one RunPool invocation. It is
// intentionally separate from dependency, candidate, and durable provider
// recovery cooldowns: an unresolved caller budget is only a prompt for a fresh
// exact-instance verification, never evidence that a dependency or the shared
// control plane failed.
type providerDeadlineRecoveryState struct {
	episodes map[providerDeadlineIncidentKey]*providerDeadlineEpisode
	enabled  bool
}

func (s *providerDeadlineRecoveryState) episode(failure *providerCallerBudgetTimeout, now time.Time) (*providerDeadlineEpisode, providerDeadlineIncidentKey) {
	if s.episodes == nil {
		s.episodes = make(map[providerDeadlineIncidentKey]*providerDeadlineEpisode)
	}
	key := providerDeadlineIncidentKey{name: failure.instance.Name, providerID: failure.instance.ProviderID}
	episode := s.episodes[key]
	if episode == nil {
		episode = &providerDeadlineEpisode{first: now, operation: failure.operation}
		s.episodes[key] = episode
	}
	return episode, key
}

func (s *providerDeadlineRecoveryState) reset(key providerDeadlineIncidentKey) {
	delete(s.episodes, key)
}

func (s *providerDeadlineRecoveryState) resetInstance(instance provider.Instance) {
	if s == nil {
		return
	}
	delete(s.episodes, providerDeadlineIncidentKey{name: instance.Name, providerID: instance.ProviderID})
}

func (s *providerDeadlineRecoveryState) sync(active map[string]ProvisionedInstance) {
	if s == nil || len(s.episodes) == 0 {
		return
	}
	identities := make(map[providerDeadlineIncidentKey]struct{}, len(active))
	for _, instance := range active {
		identities[providerDeadlineIncidentKey{name: instance.Name, providerID: instance.ProviderID}] = struct{}{}
	}
	for key := range s.episodes {
		if _, found := identities[key]; !found {
			delete(s.episodes, key)
		}
	}
}

func (s *providerDeadlineRecoveryState) nextAttempt() time.Time {
	if s == nil {
		return time.Time{}
	}
	var next time.Time
	for _, episode := range s.episodes {
		if episode.next.IsZero() || (!next.IsZero() && !episode.next.Before(next)) {
			continue
		}
		next = episode.next
	}
	return next
}

func (s *providerDeadlineRecoveryState) wait(ctx context.Context, now time.Time) error {
	next := s.nextAttempt()
	if next.IsZero() || !now.Before(next) {
		return nil
	}
	timer := time.NewTimer(next.Sub(now))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// recoverProviderFailure extends typed provider recovery with a two-step path
// for caller-budget uncertainty. Repeated observations merely authorize a
// fresh exact-instance probe. Only the provider's independently bounded typed
// verdict can authorize the existing coordinated recovery workflow.
func (m *Manager) recoverProviderFailure(ctx context.Context, state *providerDeadlineRecoveryState, cause error, active map[string]ProvisionedInstance, handoffs map[string]bool) (handled bool, err error) {
	failure, callerBudget := asProviderCallerBudgetTimeout(cause)
	if !callerBudget {
		return m.recoverProviderControlPlane(ctx, cause)
	}
	if state == nil || !state.enabled {
		return false, nil
	}
	if ctx.Err() != nil || failure.instance.Name == "" || failure.instance.ProviderID == "" {
		return false, nil
	}
	if strings.EqualFold(strings.TrimSpace(m.Config.Provider.Type), "docker-sandboxes") && !m.dockerSandboxesExclusiveRecovery() {
		return false, nil
	}
	lifecycle := m.providerLifecycle()
	verifier, supported := lifecycle.(provider.ControlPlaneIncidentVerifier)
	if !supported {
		return false, nil
	}
	now := m.currentTime()
	episode, key := state.episode(failure, now)
	if episode.active(now) {
		return true, nil
	}
	episode.operation = failure.operation
	episode.schedule(m, now, cause)
	if episode.attempt == 1 {
		m.warnf("[%s] provider operation exceeded its caller maintenance budget; retaining exact capacity before independent verification: providerID=%s operation=%s attempt=%d retryIn=%s nextRetry=%s\n", failure.instance.Name, failure.instance.ProviderID, failure.operation, episode.attempt, episode.remaining(now), episode.next.In(time.Local).Format(time.RFC3339))
		return true, nil
	}

	peers := make(map[string]ProvisionedInstance, len(active))
	for name, instance := range active {
		if instance.Name == failure.instance.Name && instance.ProviderID == failure.instance.ProviderID {
			continue
		}
		peers[name] = instance
	}
	_, stopLeaseKeeper := m.startHostTrustLeaseKeeperForRunners(ctx, peers, handoffs)
	probeCtx, cancelProbe := context.WithTimeout(ctx, providerRecoveryProbeTimeout)
	probeErr := verifier.VerifyControlPlaneIncident(probeCtx, failure.instance)
	cancelProbe()
	stopLeaseKeeper(active)
	if ctx.Err() != nil {
		return false, nil
	}
	if probeErr == nil {
		m.infof("[%s] provider command path is healthy again after %d caller-budget observation(s); resuming exact reconciliation: providerID=%s operation=%s duration=%s\n", failure.instance.Name, episode.attempt, failure.instance.ProviderID, failure.operation, now.Sub(episode.first).Round(time.Second))
		state.reset(key)
		return true, nil
	}
	if errors.Is(probeErr, provider.ErrControlPlaneAdmissionFailure) || errors.Is(probeErr, provider.ErrControlPlaneFailure) {
		m.warnf("[%s] independent provider command-path verification confirmed a control-plane incident: providerID=%s operation=%s attempt=%d: %v\n", failure.instance.Name, failure.instance.ProviderID, failure.operation, episode.attempt, probeErr)
		recoveryCause := probeErr
		if !errors.Is(probeErr, provider.ErrControlPlaneAdmissionFailure) {
			// A fresh exact-instance failure is stronger evidence than the global
			// inventory failures handled by the ordinary pre-intervention recheck.
			// Route it through admission recovery so healthy global inventory cannot
			// incorrectly clear a still-wedged guest-session command path.
			recoveryCause = provider.NewControlPlaneAdmissionFailure("verify exact provider command-path incident", probeErr)
		}
		recovered, recoveryErr := m.recoverProviderControlPlane(ctx, recoveryCause)
		if recovered {
			if recoveryErr == nil {
				state.reset(key)
			}
			return true, recoveryErr
		}
		// Providers that independently verify an incident but do not implement the
		// shared recovery contract keep the exact capacity fence and retry later.
		m.warnf("[%s] confirmed provider command-path incident is retained for observation; exact capacity remains fenced: providerID=%s attempt=%d retryIn=%s nextRetry=%s\n", failure.instance.Name, failure.instance.ProviderID, episode.attempt, episode.remaining(now), episode.next.In(time.Local).Format(time.RFC3339))
		return true, nil
	}

	m.warnf("[%s] independent provider command-path verification was inconclusive; retaining exact capacity: providerID=%s operation=%s attempt=%d retryIn=%s nextRetry=%s: %v\n", failure.instance.Name, failure.instance.ProviderID, failure.operation, episode.attempt, episode.remaining(now), episode.next.In(time.Local).Format(time.RFC3339), probeErr)
	return true, nil
}
