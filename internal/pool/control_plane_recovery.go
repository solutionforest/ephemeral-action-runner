package pool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
	poolstate "github.com/solutionforest/ephemeral-action-runner/internal/pool/state"
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

var errProviderRecoveryWindowNotReady = errors.New("provider control-plane recovery window is not ready")

type providerRecoveryReservation struct {
	reservationToken       uint64
	admissionIncidentToken uint64
	attempt                int
}

type providerRecoveryIdentityExpectations struct {
	identities        map[string]string
	absenceCandidates map[string]provider.Instance
}

// recoverProviderControlPlane performs the shared orchestration around a
// provider-owned recovery operation. The provider performs the exact daemon
// stop/start sequence and holds cross-process exclusion; the pool owns policy,
// retry backoff, and repeated post-recovery inventory verification.
//
// The handled result means the caller should retain its current lifecycle map
// and retry rather than clean up or terminate the pool. A provider that is not
// recovery-capable, an explicit observe mode, or a non-control-plane failure
// returns handled=false so existing fail-closed behavior remains unchanged.
func (m *Manager) recoverProviderControlPlane(ctx context.Context, cause error) (handled bool, err error) {
	// A cancelled caller is not evidence of a provider control-plane incident.
	// In particular, a parent shutdown must never authorize a daemon restart or
	// mutate the durable recovery budget while the controller is stopping.
	if ctx.Err() != nil || errors.Is(cause, context.Canceled) {
		return false, nil
	}
	var uncertainCreateFailure *provider.UncertainCreateFailure
	if errors.As(cause, &uncertainCreateFailure) {
		// A successful provider create followed by an incomplete identity
		// readback is not a safe signal for restarting the shared daemon: the
		// runtime may already exist and only its exact receipt is missing. Keep
		// the lifecycle record fenced and let reconciliation/report-only cleanup
		// handle it. Caller-owned cancellation is covered by the same rule.
		return false, nil
	}
	admissionFailure := errors.Is(cause, provider.ErrControlPlaneAdmissionFailure)
	if (!admissionFailure && !errors.Is(cause, provider.ErrControlPlaneFailure)) || !m.dockerSandboxesExclusiveRecovery() {
		return false, nil
	}
	lifecycle := m.providerLifecycle()
	recoverer, ok := lifecycle.(provider.ControlPlaneRecoverer)
	if !ok {
		return false, nil
	}
	coordinator, ok := lifecycle.(provider.ControlPlaneRecoveryCoordinator)
	if !ok {
		return m.providerRecoverySupervisorError(ctx, errors.New("Docker Sandboxes provider does not implement host-wide recovery coordination; refusing unlocked automatic recovery"))
	}
	if err := m.refreshProviderRecoveryState(ctx); err != nil {
		return m.providerRecoverySupervisorError(ctx, fmt.Errorf("load durable Docker Sandboxes control-plane recovery state: %w", err))
	}
	if !m.providerRecoveryWindowReady() {
		return true, nil
	}
	observedGeneration := m.providerRecoveryStateGeneration()
	verificationRequired := m.providerRecoveryVerificationRequired()
	expectedIdentities, expectedErr := m.providerRecoveryExpectedIdentities(ctx)
	if expectedErr != nil {
		return m.providerRecoverySupervisorError(ctx, expectedErr)
	}
	if !verificationRequired && m.providerRecoveryAdmissionSuppressed(admissionFailure) {
		next, applied, scheduleErr := m.scheduleProviderRecoveryIfGenerationWithContext(ctx, observedGeneration, providerRecoveryBackoffMaximum)
		if scheduleErr != nil {
			return m.providerRecoverySupervisorError(ctx, fmt.Errorf("persist Docker Sandboxes control-plane recovery suppression window: %w", scheduleErr))
		}
		if !applied {
			if refreshErr := m.refreshProviderRecoveryState(ctx); refreshErr != nil {
				return m.providerRecoverySupervisorError(ctx, fmt.Errorf("refresh Docker Sandboxes recovery state after stale suppression: %w", refreshErr))
			}
			return true, nil
		}
		m.warnf("Docker Sandboxes create-admission recovery was already attempted; preserving exact capacity and retrying after %s\n", time.Until(next).Round(time.Second))
		return true, nil
	}
	proofOnly := false

	// The timeout may have been caused by a transient runtime stall that
	// cleared before recovery began. Recheck first so an already-healthy daemon
	// is never restarted unnecessarily. An interrupted intervention or
	// verification is different: even a successful empty inventory is not proof
	// that durable identities disappeared, so takeover must run the full stable
	// inventory proof and fence omissions before reconciliation resumes.
	if !admissionFailure && !verificationRequired {
		items, probeErr := m.probeProviderInventory(ctx)
		if probeErr == nil {
			if _, evidenceErr := providerInventoryEvidenceSet(items); evidenceErr != nil {
				return m.providerRecoveryPreInterventionError(ctx, observedGeneration, fmt.Errorf("pre-intervention Docker Sandboxes inventory returned invalid safety evidence: %w", evidenceErr))
			}
			needsAbsenceVerification, identityErr := providerInventoryNeedsDiscoveryAbsenceVerification(items, expectedIdentities)
			if identityErr != nil {
				return m.providerRecoveryPreInterventionError(ctx, observedGeneration, fmt.Errorf("pre-intervention Docker Sandboxes inventory omitted durable safety identity: %w", identityErr))
			}
			if !needsAbsenceVerification {
				applied, resetErr := m.resetProviderRecoveryIfGenerationWithContext(ctx, observedGeneration)
				if resetErr != nil {
					return m.providerRecoverySupervisorError(ctx, fmt.Errorf("persist Docker Sandboxes inventory recovery reset: %w", resetErr))
				}
				if !applied {
					if refreshErr := m.refreshProviderRecoveryState(ctx); refreshErr != nil {
						return m.providerRecoverySupervisorError(ctx, fmt.Errorf("refresh Docker Sandboxes control-plane recovery state after stale reset: %w", refreshErr))
					}
					return true, nil
				}
				m.infof("Docker Sandboxes inventory recovered before daemon intervention; resuming pool reconciliation\n")
				return true, nil
			}
			// An otherwise healthy inventory omission is not enough to clear
			// an append-only discovery. Continue into the host-wide recovery
			// lease so the provider can prove exact absence independently.
			proofOnly = true
		}
		if ctx.Err() != nil {
			return true, ctx.Err()
		}
	}

	callbackInvoked := false
	callbackHandled := true
	var callbackErr error
	coordinateErr := coordinator.CoordinateControlPlaneRecovery(ctx, func(coordinatedCtx context.Context) error {
		if callbackInvoked {
			callbackErr = errors.New("Docker Sandboxes recovery coordinator invoked its operation more than once")
			return callbackErr
		}
		callbackInvoked = true
		callbackHandled, callbackErr = m.recoverProviderControlPlaneUnderLease(coordinatedCtx, cause, admissionFailure, verificationRequired, proofOnly, observedGeneration, expectedIdentities, recoverer)
		return callbackErr
	})
	if callbackInvoked {
		if callbackErr != nil {
			return callbackHandled, callbackErr
		}
		if coordinateErr != nil {
			return m.providerRecoverySupervisorError(ctx, fmt.Errorf("release Docker Sandboxes host-wide recovery coordinator: %w", coordinateErr))
		}
		return callbackHandled, nil
	}
	if errors.Is(coordinateErr, provider.ErrControlPlaneRecoveryBusy) {
		next, applied, scheduleErr := m.scheduleProviderRecoveryIfGenerationWithContext(ctx, observedGeneration, providerRecoveryLockRetry)
		if scheduleErr != nil {
			return m.providerRecoverySupervisorError(ctx, fmt.Errorf("persist Docker Sandboxes recovery coordinator lock backoff: %w", scheduleErr))
		}
		if !applied {
			if refreshErr := m.refreshProviderRecoveryState(ctx); refreshErr != nil {
				return m.providerRecoverySupervisorError(ctx, fmt.Errorf("refresh Docker Sandboxes recovery state after busy coordinator: %w", refreshErr))
			}
			return true, nil
		}
		m.warnf("Docker Sandboxes control-plane recovery is already running on this host; preserving exact capacity and retrying after %s\n", time.Until(next).Round(time.Second))
		return true, nil
	}
	if ctx.Err() != nil {
		return true, ctx.Err()
	}
	if coordinateErr == nil {
		return m.providerRecoverySupervisorError(ctx, errors.New("Docker Sandboxes recovery coordinator returned without invoking its operation"))
	}
	return m.providerRecoverySupervisorError(ctx, fmt.Errorf("acquire Docker Sandboxes host-wide recovery coordinator: %w", coordinateErr))
}

func (m *Manager) recoverProviderControlPlaneUnderLease(ctx context.Context, cause error, admissionFailure, verificationRequired, proofOnly bool, observedGeneration uint64, expectedIdentities providerRecoveryIdentityExpectations, recoverer provider.ControlPlaneRecoverer) (handled bool, err error) {
	if !provider.ControlPlaneRecoveryCoordinatorHeld(ctx) {
		return m.providerRecoverySupervisorError(ctx, errors.New("Docker Sandboxes recovery coordinator did not mark its host-wide lease context; refusing unlocked automatic recovery"))
	}

	// Every new intervention is authorized by a fresh complete host-wide census.
	// It is validated before the durable reservation can enter Intervening and
	// is persisted atomically with that reservation. Verification-only takeover
	// uses the already durable census and must never replace it with a fresh,
	// potentially incomplete snapshot.
	reservationIdentities := expectedIdentities.identities
	if !verificationRequired {
		censusCtx, cancelCensus := context.WithTimeout(ctx, providerRecoveryProbeTimeout)
		var censusErr error
		reservationIdentities, censusErr = m.captureProviderRecoveryIdentityCensus(censusCtx, expectedIdentities)
		cancelCensus()
		if censusErr != nil {
			return m.providerRecoveryPreInterventionError(ctx, observedGeneration, censusErr)
		}
	} else if len(expectedIdentities.absenceCandidates) != 0 {
		// The frozen positive census survives a crash, but exclusions of
		// append-only historical discoveries are not stored. Re-prove those
		// exclusions before merging expectations on takeover, without ever
		// dropping a frozen identity or replacing the persisted census.
		proofCtx, cancelProof := context.WithTimeout(ctx, providerRecoveryProbeTimeout)
		var proofErr error
		reservationIdentities, proofErr = m.reproveRecoveryDiscoveryAbsence(proofCtx, expectedIdentities)
		cancelProof()
		if proofErr != nil {
			return m.providerRecoverySupervisorError(ctx, proofErr)
		}
	}
	if proofOnly {
		applied, resetErr := m.resetProviderRecoveryIfGenerationWithContext(ctx, observedGeneration)
		if resetErr != nil {
			return m.providerRecoverySupervisorError(ctx, fmt.Errorf("persist Docker Sandboxes inventory recovery reset after exact discovery absence: %w", resetErr))
		}
		if !applied {
			if refreshErr := m.refreshProviderRecoveryState(ctx); refreshErr != nil {
				return m.providerRecoverySupervisorError(ctx, fmt.Errorf("refresh Docker Sandboxes control-plane recovery state after stale exact discovery absence: %w", refreshErr))
			}
			return true, nil
		}
		m.infof("Docker Sandboxes inventory and historical discovery absence were verified under the host-wide recovery lease; resuming pool reconciliation\n")
		return true, nil
	}

	recoveryCause := "inventory failure"
	if admissionFailure {
		recoveryCause = "create-admission failure"
	}
	quiescence := time.Duration(m.Config.DockerSandboxes.RecoveryQuiescenceSeconds) * time.Second
	if quiescence <= 0 {
		quiescence = config.DockerSandboxesDefaultRecoveryQuiescenceSeconds * time.Second
	}
	recoveryBudget := providerRecoveryBudgetFor(quiescence)
	start, beginErr := m.beginProviderRecoveryWithContext(ctx, admissionFailure, recoveryBudget, reservationIdentities)
	if beginErr != nil {
		return m.providerRecoverySupervisorError(ctx, fmt.Errorf("persist Docker Sandboxes control-plane recovery reservation: %w", beginErr))
	}
	if !start.permitted {
		next := m.providerRecoveryNext
		if start.admissionIncident && !start.inFlight {
			var scheduleErr error
			next, scheduleErr = m.scheduleProviderRecoveryWithContext(ctx, providerRecoveryBackoffMaximum)
			if scheduleErr != nil {
				return m.providerRecoverySupervisorError(ctx, fmt.Errorf("persist Docker Sandboxes control-plane recovery suppression window: %w", scheduleErr))
			}
		}
		if start.admissionIncident {
			m.warnf("Docker Sandboxes create-admission recovery was already attempted; preserving exact capacity and retrying after %s\n", time.Until(next).Round(time.Second))
		} else {
			m.warnf("Docker Sandboxes inventory recovery is cooling down; preserving exact capacity and retrying after %s\n", time.Until(next).Round(time.Second))
		}
		return true, nil
	}
	recoveryCtx, cancel := context.WithTimeout(ctx, providerRecoveryBudgetFor(quiescence))
	defer cancel()
	reservation := start.reservation
	activeExpectedIdentities := start.expectedIdentities
	if !start.verificationOnly {
		if ctx.Err() != nil {
			return true, ctx.Err()
		}
		applied, markErr := m.markProviderRecoveryInterveningWithContext(ctx, reservation)
		if markErr != nil {
			return m.providerRecoverySupervisorError(ctx, fmt.Errorf("persist Docker Sandboxes control-plane intervention reservation: %w", markErr))
		}
		if !applied {
			if refreshErr := m.refreshProviderRecoveryState(ctx); refreshErr != nil {
				return m.providerRecoverySupervisorError(ctx, fmt.Errorf("refresh Docker Sandboxes control-plane recovery reservation: %w", refreshErr))
			}
			return true, nil
		}
		m.warnf("Docker Sandboxes control-plane recovery attempt %d starting after %s: %v\n", reservation.attempt, recoveryCause, cause)
		if recoveryErr := recoverer.RecoverControlPlane(recoveryCtx, provider.ControlPlaneRecoveryRequest{Quiescence: quiescence}); recoveryErr != nil {
			if errors.Is(recoveryErr, provider.ErrControlPlaneRecoveryBusy) {
				next, applied, scheduleErr := m.rescheduleProviderRecoveryReservationWithContext(ctx, reservation, admissionFailure, providerRecoveryLockRetry)
				if scheduleErr != nil {
					return m.providerRecoverySupervisorError(ctx, fmt.Errorf("persist Docker Sandboxes control-plane recovery lock backoff: %w", scheduleErr))
				}
				if !applied {
					if refreshErr := m.refreshProviderRecoveryState(ctx); refreshErr != nil {
						return m.providerRecoverySupervisorError(ctx, fmt.Errorf("refresh Docker Sandboxes control-plane recovery state after stale cancellation: %w", refreshErr))
					}
					return true, nil
				}
				m.warnf("Docker Sandboxes control-plane recovery is already running on this host; preserving exact capacity and retrying after %s\n", time.Until(next).Round(time.Second))
				return true, nil
			}
			// RecoverControlPlane may have crossed the daemon boundary before
			// returning an error, including when the caller deadline fired. Fence
			// every exact identity with a detached context before deciding whether
			// the recovery attempt can be recorded or retried.
			if fenceErr := m.markProviderRecoveryInventoryUncertain(context.WithoutCancel(ctx), activeExpectedIdentities); fenceErr != nil {
				return m.providerRecoverySupervisorError(ctx, fmt.Errorf("persist provider inventory uncertainty after recovery intervention failure: %w", fenceErr))
			}
			if ctx.Err() != nil {
				return true, ctx.Err()
			}
			attempt, next, applied, recordErr := m.recordProviderRecoveryFailureForReservationWithContext(ctx, reservation)
			if recordErr != nil {
				return m.providerRecoverySupervisorError(ctx, fmt.Errorf("persist Docker Sandboxes control-plane recovery failure: %w", recordErr))
			}
			if !applied {
				if refreshErr := m.refreshProviderRecoveryState(ctx); refreshErr != nil {
					return m.providerRecoverySupervisorError(ctx, fmt.Errorf("refresh Docker Sandboxes control-plane recovery state after stale failure: %w", refreshErr))
				}
				return true, nil
			}
			m.warnf("Docker Sandboxes control-plane recovery attempt %d failed; preserving exact capacity and retrying after %s: %v\n", attempt, time.Until(next).Round(time.Second), recoveryErr)
			return true, nil
		}
	}
	if !start.verificationOnly {
		applied, markErr := m.markProviderRecoveryVerifyingWithContext(ctx, reservation)
		if markErr != nil {
			return m.providerRecoverySupervisorError(ctx, fmt.Errorf("persist Docker Sandboxes control-plane verification reservation: %w", markErr))
		}
		if !applied {
			if refreshErr := m.refreshProviderRecoveryState(ctx); refreshErr != nil {
				return m.providerRecoverySupervisorError(ctx, fmt.Errorf("refresh Docker Sandboxes control-plane verification reservation: %w", refreshErr))
			}
			return true, nil
		}
	}

	if verifyErr := m.verifyProviderInventoryAfterRecoveryWithExpected(recoveryCtx, activeExpectedIdentities); verifyErr != nil {
		if ctx.Err() != nil {
			return true, ctx.Err()
		}
		if fenceErr := m.markProviderRecoveryInventoryUncertain(context.WithoutCancel(ctx), activeExpectedIdentities); fenceErr != nil {
			return m.providerRecoverySupervisorError(ctx, fmt.Errorf("persist post-recovery provider inventory uncertainty: %w", fenceErr))
		}
		attempt, next, applied, recordErr := m.recordProviderRecoveryFailureForReservationWithContext(ctx, reservation)
		if recordErr != nil {
			return m.providerRecoverySupervisorError(ctx, fmt.Errorf("persist Docker Sandboxes recovery verification failure: %w", recordErr))
		}
		if !applied {
			if refreshErr := m.refreshProviderRecoveryState(ctx); refreshErr != nil {
				return m.providerRecoverySupervisorError(ctx, fmt.Errorf("refresh Docker Sandboxes control-plane recovery state after stale verification: %w", refreshErr))
			}
			return true, nil
		}
		m.warnf("Docker Sandboxes recovery attempt %d did not produce stable inventory; preserving exact capacity and retrying after %s: %v\n", attempt, time.Until(next).Round(time.Second), verifyErr)
		return true, nil
	}

	applied, err := m.completeProviderRecoveryReservationWithContext(ctx, reservation)
	if err != nil {
		return m.providerRecoverySupervisorError(ctx, fmt.Errorf("persist Docker Sandboxes control-plane recovery success: %w", err))
	}
	if !applied {
		if refreshErr := m.refreshProviderRecoveryState(ctx); refreshErr != nil {
			return m.providerRecoverySupervisorError(ctx, fmt.Errorf("refresh Docker Sandboxes control-plane recovery state after stale success: %w", refreshErr))
		}
		return true, nil
	}
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

func (m *Manager) probeProviderInventory(parent context.Context) ([]provider.InventoryItem, error) {
	lifecycle := m.providerLifecycle()
	if lifecycle == nil {
		return nil, errors.New("provider lifecycle is unavailable")
	}
	ctx, cancel := context.WithTimeout(parent, providerRecoveryProbeTimeout)
	defer cancel()
	return lifecycle.Inventory(ctx)
}

func (m *Manager) verifyProviderInventoryAfterRecovery(parent context.Context) error {
	return m.verifyProviderInventoryAfterRecoveryWithExpected(parent, nil)
}

func (m *Manager) verifyProviderInventoryAfterRecoveryWithExpected(parent context.Context, expected map[string]string) error {
	var previous []string
	for attempt := 1; attempt <= providerRecoveryProbeCount; attempt++ {
		items, err := m.probeProviderInventory(parent)
		if err != nil {
			return fmt.Errorf("post-recovery inventory probe %d/%d failed: %w", attempt, providerRecoveryProbeCount, err)
		}
		current, evidenceErr := providerInventoryEvidenceSet(items)
		if evidenceErr != nil {
			return fmt.Errorf("post-recovery inventory probe %d/%d returned invalid safety evidence: %w", attempt, providerRecoveryProbeCount, evidenceErr)
		}
		if identityErr := providerInventoryExpectedIdentitiesPresent(items, expected); identityErr != nil {
			return fmt.Errorf("post-recovery inventory probe %d/%d omitted durable safety identities: %w", attempt, providerRecoveryProbeCount, identityErr)
		}
		if attempt > 1 && !sameProviderInventoryEvidenceSet(previous, current) {
			return fmt.Errorf("post-recovery inventory probe %d/%d changed the provider identity set or safety evidence", attempt, providerRecoveryProbeCount)
		}
		previous = current
		if attempt < providerRecoveryProbeCount {
			if err := waitWithContext(parent, providerRecoveryProbeInterval); err != nil {
				return fmt.Errorf("wait between post-recovery inventory probes: %w", err)
			}
		}
	}
	return nil
}

func (m *Manager) providerRecoveryExpectedIdentities(ctx context.Context) (providerRecoveryIdentityExpectations, error) {
	m.providerRecoveryMu.Lock()
	durable := cloneProviderRecoveryIdentities(m.providerRecoveryIdentityCensus)
	verificationRequired := providerRecoveryReservationNeedsVerification(m.providerRecoveryReservationToken, m.providerRecoveryReservationPhase)
	m.providerRecoveryMu.Unlock()
	if verificationRequired && durable == nil {
		return providerRecoveryIdentityExpectations{}, errors.New("interrupted Docker Sandboxes recovery has no durable host-wide identity census; automatic verification cannot establish safety")
	}
	expected := durable
	if expected == nil {
		expected = make(map[string]string)
	}
	expectations := providerRecoveryIdentityExpectations{identities: expected, absenceCandidates: make(map[string]provider.Instance)}
	if m.LifecycleState == nil {
		if m.LifecycleStateEnabled {
			return providerRecoveryIdentityExpectations{}, errors.New("durable lifecycle state is required to establish Docker Sandboxes recovery identities")
		}
		return expectations, nil
	}
	records, err := m.LifecycleState.List(ctx)
	if err != nil {
		return providerRecoveryIdentityExpectations{}, fmt.Errorf("read lifecycle state for Docker Sandboxes recovery identities: %w", err)
	}
	byProviderID := make(map[string]string, len(expected))
	for name, providerID := range expected {
		byProviderID[providerID] = name
	}
	nonterminalLifecycleNames := make(map[string]struct{})
	for _, record := range records {
		if record.ProviderType != m.Config.Provider.Type {
			continue
		}
		switch record.Phase {
		case poolstate.PhaseLocalAbsent, poolstate.PhaseTombstoned:
			continue
		}
		nonterminalLifecycleNames[record.Name] = struct{}{}
		if record.ProviderID == "" {
			continue
		}
		if err := mergeProviderRecoveryIdentity(expected, byProviderID, record.Name, record.ProviderID, "lifecycle state"); err != nil {
			return providerRecoveryIdentityExpectations{}, err
		}
	}
	discoveries, err := m.LifecycleState.Discoveries(ctx)
	if err != nil {
		return providerRecoveryIdentityExpectations{}, fmt.Errorf("read report-only discovery state for Docker Sandboxes recovery identities: %w", err)
	}
	for _, discovery := range discoveries {
		if discovery.ProviderType != m.Config.Provider.Type {
			continue
		}
		_, alreadyProtected := expected[discovery.ExactName]
		if err := mergeProviderRecoveryIdentity(expected, byProviderID, discovery.ExactName, discovery.ProviderID, "report-only discovery state"); err != nil {
			return providerRecoveryIdentityExpectations{}, err
		}
		if _, nonterminal := nonterminalLifecycleNames[discovery.ExactName]; alreadyProtected || nonterminal {
			continue
		}
		expectations.absenceCandidates[discovery.ExactName] = provider.Instance{
			Name:           discovery.ExactName,
			ProviderID:     discovery.ProviderID,
			ReceiptVersion: discovery.Receipt.Version,
			Receipt:        append(json.RawMessage(nil), discovery.Receipt.Payload...),
		}
	}
	return expectations, nil
}

func (m *Manager) captureProviderRecoveryIdentityCensus(ctx context.Context, expected providerRecoveryIdentityExpectations) (map[string]string, error) {
	items, err := m.probeProviderInventory(ctx)
	if err != nil {
		return nil, fmt.Errorf("establish host-wide Docker Sandboxes recovery identity census: inventory failed: %w", err)
	}
	if _, err := providerInventoryEvidenceSet(items); err != nil {
		return nil, fmt.Errorf("establish host-wide Docker Sandboxes recovery identity census: invalid safety evidence: %w", err)
	}
	needsAbsenceVerification, err := providerInventoryNeedsDiscoveryAbsenceVerification(items, expected)
	if err != nil {
		return nil, fmt.Errorf("establish host-wide Docker Sandboxes recovery identity census: omitted durable safety identity: %w", err)
	}
	if needsAbsenceVerification {
		if !provider.ControlPlaneRecoveryCoordinatorHeld(ctx) {
			return nil, errors.New("establish host-wide Docker Sandboxes recovery identity census: independent absence verification requires the host-wide recovery coordinator lease")
		}
		verifier, ok := m.providerLifecycle().(provider.ControlPlaneIdentityAbsenceVerifier)
		if !ok {
			return nil, errors.New("establish host-wide Docker Sandboxes recovery identity census: omitted discovery identities require independent exact absence verification")
		}
		observed := make(map[string]string, len(items))
		for _, item := range items {
			observed[item.Instance.Name] = item.Instance.ProviderID
		}
		names := make([]string, 0, len(expected.absenceCandidates))
		for name := range expected.absenceCandidates {
			if _, found := observed[name]; !found {
				names = append(names, name)
			}
		}
		sort.Strings(names)
		for _, name := range names {
			identity := expected.absenceCandidates[name]
			absent, verifyErr := verifier.VerifyControlPlaneIdentityAbsent(ctx, identity)
			if verifyErr != nil {
				return nil, fmt.Errorf("establish host-wide Docker Sandboxes recovery identity census: independently verify discovery identity %q id=%q absence: %w", identity.Name, identity.ProviderID, verifyErr)
			}
			if ctx.Err() != nil {
				return nil, fmt.Errorf("establish host-wide Docker Sandboxes recovery identity census: independently verify discovery identity %q id=%q absence: %w", identity.Name, identity.ProviderID, ctx.Err())
			}
			if !absent {
				return nil, fmt.Errorf("establish host-wide Docker Sandboxes recovery identity census: discovery identity %q id=%q was omitted and independent exact absence was not established", identity.Name, identity.ProviderID)
			}
		}
	}
	census := make(map[string]string, len(items))
	for _, item := range items {
		census[item.Instance.Name] = item.Instance.ProviderID
	}
	return census, nil
}

func (m *Manager) reproveRecoveryDiscoveryAbsence(ctx context.Context, expected providerRecoveryIdentityExpectations) (map[string]string, error) {
	if !provider.ControlPlaneRecoveryCoordinatorHeld(ctx) {
		return nil, errors.New("recovery discovery absence takeover requires the host-wide coordinator lease")
	}
	verifier, ok := m.providerLifecycle().(provider.ControlPlaneIdentityAbsenceVerifier)
	if !ok {
		return nil, errors.New("recovery takeover requires independent exact absence verification for historical discoveries")
	}
	m.providerRecoveryMu.Lock()
	frozen := cloneProviderRecoveryIdentities(m.providerRecoveryIdentityCensus)
	m.providerRecoveryMu.Unlock()
	if frozen == nil {
		return nil, errors.New("recovery takeover has no frozen identity census for historical discovery reconciliation")
	}
	items, err := m.probeProviderInventory(ctx)
	if err != nil {
		return nil, fmt.Errorf("recovery takeover discovery inventory: %w", err)
	}
	if _, err := providerInventoryEvidenceSet(items); err != nil {
		return nil, fmt.Errorf("recovery takeover discovery inventory evidence: %w", err)
	}
	for _, item := range items {
		for name, candidate := range expected.absenceCandidates {
			if item.Instance.Name == name || item.Instance.ProviderID == candidate.ProviderID {
				return nil, fmt.Errorf("historical discovery %q id=%q is present or rebound during recovery takeover; preserving the frozen census", name, candidate.ProviderID)
			}
		}
	}
	filtered := cloneProviderRecoveryIdentities(expected.identities)
	names := make([]string, 0, len(expected.absenceCandidates))
	for name := range expected.absenceCandidates {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		identity := expected.absenceCandidates[name]
		if _, protected := frozen[name]; protected || identity.Name != name || filtered[name] != identity.ProviderID {
			return nil, fmt.Errorf("refusing to exclude protected or mismatched recovery identity %q", name)
		}
		absent, err := verifier.VerifyControlPlaneIdentityAbsent(ctx, identity)
		if err != nil {
			return nil, fmt.Errorf("reprove historical discovery %q absence during recovery takeover: %w", name, err)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !absent {
			return nil, fmt.Errorf("historical discovery %q absence was not independently established during recovery takeover; preserving the frozen census", name)
		}
		delete(filtered, name)
	}
	return filtered, nil
}

func providerInventoryNeedsDiscoveryAbsenceVerification(items []provider.InventoryItem, expected providerRecoveryIdentityExpectations) (bool, error) {
	observedByName := make(map[string]string, len(items))
	observedByProviderID := make(map[string]string, len(items))
	for _, item := range items {
		observedByName[item.Instance.Name] = item.Instance.ProviderID
		observedByProviderID[item.Instance.ProviderID] = item.Instance.Name
	}
	needsAbsenceVerification := false
	for name, providerID := range expected.identities {
		observedID, found := observedByName[name]
		if found {
			if observedID != providerID {
				return false, fmt.Errorf("durable provider identity %q changed from id=%q to id=%q", name, providerID, observedID)
			}
			continue
		}
		if observedName, rebound := observedByProviderID[providerID]; rebound {
			return false, fmt.Errorf("durable provider identity %q id=%q was observed with name %q", name, providerID, observedName)
		}
		candidate, verifiable := expected.absenceCandidates[name]
		if !verifiable || candidate.ProviderID != providerID {
			return false, fmt.Errorf("durable provider identity %q id=%q was not observed", name, providerID)
		}
		needsAbsenceVerification = true
	}
	return needsAbsenceVerification, nil
}

func mergeProviderRecoveryIdentity(expected, byProviderID map[string]string, name, providerID, source string) error {
	if previous, found := expected[name]; found && previous != providerID {
		return fmt.Errorf("%s assigned multiple provider ids to %q", source, name)
	}
	if previous, found := byProviderID[providerID]; found && previous != name {
		return fmt.Errorf("%s assigned provider id %q to both %q and %q", source, providerID, previous, name)
	}
	expected[name] = providerID
	byProviderID[providerID] = name
	return nil
}

func cloneProviderRecoveryIdentities(identities map[string]string) map[string]string {
	if identities == nil {
		return nil
	}
	cloned := make(map[string]string, len(identities))
	for name, providerID := range identities {
		cloned[name] = providerID
	}
	return cloned
}

func mergeProviderRecoveryIdentityMaps(durable, additional map[string]string) (map[string]string, error) {
	merged := cloneProviderRecoveryIdentities(durable)
	if merged == nil {
		merged = make(map[string]string)
	}
	byProviderID := make(map[string]string, len(merged)+len(additional))
	for name, providerID := range merged {
		byProviderID[providerID] = name
	}
	for name, providerID := range additional {
		if err := mergeProviderRecoveryIdentity(merged, byProviderID, name, providerID, "durable recovery identity census"); err != nil {
			return nil, err
		}
	}
	return merged, nil
}

func providerRecoveryIdentityMapContains(observed, expected map[string]string) error {
	for name, providerID := range expected {
		observedID, found := observed[name]
		if !found {
			return fmt.Errorf("durable provider identity %q id=%q was not observed", name, providerID)
		}
		if observedID != providerID {
			return fmt.Errorf("durable provider identity %q changed from id=%q to id=%q", name, providerID, observedID)
		}
	}
	return nil
}

func providerInventoryExpectedIdentitiesPresent(items []provider.InventoryItem, expected map[string]string) error {
	if len(expected) == 0 {
		return nil
	}
	observed := make(map[string]string, len(items))
	for _, item := range items {
		observed[item.Instance.Name] = item.Instance.ProviderID
	}
	for name, providerID := range expected {
		observedID, found := observed[name]
		if !found {
			return fmt.Errorf("durable provider identity %q id=%q was not observed", name, providerID)
		}
		if observedID != providerID {
			return fmt.Errorf("durable provider identity %q changed from id=%q to id=%q", name, providerID, observedID)
		}
	}
	return nil
}

func (m *Manager) markProviderRecoveryInventoryUncertain(ctx context.Context, expected map[string]string) error {
	if len(expected) == 0 || m.LifecycleState == nil {
		return nil
	}
	records, err := m.LifecycleState.List(ctx)
	if err != nil {
		return fmt.Errorf("read lifecycle state for post-recovery uncertainty fence: %w", err)
	}
	for _, record := range records {
		if record.ProviderType != m.Config.Provider.Type || record.ProviderID == "" || expected[record.Name] != record.ProviderID {
			continue
		}
		if record.Phase == poolstate.PhaseLocalAbsent || record.Phase == poolstate.PhaseTombstoned || record.RecoveryInventoryUncertain {
			continue
		}
		if _, err := m.LifecycleState.Transition(ctx, record.Name, poolstate.Transition{Action: poolstate.ActionRecoveryInventoryUncertain, Reason: recoveryInventoryUncertainReason}); err != nil {
			return fmt.Errorf("persist post-recovery uncertainty for %s: %w", record.Name, err)
		}
	}
	return nil
}

type providerInventoryEvidence struct {
	Name            string   `json:"name"`
	ProviderID      string   `json:"providerId"`
	InstanceState   string   `json:"instanceState"`
	InventoryState  string   `json:"inventoryState"`
	InstanceSource  string   `json:"instanceSource"`
	InventorySource string   `json:"inventorySource"`
	ReceiptVersion  string   `json:"receiptVersion"`
	Receipt         []byte   `json:"receipt"`
	Workspaces      []string `json:"workspaces"`
}

func providerInventoryEvidenceSet(items []provider.InventoryItem) ([]string, error) {
	evidence := make([]string, 0, len(items))
	seenNames := make(map[string]struct{}, len(items))
	seenProviderIDs := make(map[string]struct{}, len(items))
	for index, item := range items {
		name := item.Instance.Name
		providerID := item.Instance.ProviderID
		if strings.TrimSpace(name) != name || name == "" || strings.ContainsRune(name, 0) {
			return nil, fmt.Errorf("inventory item %d omitted a valid immutable name", index)
		}
		if strings.TrimSpace(providerID) != providerID || providerID == "" || strings.ContainsRune(providerID, 0) {
			return nil, fmt.Errorf("inventory item %d omitted a valid immutable provider id", index)
		}
		if _, duplicate := seenNames[name]; duplicate {
			return nil, fmt.Errorf("inventory returned duplicate immutable name %q", name)
		}
		if _, duplicate := seenProviderIDs[providerID]; duplicate {
			return nil, fmt.Errorf("inventory returned duplicate immutable provider id %q", providerID)
		}
		seenNames[name] = struct{}{}
		seenProviderIDs[providerID] = struct{}{}
		if strings.TrimSpace(item.State) != item.State || item.State == "" || strings.ContainsRune(item.State, 0) {
			return nil, fmt.Errorf("inventory item %q omitted a valid state", name)
		}
		if item.Instance.State != item.State {
			return nil, fmt.Errorf("inventory item %q has inconsistent instance and inventory state", name)
		}
		if item.Instance.Source != item.Source {
			return nil, fmt.Errorf("inventory item %q has inconsistent instance and inventory source", name)
		}
		workspaces := append([]string(nil), item.Workspaces...)
		if len(workspaces) == 0 {
			return nil, fmt.Errorf("inventory item %q omitted workspaces", name)
		}
		seenWorkspaces := make(map[string]struct{}, len(workspaces))
		for _, workspace := range workspaces {
			if strings.TrimSpace(workspace) != workspace || workspace == "" || strings.ContainsRune(workspace, 0) {
				return nil, fmt.Errorf("inventory item %q contained an invalid workspace", name)
			}
			if _, duplicate := seenWorkspaces[workspace]; duplicate {
				return nil, fmt.Errorf("inventory item %q contained duplicate workspace %q", name, workspace)
			}
			seenWorkspaces[workspace] = struct{}{}
		}
		sort.Strings(workspaces)
		encoded, err := json.Marshal(providerInventoryEvidence{
			Name:            name,
			ProviderID:      providerID,
			InstanceState:   item.Instance.State,
			InventoryState:  item.State,
			InstanceSource:  item.Instance.Source,
			InventorySource: item.Source,
			ReceiptVersion:  item.Instance.ReceiptVersion,
			Receipt:         item.Instance.Receipt,
			Workspaces:      workspaces,
		})
		if err != nil {
			return nil, fmt.Errorf("encode inventory item %q safety evidence: %w", name, err)
		}
		evidence = append(evidence, string(encoded))
	}
	sort.Strings(evidence)
	return evidence, nil
}

func sameProviderInventoryEvidenceSet(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func (m *Manager) waitForProviderRecoveryWindow(ctx context.Context) error {
	if !m.dockerSandboxesExclusiveRecovery() {
		return nil
	}
	for {
		if err := m.refreshProviderRecoveryState(ctx); err != nil {
			return fmt.Errorf("load durable Docker Sandboxes control-plane recovery state: %w", err)
		}
		m.providerRecoveryMu.Lock()
		next := m.providerRecoveryNext
		verificationRequired := providerRecoveryReservationNeedsVerification(m.providerRecoveryReservationToken, m.providerRecoveryReservationPhase)
		m.providerRecoveryMu.Unlock()
		now := m.currentTime()
		if !next.IsZero() && now.Before(next) {
			if err := waitWithContext(ctx, next.Sub(now)); err != nil {
				return err
			}
			continue
		}
		if !verificationRequired {
			return nil
		}
		handled, recoveryErr := m.recoverProviderControlPlane(ctx, provider.NewControlPlaneFailure(
			"resume interrupted Docker Sandboxes control-plane recovery",
			errors.New("durable recovery reservation requires post-restart verification"),
		))
		if !handled {
			return errors.New("Docker Sandboxes control-plane recovery reservation cannot be safely verified")
		}
		// Recovery verification either cleared the reservation or durably fenced
		// every omitted exact identity before returning. Let reconciliation read
		// that result; a recorded cooldown is enforced by the next recovery gate.
		return recoveryErr
	}
}

func (m *Manager) providerRecoveryWindowReady() bool {
	m.providerRecoveryMu.Lock()
	next := m.providerRecoveryNext
	m.providerRecoveryMu.Unlock()
	return next.IsZero() || !m.currentTime().Before(next)
}

func (m *Manager) providerRecoveryVerificationRequired() bool {
	m.providerRecoveryMu.Lock()
	defer m.providerRecoveryMu.Unlock()
	return providerRecoveryReservationNeedsVerification(m.providerRecoveryReservationToken, m.providerRecoveryReservationPhase)
}

func (m *Manager) providerRecoveryAdmissionSuppressed(admissionFailure bool) bool {
	m.providerRecoveryMu.Lock()
	defer m.providerRecoveryMu.Unlock()
	if !m.providerAdmissionRecoveryAttempted {
		return false
	}
	return !(admissionFailure && m.providerRecoveryReservationToken != 0 && m.providerRecoveryReservationPhase == provider.RecoveryReservationReserved)
}

func providerRecoveryReservationNeedsVerification(token uint64, phase provider.RecoveryReservationPhase) bool {
	if token == 0 {
		return false
	}
	return phase == provider.RecoveryReservationIntervening || phase == provider.RecoveryReservationVerifying
}

func (m *Manager) loadProviderRecoveryState(ctx context.Context) error {
	m.providerRecoveryMu.Lock()
	defer m.providerRecoveryMu.Unlock()
	if m.providerRecoveryStateLoaded {
		return nil
	}
	if m.providerRecoveryLedger == nil && (m.LifecycleState != nil || m.LifecycleStateEnabled) {
		ledger, err := provider.OpenControlPlaneRecoveryLedger()
		if err != nil {
			return err
		}
		m.providerRecoveryLedger = ledger
	}
	if m.providerRecoveryLedger != nil {
		state, err := m.providerRecoveryLedger.State(ctx)
		if err != nil {
			return err
		}
		m.applyProviderRecoveryStateLocked(state)
	}
	m.providerRecoveryStateLoaded = true
	return nil
}

func (m *Manager) refreshProviderRecoveryState(ctx context.Context) error {
	if err := m.loadProviderRecoveryState(ctx); err != nil {
		return err
	}
	m.providerRecoveryMu.Lock()
	defer m.providerRecoveryMu.Unlock()
	if m.providerRecoveryLedger == nil {
		return nil
	}
	state, err := m.providerRecoveryLedger.State(ctx)
	if err != nil {
		return err
	}
	m.applyProviderRecoveryStateLocked(state)
	return nil
}

// captureProviderAdmissionRecoveryToken refreshes the host-global ledger at
// the last safe point before create can cross the provider boundary. The
// captured token may resolve only that incident after an exact create; a newer
// incident published while create is in flight remains protected by the
// conditional reset.
func (m *Manager) captureProviderAdmissionRecoveryToken(ctx context.Context) (uint64, error) {
	if err := m.refreshProviderRecoveryState(ctx); err != nil {
		return 0, err
	}
	m.providerRecoveryMu.Lock()
	defer m.providerRecoveryMu.Unlock()
	return m.providerAdmissionRecoveryToken, nil
}

func (m *Manager) applyProviderRecoveryStateLocked(state provider.ControlPlaneRecoveryState) {
	m.providerRecoveryTries = state.Attempts
	m.providerRecoveryNext = state.NextAttemptAt
	m.providerAdmissionRecoveryAttempted = state.AdmissionIncident
	m.providerRecoveryGeneration = state.Generation
	m.providerRecoveryReservationToken = state.RecoveryReservationToken
	m.providerAdmissionRecoveryToken = state.AdmissionIncidentToken
	m.providerRecoveryReservationPhase = state.RecoveryReservationPhase
	m.providerRecoveryReservationExpiresAt = state.RecoveryReservationExpiresAt
	m.providerRecoveryIdentityCensus = cloneProviderRecoveryIdentities(state.ExpectedIdentities)
}

func (m *Manager) providerRecoveryStateGeneration() uint64 {
	m.providerRecoveryMu.Lock()
	defer m.providerRecoveryMu.Unlock()
	return m.providerRecoveryGeneration
}

func (m *Manager) updateProviderRecoveryState(ctx context.Context, mutate func(*provider.ControlPlaneRecoveryState) error) error {
	if err := m.loadProviderRecoveryState(ctx); err != nil {
		return err
	}
	m.providerRecoveryMu.Lock()
	defer m.providerRecoveryMu.Unlock()
	if m.providerRecoveryLedger == nil {
		state := provider.ControlPlaneRecoveryState{
			Attempts:                     m.providerRecoveryTries,
			NextAttemptAt:                m.providerRecoveryNext,
			AdmissionIncident:            m.providerAdmissionRecoveryAttempted,
			RecoveryReservationToken:     m.providerRecoveryReservationToken,
			AdmissionIncidentToken:       m.providerAdmissionRecoveryToken,
			RecoveryReservationPhase:     m.providerRecoveryReservationPhase,
			RecoveryReservationExpiresAt: m.providerRecoveryReservationExpiresAt,
			ExpectedIdentities:           cloneProviderRecoveryIdentities(m.providerRecoveryIdentityCensus),
			Generation:                   m.providerRecoveryGeneration,
		}
		if err := mutate(&state); err != nil {
			return err
		}
		state.Generation++
		m.providerRecoveryTries = state.Attempts
		m.providerRecoveryNext = state.NextAttemptAt
		m.providerAdmissionRecoveryAttempted = state.AdmissionIncident
		m.providerRecoveryGeneration = state.Generation
		m.providerRecoveryReservationToken = state.RecoveryReservationToken
		m.providerAdmissionRecoveryToken = state.AdmissionIncidentToken
		m.providerRecoveryReservationPhase = state.RecoveryReservationPhase
		m.providerRecoveryReservationExpiresAt = state.RecoveryReservationExpiresAt
		m.providerRecoveryIdentityCensus = cloneProviderRecoveryIdentities(state.ExpectedIdentities)
		return nil
	}
	state, err := m.providerRecoveryLedger.Update(ctx, mutate)
	if err != nil {
		return err
	}
	m.applyProviderRecoveryStateLocked(state)
	return nil
}

func (m *Manager) updateProviderRecoveryStateIfGeneration(ctx context.Context, expected uint64, mutate func(*provider.ControlPlaneRecoveryState) error) (bool, error) {
	return m.updateProviderRecoveryStateIf(ctx, func(state provider.ControlPlaneRecoveryState) bool {
		return state.Generation == expected
	}, mutate)
}

func (m *Manager) updateProviderRecoveryStateIfReservation(ctx context.Context, reservation providerRecoveryReservation, mutate func(*provider.ControlPlaneRecoveryState) error) (bool, error) {
	return m.updateProviderRecoveryStateIf(ctx, func(state provider.ControlPlaneRecoveryState) bool {
		return reservation.reservationToken != 0 && state.RecoveryReservationToken == reservation.reservationToken
	}, mutate)
}

func (m *Manager) markProviderRecoveryInterveningWithContext(ctx context.Context, reservation providerRecoveryReservation) (bool, error) {
	return m.updateProviderRecoveryStateIfReservation(ctx, reservation, func(state *provider.ControlPlaneRecoveryState) error {
		if state.RecoveryReservationPhase != provider.RecoveryReservationReserved {
			return errProviderRecoveryWindowNotReady
		}
		state.RecoveryReservationPhase = provider.RecoveryReservationIntervening
		return nil
	})
}

func (m *Manager) markProviderRecoveryVerifyingWithContext(ctx context.Context, reservation providerRecoveryReservation) (bool, error) {
	return m.updateProviderRecoveryStateIfReservation(ctx, reservation, func(state *provider.ControlPlaneRecoveryState) error {
		if state.RecoveryReservationPhase != provider.RecoveryReservationIntervening {
			if state.RecoveryReservationPhase == provider.RecoveryReservationVerifying {
				return nil
			}
			return errProviderRecoveryWindowNotReady
		}
		state.RecoveryReservationPhase = provider.RecoveryReservationVerifying
		state.RecoveryReservationExpiresAt = m.currentTime().Add(
			(time.Duration(providerRecoveryProbeCount) * providerRecoveryProbeTimeout) +
				(time.Duration(providerRecoveryProbeCount-1) * providerRecoveryProbeInterval) +
				providerRecoverySafetyMargin,
		)
		state.NextAttemptAt = state.RecoveryReservationExpiresAt
		return nil
	})
}

func (m *Manager) updateProviderRecoveryStateIfAdmissionToken(ctx context.Context, admissionToken uint64, mutate func(*provider.ControlPlaneRecoveryState) error) error {
	_, err := m.updateProviderRecoveryStateIf(ctx, func(state provider.ControlPlaneRecoveryState) bool {
		if !state.AdmissionIncident {
			return false
		}
		// A pre-token ledger may have an active incident with token zero. An
		// exact create captured from that same legacy state may resolve it, but
		// it must not be allowed to clear any later incident that has a token.
		return state.AdmissionIncidentToken == admissionToken
	}, mutate)
	return err
}

func (m *Manager) updateProviderRecoveryStateIf(ctx context.Context, matches func(provider.ControlPlaneRecoveryState) bool, mutate func(*provider.ControlPlaneRecoveryState) error) (bool, error) {
	if err := m.loadProviderRecoveryState(ctx); err != nil {
		return false, err
	}
	m.providerRecoveryMu.Lock()
	defer m.providerRecoveryMu.Unlock()
	if m.providerRecoveryLedger == nil {
		state := provider.ControlPlaneRecoveryState{
			Attempts:                     m.providerRecoveryTries,
			NextAttemptAt:                m.providerRecoveryNext,
			AdmissionIncident:            m.providerAdmissionRecoveryAttempted,
			RecoveryReservationToken:     m.providerRecoveryReservationToken,
			AdmissionIncidentToken:       m.providerAdmissionRecoveryToken,
			RecoveryReservationPhase:     m.providerRecoveryReservationPhase,
			RecoveryReservationExpiresAt: m.providerRecoveryReservationExpiresAt,
			ExpectedIdentities:           cloneProviderRecoveryIdentities(m.providerRecoveryIdentityCensus),
			Generation:                   m.providerRecoveryGeneration,
		}
		if !matches(state) {
			return false, nil
		}
		if err := mutate(&state); err != nil {
			return false, err
		}
		state.Generation++
		m.providerRecoveryTries = state.Attempts
		m.providerRecoveryNext = state.NextAttemptAt
		m.providerAdmissionRecoveryAttempted = state.AdmissionIncident
		m.providerRecoveryGeneration = state.Generation
		m.providerRecoveryReservationToken = state.RecoveryReservationToken
		m.providerAdmissionRecoveryToken = state.AdmissionIncidentToken
		m.providerRecoveryReservationPhase = state.RecoveryReservationPhase
		m.providerRecoveryReservationExpiresAt = state.RecoveryReservationExpiresAt
		m.providerRecoveryIdentityCensus = cloneProviderRecoveryIdentities(state.ExpectedIdentities)
		return true, nil
	}
	state, applied, err := m.providerRecoveryLedger.UpdateIf(ctx, matches, mutate)
	if err != nil {
		return false, err
	}
	m.applyProviderRecoveryStateLocked(state)
	return applied, nil
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

func (m *Manager) beginProviderRecoveryAttemptWithContext(ctx context.Context, budget time.Duration) (int, error) {
	reservation, err := m.beginProviderRecoveryReservationWithContext(ctx, budget)
	return reservation.attempt, err
}

type providerRecoveryStart struct {
	reservation        providerRecoveryReservation
	expectedIdentities map[string]string
	permitted          bool
	admissionIncident  bool
	inFlight           bool
	verificationOnly   bool
}

// beginProviderRecoveryWithContext atomically claims both the admission
// incident and the recovery reservation. A durable reservation phase lets a
// restarted controller distinguish a reservation that never crossed the
// provider boundary from an intervention that may already have mutated the
// shared daemon.
func (m *Manager) beginProviderRecoveryWithContext(ctx context.Context, admissionFailure bool, budget time.Duration, expectedIdentities map[string]string) (providerRecoveryStart, error) {
	start := providerRecoveryStart{}
	now := m.currentTime()
	reservationExpiry := now.Add(budget)
	if budget <= 0 {
		// A zero budget is retained for deterministic in-process tests and
		// legacy callers: the reservation is immediately eligible for takeover.
		reservationExpiry = now
	}
	applied, err := m.updateProviderRecoveryStateIf(ctx, func(state provider.ControlPlaneRecoveryState) bool {
		start.admissionIncident = state.AdmissionIncident
		reservationActive := state.RecoveryReservationToken != 0
		if (!state.NextAttemptAt.IsZero() && now.Before(state.NextAttemptAt)) ||
			(reservationActive && !state.RecoveryReservationExpiresAt.IsZero() && now.Before(state.RecoveryReservationExpiresAt)) {
			start.inFlight = reservationActive
			return false
		}
		if reservationActive && (state.RecoveryReservationPhase == provider.RecoveryReservationIntervening || state.RecoveryReservationPhase == provider.RecoveryReservationVerifying) {
			// The external mutation may already have started. On takeover only
			// perform read-only verification; never issue a second daemon restart.
			start.inFlight = true
			start.verificationOnly = true
			return true
		}
		if state.AdmissionIncident && admissionFailure && !(reservationActive && state.RecoveryReservationPhase == provider.RecoveryReservationReserved) {
			// An admission incident remains latched until an exact provider
			// identity is durably recorded. A later admission timeout must not
			// restart the daemon a second time after a completed recovery. The
			// only safe takeover is an expired reservation that never crossed the
			// provider boundary (the Reserved phase).
			return false
		}
		if state.AdmissionIncident && !admissionFailure {
			return false
		}
		return true
	}, func(state *provider.ControlPlaneRecoveryState) error {
		if start.verificationOnly {
			if state.ExpectedIdentities == nil {
				return errors.New("interrupted Docker Sandboxes recovery has no durable host-wide identity census")
			}
			merged, mergeErr := mergeProviderRecoveryIdentityMaps(state.ExpectedIdentities, expectedIdentities)
			if mergeErr != nil {
				return mergeErr
			}
			state.ExpectedIdentities = merged
			start.expectedIdentities = cloneProviderRecoveryIdentities(merged)
			if state.Attempts == 0 {
				state.Attempts = 1
			}
			start.reservation.attempt = state.Attempts
			start.reservation.reservationToken = nextProviderRecoveryToken(state)
			start.reservation.admissionIncidentToken = state.AdmissionIncidentToken
			state.RecoveryReservationToken = start.reservation.reservationToken
			state.RecoveryReservationPhase = provider.RecoveryReservationVerifying
			state.RecoveryReservationExpiresAt = reservationExpiry
			state.NextAttemptAt = state.RecoveryReservationExpiresAt
			return nil
		}
		if expectedIdentities == nil {
			return errors.New("new Docker Sandboxes recovery intervention requires a validated host-wide identity census")
		}
		if err := providerRecoveryIdentityMapContains(expectedIdentities, state.ExpectedIdentities); err != nil {
			return fmt.Errorf("validated host-wide recovery census omitted previously durable identity: %w", err)
		}
		state.ExpectedIdentities = cloneProviderRecoveryIdentities(expectedIdentities)
		start.expectedIdentities = cloneProviderRecoveryIdentities(expectedIdentities)
		state.Attempts++
		start.reservation.attempt = state.Attempts
		start.reservation.reservationToken = nextProviderRecoveryToken(state)
		if admissionFailure {
			state.AdmissionIncident = true
			state.AdmissionIncidentToken = nextDistinctProviderRecoveryToken(state, start.reservation.reservationToken)
		}
		start.reservation.admissionIncidentToken = state.AdmissionIncidentToken
		state.RecoveryReservationToken = start.reservation.reservationToken
		state.RecoveryReservationPhase = provider.RecoveryReservationReserved
		state.RecoveryReservationExpiresAt = reservationExpiry
		state.NextAttemptAt = state.RecoveryReservationExpiresAt
		return nil
	})
	if err != nil {
		return start, err
	}
	if !applied {
		start.permitted = false
		return start, nil
	}
	start.permitted = true
	return start, nil
}

func (m *Manager) beginProviderRecoveryReservationWithContext(ctx context.Context, budget time.Duration) (providerRecoveryReservation, error) {
	start, err := m.beginProviderRecoveryWithContext(ctx, false, budget, make(map[string]string))
	if err != nil {
		return providerRecoveryReservation{}, err
	}
	if !start.permitted || start.verificationOnly {
		return providerRecoveryReservation{}, errProviderRecoveryWindowNotReady
	}
	return start.reservation, nil
}

func nextProviderRecoveryToken(state *provider.ControlPlaneRecoveryState) uint64 {
	token := state.Generation + 1
	if token == 0 {
		return 1
	}
	return token
}

func nextDistinctProviderRecoveryToken(state *provider.ControlPlaneRecoveryState, avoid uint64) uint64 {
	token := nextProviderRecoveryToken(state)
	if token == avoid {
		token++
		if token == 0 {
			token = 1
		}
	}
	return token
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

func (m *Manager) recordProviderRecoveryFailureForReservationWithContext(ctx context.Context, reservation providerRecoveryReservation) (int, time.Time, bool, error) {
	var attempt int
	var next time.Time
	applied, err := m.updateProviderRecoveryStateIfReservation(ctx, reservation, func(state *provider.ControlPlaneRecoveryState) error {
		attempt = state.Attempts
		if attempt <= 0 {
			attempt = 1
			state.Attempts = attempt
		}
		delay := providerRecoveryBackoffInitial
		for i := 1; i < attempt; i++ {
			if delay >= providerRecoveryBackoffMaximum/2 {
				delay = providerRecoveryBackoffMaximum
				break
			}
			delay *= 2
		}
		if delay > providerRecoveryBackoffMaximum {
			delay = providerRecoveryBackoffMaximum
		}
		next = m.currentTime().Add(delay)
		state.NextAttemptAt = next
		// RecoverControlPlane may already have crossed the shared daemon
		// boundary, and a failed stable proof is still an interrupted recovery.
		// Retain both the reservation and its host-wide census so every takeover
		// remains verification-only until all expected identities are stable.
		state.RecoveryReservationPhase = provider.RecoveryReservationVerifying
		state.RecoveryReservationExpiresAt = next
		return nil
	})
	if !applied && err == nil {
		m.providerRecoveryMu.Lock()
		attempt = m.providerRecoveryTries
		next = m.providerRecoveryNext
		m.providerRecoveryMu.Unlock()
	}
	return attempt, next, applied, err
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

// providerRecoverySupervisorError keeps a durable-ledger or lifecycle-state
// failure from turning the caller's retry loop into a hot loop. The durable
// mutation may be unavailable or corrupt, so this is deliberately an
// in-memory fail-closed cooldown; it never invents a successful recovery or
// authorizes another daemon intervention.
func (m *Manager) providerRecoverySupervisorError(ctx context.Context, err error) (bool, error) {
	if err == nil {
		return true, nil
	}
	if ctx.Err() == nil {
		m.scheduleProviderRecovery(providerRecoveryBackoffMaximum)
	}
	return true, err
}

func (m *Manager) providerRecoveryPreInterventionError(ctx context.Context, generation uint64, cause error) (bool, error) {
	if cause == nil {
		return true, nil
	}
	if ctx.Err() != nil {
		return true, ctx.Err()
	}
	applied, err := m.updateProviderRecoveryStateIfGeneration(ctx, generation, func(state *provider.ControlPlaneRecoveryState) error {
		candidate := m.currentTime().Add(providerRecoveryBackoffMaximum)
		if state.NextAttemptAt.IsZero() || state.NextAttemptAt.Before(candidate) {
			state.NextAttemptAt = candidate
		}
		return nil
	})
	if err != nil {
		return m.providerRecoverySupervisorError(ctx, errors.Join(cause, fmt.Errorf("persist Docker Sandboxes pre-intervention recovery backoff: %w", err)))
	}
	if !applied {
		if err := m.refreshProviderRecoveryState(ctx); err != nil {
			return m.providerRecoverySupervisorError(ctx, errors.Join(cause, fmt.Errorf("refresh Docker Sandboxes recovery state after stale pre-intervention backoff: %w", err)))
		}
	}
	return true, cause
}

func (m *Manager) scheduleProviderRecoveryWithContext(ctx context.Context, delay time.Duration) (time.Time, error) {
	var next time.Time
	err := m.updateProviderRecoveryState(ctx, func(state *provider.ControlPlaneRecoveryState) error {
		if delay <= 0 {
			delay = providerRecoveryLockRetry
		}
		candidate := m.currentTime().Add(delay)
		if state.NextAttemptAt.IsZero() || state.NextAttemptAt.Before(candidate) {
			state.NextAttemptAt = candidate
		}
		next = state.NextAttemptAt
		return nil
	})
	return next, err
}

func (m *Manager) scheduleProviderRecoveryIfGenerationWithContext(ctx context.Context, generation uint64, delay time.Duration) (time.Time, bool, error) {
	var next time.Time
	applied, err := m.updateProviderRecoveryStateIfGeneration(ctx, generation, func(state *provider.ControlPlaneRecoveryState) error {
		if delay <= 0 {
			delay = providerRecoveryLockRetry
		}
		candidate := m.currentTime().Add(delay)
		if state.NextAttemptAt.IsZero() || state.NextAttemptAt.Before(candidate) {
			state.NextAttemptAt = candidate
		}
		next = state.NextAttemptAt
		return nil
	})
	if !applied && err == nil {
		m.providerRecoveryMu.Lock()
		next = m.providerRecoveryNext
		m.providerRecoveryMu.Unlock()
	}
	return next, applied, err
}

func (m *Manager) rescheduleProviderRecoveryReservationWithContext(ctx context.Context, reservation providerRecoveryReservation, admissionFailure bool, delay time.Duration) (time.Time, bool, error) {
	var next time.Time
	applied, err := m.updateProviderRecoveryStateIfReservation(ctx, reservation, func(state *provider.ControlPlaneRecoveryState) error {
		if admissionFailure {
			if reservation.admissionIncidentToken != 0 && state.AdmissionIncidentToken == reservation.admissionIncidentToken {
				state.AdmissionIncident = false
				state.AdmissionIncidentToken = 0
			}
		}
		if delay <= 0 {
			delay = providerRecoveryLockRetry
		}
		next = m.currentTime().Add(delay)
		state.NextAttemptAt = next
		// Busy means this caller did not acquire the provider mutation lock, but
		// another recovery may be crossing the same daemon boundary. Preserve the
		// census and require read-only stable verification before rearming.
		state.RecoveryReservationPhase = provider.RecoveryReservationVerifying
		state.RecoveryReservationExpiresAt = next
		return nil
	})
	if !applied && err == nil {
		m.providerRecoveryMu.Lock()
		next = m.providerRecoveryNext
		m.providerRecoveryMu.Unlock()
	}
	return next, applied, err
}

func (m *Manager) resetProviderRecovery() {
	m.providerRecoveryMu.Lock()
	m.providerRecoveryTries = 0
	m.providerRecoveryNext = time.Time{}
	m.providerRecoveryMu.Unlock()
}

func (m *Manager) resetProviderRecoveryWithContext(ctx context.Context) error {
	return m.updateProviderRecoveryState(ctx, func(state *provider.ControlPlaneRecoveryState) error {
		if !state.NextAttemptAt.IsZero() && m.currentTime().Before(state.NextAttemptAt) {
			return nil
		}
		state.Attempts = 0
		state.NextAttemptAt = time.Time{}
		return nil
	})
}

func (m *Manager) resetProviderRecoveryIfGenerationWithContext(ctx context.Context, generation uint64) (bool, error) {
	return m.updateProviderRecoveryStateIfGeneration(ctx, generation, func(state *provider.ControlPlaneRecoveryState) error {
		state.Attempts = 0
		state.NextAttemptAt = time.Time{}
		if state.RecoveryReservationPhase == provider.RecoveryReservationReserved {
			// Reserved is durable proof that no provider intervention began. Once a
			// fresh inventory probe succeeds, this expired pre-boundary reservation
			// can be released without carrying an invalid zero-attempt token.
			state.RecoveryReservationToken = 0
			state.RecoveryReservationPhase = provider.RecoveryReservationNone
			state.RecoveryReservationExpiresAt = time.Time{}
		}
		state.ExpectedIdentities = make(map[string]string)
		return nil
	})
}

func (m *Manager) completeProviderRecoveryReservationWithContext(ctx context.Context, reservation providerRecoveryReservation) (bool, error) {
	return m.updateProviderRecoveryStateIfReservation(ctx, reservation, func(state *provider.ControlPlaneRecoveryState) error {
		state.Attempts = 0
		state.NextAttemptAt = time.Time{}
		state.RecoveryReservationToken = 0
		state.RecoveryReservationPhase = provider.RecoveryReservationNone
		state.RecoveryReservationExpiresAt = time.Time{}
		state.ExpectedIdentities = make(map[string]string)
		return nil
	})
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

func (m *Manager) reserveProviderRecoveryWithContext(ctx context.Context, admissionFailure bool) (permitted, admissionIncident bool, err error) {
	var applied bool
	applied, err = m.updateProviderRecoveryStateIf(ctx, func(state provider.ControlPlaneRecoveryState) bool {
		if !state.NextAttemptAt.IsZero() && m.currentTime().Before(state.NextAttemptAt) {
			admissionIncident = state.AdmissionIncident
			return false
		}
		if state.AdmissionIncident {
			admissionIncident = true
			return false
		}
		return true
	}, func(state *provider.ControlPlaneRecoveryState) error {
		if admissionFailure {
			state.AdmissionIncident = true
			state.AdmissionIncidentToken = nextProviderRecoveryToken(state)
		}
		permitted = true
		return nil
	})
	if err == nil && !applied {
		permitted = false
	}
	return permitted, admissionIncident, err
}

func (m *Manager) cancelProviderAdmissionRecovery() {
	m.providerRecoveryMu.Lock()
	m.providerAdmissionRecoveryAttempted = false
	m.providerAdmissionRecoveryToken = 0
	m.providerRecoveryMu.Unlock()
}

func (m *Manager) cancelProviderAdmissionRecoveryWithContext(ctx context.Context) error {
	return m.updateProviderRecoveryState(ctx, func(state *provider.ControlPlaneRecoveryState) error {
		state.AdmissionIncident = false
		state.AdmissionIncidentToken = 0
		return nil
	})
}

func (m *Manager) resetProviderAdmissionRecovery() {
	m.providerRecoveryMu.Lock()
	m.providerAdmissionRecoveryAttempted = false
	m.providerAdmissionRecoveryToken = 0
	m.providerRecoveryMu.Unlock()
}

func (m *Manager) resetProviderAdmissionRecoveryWithContext(ctx context.Context, admissionToken uint64) error {
	return m.updateProviderRecoveryStateIfAdmissionToken(ctx, admissionToken, func(state *provider.ControlPlaneRecoveryState) error {
		state.AdmissionIncident = false
		state.AdmissionIncidentToken = 0
		return nil
	})
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
