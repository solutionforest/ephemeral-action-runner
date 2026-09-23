package pool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
	poolstate "github.com/solutionforest/ephemeral-action-runner/internal/pool/state"
	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
)

func (m *Manager) cleanupOwnedLifecycle(ctx context.Context) error {
	records, err := m.LifecycleState.List(ctx)
	if err != nil {
		return fmt.Errorf("read provider-neutral lifecycle state: %w", err)
	}
	inventory, err := m.inventoryProvider(ctx)
	if err != nil {
		return fmt.Errorf("read provider inventory for exact cleanup: %w", err)
	}
	byName := inventoryByName(inventory)
	protectedNames := make(map[string]struct{}, len(records))
	for _, record := range records {
		if record.Phase != poolstate.PhaseTombstoned {
			protectedNames[record.Name] = struct{}{}
		}
	}
	var firstErr error
	for _, item := range inventory {
		if !HasPrefix(item.Instance.Name, m.Config.Pool.NamePrefix) {
			continue
		}
		if _, found := protectedNames[item.Instance.Name]; found {
			continue
		}
		cleaned, cleanupErr := m.cleanupPrefixOrphanInventory(ctx, item)
		if cleanupErr != nil {
			firstErr = errors.Join(firstErr, fmt.Errorf("cleanup prefix-owned orphan %s: %w", item.Instance.Name, cleanupErr))
			continue
		}
		if !cleaned {
			if err := m.reportUnknownInventory(ctx, item); err != nil {
				return err
			}
		}
	}

	for _, record := range records {
		if record.ProviderType != m.Config.Provider.Type || record.Phase == poolstate.PhaseTombstoned {
			continue
		}
		if err := m.cleanupLifecycleRecord(ctx, record, byName[record.Name]); err != nil {
			firstErr = errors.Join(firstErr, fmt.Errorf("cleanup %s: %w", record.Name, err))
		}
	}
	return firstErr
}

// cleanupPrefixOrphanInventory removes an inventory item that matches this
// configuration's exact prefix but has no non-tombstoned lifecycle record. A
// provider must opt in by reconstructing an immutable cleanup receipt from
// the inventory evidence; a bare name or prefix never authorizes deletion.
func (m *Manager) cleanupPrefixOrphanInventory(ctx context.Context, item provider.InventoryItem) (bool, error) {
	if !HasPrefix(item.Instance.Name, m.Config.Pool.NamePrefix) {
		return false, nil
	}
	if m.LifecycleState != nil {
		record, err := m.LifecycleState.Read(ctx, item.Instance.Name)
		if err == nil {
			if record.Phase != poolstate.PhaseTombstoned {
				return false, nil
			}
		} else if !errors.Is(err, poolstate.ErrNotFound) {
			return false, err
		}
	}
	preparer, ok := m.providerLifecycle().(provider.OrphanCleanupPreparer)
	if !ok {
		return false, nil
	}
	instance, err := preparer.PrepareOrphanCleanup(ctx, item, m.expectedOrphanWorkspace(item.Instance.Name))
	if err != nil {
		return false, nil
	}
	if instance.Name != item.Instance.Name || instance.ProviderID == "" || instance.ProviderID != item.Instance.ProviderID || instance.ReceiptVersion == "" || len(instance.Receipt) == 0 {
		return false, nil
	}

	retain := func(cause error) error {
		retained := item
		retained.Instance = instance
		if retainErr := m.reportUnknownInventory(ctx, retained); retainErr != nil {
			return errors.Join(cause, retainErr)
		}
		return cause
	}
	if m.GitHub != nil {
		runner, found, lookupErr := m.GitHub.RunnerByName(ctx, item.Instance.Name)
		if lookupErr != nil {
			return false, retain(lookupErr)
		}
		if found {
			if runner.Busy {
				return false, retain(fmt.Errorf("same-name GitHub runner id=%d is busy; refusing orphan cleanup", runner.ID))
			}
			deleteCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
			deleteErr := m.GitHub.DeleteRunnerIfExists(deleteCtx, runner.ID)
			cancel()
			if deleteErr != nil {
				return false, retain(fmt.Errorf("delete exact prefix-owned GitHub runner %s id=%d: %w", runner.Name, runner.ID, deleteErr))
			}
			after, remains, verifyErr := m.GitHub.RunnerByName(ctx, item.Instance.Name)
			if verifyErr != nil {
				return false, retain(fmt.Errorf("verify exact prefix-owned GitHub runner absence: %w", verifyErr))
			}
			if remains {
				return false, retain(fmt.Errorf("GitHub runner remains after exact deletion: name=%s id=%d", after.Name, after.ID))
			}
			m.infof("cleanup: deleted exact prefix-owned GitHub runner %s id=%d\n", runner.Name, runner.ID)
		}
	}
	stopCtx, stopCancel := context.WithTimeout(ctx, 60*time.Second)
	_ = m.stopProviderInstance(stopCtx, instance)
	stopCancel()
	deleteCtx, deleteCancel := context.WithTimeout(ctx, 60*time.Second)
	deleteErr := m.deleteProviderInstance(deleteCtx, instance)
	deleteCancel()
	if deleteErr != nil {
		return false, retain(fmt.Errorf("delete exact prefix-owned provider instance %s id=%s: %w", instance.Name, instance.ProviderID, deleteErr))
	}
	remaining, inventoryErr := m.inventoryProvider(ctx)
	if inventoryErr != nil {
		return false, retain(fmt.Errorf("verify exact prefix-owned provider instance absence: %w", inventoryErr))
	}
	for _, remainingItem := range remaining {
		if remainingItem.Instance.ProviderID == instance.ProviderID {
			return false, retain(fmt.Errorf("provider instance remains after exact deletion: name=%s id=%s", remainingItem.Instance.Name, remainingItem.Instance.ProviderID))
		}
		if remainingItem.Instance.Name == instance.Name {
			return false, retain(fmt.Errorf("same-name provider instance id=%s appeared after exact deletion; refusing to treat it as the deleted resource", remainingItem.Instance.ProviderID))
		}
	}
	if m.LifecycleState != nil {
		if err := m.LifecycleState.ForgetUnknown(ctx, m.Config.Provider.Type, instance.ProviderID); err != nil {
			return false, retain(err)
		}
	}
	paths := ProvisionedInstance{Name: instance.Name, LogPath: m.instanceLogPath(instance.Name, "."+m.Config.Provider.Type+".log"), GuestLogPath: m.instanceLogPath(instance.Name, ".guest.log")}
	if releaseErr := m.releaseInstanceTranscripts(paths); releaseErr != nil {
		m.logger().Warn("instance transcript close failed after prefix-owned orphan cleanup", "provider", m.Config.Provider.Type, "instance", instance.Name, "operation", "cleanup", "error", releaseErr)
	}
	m.infof("cleanup: deleted exact prefix-owned instance %s id=%s\n", instance.Name, instance.ProviderID)
	return true, nil
}

func (m *Manager) expectedOrphanWorkspace(name string) string {
	if m.Config.Provider.Type != "docker-sandboxes" || name == "" {
		return ""
	}
	root, err := filepath.Abs(config.ProjectPath(m.ProjectRoot, m.Config.DockerSandboxes.StagingRoot))
	if err != nil {
		return ""
	}
	return filepath.Join(filepath.Clean(root), name)
}

func inventoryByName(items []provider.InventoryItem) map[string][]provider.InventoryItem {
	result := make(map[string][]provider.InventoryItem)
	for _, item := range items {
		result[item.Instance.Name] = append(result[item.Instance.Name], item)
	}
	return result
}

func (m *Manager) reportUnknownInventory(ctx context.Context, item provider.InventoryItem) error {
	providerID := item.Instance.ProviderID
	if providerID == "" {
		providerID = "unidentified:" + item.Instance.Name
	}
	payload, _ := json.Marshal(map[string]string{"state": item.State, "source": item.Source})
	receipt := poolstate.Receipt{Version: "v1", Payload: payload}
	if item.Instance.ReceiptVersion != "" && len(item.Instance.Receipt) != 0 && json.Valid(item.Instance.Receipt) {
		receipt = poolstate.Receipt{Version: item.Instance.ReceiptVersion, Payload: append([]byte(nil), item.Instance.Receipt...)}
	}
	_, err := m.LifecycleState.ReportUnknown(ctx, poolstate.Discovery{
		ProviderType: m.Config.Provider.Type,
		ProviderID:   providerID,
		ExactName:    item.Instance.Name,
		Receipt:      receipt,
	})
	if err != nil {
		return fmt.Errorf("quarantine unowned provider instance %q: %w", item.Instance.Name, err)
	}
	m.warnf("cleanup: quarantined unowned instance %s id=%s; prefix-only or unidentified resources are report-only\n", item.Instance.Name, providerID)
	return nil
}

func (m *Manager) cleanupLifecycleRecord(ctx context.Context, initial poolstate.Record, sameName []provider.InventoryItem) error {
	return m.cleanupLifecycleRecordWithRemoteAbsence(ctx, initial, sameName, false)
}

func (m *Manager) cleanupLifecycleRecordWithRemoteAbsence(ctx context.Context, initial poolstate.Record, sameName []provider.InventoryItem, remoteKnownAbsent bool) error {
	return m.cleanupLifecycleRecordWithPolicy(ctx, initial, sameName, remoteKnownAbsent, false)
}

// cleanupLifecycleRecordAfterKnownCreateFailure is used only by the active
// provisioning attempt after the provider has returned an ordinary,
// non-ambiguous create failure. A restarted controller cannot make that same
// inference from a durable creating record, so the normal cleanup path keeps
// its conservative uncertainty fence.
func (m *Manager) cleanupLifecycleRecordAfterKnownCreateFailure(ctx context.Context, initial poolstate.Record, sameName []provider.InventoryItem, remoteKnownAbsent bool) error {
	return m.cleanupLifecycleRecordWithPolicy(ctx, initial, sameName, remoteKnownAbsent, true)
}

func (m *Manager) cleanupLifecycleRecordWithPolicy(ctx context.Context, initial poolstate.Record, sameName []provider.InventoryItem, remoteKnownAbsent, allowKnownCreateAbsence bool) error {
	for {
		record, err := m.LifecycleState.Read(ctx, initial.Name)
		if err != nil {
			return err
		}
		if !remoteKnownAbsent {
			absent, err := m.reconcileJobLeaseForCleanup(ctx, record)
			if err != nil {
				return err
			}
			remoteKnownAbsent = absent
			if absent {
				continue
			}
			record, err = m.LifecycleState.Read(ctx, initial.Name)
			if err != nil {
				return err
			}
		}
		if lease, protected := activeLifecycleLease(record.Leases, m.currentTime()); protected {
			return fmt.Errorf("active %s lease held by %s protects the instance until %s; refusing cleanup", lease.Purpose, lease.Holder, lease.ExpiresAt.Format(time.RFC3339))
		}
		switch record.Phase {
		case poolstate.PhaseTombstoned:
			return nil
		case poolstate.PhaseReserved, poolstate.PhaseCreating:
			if len(sameName) != 0 {
				for _, item := range sameName {
					if reportErr := m.reportUnknownInventory(ctx, item); reportErr != nil {
						return reportErr
					}
				}
				m.quarantineLifecycle(ctx, record.Name, errors.New(interruptedCreateNoIdentityReason))
				return fmt.Errorf("unidentified same-name instance is quarantined and was not deleted")
			}
			if isUncertainCreateRecord(record) && !(allowKnownCreateAbsence && record.Phase == poolstate.PhaseCreating) {
				return fmt.Errorf("provider create outcome remains uncertain; refusing absence-only tombstone")
			}
			if m.GitHub != nil {
				if _, found, err := m.GitHub.RunnerByName(ctx, record.GitHub.ExactName); err != nil {
					return err
				} else if found {
					m.quarantineLifecycle(ctx, record.Name, errors.New(interruptedCreateGitHubIdentityReason))
					return fmt.Errorf("unidentified same-name GitHub runner is quarantined and was not deleted")
				}
			}
			_, err = m.LifecycleState.Transition(ctx, record.Name, poolstate.Transition{Action: poolstate.ActionAbandonCreate})
			return err
		case poolstate.PhaseCleanupPending:
			if _, err := m.LifecycleState.Transition(ctx, record.Name, poolstate.Transition{Action: poolstate.ActionResumeCleanup}); err != nil {
				return err
			}
		case poolstate.PhaseQuarantined:
			if record.ProviderID == "" {
				if len(sameName) != 0 {
					for _, item := range sameName {
						if reportErr := m.reportUnknownInventory(ctx, item); reportErr != nil {
							return reportErr
						}
					}
					return fmt.Errorf("unidentified same-name instance is quarantined and was not deleted")
				}
				if isUncertainCreateRecord(record) {
					return fmt.Errorf("provider create outcome remains uncertain; refusing absence-only tombstone")
				}
				if m.GitHub == nil {
					return fmt.Errorf("record has no immutable provider identity and GitHub absence cannot be verified")
				}
				if _, found, err := m.GitHub.RunnerByName(ctx, record.GitHub.ExactName); err != nil {
					return err
				} else if found {
					return fmt.Errorf("unidentified same-name GitHub runner is quarantined and was not deleted")
				}
				_, err = m.LifecycleState.Transition(ctx, record.Name, poolstate.Transition{Action: poolstate.ActionAbandonCreate})
				return err
			}
			if record.RecoveryInventoryUncertain {
				exactFound := false
				for _, item := range sameName {
					if item.Instance.Name != record.Name {
						continue
					}
					if item.Instance.ProviderID != record.ProviderID {
						return fmt.Errorf("same-name provider instance id=%s does not match recorded id=%s; refusing deletion", item.Instance.ProviderID, record.ProviderID)
					}
					exactFound = true
				}
				if !exactFound {
					return fmt.Errorf("post-recovery provider inventory remains uncertain; refusing absence-only cleanup")
				}
				if _, err := m.LifecycleState.Transition(ctx, record.Name, poolstate.Transition{Action: poolstate.ActionRecoveryInventoryObserved}); err != nil {
					return err
				}
				continue
			}
			if _, err := m.LifecycleState.Transition(ctx, record.Name, poolstate.Transition{Action: poolstate.ActionFenceIntent}); err != nil {
				return err
			}
		case poolstate.PhaseCreated, poolstate.PhaseValidating, poolstate.PhaseStandby, poolstate.PhaseRegistering, poolstate.PhaseReady, poolstate.PhaseBusy, poolstate.PhaseDraining:
			if record.ProviderID == "" {
				return fmt.Errorf("record has no immutable provider identity and remains report-only")
			}
			if _, err := m.LifecycleState.Transition(ctx, record.Name, poolstate.Transition{Action: poolstate.ActionFenceIntent}); err != nil {
				return err
			}
		case poolstate.PhaseFencing:
			if _, err := m.LifecycleState.Transition(ctx, record.Name, poolstate.Transition{Action: poolstate.ActionFenced}); err != nil {
				return err
			}
		case poolstate.PhaseFenced:
			if _, err := m.LifecycleState.Transition(ctx, record.Name, poolstate.Transition{Action: poolstate.ActionVerifyRemoteIntent}); err != nil {
				return err
			}
		case poolstate.PhaseRemoteReconciling:
			if !remoteKnownAbsent {
				if err := m.removeExactGitHubRunner(ctx, record); err != nil {
					m.markCleanupPending(ctx, record.Name)
					return err
				}
			}
			if _, err := m.LifecycleState.Transition(ctx, record.Name, poolstate.Transition{Action: poolstate.ActionRemoteAbsent}); err != nil {
				return err
			}
		case poolstate.PhaseRemoteAbsent:
			if _, err := m.LifecycleState.Transition(ctx, record.Name, poolstate.Transition{Action: poolstate.ActionRemoveLocalIntent}); err != nil {
				return err
			}
		case poolstate.PhaseLocalRemoving:
			if err := m.removeExactProviderInstance(ctx, record, sameName); err != nil {
				m.markCleanupPending(ctx, record.Name)
				return err
			}
			if _, err := m.LifecycleState.Transition(ctx, record.Name, poolstate.Transition{Action: poolstate.ActionLocalAbsent}); err != nil {
				return err
			}
		case poolstate.PhaseLocalAbsent:
			if _, err := m.LifecycleState.Transition(ctx, record.Name, poolstate.Transition{Action: poolstate.ActionTombstone}); err != nil {
				return err
			}
			paths := ProvisionedInstance{Name: record.Name, LogPath: m.instanceLogPath(record.Name, "."+m.Config.Provider.Type+".log"), GuestLogPath: m.instanceLogPath(record.Name, ".guest.log")}
			if releaseErr := m.releaseInstanceTranscripts(paths); releaseErr != nil {
				m.logger().Warn("instance transcript close failed after cleanup", "provider", m.Config.Provider.Type, "instance", record.Name, "operation", "cleanup", "error", releaseErr)
			}
		default:
			return fmt.Errorf("unsupported cleanup phase %s", record.Phase)
		}
	}
}

func (m *Manager) reconcileJobLeaseForCleanup(ctx context.Context, record poolstate.Record) (bool, error) {
	shouldReconcile := record.Phase == poolstate.PhaseBusy
	expectedHolder := fmt.Sprintf("github-%d", record.GitHub.RunnerID)
	for _, lease := range record.Leases {
		if lease.Purpose == "job" && lease.Holder == expectedHolder {
			shouldReconcile = true
			break
		}
	}
	if !shouldReconcile {
		return false, nil
	}
	if m.GitHub == nil || record.GitHub.ExactName == "" || record.GitHub.RunnerID == 0 {
		return false, fmt.Errorf("cannot verify job completion for %s without its exact GitHub runner identity", record.Name)
	}
	runner, found, err := m.GitHub.RunnerByName(ctx, record.GitHub.ExactName)
	if err != nil {
		return false, fmt.Errorf("verify job completion against exact GitHub runner %s id=%d: %w", record.GitHub.ExactName, record.GitHub.RunnerID, err)
	}
	if !found {
		if err := m.recordLifecycleRemoteAbsence(ctx, record.Name); err != nil {
			return false, fmt.Errorf("record exact GitHub runner absence before cleanup: %w", err)
		}
		return true, nil
	}
	if runner.ID != record.GitHub.RunnerID {
		return false, fmt.Errorf("same-name GitHub runner id=%d does not match recorded id=%d; preserving active job lease", runner.ID, record.GitHub.RunnerID)
	}
	if err := m.recordLifecycleJobObservation(ctx, runner); err != nil {
		return false, fmt.Errorf("record exact GitHub runner state before cleanup: %w", err)
	}
	return false, nil
}

func activeLifecycleLease(leases []poolstate.Lease, now time.Time) (poolstate.Lease, bool) {
	for _, lease := range leases {
		if lease.ExpiresAt.After(now) {
			return lease, true
		}
	}
	return poolstate.Lease{}, false
}

func (m *Manager) removeExactGitHubRunner(ctx context.Context, record poolstate.Record) error {
	if m.GitHub == nil {
		if record.GitHub.RunnerID != 0 {
			return fmt.Errorf("cannot verify GitHub runner id=%d without a GitHub client", record.GitHub.RunnerID)
		}
		return nil
	}
	runner, found, err := m.GitHub.RunnerByName(ctx, record.GitHub.ExactName)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	if record.GitHub.RunnerID == 0 || runner.ID != record.GitHub.RunnerID {
		return fmt.Errorf("same-name GitHub runner id=%d does not match recorded id=%d; refusing deletion", runner.ID, record.GitHub.RunnerID)
	}
	deleteCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if err := m.GitHub.DeleteRunnerIfExists(deleteCtx, runner.ID); err != nil {
		return err
	}
	after, found, err := m.GitHub.RunnerByName(ctx, record.GitHub.ExactName)
	if err != nil {
		return err
	}
	if found {
		return fmt.Errorf("GitHub runner remains after exact deletion: name=%s id=%d", after.Name, after.ID)
	}
	m.infof("cleanup: deleted exact GitHub runner %s id=%d\n", runner.Name, runner.ID)
	return nil
}

func (m *Manager) removeExactProviderInstance(ctx context.Context, record poolstate.Record, sameName []provider.InventoryItem) error {
	var exact *provider.Instance
	for i := range sameName {
		item := sameName[i]
		if item.Instance.ProviderID == record.ProviderID {
			copy := item.Instance
			exact = &copy
			continue
		}
		if item.Instance.Name == record.Name {
			return fmt.Errorf("same-name provider instance id=%s does not match recorded id=%s; refusing deletion", item.Instance.ProviderID, record.ProviderID)
		}
	}
	if exact == nil {
		return nil
	}
	exact.ReceiptVersion = record.Receipt.Version
	exact.Receipt = append([]byte(nil), record.Receipt.Payload...)
	stopCtx, stopCancel := context.WithTimeout(ctx, 60*time.Second)
	_ = m.stopProviderInstance(stopCtx, *exact)
	stopCancel()
	deleteCtx, deleteCancel := context.WithTimeout(ctx, 60*time.Second)
	err := m.deleteProviderInstance(deleteCtx, *exact)
	deleteCancel()
	if err != nil {
		return err
	}
	remaining, err := m.inventoryProvider(ctx)
	if err != nil {
		return err
	}
	for _, item := range remaining {
		if item.Instance.ProviderID == record.ProviderID {
			return fmt.Errorf("provider instance remains after exact deletion: name=%s id=%s", item.Instance.Name, item.Instance.ProviderID)
		}
	}
	m.infof("cleanup: deleted exact owned instance %s id=%s\n", record.Name, record.ProviderID)
	return nil
}

func (m *Manager) markCleanupPending(ctx context.Context, name string) {
	if _, err := m.LifecycleState.Transition(ctx, name, poolstate.Transition{Action: poolstate.ActionCleanupPending}); err != nil && !errors.Is(err, poolstate.ErrInvalidTransition) {
		m.warnf("[%s] failed to persist cleanup-pending phase: %v\n", name, err)
	}
}
