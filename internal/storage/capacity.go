package storage

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
)

// ResolveOperationPlan binds provider-neutral role allocations to exact
// surfaces and capacity domains, then computes one peak requirement per
// domain. Allocations overlap within a phase; phases do not overlap.
func ResolveOperationPlan(plan OperationPlan, surfaces []Surface, domains []CapacityDomain) (ResolvedOperationPlan, error) {
	normalizedSurfaces, normalizedDomains, err := normalizeCapacityTopology(surfaces, domains)
	if err != nil {
		return ResolvedOperationPlan{}, err
	}
	plan, err = normalizeOperationPlan(plan)
	if err != nil {
		return ResolvedOperationPlan{}, err
	}
	bySurface := make(map[string]Surface, len(normalizedSurfaces))
	byRole := make(map[StorageRole][]Surface)
	for _, surface := range normalizedSurfaces {
		bySurface[surface.ID] = surface
		if surface.Role != "" {
			byRole[surface.Role] = append(byRole[surface.Role], surface)
		}
	}
	byDomain := make(map[string]CapacityDomain, len(normalizedDomains))
	for _, domain := range normalizedDomains {
		byDomain[domain.ID] = domain
	}

	resolved := ResolvedOperationPlan{Plan: plan}
	peakByDomain := make(map[string]uint64)
	for _, phase := range plan.Phases {
		phaseBytes := make(map[string]uint64)
		for _, allocation := range phase.Allocations {
			surface, resolveErr := resolveAllocationSurface(plan.ID, phase.ID, allocation, bySurface, byRole)
			if resolveErr != nil {
				return ResolvedOperationPlan{}, resolveErr
			}
			if _, exists := byDomain[surface.DomainID]; !exists {
				return ResolvedOperationPlan{}, fmt.Errorf("storage operation %q allocation %q references surface %q with unknown capacity domain %q", plan.ID, allocation.ID, surface.ID, surface.DomainID)
			}
			if allocation.Bytes > math.MaxUint64-phaseBytes[surface.DomainID] {
				return ResolvedOperationPlan{}, fmt.Errorf("storage operation %q phase %q capacity for domain %q overflows", plan.ID, phase.ID, surface.DomainID)
			}
			phaseBytes[surface.DomainID] += allocation.Bytes
			role := allocation.Role
			if role == "" {
				role = surface.Role
			}
			resolved.Allocations = append(resolved.Allocations, ResolvedAllocation{
				OperationID:  plan.ID,
				PhaseID:      phase.ID,
				AllocationID: allocation.ID,
				Role:         role,
				SurfaceID:    surface.ID,
				DomainID:     surface.DomainID,
				Bytes:        allocation.Bytes,
			})
		}
		for domainID, bytes := range phaseBytes {
			if current, exists := peakByDomain[domainID]; !exists || bytes > current {
				peakByDomain[domainID] = bytes
			}
		}
	}
	for domainID, peakBytes := range peakByDomain {
		if peakBytes > math.MaxUint64-plan.MinimumFreeBytes {
			return ResolvedOperationPlan{}, fmt.Errorf("storage operation %q requirement for domain %q overflows required available bytes", plan.ID, domainID)
		}
		resolved.Requirements = append(resolved.Requirements, DomainRequirement{
			OperationID:            plan.ID,
			DomainID:               domainID,
			PeakBytes:              peakBytes,
			MinimumFreeBytes:       plan.MinimumFreeBytes,
			RequiredAvailableBytes: peakBytes + plan.MinimumFreeBytes,
		})
	}
	sort.Slice(resolved.Allocations, func(i, j int) bool {
		if resolved.Allocations[i].PhaseID != resolved.Allocations[j].PhaseID {
			return resolved.Allocations[i].PhaseID < resolved.Allocations[j].PhaseID
		}
		return resolved.Allocations[i].AllocationID < resolved.Allocations[j].AllocationID
	})
	sort.Slice(resolved.Requirements, func(i, j int) bool { return resolved.Requirements[i].DomainID < resolved.Requirements[j].DomainID })
	return resolved, nil
}

// EvaluateOperationPlan resolves a plan and evaluates every physical domain
// requirement against the exact domain capacity observation.
func EvaluateOperationPlan(plan OperationPlan, surfaces []Surface, domains []CapacityDomain) (OperationEvaluation, error) {
	resolved, err := ResolveOperationPlan(plan, surfaces, domains)
	if err != nil {
		return OperationEvaluation{}, err
	}
	_, normalizedDomains, err := normalizeCapacityTopology(surfaces, domains)
	if err != nil {
		return OperationEvaluation{}, err
	}
	byDomain := make(map[string]CapacityDomain, len(normalizedDomains))
	for _, domain := range normalizedDomains {
		byDomain[domain.ID] = domain
	}
	evaluation := OperationEvaluation{ResolvedOperationPlan: resolved}
	for _, requirement := range resolved.Requirements {
		domain, exists := byDomain[requirement.DomainID]
		if !exists {
			return OperationEvaluation{}, fmt.Errorf("storage operation %q references unknown capacity domain %q", plan.ID, requirement.DomainID)
		}
		check, checkErr := evaluateDomainCapacity(domain, requirement, plan.Provider)
		if checkErr != nil {
			return OperationEvaluation{}, checkErr
		}
		evaluation.CapacityChecks = append(evaluation.CapacityChecks, check)
	}
	return evaluation, nil
}

func evaluateDomainCapacity(domain CapacityDomain, requirement DomainRequirement, provider string) (CapacityCheck, error) {
	if domain.Capacity.TotalBytes > 0 && domain.Capacity.AvailableBytes > domain.Capacity.TotalBytes {
		return CapacityCheck{}, fmt.Errorf("storage capacity domain %q reports available bytes greater than total bytes", domain.ID)
	}
	legacy := Requirement{
		ID:               requirement.OperationID + "-" + requirement.DomainID,
		Provider:         provider,
		SurfaceID:        requirement.DomainID,
		PeakBytes:        requirement.PeakBytes,
		MinimumFreeBytes: requirement.MinimumFreeBytes,
	}
	requirementCopy := requirement
	check := CapacityCheck{
		Requirement:            legacy,
		DomainRequirement:      &requirementCopy,
		Capacity:               domain.Capacity,
		RequiredAvailableBytes: requirement.RequiredAvailableBytes,
	}
	if !domain.Capacity.Known {
		check.Status = CapacityUnknown
		check.Reason = "capacity domain observation is unavailable"
		return check, nil
	}
	if domain.Capacity.AvailableBytes < requirement.RequiredAvailableBytes {
		check.Status = CapacityInsufficient
		check.DeficitBytes = requirement.RequiredAvailableBytes - domain.Capacity.AvailableBytes
		check.Reason = "available domain capacity is below phase peak plus one minimum free-space reserve"
		return check, nil
	}
	check.Status = CapacityReady
	check.Reason = "available domain capacity satisfies phase peak plus one minimum free-space reserve"
	return check, nil
}

func normalizeOperationPlan(plan OperationPlan) (OperationPlan, error) {
	if strings.TrimSpace(plan.ID) == "" {
		return OperationPlan{}, errors.New("storage operation plan ID is required")
	}
	if plan.MinimumFreeBytes == 0 {
		plan.MinimumFreeBytes = DefaultMinimumFreeBytes
	}
	if len(plan.Phases) == 0 {
		return OperationPlan{}, fmt.Errorf("storage operation plan %q requires at least one phase", plan.ID)
	}
	plan.Phases = append([]OperationPhase(nil), plan.Phases...)
	seenPhases := make(map[string]struct{}, len(plan.Phases))
	seenAllocations := make(map[string]struct{})
	for phaseIndex := range plan.Phases {
		phase := &plan.Phases[phaseIndex]
		if strings.TrimSpace(phase.ID) == "" {
			return OperationPlan{}, fmt.Errorf("storage operation plan %q phase ID is required", plan.ID)
		}
		if _, duplicate := seenPhases[phase.ID]; duplicate {
			return OperationPlan{}, fmt.Errorf("storage operation plan %q has duplicate phase ID %q", plan.ID, phase.ID)
		}
		seenPhases[phase.ID] = struct{}{}
		phase.Allocations = append([]Allocation(nil), phase.Allocations...)
		for _, allocation := range phase.Allocations {
			if strings.TrimSpace(allocation.ID) == "" {
				return OperationPlan{}, fmt.Errorf("storage operation plan %q phase %q allocation ID is required", plan.ID, phase.ID)
			}
			if _, duplicate := seenAllocations[allocation.ID]; duplicate {
				return OperationPlan{}, fmt.Errorf("storage operation plan %q has duplicate allocation ID %q", plan.ID, allocation.ID)
			}
			seenAllocations[allocation.ID] = struct{}{}
			if allocation.Role == "" && strings.TrimSpace(allocation.SurfaceID) == "" {
				return OperationPlan{}, fmt.Errorf("storage operation plan %q allocation %q requires a role or surface ID", plan.ID, allocation.ID)
			}
		}
		sort.Slice(phase.Allocations, func(i, j int) bool { return phase.Allocations[i].ID < phase.Allocations[j].ID })
	}
	sort.Slice(plan.Phases, func(i, j int) bool { return plan.Phases[i].ID < plan.Phases[j].ID })
	return plan, nil
}

func resolveAllocationSurface(operationID, phaseID string, allocation Allocation, bySurface map[string]Surface, byRole map[StorageRole][]Surface) (Surface, error) {
	if allocation.SurfaceID != "" {
		surface, exists := bySurface[allocation.SurfaceID]
		if !exists {
			return Surface{}, fmt.Errorf("storage operation %q allocation %q references unknown surface %q", operationID, allocation.ID, allocation.SurfaceID)
		}
		if allocation.Role != "" && surface.Role != "" && allocation.Role != surface.Role {
			return Surface{}, fmt.Errorf("storage operation %q allocation %q role %q does not match surface %q role %q", operationID, allocation.ID, allocation.Role, surface.ID, surface.Role)
		}
		return surface, nil
	}
	matches := byRole[allocation.Role]
	switch len(matches) {
	case 0:
		return Surface{}, fmt.Errorf("storage operation %q phase %q allocation %q references unknown storage role %q", operationID, phaseID, allocation.ID, allocation.Role)
	case 1:
		return matches[0], nil
	default:
		ids := make([]string, len(matches))
		for index, match := range matches {
			ids[index] = match.ID
		}
		sort.Strings(ids)
		return Surface{}, fmt.Errorf("storage operation %q allocation %q role %q is ambiguous across surfaces %s", operationID, allocation.ID, allocation.Role, strings.Join(ids, ", "))
	}
}

func normalizeCapacityTopology(surfaces []Surface, domains []CapacityDomain) ([]Surface, []CapacityDomain, error) {
	normalizedSurfaces := append([]Surface(nil), surfaces...)
	normalizedDomains := append([]CapacityDomain(nil), domains...)
	domainByID := make(map[string]int, len(normalizedDomains))
	identityToID := make(map[string]string, len(normalizedDomains))
	for index := range normalizedDomains {
		domain := &normalizedDomains[index]
		if strings.TrimSpace(domain.ID) == "" {
			return nil, nil, errors.New("storage capacity domain ID is required")
		}
		if !validSurfaceKind(domain.Kind) {
			return nil, nil, fmt.Errorf("storage capacity domain %q has invalid kind %q", domain.ID, domain.Kind)
		}
		if _, duplicate := domainByID[domain.ID]; duplicate {
			return nil, nil, fmt.Errorf("duplicate storage capacity domain ID %q", domain.ID)
		}
		if domain.Identity != "" {
			if existing, duplicate := identityToID[domain.Identity]; duplicate {
				return nil, nil, fmt.Errorf("storage capacity domain identity %q is duplicated by %q and %q", domain.Identity, existing, domain.ID)
			}
			identityToID[domain.Identity] = domain.ID
		}
		domain.Capacity.ObservedAt = domain.Capacity.ObservedAt.UTC()
		domainByID[domain.ID] = index
	}
	surfaceIDs := make(map[string]struct{}, len(normalizedSurfaces))
	for index := range normalizedSurfaces {
		surface := &normalizedSurfaces[index]
		if strings.TrimSpace(surface.ID) == "" {
			return nil, nil, errors.New("storage surface ID is required")
		}
		if !validSurfaceKind(surface.Kind) {
			return nil, nil, fmt.Errorf("storage surface %q has invalid kind %q", surface.ID, surface.Kind)
		}
		if _, duplicate := surfaceIDs[surface.ID]; duplicate {
			return nil, nil, fmt.Errorf("duplicate storage surface ID %q", surface.ID)
		}
		surfaceIDs[surface.ID] = struct{}{}
		if surface.Path == "" {
			surface.Path = surface.Location
		}
		if surface.Location == "" {
			surface.Location = surface.Path
		}
		surface.Capacity.ObservedAt = surface.Capacity.ObservedAt.UTC()
		if surface.DomainID != "" {
			if _, exists := domainByID[surface.DomainID]; !exists {
				return nil, nil, fmt.Errorf("storage surface %q references unknown capacity domain %q", surface.ID, surface.DomainID)
			}
			continue
		}
		surface.DomainID = surface.ID
		if _, exists := domainByID[surface.DomainID]; exists {
			continue
		}
		normalizedDomains = append(normalizedDomains, CapacityDomain{
			ID:         surface.DomainID,
			Kind:       surface.Kind,
			Path:       surface.Path,
			Provenance: surface.Provenance,
			Confidence: surface.Confidence,
			Capacity:   surface.Capacity,
		})
		domainByID[surface.DomainID] = len(normalizedDomains) - 1
	}
	sort.Slice(normalizedSurfaces, func(i, j int) bool { return normalizedSurfaces[i].ID < normalizedSurfaces[j].ID })
	sort.Slice(normalizedDomains, func(i, j int) bool { return normalizedDomains[i].ID < normalizedDomains[j].ID })
	return normalizedSurfaces, normalizedDomains, nil
}

func validSurfaceKind(kind SurfaceKind) bool {
	switch kind {
	case SurfaceHostFilesystem, SurfaceDockerEngine, SurfaceSandboxCache, SurfaceExternal:
		return true
	default:
		return false
	}
}

// EvaluateCapacity evaluates one requirement against one surface observation.
func EvaluateCapacity(surface Surface, requirement Requirement) (CapacityCheck, error) {
	if strings.TrimSpace(surface.ID) == "" {
		return CapacityCheck{}, errors.New("storage surface ID is required")
	}
	if strings.TrimSpace(requirement.ID) == "" {
		return CapacityCheck{}, errors.New("storage requirement ID is required")
	}
	if requirement.SurfaceID != surface.ID {
		return CapacityCheck{}, fmt.Errorf("storage requirement %q targets surface %q, not %q", requirement.ID, requirement.SurfaceID, surface.ID)
	}
	if requirement.MinimumFreeBytes == 0 {
		requirement.MinimumFreeBytes = DefaultMinimumFreeBytes
	}
	if requirement.PeakBytes > math.MaxUint64-requirement.MinimumFreeBytes {
		return CapacityCheck{}, fmt.Errorf("storage requirement %q overflows required available bytes", requirement.ID)
	}
	required := requirement.PeakBytes + requirement.MinimumFreeBytes
	check := CapacityCheck{
		Requirement:            requirement,
		Capacity:               surface.Capacity,
		RequiredAvailableBytes: required,
	}
	if !surface.Capacity.Known {
		check.Status = CapacityUnknown
		check.Reason = "capacity observation is unavailable"
		return check, nil
	}
	if surface.Capacity.TotalBytes > 0 && surface.Capacity.AvailableBytes > surface.Capacity.TotalBytes {
		return CapacityCheck{}, fmt.Errorf("storage surface %q reports available bytes greater than total bytes", surface.ID)
	}
	if surface.Capacity.AvailableBytes < required {
		check.Status = CapacityInsufficient
		check.DeficitBytes = required - surface.Capacity.AvailableBytes
		check.Reason = "available capacity is below peak bytes plus minimum free bytes"
		return check, nil
	}
	check.Status = CapacityReady
	check.Reason = "available capacity satisfies peak bytes plus minimum free bytes"
	return check, nil
}

func capacityPreflight(surfaces []Surface, requirements []Requirement) ([]CapacityCheck, error) {
	byID := make(map[string]Surface, len(surfaces))
	for _, surface := range surfaces {
		if strings.TrimSpace(surface.ID) == "" {
			return nil, errors.New("storage surface ID is required")
		}
		switch surface.Kind {
		case SurfaceHostFilesystem, SurfaceDockerEngine, SurfaceSandboxCache, SurfaceExternal:
		default:
			return nil, fmt.Errorf("storage surface %q has invalid kind %q", surface.ID, surface.Kind)
		}
		if _, exists := byID[surface.ID]; exists {
			return nil, fmt.Errorf("duplicate storage surface ID %q", surface.ID)
		}
		byID[surface.ID] = surface
	}
	seenRequirements := make(map[string]struct{}, len(requirements))
	checks := make([]CapacityCheck, 0, len(requirements))
	for _, requirement := range requirements {
		if _, exists := seenRequirements[requirement.ID]; exists {
			return nil, fmt.Errorf("duplicate storage requirement ID %q", requirement.ID)
		}
		seenRequirements[requirement.ID] = struct{}{}
		surface, exists := byID[requirement.SurfaceID]
		if !exists {
			return nil, fmt.Errorf("storage requirement %q references unknown surface %q", requirement.ID, requirement.SurfaceID)
		}
		check, err := EvaluateCapacity(surface, requirement)
		if err != nil {
			return nil, err
		}
		checks = append(checks, check)
	}
	sort.Slice(checks, func(i, j int) bool { return checks[i].Requirement.ID < checks[j].Requirement.ID })
	return checks, nil
}
