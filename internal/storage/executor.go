package storage

import (
	"context"
	"errors"
	"fmt"
)

// Observation is the exact target state returned by an executor.
type Observation struct {
	Exists bool   `json:"exists"`
	Target Target `json:"target"`
}

// Removal contains exactly one immutable approved target. An implementation of
// RemoveExact must condition removal on Target.Identity and Target.Fingerprint
// and must fail closed when the locator resolves to a different object.
type Removal struct {
	ArtifactID string `json:"artifactId"`
	Target     Target `json:"target"`
	SizeBytes  uint64 `json:"sizeBytes"`
}

// ExactExecutor is deliberately unable to receive a prefix, glob, surface, or
// unbounded selector. Implementations must provide conditional exact removal
// and exact post-removal observation.
type ExactExecutor interface {
	ObserveExact(context.Context, Target) (Observation, error)
	RemoveExact(context.Context, Removal) error
}

// ExecutionStatus describes an attempted exact removal.
type ExecutionStatus string

const (
	ExecutionRemoved ExecutionStatus = "removed"
	ExecutionDrifted ExecutionStatus = "drifted"
	ExecutionFailed  ExecutionStatus = "failed"
)

// ExecutionEntry records one exact target outcome.
type ExecutionEntry struct {
	Removal Removal         `json:"removal"`
	Status  ExecutionStatus `json:"status"`
	Error   string          `json:"error,omitempty"`
}

// ExecutionReport is a partial-completion-safe journal returned in plan order.
type ExecutionReport struct {
	PlanHash       string           `json:"planHash"`
	Entries        []ExecutionEntry `json:"entries"`
	RemovedCount   int              `json:"removedCount"`
	ReclaimedBytes uint64           `json:"reclaimedBytes"`
}

// Execute applies only ActionRemove entries from a hash-approved plan. It
// re-observes identity before removal and verifies exact absence afterward.
// Execution stops on the first drift or error and returns the partial journal.
func Execute(ctx context.Context, plan Plan, approvedHash string, executor ExactExecutor) (ExecutionReport, error) {
	if executor == nil {
		return ExecutionReport{}, errors.New("exact storage executor is required")
	}
	if err := ValidatePlanHash(plan, approvedHash); err != nil {
		return ExecutionReport{}, err
	}
	report := ExecutionReport{PlanHash: plan.Hash}
	for _, decision := range plan.Decisions {
		if decision.Action != ActionRemove {
			continue
		}
		removal := Removal{ArtifactID: decision.Artifact.ID, Target: decision.Artifact.Target, SizeBytes: decision.Artifact.SizeBytes}
		if err := validateExactTarget(removal.Target); err != nil {
			return report, fmt.Errorf("planned removal %q is not exact: %w", removal.ArtifactID, err)
		}
		observation, err := executor.ObserveExact(ctx, removal.Target)
		if err != nil {
			entry := ExecutionEntry{Removal: removal, Status: ExecutionFailed, Error: err.Error()}
			report.Entries = append(report.Entries, entry)
			return report, fmt.Errorf("observe exact storage target %q: %w", removal.ArtifactID, err)
		}
		if !observation.Exists || observation.Target != removal.Target {
			entry := ExecutionEntry{Removal: removal, Status: ExecutionDrifted, Error: "exact target identity changed or disappeared"}
			report.Entries = append(report.Entries, entry)
			return report, fmt.Errorf("exact storage target %q drifted", removal.ArtifactID)
		}
		if err := executor.RemoveExact(ctx, removal); err != nil {
			entry := ExecutionEntry{Removal: removal, Status: ExecutionFailed, Error: err.Error()}
			report.Entries = append(report.Entries, entry)
			return report, fmt.Errorf("remove exact storage target %q: %w", removal.ArtifactID, err)
		}
		after, err := executor.ObserveExact(ctx, removal.Target)
		if err != nil {
			entry := ExecutionEntry{Removal: removal, Status: ExecutionFailed, Error: err.Error()}
			report.Entries = append(report.Entries, entry)
			return report, fmt.Errorf("verify exact storage target %q absence: %w", removal.ArtifactID, err)
		}
		if after.Exists {
			entry := ExecutionEntry{Removal: removal, Status: ExecutionFailed, Error: "exact target still exists after removal"}
			report.Entries = append(report.Entries, entry)
			return report, fmt.Errorf("exact storage target %q still exists after removal", removal.ArtifactID)
		}
		report.Entries = append(report.Entries, ExecutionEntry{Removal: removal, Status: ExecutionRemoved})
		report.RemovedCount++
		report.ReclaimedBytes += removal.SizeBytes
	}
	return report, nil
}

func validateExactTarget(target Target) error {
	if target.Match != MatchExact {
		return fmt.Errorf("target match is %q", target.Match)
	}
	if target.Kind == "" || target.Locator == "" || target.Identity == "" {
		return errors.New("target kind, locator, and identity are required")
	}
	return nil
}
