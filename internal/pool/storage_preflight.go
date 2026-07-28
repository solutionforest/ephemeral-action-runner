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
	imagePullExpansionBytes      = 20 * storage.GiB
	imageBuildExpansionBytes     = 30 * storage.GiB
	sourceUpdateExpansionBytes   = 5 * storage.GiB
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
			return fmt.Errorf("provider storage surface cannot be measured before %s: %w\n\nInspect storage with:\n  %s", operation, err, invocation.Command("storage", "status", "--provider", m.Config.Provider.Type))
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
				return storageAdmissionError(operation, surface, requirement, check, m.Config.Provider.Type)
			}
		}
		return nil
	}
	capacity, err := storage.ProbeFilesystemCapacity(m.ProjectRoot, m.currentTime())
	if err != nil {
		return fmt.Errorf("storage surface %q cannot be measured before %s: %w\n\nInspect storage with:\n  %s", m.ProjectRoot, operation, err, invocation.Command("storage", "status", "--provider", m.Config.Provider.Type))
	}
	surface := storage.Surface{
		ID:       "project",
		Provider: m.Config.Provider.Type,
		Kind:     storage.SurfaceHostFilesystem,
		Location: m.ProjectRoot,
		Capacity: capacity,
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
		return storageAdmissionError(operation, surface, requirement, check, m.Config.Provider.Type)
	}
	return nil
}

func (m *Manager) instanceCreateExpansion() uint64 {
	return uint64(instanceCreateExpansionBytes)
}

func storageAdmissionError(operation string, surface storage.Surface, requirement storage.Requirement, check storage.CapacityCheck, providerType string) error {
	action := "complete " + strings.ReplaceAll(operation, "-", " ")
	if operation == "instance-create" {
		action = "initialize the runner"
	}
	return storage.CapacityAdmissionError(action, surface, requirement, check, invocation.Command("storage", "prune", "--provider", providerType))
}

// PreflightProviderStorage lets provider-side controllers apply the same
// fail-closed reserve rule to an exact operation before provider side effects.
func (m *Manager) PreflightProviderStorage(operation string, peakBytes uint64) error {
	return m.preflightStorage(operation, peakBytes)
}
