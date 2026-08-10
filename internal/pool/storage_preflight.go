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

const instanceCreateExpansionBytes = 10 * storage.GiB

func (m *Manager) preflightStorage(plan storage.OperationPlan) error {
	return m.preflightStorageAttempt(plan, false)
}

func (m *Manager) preflightStorageAttempt(plan storage.OperationPlan, housekeepingRetried bool) error {
	operation := plan.ID
	if !housekeepingRetried && m.AutomaticImageLifecycle && operation == "instance-create" {
		pending, pendingErr := m.imageCoordinator().StorageCleanupPending()
		if pendingErr != nil {
			m.warnf("EPAR cleanup-pending check before %s was deferred: %v\n", operation, pendingErr)
		} else if pending {
			if cleanupErr := m.imageCoordinator().HousekeepStorage(context.Background()); cleanupErr != nil {
				m.warnf("EPAR cleanup-pending retry before %s was deferred: %v\n", operation, cleanupErr)
			}
			housekeepingRetried = true
		}
	}
	minimumFree, err := config.EffectiveMinimumFreeBytes(m.Config)
	if err != nil {
		return err
	}
	plan.MinimumFreeBytes = minimumFree
	if plan.Provider == "" || plan.Provider == "shared" {
		plan.Provider = m.Config.Provider.Type
	}
	projectPlan := plan
	projectPlan.Phases = append([]storage.OperationPhase(nil), plan.Phases...)
	for phaseIndex := range projectPlan.Phases {
		projectPlan.Phases[phaseIndex].Allocations = append([]storage.Allocation(nil), projectPlan.Phases[phaseIndex].Allocations...)
		for allocationIndex := range projectPlan.Phases[phaseIndex].Allocations {
			projectPlan.Phases[phaseIndex].Allocations[allocationIndex].Role = ""
			projectPlan.Phases[phaseIndex].Allocations[allocationIndex].SurfaceID = "project"
		}
	}
	if m.Storage != nil {
		snapshot, err := m.Storage.StorageSnapshot(context.Background(), provider.StorageRequest{OperationPlan: plan, Now: m.currentTime()})
		if err != nil {
			return fmt.Errorf("resolve provider storage topology before %s: %w\n\nInspect the same operation with:\n  %s", operation, err, m.storageStatusCommand(operation))
		}
		m.warnStorageDiscovery(snapshot.Warnings)
		if len(snapshot.Surfaces) == 0 || len(snapshot.Domains) == 0 {
			return fmt.Errorf("provider %q returned no measurable storage capacity domains for %s", m.Config.Provider.Type, operation)
		}
		evaluation, err := storage.EvaluateOperationPlan(plan, snapshot.Surfaces, snapshot.Domains)
		if err != nil {
			return fmt.Errorf("resolve storage operation %s: %w", operation, err)
		}
		for _, check := range evaluation.CapacityChecks {
			switch check.Status {
			case storage.CapacityReady:
				continue
			case storage.CapacityUnknown:
				m.warnUnknownStorage(operation, check, evaluation.Allocations)
				continue
			case storage.CapacityInsufficient:
			default:
				return fmt.Errorf("storage capacity check for %s returned unsupported status %q", operation, check.Status)
			}
			if !housekeepingRetried && m.AutomaticImageLifecycle && operation == "instance-create" {
				if cleanupErr := m.imageCoordinator().HousekeepStorage(context.Background()); cleanupErr != nil {
					m.warnf("EPAR storage housekeeping retry before %s was deferred: %v\n", operation, cleanupErr)
				}
				return m.preflightStorageAttempt(plan, true)
			}
			admissionErr := operationAdmissionError(operation, snapshot.Domains, check, m.Config.Provider.Type, m.storagePruneCommand(), m.StorageOverrideCommand)
			if m.AllowInsufficientStorage {
				m.warnStorageOverride(operation, admissionErr)
				continue
			}
			return admissionErr
		}
		return nil
	}

	domain, err := storage.ProbeFilesystemCapacityDomain(m.ProjectRoot, m.currentTime())
	if err != nil {
		unknownDomain := storage.CapacityDomain{ID: "project-domain", Kind: storage.SurfaceHostFilesystem, Path: m.ProjectRoot, CapacityUnavailableReason: err.Error(), Capacity: storage.Capacity{ObservedAt: m.currentTime()}}
		unknownSurface := storage.Surface{ID: "project", Provider: m.Config.Provider.Type, Role: storage.StorageRoleProject, Kind: storage.SurfaceHostFilesystem, DomainID: unknownDomain.ID, Path: m.ProjectRoot, Location: m.ProjectRoot, Classification: "physical", AdmissionAuthoritative: true, Capacity: unknownDomain.Capacity}
		evaluation, evaluationErr := storage.EvaluateOperationPlan(projectPlan, []storage.Surface{unknownSurface}, []storage.CapacityDomain{unknownDomain})
		if evaluationErr != nil {
			return fmt.Errorf("resolve project storage operation %s after capacity probe failure: %w", operation, evaluationErr)
		}
		for _, check := range evaluation.CapacityChecks {
			if check.Status != storage.CapacityUnknown {
				return fmt.Errorf("project storage capacity check for %s returned unexpected status %q after probe failure", operation, check.Status)
			}
			m.warnUnknownStorage(operation, check, evaluation.Allocations)
		}
		return nil
	}
	domain.ID = "project-domain"
	surface := storage.Surface{
		ID:                     "project",
		Provider:               m.Config.Provider.Type,
		Role:                   storage.StorageRoleProject,
		Kind:                   storage.SurfaceHostFilesystem,
		DomainID:               domain.ID,
		Path:                   m.ProjectRoot,
		Location:               m.ProjectRoot,
		Classification:         "physical",
		Provenance:             domain.Provenance,
		Confidence:             domain.Confidence,
		AdmissionAuthoritative: true,
		Capacity:               domain.Capacity,
	}
	evaluation, err := storage.EvaluateOperationPlan(projectPlan, []storage.Surface{surface}, []storage.CapacityDomain{domain})
	if err != nil {
		return fmt.Errorf("resolve project storage operation %s: %w", operation, err)
	}
	for _, check := range evaluation.CapacityChecks {
		switch check.Status {
		case storage.CapacityReady:
			continue
		case storage.CapacityUnknown:
			m.warnUnknownStorage(operation, check, evaluation.Allocations)
			continue
		case storage.CapacityInsufficient:
		default:
			return fmt.Errorf("storage capacity check for %s returned unsupported status %q", operation, check.Status)
		}
		if !housekeepingRetried && m.AutomaticImageLifecycle && operation == "instance-create" {
			if cleanupErr := m.imageCoordinator().HousekeepStorage(context.Background()); cleanupErr != nil {
				m.warnf("EPAR storage housekeeping retry before %s was deferred: %v\n", operation, cleanupErr)
			}
			return m.preflightStorageAttempt(plan, true)
		}
		admissionErr := operationAdmissionError(operation, []storage.CapacityDomain{domain}, check, m.Config.Provider.Type, m.storagePruneCommand(), m.StorageOverrideCommand)
		if m.AllowInsufficientStorage {
			m.warnStorageOverride(operation, admissionErr)
			return nil
		}
		return admissionErr
	}
	return nil
}

func (m *Manager) instanceCreateOperationPlan() storage.OperationPlan {
	role := storage.StorageRoleProject
	switch m.Config.Provider.Type {
	case "docker-container":
		role = storage.StorageRoleDockerEngine
	case "docker-sandboxes":
		role = storage.StorageRoleSandboxRuntime
	case "wsl":
		role = storage.StorageRoleWSLDistribution
	case "tart":
		role = storage.StorageRoleTartStore
	}
	return storage.OperationPlan{
		ID:       "instance-create",
		Provider: m.Config.Provider.Type,
		Phases: []storage.OperationPhase{{
			ID: "instance-create",
			Allocations: []storage.Allocation{{
				ID: "provider-runtime-instance", Role: role, Bytes: instanceCreateExpansionBytes,
			}},
		}},
	}
}

func (m *Manager) storageStatusCommand(operation string) string {
	return invocation.ScopedCommand(m.ConfigPath, m.ProjectRoot, "storage", "status", "--operation", operation, "--provider", m.Config.Provider.Type)
}

func (m *Manager) storagePruneCommand() string {
	return invocation.ScopedCommand(m.ConfigPath, m.ProjectRoot, "storage", "prune", "--provider", m.Config.Provider.Type)
}

func operationAdmissionError(operation string, domains []storage.CapacityDomain, check storage.CapacityCheck, providerType, pruneCommand, overrideCommand string) error {
	if check.DomainRequirement == nil {
		return fmt.Errorf("storage capacity check for %s did not identify a capacity domain", operation)
	}
	var domain storage.CapacityDomain
	for _, candidate := range domains {
		if candidate.ID == check.DomainRequirement.DomainID {
			domain = candidate
			break
		}
	}
	if domain.ID == "" {
		return fmt.Errorf("storage capacity check for %s references unknown domain %q", operation, check.DomainRequirement.DomainID)
	}
	action := "complete " + strings.ReplaceAll(operation, "-", " ")
	if operation == "instance-create" {
		action = "initialize the runner"
	}
	surface := storage.Surface{ID: domain.ID, Provider: providerType, Kind: domain.Kind, Location: domain.Path, Path: domain.Path, DomainID: domain.ID, Provenance: domain.Provenance, Confidence: domain.Confidence, Capacity: domain.Capacity}
	requirement := storage.Requirement{ID: check.DomainRequirement.OperationID + "-" + domain.ID, Provider: providerType, SurfaceID: domain.ID, PeakBytes: check.DomainRequirement.PeakBytes, MinimumFreeBytes: check.DomainRequirement.MinimumFreeBytes}
	err := storage.CapacityAdmissionError(action, surface, requirement, check, pruneCommand)
	if overrideCommand != "" {
		return fmt.Errorf("%w\n\nContinue this invocation despite the storage risk with:\n  %s", err, overrideCommand)
	}
	return err
}

func (m *Manager) warnStorageDiscovery(warnings []string) {
	for _, warning := range warnings {
		m.warnf("\n*** STORAGE DISCOVERY WARNING ***\n%s\nCapacity evidence does not grant EPAR cleanup authority.\n\n", warning)
	}
}

func (m *Manager) warnStorageOverride(operation string, err error) {
	m.warnf("\n*** STORAGE SAFETY OVERRIDE ACTIVE ***\n%s\nContinuing %s because --allow-insufficient-storage was explicitly supplied for this invocation.\n\n", err, strings.ReplaceAll(operation, "-", " "))
}

func (m *Manager) warnUnknownStorage(operation string, check storage.CapacityCheck, allocations []storage.ResolvedAllocation) {
	domainID := check.Requirement.SurfaceID
	if check.DomainRequirement != nil {
		domainID = check.DomainRequirement.DomainID
	}
	roles := make([]string, 0)
	seen := make(map[storage.StorageRole]struct{})
	for _, allocation := range allocations {
		if allocation.DomainID != domainID || allocation.Role == "" {
			continue
		}
		if _, exists := seen[allocation.Role]; exists {
			continue
		}
		seen[allocation.Role] = struct{}{}
		roles = append(roles, string(allocation.Role))
	}
	if len(roles) == 0 {
		roles = append(roles, "unknown")
	}
	estimatedBytes := check.Requirement.PeakBytes
	if check.DomainRequirement != nil {
		estimatedBytes = check.DomainRequirement.PeakBytes
	}
	m.warnf("\n*** STORAGE CAPACITY UNKNOWN ***\noperation=%s domain=%s roles=%s estimatedBytes=%d requiredBytes=%d reason=%s\nEPAR will continue because capacity could not be measured. Confirmed insufficient capacity remains enforced.\n\nInspect the same operation with:\n  %s\n\n", operation, domainID, strings.Join(roles, ","), estimatedBytes, check.RequiredAvailableBytes, check.Reason, m.storageStatusCommand(operation))
}

// PreflightProviderStorage lets provider-side controllers apply the same
// domain evaluator to an exact operation before provider side effects.
func (m *Manager) PreflightProviderStorage(plan storage.OperationPlan) error {
	return m.preflightStorage(plan)
}
