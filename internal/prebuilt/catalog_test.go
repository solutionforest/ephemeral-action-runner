package prebuilt

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestCanonicalPackageTagUsesFullIndexDigest(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	got, err := CanonicalPackageTag(ProfileAct, digest)
	if err != nil {
		t.Fatal(err)
	}
	want := "act-latest-pkg-" + strings.Repeat("a", 64)
	if got != want {
		t.Fatalf("tag = %q, want %q", got, want)
	}
	if _, err := CanonicalPackageTag(ProfileFull, digest); err != nil {
		t.Fatal(err)
	}
}

func TestCatalogAppendAndAliasMoveAreIdempotentAndGuarded(t *testing.T) {
	entry := validEntry(ProfileAct, "a", StatusActive)
	catalog := Catalog{SchemaVersion: CatalogSchemaVersion, ArtifactKind: CatalogArtifactKind, PackageRepository: DefaultPackageRepository, Aliases: map[string]Alias{}}
	added, err := catalog.AppendEntry(entry)
	if err != nil || !added {
		t.Fatalf("append = %v, %v", added, err)
	}
	added, err = catalog.AppendEntry(entry)
	if err != nil || added {
		t.Fatalf("repeat append = %v, %v", added, err)
	}
	if err := catalog.MoveAlias(ProfileAct, DefaultPackageRepository+":act-latest", entry.PackageIndexDigest, ChannelStable, "", time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	if err := catalog.MoveAlias(ProfileAct, DefaultPackageRepository+":act-latest", entry.PackageIndexDigest, ChannelStable, "sha256:"+strings.Repeat("b", 64), time.Unix(2, 0)); err == nil {
		t.Fatal("concurrent alias move unexpectedly succeeded")
	}
	if err := catalog.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestPlanAliasReconciliationRepairsInterruptedCatalogFirstPromotion(t *testing.T) {
	oldDigest := "sha256:" + strings.Repeat("b", 64)
	entry := validEntry(ProfileAct, "a", StatusCandidate)
	catalog := Catalog{SchemaVersion: CatalogSchemaVersion, ArtifactKind: CatalogArtifactKind, PackageRepository: DefaultPackageRepository, Policies: map[string]ProfilePolicy{ProfileAct: {Enabled: true, AutoAdvance: true}}, Aliases: map[string]Alias{}}
	if _, err := catalog.AppendEntry(entry); err != nil {
		t.Fatal(err)
	}
	if err := catalog.MoveAlias(ProfileAct, DefaultPackageRepository+":act-latest", entry.PackageIndexDigest, ChannelStable, "", time.Unix(2, 0)); err != nil {
		t.Fatal(err)
	}

	plan, err := catalog.PlanAliasReconciliation(ProfileAct, oldDigest)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.NeedsRepair || plan.TargetDigest != entry.PackageIndexDigest || plan.ObservedDigest != oldDigest {
		t.Fatalf("interrupted promotion repair plan = %#v", plan)
	}

	plan, err = catalog.PlanAliasReconciliation(ProfileAct, entry.PackageIndexDigest)
	if err != nil {
		t.Fatal(err)
	}
	if plan.NeedsRepair {
		t.Fatalf("completed promotion unexpectedly needs repair: %#v", plan)
	}
}

func TestPlanAliasReconciliationRepairsMissingFirstPublicationAlias(t *testing.T) {
	entry := validEntry(ProfileAct, "a", StatusCandidate)
	catalog := Catalog{SchemaVersion: CatalogSchemaVersion, ArtifactKind: CatalogArtifactKind, PackageRepository: DefaultPackageRepository, Policies: map[string]ProfilePolicy{ProfileAct: {Enabled: true, AutoAdvance: true}}, Aliases: map[string]Alias{}}
	if _, err := catalog.AppendEntry(entry); err != nil {
		t.Fatal(err)
	}
	if err := catalog.MoveAlias(ProfileAct, DefaultPackageRepository+":act-latest", entry.PackageIndexDigest, ChannelStable, "", time.Unix(2, 0)); err != nil {
		t.Fatal(err)
	}
	plan, err := catalog.PlanAliasReconciliation(ProfileAct, "")
	if err != nil {
		t.Fatal(err)
	}
	if !plan.NeedsRepair || plan.TargetDigest != entry.PackageIndexDigest {
		t.Fatalf("first-publication repair plan = %#v", plan)
	}
}

func TestCatalogRejectsActiveEntryWithoutAllGates(t *testing.T) {
	entry := validEntry(ProfileAct, "a", StatusActive)
	entry.Gates.AttestationVerified = false
	catalog := Catalog{SchemaVersion: CatalogSchemaVersion, ArtifactKind: CatalogArtifactKind, PackageRepository: DefaultPackageRepository, Entries: []Entry{entry}}
	if err := catalog.Validate(); err == nil {
		t.Fatal("active entry without attestation gate was accepted")
	}
}

func TestCatalogAcceptanceCompletesCandidateGatesOnlyAfterBothPlatforms(t *testing.T) {
	entry := validEntry(ProfileAct, "a", StatusCandidate)
	entry.Gates.ImportReadback = false
	entry.Gates.RuntimeValidated = false
	catalog := Catalog{SchemaVersion: CatalogSchemaVersion, ArtifactKind: CatalogArtifactKind, PackageRepository: DefaultPackageRepository, Policies: map[string]ProfilePolicy{ProfileAct: {Enabled: true}}, Aliases: map[string]Alias{}}
	if _, err := catalog.AppendEntry(entry); err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.AppendAcceptance(validAcceptance(entry.PackageIndexDigest, "linux/amd64", 101, 102)); err != nil {
		t.Fatal(err)
	}
	if gates, err := catalog.EffectiveGates(entry.PackageIndexDigest); err != nil || gates.AllPass() {
		t.Fatalf("one-platform effective gates = %+v, %v; want incomplete", gates, err)
	}
	if err := catalog.MoveAlias(ProfileAct, DefaultPackageRepository+":act-latest", entry.PackageIndexDigest, ChannelStable, "", time.Unix(3, 0)); err == nil || !strings.Contains(err.Error(), "incomplete gates") {
		t.Fatalf("one-platform alias move error = %v", err)
	}
	if _, err := catalog.AppendAcceptance(validAcceptance(entry.PackageIndexDigest, "linux/arm64", 201, 202)); err != nil {
		t.Fatal(err)
	}
	if gates, err := catalog.EffectiveGates(entry.PackageIndexDigest); err != nil || !gates.AllPass() {
		t.Fatalf("two-platform effective gates = %+v, %v; want complete", gates, err)
	}
	if err := catalog.MoveAlias(ProfileAct, DefaultPackageRepository+":act-latest", entry.PackageIndexDigest, ChannelStable, "", time.Unix(4, 0)); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestCatalogAcceptanceRejectsWrongWorkflowAndConflictingPlatformEvidence(t *testing.T) {
	entry := validEntry(ProfileAct, "a", StatusCandidate)
	catalog := Catalog{SchemaVersion: CatalogSchemaVersion, ArtifactKind: CatalogArtifactKind, PackageRepository: DefaultPackageRepository, Entries: []Entry{entry}}
	wrong := validAcceptance(entry.PackageIndexDigest, "linux/amd64", 101, 102)
	wrong.WorkflowRuns[0].Workflow = "unapproved.yml"
	if _, err := catalog.AppendAcceptance(wrong); err == nil || !strings.Contains(err.Error(), "unexpected") {
		t.Fatalf("wrong workflow error = %v", err)
	}
	first := validAcceptance(entry.PackageIndexDigest, "linux/amd64", 101, 102)
	if _, err := catalog.AppendAcceptance(first); err != nil {
		t.Fatal(err)
	}
	changed := first
	changed.RunnerName = first.RunnerLabel + "-20260810-000000-002"
	if _, err := catalog.AppendAcceptance(changed); err == nil || !strings.Contains(err.Error(), "different evidence") {
		t.Fatalf("conflicting acceptance error = %v", err)
	}
}

func TestAcceptanceRejectsFailedOrMisroutedRuns(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	cases := []struct {
		name string
		edit func(*PlatformAcceptance)
		want string
	}{
		{name: "failed", edit: func(value *PlatformAcceptance) { value.WorkflowRuns[0].Conclusion = "failure" }, want: "conclusion"},
		{name: "wrong repository", edit: func(value *PlatformAcceptance) {
			value.WorkflowRuns[0].Repository = "solutionforest/another-repository"
		}, want: "repository"},
		{name: "wrong group", edit: func(value *PlatformAcceptance) { value.RunnerGroup = "Default" }, want: "epar-dev-test"},
		{name: "wrong label", edit: func(value *PlatformAcceptance) { value.RunnerLabel = "epar-docker-sandboxes" }, want: "runner label"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			acceptance := validAcceptance(digest, "linux/amd64", 101, 102)
			tc.edit(&acceptance)
			if err := acceptance.Validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("acceptance error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestRecheckUnchangedDetectsMutableTagRace(t *testing.T) {
	resolver := fakeResolver{resolved: ResolvedReference{Reference: "ghcr.io/catthehacker/ubuntu:act-latest", Digest: "sha256:" + strings.Repeat("c", 64)}}
	_, err := RecheckUnchanged(context.Background(), resolver, resolver.resolved.Reference, "sha256:"+strings.Repeat("d", 64))
	if err == nil || !strings.Contains(err.Error(), "moved") {
		t.Fatalf("race error = %v", err)
	}
}

func TestNormalizePlatformArmV8(t *testing.T) {
	if got := NormalizePlatform(" linux/arm64/v8 "); got != "linux/arm64" {
		t.Fatalf("normalized platform = %q", got)
	}
}

func TestCatalogRejectsIncompleteActiveActPlatforms(t *testing.T) {
	entry := validEntry(ProfileAct, "a", StatusActive)
	entry.Platforms = entry.Platforms[:1]
	if err := entry.Validate(DefaultPackageRepository); err == nil {
		t.Fatal("active Act entry with one platform was accepted")
	}
	entry = validEntry(ProfileAct, "a", StatusActive)
	entry.Platforms[1].Platform = "linux/386"
	if err := entry.Validate(DefaultPackageRepository); err == nil {
		t.Fatal("active Act entry with unsupported platform was accepted")
	}
	entry = validEntry(ProfileAct, "a", StatusActive)
	entry.Platforms[1].SourceManifestDigest = "sha256:" + strings.Repeat("b", 64)
	if err := entry.Validate(DefaultPackageRepository); err == nil {
		t.Fatal("active Act entry with mismatched source platform digest was accepted")
	}
}

func TestCatalogRejectsNonCanonicalPackageRepository(t *testing.T) {
	catalog := Catalog{SchemaVersion: CatalogSchemaVersion, ArtifactKind: CatalogArtifactKind, PackageRepository: "docker.io/example/docker-sandboxes-template"}
	if err := catalog.Validate(); err == nil || !strings.Contains(err.Error(), "canonical GHCR") {
		t.Fatalf("Docker Hub repository error = %v", err)
	}
}

func TestCatalogRejectsDuplicateActiveActPlatform(t *testing.T) {
	entry := validEntry(ProfileAct, "a", StatusActive)
	entry.Platforms[1].Platform = "linux/amd64"
	if err := entry.Validate(DefaultPackageRepository); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate platform error = %v", err)
	}
}

func validEntry(profile, hexChar, status string) Entry {
	digest := "sha256:" + strings.Repeat(hexChar, 64)
	entry := Entry{
		SchemaVersion: CatalogSchemaVersion, ArtifactKind: CatalogArtifactKind, Profile: profile, Channel: ChannelStable, Status: status,
		PackageRepository: DefaultPackageRepository, PackageReference: DefaultPackageRepository + "@" + digest, PackageIndexDigest: digest,
		Source: SourceDescriptor{Repository: "ghcr.io/catthehacker/ubuntu", SourceTag: profile + "-latest", Reference: "ghcr.io/catthehacker/ubuntu@" + digest, IndexDigest: digest, PlatformDigests: map[string]string{"linux/amd64": digest, "linux/arm64": digest}},
		Recipe: RecipeDescriptor{Digest: digest, RuntimeContract: "docker-sandboxes-v1", TemplateSchema: 2, RecipeRevision: strings.Repeat(hexChar, 40), SourceLockDigest: digest, ToolDigest: digest},
		Runner: RunnerDescriptor{Selector: "latest", Version: "2.336.0", AssetDigests: map[string]string{"linux/amd64": digest}, OverlayRequired: true},
		Tools:  []ToolDescriptor{{Name: "dockerfile-frontend", Digest: digest}}, Platforms: []PlatformPublication{{Platform: "linux/amd64", PackageManifestDigest: digest, SourceManifestDigest: digest, Validated: true}, {Platform: "linux/arm64", PackageManifestDigest: digest, SourceManifestDigest: digest, Validated: true}},
		Evidence: EvidenceDescriptor{ProvenanceDigest: digest, SBOMDigest: digest, AttestationDigest: digest}, PublishedAt: time.Unix(1, 0),
		Gates: GateResults{SourceResolved: true, SourceRechecked: true, BuildSucceeded: true, PlatformsValidated: true, ImportReadback: true, RuntimeValidated: true, ProvenanceGenerated: true, SBOMGenerated: true, AttestationVerified: true},
	}
	return entry
}

func validAcceptance(digest, platform string, playwrightRun, dockerHubRun int64) PlatformAcceptance {
	arch := strings.TrimPrefix(platform, "linux/")
	return PlatformAcceptance{
		SchemaVersion: AcceptanceRecordSchemaVersion, PackageIndexDigest: digest, Platform: platform,
		RunnerGroup: "epar-dev-test", RunnerLabel: "epar-prebuilt-act-" + strings.TrimPrefix(digest, "sha256:")[:12] + "-" + arch, RunnerName: "epar-prebuilt-act-" + strings.TrimPrefix(digest, "sha256:")[:12] + "-" + arch + "-20260810-000000-001",
		ReceiptSHA256: "sha256:" + strings.Repeat("f", 64), ImportReadback: true, RuntimeValidated: true, CleanupValidated: true,
		WorkflowRuns: []WorkflowRunEvidence{
			{Repository: "solutionforest/ephemeral-action-runner-test", Workflow: "playwright-docker.yml", RunID: playwrightRun, URL: "https://github.com/solutionforest/ephemeral-action-runner-test/actions/runs/" + fmt.Sprint(playwrightRun), Conclusion: "success"},
			{Repository: "solutionforest/ephemeral-action-runner-test", Workflow: "dockerhub-private-pull.yml", RunID: dockerHubRun, URL: "https://github.com/solutionforest/ephemeral-action-runner-test/actions/runs/" + fmt.Sprint(dockerHubRun), Conclusion: "success"},
		},
		ReviewedBy: "reviewer", AcceptedAt: time.Unix(2, 0),
	}
}

type fakeResolver struct{ resolved ResolvedReference }

func (f fakeResolver) Resolve(context.Context, string) (ResolvedReference, error) {
	return f.resolved, nil
}
