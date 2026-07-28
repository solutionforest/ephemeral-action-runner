package pool

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"time"

	gh "github.com/solutionforest/ephemeral-action-runner/internal/github"
	poolstate "github.com/solutionforest/ephemeral-action-runner/internal/pool/state"
	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
)

// OpenLifecycleState opens the provider-neutral state namespace for one exact
// configuration file. The namespace hash prevents two configurations in the
// same checkout from claiming each other's instances.
func OpenLifecycleState(projectRoot, configPath string) (*poolstate.Store, error) {
	absoluteConfig, err := filepath.Abs(configPath)
	if err != nil {
		return nil, fmt.Errorf("resolve lifecycle config path: %w", err)
	}
	sum := sha256.Sum256([]byte(filepath.Clean(absoluteConfig)))
	namespace := hex.EncodeToString(sum[:8])
	return poolstate.Open(filepath.Join(projectRoot, ".local", "state", "pools", namespace))
}

func (m *Manager) reserveLifecycle(ctx context.Context, name string) error {
	if m.LifecycleState == nil {
		return nil
	}
	_, err := m.LifecycleState.Reserve(ctx, poolstate.CreateSpec{
		Name:         name,
		ProviderType: m.Config.Provider.Type,
		GitHub:       poolstate.GitHubIdentity{ExactName: name},
	})
	if err != nil {
		return fmt.Errorf("reserve provider-neutral lifecycle record: %w", err)
	}
	_, err = m.LifecycleState.Transition(ctx, name, poolstate.Transition{Action: poolstate.ActionCreateIntent})
	if err != nil {
		return fmt.Errorf("record provider create intent: %w", err)
	}
	return nil
}

func (m *Manager) acquireLifecycleLease(ctx context.Context, name, purpose, holder string, lifetime time.Duration) error {
	if m.LifecycleState == nil {
		return nil
	}
	_, err := m.LifecycleState.AcquireLease(ctx, name, poolstate.Lease{Purpose: purpose, Holder: holder, ExpiresAt: m.currentTime().Add(lifetime)})
	return err
}

func (m *Manager) releaseLifecycleLease(ctx context.Context, name, purpose, holder string) {
	if m.LifecycleState == nil {
		return
	}
	if _, err := m.LifecycleState.ReleaseLease(ctx, name, purpose, holder); err != nil && !errors.Is(err, poolstate.ErrNotFound) {
		m.warnf("[%s] release %s lifecycle lease failed: %v\n", name, purpose, err)
	}
}

// recoverInterruptedProvisionLeases removes only leases owned by a previous
// common controller after the caller has acquired the exclusive pool lock.
// Job and provider-specific leases remain authoritative cleanup barriers.
func (m *Manager) recoverInterruptedProvisionLeases(ctx context.Context) error {
	if m.LifecycleState == nil {
		return nil
	}
	records, err := m.LifecycleState.List(ctx)
	if err != nil {
		return fmt.Errorf("read lifecycle state for interrupted provisioning recovery: %w", err)
	}
	for _, record := range records {
		if record.ProviderType != m.Config.Provider.Type || record.Phase == poolstate.PhaseTombstoned {
			continue
		}
		for _, lease := range record.Leases {
			if lease.Purpose != "provision" || lease.Holder != "controller" {
				continue
			}
			if _, err := m.LifecycleState.ReleaseLease(ctx, record.Name, lease.Purpose, lease.Holder); err != nil {
				return fmt.Errorf("release interrupted provisioning lease for %s: %w", record.Name, err)
			}
			m.warnf("[%s] recovered interrupted provisioning lease after acquiring the exclusive pool lock\n", record.Name)
			break
		}
	}
	return nil
}

func (m *Manager) recordLifecycleJobObservation(ctx context.Context, runner gh.Runner) error {
	if m.LifecycleState == nil {
		return nil
	}
	record, err := m.LifecycleState.Read(ctx, runner.Name)
	if err != nil {
		return err
	}
	holder := "github-" + strconv.FormatInt(runner.ID, 10)
	if runner.Busy {
		if record.Phase == poolstate.PhaseReady {
			if _, err := m.LifecycleState.Transition(ctx, runner.Name, poolstate.Transition{Action: poolstate.ActionJobStarted}); err != nil {
				return err
			}
		}
		return m.acquireLifecycleLease(ctx, runner.Name, "job", holder, 10*time.Minute)
	}
	if record.Phase == poolstate.PhaseBusy {
		if _, err := m.LifecycleState.Transition(ctx, runner.Name, poolstate.Transition{Action: poolstate.ActionJobFinished}); err != nil {
			return err
		}
		m.releaseLifecycleLease(ctx, runner.Name, "job", holder)
		return nil
	}
	for _, lease := range record.Leases {
		if lease.Purpose == "job" && lease.Holder == holder {
			m.releaseLifecycleLease(ctx, runner.Name, "job", holder)
			break
		}
	}
	return nil
}

func (m *Manager) recordLifecycleRemoteAbsence(ctx context.Context, name string) error {
	if m.LifecycleState == nil {
		return nil
	}
	record, err := m.LifecycleState.Read(ctx, name)
	if err != nil {
		return err
	}
	if record.Phase == poolstate.PhaseBusy {
		if _, err := m.LifecycleState.Transition(ctx, name, poolstate.Transition{Action: poolstate.ActionJobFinished}); err != nil {
			return err
		}
	}
	if record.GitHub.RunnerID != 0 {
		m.releaseLifecycleLease(ctx, name, "job", "github-"+strconv.FormatInt(record.GitHub.RunnerID, 10))
	}
	return nil
}

func (m *Manager) recordLifecycleCreated(ctx context.Context, instance provider.Instance) error {
	if m.LifecycleState == nil {
		return nil
	}
	version := instance.ReceiptVersion
	payload := append([]byte(nil), instance.Receipt...)
	if version == "" || len(payload) == 0 {
		version = "v1"
		var err error
		payload, err = json.Marshal(map[string]string{"exactName": instance.Name, "providerId": instance.ProviderID, "source": instance.Source})
		if err != nil {
			return fmt.Errorf("encode provider lifecycle receipt: %w", err)
		}
	}
	_, err := m.LifecycleState.Transition(ctx, instance.Name, poolstate.Transition{
		Action:     poolstate.ActionCreated,
		ProviderID: instance.ProviderID,
		Receipt:    poolstate.Receipt{Version: version, Payload: payload},
	})
	if err != nil {
		return fmt.Errorf("record provider instance creation: %w", err)
	}
	return nil
}

func (m *Manager) recordLifecycleValidationIntent(ctx context.Context, name string) error {
	if m.LifecycleState == nil {
		return nil
	}
	_, err := m.LifecycleState.Transition(ctx, name, poolstate.Transition{Action: poolstate.ActionValidateIntent})
	return err
}

func (m *Manager) recordLifecycleValidated(ctx context.Context, name string) error {
	if m.LifecycleState == nil {
		return nil
	}
	_, err := m.LifecycleState.Transition(ctx, name, poolstate.Transition{Action: poolstate.ActionValidated})
	return err
}

func (m *Manager) recordLifecycleRegistrationIntent(ctx context.Context, name string) error {
	if m.LifecycleState == nil {
		return nil
	}
	_, err := m.LifecycleState.Transition(ctx, name, poolstate.Transition{Action: poolstate.ActionRegisterIntent})
	return err
}

func (m *Manager) recordLifecycleRegistered(ctx context.Context, name string, runnerID int64) error {
	if m.LifecycleState == nil {
		return nil
	}
	_, err := m.LifecycleState.Transition(ctx, name, poolstate.Transition{Action: poolstate.ActionRegistered, RunnerID: runnerID})
	return err
}

func (m *Manager) quarantineLifecycle(ctx context.Context, name string, cause error) {
	if m.LifecycleState == nil || cause == nil {
		return
	}
	if _, err := m.LifecycleState.Transition(ctx, name, poolstate.Transition{Action: poolstate.ActionQuarantine, Reason: cause.Error()}); err != nil && !errors.Is(err, poolstate.ErrInvalidTransition) {
		m.warnf("[%s] provider-neutral lifecycle quarantine failed: %v\n", name, err)
	}
}

func (m *Manager) lifecycleOwns(ctx context.Context, name, providerID string) (bool, error) {
	if m.LifecycleState == nil {
		return true, nil
	}
	record, err := m.LifecycleState.Read(ctx, name)
	if errors.Is(err, poolstate.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return record.ProviderType == m.Config.Provider.Type && record.ProviderID != "" && record.ProviderID == providerID && record.Phase != poolstate.PhaseTombstoned, nil
}

func (m *Manager) lifecycleOwnsRunner(ctx context.Context, name string, runnerID int64) (bool, error) {
	if m.LifecycleState == nil {
		return true, nil
	}
	record, err := m.LifecycleState.Read(ctx, name)
	if errors.Is(err, poolstate.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return record.ProviderType == m.Config.Provider.Type && record.GitHub.RunnerID != 0 && record.GitHub.RunnerID == runnerID && record.Phase != poolstate.PhaseTombstoned, nil
}

func (m *Manager) reportUnknownLifecycle(ctx context.Context, name, providerID, source, observedState string) error {
	if m.LifecycleState == nil {
		return nil
	}
	payload, err := json.Marshal(map[string]string{"source": source, "state": observedState})
	if err != nil {
		return err
	}
	_, err = m.LifecycleState.ReportUnknown(ctx, poolstate.Discovery{
		ProviderType: m.Config.Provider.Type,
		ProviderID:   providerID,
		ExactName:    name,
		Receipt:      poolstate.Receipt{Version: "v1", Payload: payload},
	})
	return err
}
