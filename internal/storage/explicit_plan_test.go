package storage

import (
	"strings"
	"testing"
	"time"
)

func TestExplicitRemovalPlanRequiresExactOwnedTargets(t *testing.T) {
	now := time.Date(2026, time.August, 12, 2, 0, 0, 0, time.UTC)
	artifact := Artifact{ID: "cache", Ownership: Ownership{Kind: OwnershipExact, OwnerID: "config"}, Target: Target{Kind: TargetDirectory, Locator: "/tmp/cache", Identity: "directory-id", Fingerprint: "sha256:" + strings.Repeat("a", 64), Match: MatchExact}, SizeBytes: 42}
	plan, err := ExplicitRemovalPlan(now, []Artifact{artifact}, "operator-requested config reset")
	if err != nil {
		t.Fatal(err)
	}
	if plan.RemovalCount != 1 || plan.ReclaimableBytes != 42 || plan.Hash == "" || plan.Decisions[0].Action != ActionRemove {
		t.Fatalf("explicit plan = %+v", plan)
	}
	artifact.Ownership.Kind = OwnershipUnknown
	if _, err := ExplicitRemovalPlan(now, []Artifact{artifact}, "reset"); err == nil {
		t.Fatal("ExplicitRemovalPlan accepted unknown ownership")
	}
	artifact.Ownership.Kind = OwnershipExact
	artifact.Target.Match = MatchPrefix
	if _, err := ExplicitRemovalPlan(now, []Artifact{artifact}, "reset"); err == nil {
		t.Fatal("ExplicitRemovalPlan accepted prefix target")
	}
}
