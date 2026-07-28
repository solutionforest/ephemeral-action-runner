package storage

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

const planSchemaVersion = 1

var automaticKinds = map[ArtifactKind]bool{
	ArtifactNativeControllerRevision: true,
	ArtifactTemplateArchive:          true,
}

// Preview deterministically classifies a complete storage snapshot. It never
// mutates the snapshot or any host resource.
func Preview(request PreviewRequest) (Plan, error) {
	now := request.Now.UTC()
	if request.Now.IsZero() {
		return Plan{}, errors.New("storage preview time is required")
	}
	policy, err := normalizePolicy(request.Policy)
	if err != nil {
		return Plan{}, err
	}
	surfaces := append([]Surface(nil), request.Surfaces...)
	for index := range surfaces {
		surfaces[index].Capacity.ObservedAt = surfaces[index].Capacity.ObservedAt.UTC()
	}
	sort.Slice(surfaces, func(i, j int) bool { return surfaces[i].ID < surfaces[j].ID })
	checks, err := capacityPreflight(surfaces, request.Requirements)
	if err != nil {
		return Plan{}, err
	}
	artifacts := append([]Artifact(nil), request.Artifacts...)
	if err := normalizeAndValidateArtifacts(artifacts, surfaces); err != nil {
		return Plan{}, err
	}
	sort.Slice(artifacts, func(i, j int) bool { return artifacts[i].ID < artifacts[j].ID })

	decisions := make([]Decision, len(artifacts))
	candidates := make(map[string]time.Time)
	for index, artifact := range artifacts {
		decision, candidateAt := classifyArtifact(now, policy, artifact)
		decisions[index] = decision
		if candidateAt != nil {
			candidates[artifact.ID] = *candidateAt
		}
	}
	applyGenerationCount(policy, decisions, candidates)
	applyCacheBudgets(policy, decisions, candidates)

	plan := Plan{
		SchemaVersion:  planSchemaVersion,
		CreatedAt:      now,
		Policy:         policy,
		Surfaces:       surfaces,
		CapacityChecks: checks,
		Decisions:      decisions,
	}
	for _, check := range checks {
		if check.Status != CapacityReady {
			plan.Warnings = append(plan.Warnings, fmt.Sprintf("capacity %s: requirement=%s surface=%s reason=%s", check.Status, check.Requirement.ID, check.Requirement.SurfaceID, check.Reason))
		}
	}
	for _, decision := range decisions {
		if decision.Action == ActionRemove {
			plan.RemovalCount++
			plan.ReclaimableBytes += decision.Artifact.SizeBytes
		}
	}
	plan.Hash, err = ComputePlanHash(plan)
	if err != nil {
		return Plan{}, err
	}
	return plan, nil
}

func normalizePolicy(policy Policy) (Policy, error) {
	if policy.GracePeriod == 0 && policy.KeepPrevious == 0 && len(policy.Budgets) == 0 {
		policy = DefaultPolicy()
	}
	if policy.GracePeriod <= 0 {
		return Policy{}, errors.New("storage retention grace period must be positive")
	}
	if policy.KeepPrevious < 0 {
		return Policy{}, errors.New("storage keepPrevious must not be negative")
	}
	policy.Budgets = append([]Budget(nil), policy.Budgets...)
	sort.Slice(policy.Budgets, func(i, j int) bool { return policy.Budgets[i].Kind < policy.Budgets[j].Kind })
	seen := make(map[ArtifactKind]struct{}, len(policy.Budgets))
	for _, budget := range policy.Budgets {
		if budget.Kind != ArtifactBuildKitCache && budget.Kind != ArtifactGoCache {
			return Policy{}, fmt.Errorf("storage budget for unsupported automatic artifact kind %q", budget.Kind)
		}
		if budget.MaxBytes == 0 {
			return Policy{}, fmt.Errorf("storage budget for %q must be positive", budget.Kind)
		}
		if _, exists := seen[budget.Kind]; exists {
			return Policy{}, fmt.Errorf("duplicate storage budget for %q", budget.Kind)
		}
		seen[budget.Kind] = struct{}{}
	}
	return policy, nil
}

func normalizeAndValidateArtifacts(artifacts []Artifact, surfaces []Surface) error {
	surfaceIDs := make(map[string]struct{}, len(surfaces))
	for _, surface := range surfaces {
		surfaceIDs[surface.ID] = struct{}{}
	}
	ids := make(map[string]struct{}, len(artifacts))
	for index := range artifacts {
		artifact := &artifacts[index]
		artifact.CreatedAt = artifact.CreatedAt.UTC()
		artifact.LastUsedAt = artifact.LastUsedAt.UTC()
		if artifact.SupersededAt != nil {
			supersededAt := artifact.SupersededAt.UTC()
			artifact.SupersededAt = &supersededAt
		}
		if artifact.Lease != nil {
			lease := *artifact.Lease
			lease.ExpiresAt = lease.ExpiresAt.UTC()
			artifact.Lease = &lease
		}
		artifact.Protections = append([]Protection(nil), artifact.Protections...)
		if strings.TrimSpace(artifact.ID) == "" {
			return errors.New("storage artifact ID is required")
		}
		if _, exists := ids[artifact.ID]; exists {
			return fmt.Errorf("duplicate storage artifact ID %q", artifact.ID)
		}
		ids[artifact.ID] = struct{}{}
		if _, exists := surfaceIDs[artifact.SurfaceID]; !exists {
			return fmt.Errorf("storage artifact %q references unknown surface %q", artifact.ID, artifact.SurfaceID)
		}
		if artifact.Kind == "" {
			return fmt.Errorf("storage artifact %q kind is required", artifact.ID)
		}
		if artifact.Target.Kind == "" || strings.TrimSpace(artifact.Target.Locator) == "" {
			return fmt.Errorf("storage artifact %q target kind and locator are required", artifact.ID)
		}
		if artifact.Target.Match == MatchExact && strings.TrimSpace(artifact.Target.Identity) == "" {
			return fmt.Errorf("storage artifact %q exact target identity is required", artifact.ID)
		}
		if artifact.Target.Match != MatchExact && artifact.Target.Match != MatchPrefix && artifact.Target.Match != MatchUnknown {
			return fmt.Errorf("storage artifact %q has invalid target match %q", artifact.ID, artifact.Target.Match)
		}
		if artifact.Ownership.Kind != OwnershipExact && artifact.Ownership.Kind != OwnershipShared && artifact.Ownership.Kind != OwnershipUnknown {
			return fmt.Errorf("storage artifact %q has invalid ownership %q", artifact.ID, artifact.Ownership.Kind)
		}
		if artifact.Ownership.Kind == OwnershipExact && (strings.TrimSpace(artifact.Ownership.OwnerID) == "" || strings.TrimSpace(artifact.Ownership.Evidence) == "") {
			return fmt.Errorf("storage artifact %q exact ownership requires owner ID and evidence", artifact.ID)
		}
		sort.Slice(artifact.Protections, func(i, j int) bool {
			if artifact.Protections[i].Kind == artifact.Protections[j].Kind {
				return artifact.Protections[i].Detail < artifact.Protections[j].Detail
			}
			return artifact.Protections[i].Kind < artifact.Protections[j].Kind
		})
	}
	return nil
}

func classifyArtifact(now time.Time, policy Policy, artifact Artifact) (Decision, *time.Time) {
	decision := Decision{Artifact: artifact}
	add := func(action Action, reasons ...string) (Decision, *time.Time) {
		decision.Action = action
		decision.Reasons = append(decision.Reasons, reasons...)
		return decision, nil
	}
	if artifact.Current {
		return add(ActionProtected, "current")
	}
	if artifact.Active {
		return add(ActionProtected, "active")
	}
	if len(artifact.Protections) > 0 {
		for _, protection := range artifact.Protections {
			decision.Reasons = append(decision.Reasons, "protected:"+string(protection.Kind))
		}
		decision.Action = ActionProtected
		return decision, nil
	}
	if artifact.Lease != nil {
		if strings.TrimSpace(artifact.Lease.ID) == "" || artifact.Lease.ExpiresAt.IsZero() {
			return add(ActionProtected, "lease-uncertain")
		}
		if !artifact.Lease.ExpiresAt.UTC().Before(now) {
			return add(ActionProtected, "lease-active")
		}
	}
	if artifact.Ownership.Kind != OwnershipExact {
		return add(ActionReportOnly, "ownership-"+string(artifact.Ownership.Kind))
	}
	if artifact.Target.Match != MatchExact {
		return add(ActionReportOnly, "target-"+string(artifact.Target.Match))
	}
	if !automaticKinds[artifact.Kind] {
		return add(ActionReportOnly, "explicit-cleanup-only")
	}
	if !automaticTargetKind(artifact.Kind, artifact.Target.Kind) {
		return add(ActionReportOnly, "target-kind-not-automatic")
	}

	var candidateAt time.Time
	switch artifact.Kind {
	case ArtifactNativeControllerRevision, ArtifactTemplateArchive:
		if artifact.RetentionGroup == "" || artifact.SupersededAt == nil || artifact.SupersededAt.IsZero() {
			return add(ActionReportOnly, "superseded-state-uncertain")
		}
		candidateAt = artifact.SupersededAt.UTC()
	default:
		return add(ActionReportOnly, "explicit-cleanup-only")
	}
	if candidateAt.After(now) {
		return add(ActionReportOnly, "retention-clock-skew")
	}
	if now.Sub(candidateAt) < policy.GracePeriod {
		return add(ActionKeep, "within-grace-period")
	}
	decision.Action = ActionKeep
	decision.Reasons = []string{"eligible"}
	return decision, &candidateAt
}

func automaticTargetKind(artifactKind ArtifactKind, targetKind TargetKind) bool {
	switch artifactKind {
	case ArtifactNativeControllerRevision:
		return targetKind == TargetDirectory || targetKind == TargetFile
	case ArtifactTemplateArchive:
		return targetKind == TargetFile
	default:
		return false
	}
}

func applyGenerationCount(policy Policy, decisions []Decision, candidates map[string]time.Time) {
	groups := make(map[string][]int)
	for index, decision := range decisions {
		if _, candidate := candidates[decision.Artifact.ID]; !candidate {
			continue
		}
		switch decision.Artifact.Kind {
		case ArtifactNativeControllerRevision, ArtifactTemplateArchive:
			key := string(decision.Artifact.Kind) + "\x00" + decision.Artifact.RetentionGroup
			groups[key] = append(groups[key], index)
		}
	}
	for _, indexes := range groups {
		sort.Slice(indexes, func(i, j int) bool {
			left, right := decisions[indexes[i]].Artifact, decisions[indexes[j]].Artifact
			leftAt, rightAt := candidates[left.ID], candidates[right.ID]
			if leftAt.Equal(rightAt) {
				return left.ID < right.ID
			}
			return leftAt.After(rightAt)
		})
		for position, index := range indexes {
			if position < policy.KeepPrevious {
				decisions[index].Action = ActionKeep
				decisions[index].Reasons = []string{"keep-previous"}
				continue
			}
			decisions[index].Action = ActionRemove
			decisions[index].Reasons = []string{"superseded", "grace-period-expired"}
		}
	}
}

func applyCacheBudgets(policy Policy, decisions []Decision, candidates map[string]time.Time) {
	budgets := make(map[ArtifactKind]uint64, len(policy.Budgets))
	for _, budget := range policy.Budgets {
		budgets[budget.Kind] = budget.MaxBytes
	}
	for kind, maxBytes := range budgets {
		var total uint64
		var eligible []int
		overflow := false
		for index, decision := range decisions {
			if decision.Artifact.Kind != kind {
				continue
			}
			next := total + decision.Artifact.SizeBytes
			if next < total {
				overflow = true
				break
			}
			total = next
			if _, candidate := candidates[decision.Artifact.ID]; candidate {
				eligible = append(eligible, index)
			}
		}
		if overflow || total <= maxBytes {
			continue
		}
		sort.Slice(eligible, func(i, j int) bool {
			left, right := decisions[eligible[i]].Artifact, decisions[eligible[j]].Artifact
			leftAt, rightAt := candidates[left.ID], candidates[right.ID]
			if leftAt.Equal(rightAt) {
				return left.ID < right.ID
			}
			return leftAt.Before(rightAt)
		})
		for _, index := range eligible {
			if total <= maxBytes {
				break
			}
			decisions[index].Action = ActionRemove
			decisions[index].Reasons = []string{"aggregate-budget-exceeded", "grace-period-expired"}
			if decisions[index].Artifact.SizeBytes > total {
				total = 0
			} else {
				total -= decisions[index].Artifact.SizeBytes
			}
		}
	}
}
