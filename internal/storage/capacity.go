package storage

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
)

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
