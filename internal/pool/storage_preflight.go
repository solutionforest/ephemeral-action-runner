package pool

import (
	"context"
	"fmt"
	"strings"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
	"github.com/solutionforest/ephemeral-action-runner/internal/invocation"
	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
	"github.com/solutionforest/ephemeral-action-runner/internal/storage"
)

const (
	instanceCreateExpansionBytes = 10 * storage.GiB
)

func (m *Manager) preflightStorage(operation string, peakBytes uint64) error {
	minimumFree, err := config.EffectiveMinimumFreeBytes(m.Config)
	if err != nil {
		return err
	}
	if m.Storage != nil {
		snapshot, err := m.Storage.StorageSnapshot(context.Background(), provider.StorageRequest{
			Operation:        operation,
			Now:              m.currentTime(),
			PeakBytes:        peakBytes,
			MinimumFreeBytes: minimumFree,
		})
		if err != nil {
			measurementErr := fmt.Errorf("provider storage surface cannot be measured before %s: %w\n\nInspect storage with:\n  %s", operation, err, invocation.Command("storage", "status", "--provider", m.Config.Provider.Type))
			if m.AllowInsufficientStorage {
				m.warnStorageOverride(operation, measurementErr)
				return nil
			}
			return measurementErr
		}
		if len(snapshot.Surfaces) == 0 || len(snapshot.Requirements) == 0 {
			return fmt.Errorf("provider %q returned no required storage surface for %s", m.Config.Provider.Type, operation)
		}
		surfaces := make(map[string]storage.Surface, len(snapshot.Surfaces))
		for _, surface := range snapshot.Surfaces {
			surfaces[surface.ID] = surface
		}
		for _, requirement := range snapshot.Requirements {
			surface, found := surfaces[requirement.SurfaceID]
			if !found {
				return fmt.Errorf("provider storage requirement %q references unknown surface %q", requirement.ID, requirement.SurfaceID)
			}
			check, err := storage.EvaluateCapacity(surface, requirement)
			if err != nil {
				return fmt.Errorf("evaluate storage capacity before %s: %w", operation, err)
			}
			if check.Status != storage.CapacityReady {
				admissionErr := storageAdmissionError(operation, surface, requirement, check, m.Config.Provider.Type, m.StorageOverrideCommand)
				if m.AllowInsufficientStorage {
					m.warnStorageOverride(operation, admissionErr)
					continue
				}
				return admissionErr
			}
		}
		return nil
	}
	capacity, err := storage.ProbeFilesystemCapacity(m.ProjectRoot, m.currentTime())
	if err != nil {
		measurementErr := fmt.Errorf("storage surface %q cannot be measured before %s: %w\n\nInspect storage with:\n  %s", m.ProjectRoot, operation, err, invocation.Command("storage", "status", "--provider", m.Config.Provider.Type))
		if m.AllowInsufficientStorage {
			m.warnStorageOverride(operation, measurementErr)
			return nil
		}
		return measurementErr
	}
	surface := storage.Surface{
		ID:                     "project",
		Provider:               m.Config.Provider.Type,
		Kind:                   storage.SurfaceHostFilesystem,
		Location:               m.ProjectRoot,
		Classification:         "physical",
		Confidence:             "authoritative-filesystem-probe",
		AdmissionAuthoritative: true,
		Capacity:               capacity,
	}
	requirement := storage.Requirement{
		ID:               operation,
		Provider:         m.Config.Provider.Type,
		SurfaceID:        surface.ID,
		PeakBytes:        peakBytes,
		MinimumFreeBytes: minimumFree,
	}
	check, err := storage.EvaluateCapacity(surface, requirement)
	if err != nil {
		return fmt.Errorf("evaluate storage capacity before %s: %w", operation, err)
	}
	if check.Status != storage.CapacityReady {
		admissionErr := storageAdmissionError(operation, surface, requirement, check, m.Config.Provider.Type, m.StorageOverrideCommand)
		if m.AllowInsufficientStorage {
			m.warnStorageOverride(operation, admissionErr)
			return nil
		}
		return admissionErr
	}
	return nil
}

func (m *Manager) instanceCreateExpansion() uint64 {
	return uint64(instanceCreateExpansionBytes)
}

func storageAdmissionError(operation string, surface storage.Surface, requirement storage.Requirement, check storage.CapacityCheck, providerType, overrideCommand string) error {
	action := "complete " + strings.ReplaceAll(operation, "-", " ")
	if operation == "instance-create" {
		action = "initialize the runner"
	}
	err := storage.CapacityAdmissionError(action, surface, requirement, check, invocation.Command("storage", "prune", "--provider", providerType))
	if overrideCommand != "" {
		return fmt.Errorf("%w\n\nContinue this invocation despite the storage risk with:\n  %s", err, overrideCommand)
	}
	return err
}

func (m *Manager) warnStorageOverride(operation string, err error) {
	m.warnf("\n*** STORAGE SAFETY OVERRIDE ACTIVE ***\n%s\nContinuing %s because --allow-insufficient-storage was explicitly supplied for this invocation.\n\n", err, strings.ReplaceAll(operation, "-", " "))
}

// PreflightProviderStorage lets provider-side controllers apply the same
// fail-closed reserve rule to an exact operation before provider side effects.
func (m *Manager) PreflightProviderStorage(operation string, peakBytes uint64) error {
	return m.preflightStorage(operation, peakBytes)
}
