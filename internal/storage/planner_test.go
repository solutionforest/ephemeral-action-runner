package storage

import (
	"encoding/json"
	"math/rand"
	"strings"
	"testing"
	"time"
)

func TestPreviewConservativeClassification(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	expired := now.Add(-DefaultGracePeriod - time.Hour)
	withinGrace := now.Add(-DefaultGracePeriod + time.Hour)
	activeLease := &Lease{ID: "lease-active", ExpiresAt: now.Add(time.Hour)}
	expiredLease := &Lease{ID: "lease-expired", ExpiresAt: now.Add(-time.Hour)}
	artifacts := []Artifact{
		testArtifact("expired", ArtifactNativeControllerRevision, expired),
		testArtifact("archive-expired", ArtifactTemplateArchive, expired),
		withArtifact(testArtifact("within-grace", ArtifactNativeControllerRevision, withinGrace), func(a *Artifact) {}),
		withArtifact(testArtifact("current", ArtifactNativeControllerRevision, expired), func(a *Artifact) { a.Current = true }),
		withArtifact(testArtifact("active", ArtifactNativeControllerRevision, expired), func(a *Artifact) { a.Active = true }),
		withArtifact(testArtifact("active-lease", ArtifactNativeControllerRevision, expired), func(a *Artifact) { a.Lease = activeLease }),
		withArtifact(testArtifact("expired-lease", ArtifactNativeControllerRevision, expired), func(a *Artifact) { a.Lease = expiredLease }),
		withArtifact(testArtifact("uncertain-lease", ArtifactNativeControllerRevision, expired), func(a *Artifact) { a.Lease = &Lease{} }),
		withArtifact(testArtifact("shared", ArtifactNativeControllerRevision, expired), func(a *Artifact) { a.Ownership = Ownership{Kind: OwnershipShared} }),
		withArtifact(testArtifact("unknown-owner", ArtifactNativeControllerRevision, expired), func(a *Artifact) { a.Ownership = Ownership{Kind: OwnershipUnknown} }),
		withArtifact(testArtifact("prefix", ArtifactNativeControllerRevision, expired), func(a *Artifact) { a.Target.Match = MatchPrefix }),
		withArtifact(testArtifact("protected", ArtifactTemplateArchive, expired), func(a *Artifact) {
			a.Protections = []Protection{{Kind: ProtectionCertification, Detail: "release evidence"}}
		}),
		withArtifact(testArtifact("docker-image", ArtifactDockerImage, expired), func(a *Artifact) {}),
		withArtifact(testArtifact("archive-wrong-target", ArtifactTemplateArchive, expired), func(a *Artifact) { a.Target.Kind = TargetDockerVolume }),
		withArtifact(testArtifact("no-superseded-state", ArtifactTemplateArchive, expired), func(a *Artifact) { a.SupersededAt = nil }),
		withArtifact(testArtifact("clock-skew", ArtifactTemplateArchive, now.Add(time.Hour)), func(a *Artifact) {}),
	}
	plan, err := Preview(PreviewRequest{
		Now:       now,
		Policy:    DefaultPolicy(),
		Surfaces:  []Surface{{ID: "host", Kind: SurfaceHostFilesystem, Capacity: Capacity{Known: true, AvailableBytes: 100 * GiB, TotalBytes: 200 * GiB}}},
		Artifacts: artifacts,
	})
	if err != nil {
		t.Fatalf("Preview() error = %v", err)
	}
	want := map[string]Action{
		"expired":              ActionRemove,
		"archive-expired":      ActionRemove,
		"within-grace":         ActionKeep,
		"current":              ActionProtected,
		"active":               ActionProtected,
		"active-lease":         ActionProtected,
		"expired-lease":        ActionRemove,
		"uncertain-lease":      ActionProtected,
		"shared":               ActionReportOnly,
		"unknown-owner":        ActionReportOnly,
		"prefix":               ActionReportOnly,
		"protected":            ActionProtected,
		"docker-image":         ActionReportOnly,
		"archive-wrong-target": ActionReportOnly,
		"no-superseded-state":  ActionReportOnly,
		"clock-skew":           ActionReportOnly,
	}
	for _, decision := range plan.Decisions {
		if decision.Action != want[decision.Artifact.ID] {
			t.Errorf("artifact %q action = %q reasons=%v, want %q", decision.Artifact.ID, decision.Action, decision.Reasons, want[decision.Artifact.ID])
		}
		delete(want, decision.Artifact.ID)
	}
	if len(want) != 0 {
		t.Fatalf("Preview() omitted decisions: %v", want)
	}
	if plan.RemovalCount != 3 {
		t.Fatalf("Preview() removal count = %d, want 3", plan.RemovalCount)
	}
}

func TestPreviewKeepPrevious(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	policy := DefaultPolicy()
	policy.KeepPrevious = 1
	artifacts := []Artifact{
		testArtifact("oldest", ArtifactNativeControllerRevision, now.Add(-10*24*time.Hour)),
		testArtifact("newest", ArtifactNativeControllerRevision, now.Add(-8*24*time.Hour)),
	}
	plan, err := Preview(PreviewRequest{Now: now, Policy: policy, Surfaces: testSurfaces(), Artifacts: artifacts})
	if err != nil {
		t.Fatalf("Preview() error = %v", err)
	}
	if actionFor(plan, "newest") != ActionKeep || actionFor(plan, "oldest") != ActionRemove {
		t.Fatalf("keepPrevious classifications = newest:%s oldest:%s", actionFor(plan, "newest"), actionFor(plan, "oldest"))
	}
}

func TestPreviewDedicatedCachesRemainSelfManaged(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	policy := DefaultPolicy()
	policy.Budgets = []Budget{{Kind: ArtifactGoCache, MaxBytes: 10 * GiB}, {Kind: ArtifactBuildKitCache, MaxBytes: 64 * GiB}}
	makeCache := func(id string, kind ArtifactKind, size uint64, age time.Duration) Artifact {
		artifact := testArtifact(id, kind, now.Add(-age))
		artifact.SupersededAt = nil
		artifact.LastUsedAt = now.Add(-age)
		artifact.SizeBytes = size
		if kind == ArtifactBuildKitCache {
			artifact.Target.Kind = TargetBuildKitRecord
		} else {
			artifact.Target.Kind = TargetDirectory
		}
		return artifact
	}
	artifacts := []Artifact{
		makeCache("go-old", ArtifactGoCache, 4*GiB, 10*24*time.Hour),
		makeCache("go-new", ArtifactGoCache, 4*GiB, 8*24*time.Hour),
		makeCache("go-within-grace", ArtifactGoCache, 4*GiB, 2*24*time.Hour),
		makeCache("buildkit-under-budget", ArtifactBuildKitCache, 60*GiB, 10*24*time.Hour),
	}
	plan, err := Preview(PreviewRequest{Now: now, Policy: policy, Surfaces: testSurfaces(), Artifacts: artifacts})
	if err != nil {
		t.Fatalf("Preview() error = %v", err)
	}
	for _, id := range []string{"go-old", "go-new", "go-within-grace", "buildkit-under-budget"} {
		if actionFor(plan, id) != ActionReportOnly {
			t.Fatalf("%s action = %s, want report-only", id, actionFor(plan, id))
		}
	}
}

func TestPreviewIsDeterministicAcrossInputOrder(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.FixedZone("offset", 8*60*60))
	artifacts := []Artifact{
		testArtifact("c", ArtifactTemplateArchive, now.Add(-10*24*time.Hour)),
		testArtifact("a", ArtifactNativeControllerRevision, now.Add(-10*24*time.Hour)),
		testArtifact("b", ArtifactDockerImage, now.Add(-10*24*time.Hour)),
	}
	request := PreviewRequest{
		Now:      now,
		Policy:   DefaultPolicy(),
		Surfaces: []Surface{{ID: "z", Provider: "docker-sandboxes", Kind: SurfaceDockerEngine}, {ID: "host", Kind: SurfaceHostFilesystem}},
		Requirements: []Requirement{
			{ID: "z-check", Provider: "docker-sandboxes", SurfaceID: "z", PeakBytes: GiB},
			{ID: "a-check", SurfaceID: "host", PeakBytes: GiB},
		},
		Artifacts: artifacts,
	}
	first, err := Preview(request)
	if err != nil {
		t.Fatalf("first Preview() error = %v", err)
	}
	rand.New(rand.NewSource(42)).Shuffle(len(request.Artifacts), func(i, j int) {
		request.Artifacts[i], request.Artifacts[j] = request.Artifacts[j], request.Artifacts[i]
	})
	request.Surfaces[0], request.Surfaces[1] = request.Surfaces[1], request.Surfaces[0]
	request.Requirements[0], request.Requirements[1] = request.Requirements[1], request.Requirements[0]
	second, err := Preview(request)
	if err != nil {
		t.Fatalf("second Preview() error = %v", err)
	}
	firstJSON, _ := json.Marshal(first)
	secondJSON, _ := json.Marshal(second)
	if string(firstJSON) != string(secondJSON) {
		t.Fatalf("Preview() is nondeterministic:\n%s\n%s", firstJSON, secondJSON)
	}
}

func TestPreviewDoesNotMutateArtifactProtectionOrder(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	artifact := testArtifact("protected", ArtifactTemplateArchive, now.Add(-10*24*time.Hour))
	artifact.Protections = []Protection{{Kind: ProtectionOperator, Detail: "z"}, {Kind: ProtectionCertification, Detail: "a"}}
	original := append([]Protection(nil), artifact.Protections...)
	if _, err := Preview(PreviewRequest{Now: now, Policy: DefaultPolicy(), Surfaces: testSurfaces(), Artifacts: []Artifact{artifact}}); err != nil {
		t.Fatalf("Preview() error = %v", err)
	}
	if artifact.Protections[0] != original[0] || artifact.Protections[1] != original[1] {
		t.Fatalf("Preview() mutated caller protections: got=%v want=%v", artifact.Protections, original)
	}
}

func TestPreviewCapacityWarningsAndValidation(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	plan, err := Preview(PreviewRequest{
		Now:          now,
		Policy:       DefaultPolicy(),
		Surfaces:     []Surface{{ID: "host", Kind: SurfaceHostFilesystem}},
		Requirements: []Requirement{{ID: "build", SurfaceID: "host", PeakBytes: 30 * GiB}},
	})
	if err != nil {
		t.Fatalf("Preview() error = %v", err)
	}
	if len(plan.Warnings) != 1 || !strings.Contains(plan.Warnings[0], "capacity unknown") {
		t.Fatalf("Preview() warnings = %v, want unknown capacity", plan.Warnings)
	}
	if _, err := Preview(PreviewRequest{Now: now, Policy: DefaultPolicy(), Surfaces: testSurfaces(), Artifacts: []Artifact{testArtifact("duplicate", ArtifactTemplateArchive, now.Add(-10*24*time.Hour)), testArtifact("duplicate", ArtifactTemplateArchive, now.Add(-9*24*time.Hour))}}); err == nil {
		t.Fatal("Preview() accepted duplicate artifact IDs")
	}
}

func testArtifact(id string, kind ArtifactKind, lifecycleAt time.Time) Artifact {
	at := lifecycleAt.UTC()
	return Artifact{
		ID:             id,
		Provider:       "test-provider",
		SurfaceID:      "host",
		Kind:           kind,
		RetentionGroup: "default",
		Target:         Target{Kind: TargetFile, Locator: "/exact/" + id, Identity: "identity:" + id, Fingerprint: "fingerprint:" + id, Match: MatchExact},
		Ownership:      Ownership{Kind: OwnershipExact, OwnerID: "installation:test", Evidence: "signed-manifest"},
		SizeBytes:      GiB,
		CreatedAt:      at.Add(-time.Hour),
		SupersededAt:   &at,
	}
}

func withArtifact(artifact Artifact, mutate func(*Artifact)) Artifact {
	mutate(&artifact)
	return artifact
}

func testSurfaces() []Surface {
	return []Surface{{ID: "host", Kind: SurfaceHostFilesystem, Capacity: Capacity{Known: true, AvailableBytes: 100 * GiB, TotalBytes: 200 * GiB}}}
}

func actionFor(plan Plan, id string) Action {
	for _, decision := range plan.Decisions {
		if decision.Artifact.ID == id {
			return decision.Action
		}
	}
	return ""
}
