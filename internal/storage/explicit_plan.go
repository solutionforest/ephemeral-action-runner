package storage

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// ExplicitRemovalPlan creates a hash-approved plan for an operator-requested reset.
// It accepts only exact, exclusively owned targets and never expands a path, prefix,
// backend, or provider selector.
func ExplicitRemovalPlan(now time.Time, artifacts []Artifact, reason string) (Plan, error) {
	if now.IsZero() {
		return Plan{}, errors.New("explicit storage plan time is required")
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return Plan{}, errors.New("explicit storage plan reason is required")
	}
	items := append([]Artifact(nil), artifacts...)
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	seen := make(map[string]struct{}, len(items))
	plan := Plan{SchemaVersion: planSchemaVersion, CreatedAt: now.UTC(), Policy: DefaultPolicy()}
	for _, artifact := range items {
		if strings.TrimSpace(artifact.ID) == "" {
			return Plan{}, errors.New("explicit storage artifact ID is required")
		}
		if _, duplicate := seen[artifact.ID]; duplicate {
			return Plan{}, fmt.Errorf("duplicate explicit storage artifact ID %q", artifact.ID)
		}
		seen[artifact.ID] = struct{}{}
		if artifact.Ownership.Kind != OwnershipExact {
			return Plan{}, fmt.Errorf("explicit storage artifact %q is not exactly owned", artifact.ID)
		}
		if err := validateExactTarget(artifact.Target); err != nil {
			return Plan{}, fmt.Errorf("explicit storage artifact %q is not exact: %w", artifact.ID, err)
		}
		plan.Decisions = append(plan.Decisions, Decision{Artifact: artifact, Action: ActionRemove, Reasons: []string{reason}})
		plan.RemovalCount++
		plan.ReclaimableBytes += artifact.SizeBytes
	}
	var err error
	plan.Hash, err = ComputePlanHash(plan)
	if err != nil {
		return Plan{}, err
	}
	return plan, nil
}
