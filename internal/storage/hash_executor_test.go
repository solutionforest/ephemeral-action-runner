package storage

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestPlanHashDetectsContentAndApprovalDrift(t *testing.T) {
	t.Parallel()
	plan := testPlan(t)
	if err := ValidatePlanHash(plan, plan.Hash); err != nil {
		t.Fatalf("ValidatePlanHash() error = %v", err)
	}
	drifted := plan
	drifted.Decisions = append([]Decision(nil), plan.Decisions...)
	drifted.Decisions[0].Artifact.Provider = "changed-provider"
	if err := ValidatePlanHash(drifted, plan.Hash); err == nil || !strings.Contains(err.Error(), "content drifted") {
		t.Fatalf("ValidatePlanHash() drift error = %v", err)
	}
	if err := ValidatePlanHash(plan, "sha256:"+strings.Repeat("0", 64)); err == nil || !strings.Contains(err.Error(), "approval hash") {
		t.Fatalf("ValidatePlanHash() approval error = %v", err)
	}
}

func TestValidatePlanArtifactsDetectsRemovalDrift(t *testing.T) {
	t.Parallel()
	plan := testPlan(t)
	current := make([]Artifact, 0, len(plan.Decisions))
	for _, decision := range plan.Decisions {
		current = append(current, decision.Artifact)
	}
	if err := ValidatePlanArtifacts(plan, current); err != nil {
		t.Fatalf("ValidatePlanArtifacts() error = %v", err)
	}
	for index := range current {
		if actionFor(plan, current[index].ID) == ActionRemove {
			current[index].Target.Identity = "replacement"
			break
		}
	}
	if err := ValidatePlanArtifacts(plan, current); err == nil || !strings.Contains(err.Error(), "drifted") {
		t.Fatalf("ValidatePlanArtifacts() drift error = %v", err)
	}
}

func TestExecuteOnlyPassesExactPlannedRemovals(t *testing.T) {
	t.Parallel()
	plan := testPlan(t)
	executor := &fakeExactExecutor{existing: make(map[string]Target)}
	wantRemove := make(map[string]Target)
	for _, decision := range plan.Decisions {
		if decision.Action == ActionRemove {
			executor.existing[decision.Artifact.Target.Locator] = decision.Artifact.Target
			wantRemove[decision.Artifact.ID] = decision.Artifact.Target
		}
	}
	report, err := Execute(context.Background(), plan, plan.Hash, executor)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if report.RemovedCount != len(wantRemove) || len(executor.removals) != len(wantRemove) {
		t.Fatalf("Execute() report=%+v removals=%v want=%v", report, executor.removals, wantRemove)
	}
	for _, removal := range executor.removals {
		want, exists := wantRemove[removal.ArtifactID]
		if !exists || removal.Target != want || removal.Target.Match != MatchExact {
			t.Fatalf("Execute() broadened removal: %+v", removal)
		}
	}
}

func TestExecuteStopsOnIdentityDriftWithoutRemoval(t *testing.T) {
	t.Parallel()
	plan := testPlan(t)
	executor := &fakeExactExecutor{existing: make(map[string]Target)}
	for _, decision := range plan.Decisions {
		if decision.Action == ActionRemove {
			replacement := decision.Artifact.Target
			replacement.Identity = "replacement"
			executor.existing[replacement.Locator] = replacement
			break
		}
	}
	report, err := Execute(context.Background(), plan, plan.Hash, executor)
	if err == nil || !strings.Contains(err.Error(), "drifted") {
		t.Fatalf("Execute() error = %v, want drift", err)
	}
	if len(executor.removals) != 0 || len(report.Entries) != 1 || report.Entries[0].Status != ExecutionDrifted {
		t.Fatalf("Execute() drift report=%+v removals=%v", report, executor.removals)
	}
}

func TestExecuteReturnsPartialJournalOnExactFailure(t *testing.T) {
	t.Parallel()
	plan := testPlanWithTwoRemovals(t)
	executor := &fakeExactExecutor{existing: make(map[string]Target), failAt: 2}
	for _, decision := range plan.Decisions {
		if decision.Action == ActionRemove {
			executor.existing[decision.Artifact.Target.Locator] = decision.Artifact.Target
		}
	}
	report, err := Execute(context.Background(), plan, plan.Hash, executor)
	if err == nil || !strings.Contains(err.Error(), "synthetic exact failure") {
		t.Fatalf("Execute() error = %v", err)
	}
	if report.RemovedCount != 1 || len(report.Entries) != 2 || report.Entries[0].Status != ExecutionRemoved || report.Entries[1].Status != ExecutionFailed {
		t.Fatalf("Execute() partial report = %+v", report)
	}
}

type fakeExactExecutor struct {
	existing map[string]Target
	removals []Removal
	failAt   int
}

func (executor *fakeExactExecutor) ObserveExact(_ context.Context, target Target) (Observation, error) {
	actual, exists := executor.existing[target.Locator]
	return Observation{Exists: exists, Target: actual}, nil
}

func (executor *fakeExactExecutor) RemoveExact(_ context.Context, removal Removal) error {
	executor.removals = append(executor.removals, removal)
	if executor.failAt > 0 && len(executor.removals) == executor.failAt {
		return errors.New("synthetic exact failure")
	}
	delete(executor.existing, removal.Target.Locator)
	return nil
}

func testPlan(t *testing.T) Plan {
	t.Helper()
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	plan, err := Preview(PreviewRequest{
		Now:      now,
		Policy:   DefaultPolicy(),
		Surfaces: testSurfaces(),
		Artifacts: []Artifact{
			testArtifact("remove", ArtifactTemplateArchive, now.Add(-10*24*time.Hour)),
			withArtifact(testArtifact("keep", ArtifactTemplateArchive, now.Add(-time.Hour)), func(a *Artifact) {}),
			withArtifact(testArtifact("protected", ArtifactTemplateArchive, now.Add(-10*24*time.Hour)), func(a *Artifact) { a.Current = true }),
		},
	})
	if err != nil {
		t.Fatalf("Preview() error = %v", err)
	}
	return plan
}

func testPlanWithTwoRemovals(t *testing.T) Plan {
	t.Helper()
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	plan, err := Preview(PreviewRequest{
		Now:      now,
		Policy:   DefaultPolicy(),
		Surfaces: testSurfaces(),
		Artifacts: []Artifact{
			testArtifact("remove-a", ArtifactTemplateArchive, now.Add(-11*24*time.Hour)),
			testArtifact("remove-b", ArtifactTemplateArchive, now.Add(-10*24*time.Hour)),
		},
	})
	if err != nil {
		t.Fatalf("Preview() error = %v", err)
	}
	return plan
}
