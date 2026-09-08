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

func TestSelectRebuildSourceRequiresExactActiveProfileBinding(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	catalog := catalogWithCompatibleActiveSource(t, ProfileFull, digest, time.Unix(1, 0), time.Unix(2, 0))

	selection, err := catalog.SelectRebuildSource(ProfileFull, digest)
	if err != nil {
		t.Fatal(err)
	}
	if selection.SourcePackageIndexDigest != digest || selection.Source.IndexDigest != digest || selection.Upstream.CompletedAt != time.Unix(1, 0).UTC() {
		t.Fatalf("rebuild selection = %#v", selection)
	}

	for _, tc := range []struct {
		name    string
		profile string
		digest  string
		want    string
	}{
		{name: "invalid digest", profile: ProfileFull, digest: "latest", want: "invalid sha256"},
		{name: "missing digest", profile: ProfileFull, digest: "sha256:" + strings.Repeat("b", 64), want: "missing"},
		{name: "cross profile", profile: ProfileAct, digest: digest, want: "belongs to profile full"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := catalog.SelectRebuildSource(tc.profile, tc.digest); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("selection error = %v, want %q", err, tc.want)
			}
		})
	}

	revoked := catalog
	if _, err := revoked.Revoke(digest, "source compromised", false, time.Unix(3, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := revoked.SelectRebuildSource(ProfileFull, digest); err == nil || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("revoked selection error = %v", err)
	}
}

func TestSelectRebuildSourceRequiresHistoricalCompatiblePromotion(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	entry := validEntry(ProfileAct, "a", StatusActive)
	catalog := Catalog{SchemaVersion: CatalogSchemaVersion, ArtifactKind: CatalogArtifactKind, PackageRepository: DefaultPackageRepository, Entries: []Entry{entry}, Aliases: map[string]Alias{ProfileAct: {Profile: ProfileAct, Tag: "act-latest", Reference: DefaultPackageRepository + ":act-latest", PackageIndexDigest: digest, Channel: ChannelStable, Status: StatusActive}}}
	if _, err := catalog.SelectRebuildSource(ProfileAct, digest); err == nil || !strings.Contains(err.Error(), "no compatible-promotion") {
		t.Fatalf("missing historical promotion error = %v", err)
	}
}

func TestRebuildSourceSelectionValidatesExecutableRegistryReadback(t *testing.T) {
	packageDigest := "sha256:" + strings.Repeat("a", 64)
	indexDigest := "sha256:" + strings.Repeat("d", 64)
	amd64Digest := "sha256:" + strings.Repeat("e", 64)
	arm64Digest := "sha256:" + strings.Repeat("f", 64)
	catalog := catalogWithCompatibleActiveDistinctSource(t, ProfileFull, packageDigest, indexDigest, amd64Digest, arm64Digest, time.Unix(1, 0), time.Unix(2, 0))
	selection, err := catalog.SelectRebuildSource(ProfileFull, packageDigest)
	if err != nil {
		t.Fatal(err)
	}
	if err := selection.ValidateResolution(selection.Source.Reference, indexDigest, amd64Digest, arm64Digest); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name      string
		reference string
		index     string
		amd64     string
		arm64     string
		want      string
	}{
		{name: "mutable reference", reference: upstreamSourceRepository + ":full-latest", index: indexDigest, amd64: amd64Digest, arm64: arm64Digest, want: "reference expected"},
		{name: "index mismatch", reference: selection.Source.Reference, index: "sha256:" + strings.Repeat("b", 64), amd64: amd64Digest, arm64: arm64Digest, want: "resolved index digest expected"},
		{name: "amd64 mismatch", reference: selection.Source.Reference, index: indexDigest, amd64: "sha256:" + strings.Repeat("b", 64), arm64: arm64Digest, want: "resolved linux/amd64 digest expected"},
		{name: "arm64 mismatch", reference: selection.Source.Reference, index: indexDigest, amd64: amd64Digest, arm64: "sha256:" + strings.Repeat("b", 64), want: "resolved linux/arm64 digest expected"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := selection.ValidateResolution(tc.reference, tc.index, tc.amd64, tc.arm64); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("resolution error = %v, want %q", err, tc.want)
			}
		})
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
	if err := catalog.MoveAlias(ProfileAct, DefaultPackageRepository+":act-latest", entry.PackageIndexDigest, ChannelStable, "", time.Unix(3, 0)); err == nil || !strings.Contains(err.Error(), "without reviewed") {
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

func TestCompatiblePromotionAuthorizesAliasWithoutSynthesizingRuntimeGates(t *testing.T) {
	entry := validEntry(ProfileAct, "a", StatusCandidate)
	entry.Gates.ImportReadback = false
	entry.Gates.RuntimeValidated = false
	catalog := Catalog{SchemaVersion: LegacyCatalogSchemaVersion, ArtifactKind: CatalogArtifactKind, PackageRepository: DefaultPackageRepository, Policies: map[string]ProfilePolicy{ProfileAct: {Enabled: true, AutoAdvance: true}}, Aliases: map[string]Alias{}}
	if _, err := catalog.AppendEntry(entry); err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.AppendCompatiblePromotion(entry.PackageIndexDigest, "", "hosted compatible v1", validUpstreamEvidence(ProfileAct, time.Unix(1, 0)), time.Unix(2, 0)); err != nil {
		t.Fatal(err)
	}
	if catalog.SchemaVersion != CatalogSchemaVersion {
		t.Fatalf("catalog schema = %d, want %d", catalog.SchemaVersion, CatalogSchemaVersion)
	}
	if gates, err := catalog.EffectiveGates(entry.PackageIndexDigest); err != nil || gates.ImportReadback || gates.RuntimeValidated {
		t.Fatalf("compatible promotion synthesized factual gates: %+v, %v", gates, err)
	}
	if err := catalog.MoveAlias(ProfileAct, DefaultPackageRepository+":act-latest", entry.PackageIndexDigest, ChannelStable, "", time.Unix(2, 0)); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestLegacyCatalogRemainsReadableButCannotCarryCompatiblePromotion(t *testing.T) {
	catalog := Catalog{SchemaVersion: LegacyCatalogSchemaVersion, ArtifactKind: CatalogArtifactKind, PackageRepository: DefaultPackageRepository, Entries: []Entry{validEntry(ProfileAct, "a", StatusCandidate)}, Aliases: map[string]Alias{}}
	if err := catalog.Validate(); err != nil {
		t.Fatal(err)
	}
	catalog.CompatiblePromotions = []CompatiblePromotion{{PackageIndexDigest: catalog.Entries[0].PackageIndexDigest}}
	if err := catalog.Validate(); err == nil || !strings.Contains(err.Error(), "schema 2") {
		t.Fatalf("legacy compatible promotion error = %v", err)
	}
}

func TestMoveAliasRejectsCrossProfileTarget(t *testing.T) {
	entry := validEntry(ProfileFull, "a", StatusActive)
	catalog := Catalog{SchemaVersion: CatalogSchemaVersion, ArtifactKind: CatalogArtifactKind, PackageRepository: DefaultPackageRepository, Policies: map[string]ProfilePolicy{ProfileAct: {Enabled: true}, ProfileFull: {Enabled: true}}, Entries: []Entry{entry}, Aliases: map[string]Alias{}}
	if err := catalog.MoveAlias(ProfileAct, DefaultPackageRepository+":act-latest", entry.PackageIndexDigest, ChannelStable, "", time.Unix(2, 0)); err == nil || !strings.Contains(err.Error(), "owned by profile") {
		t.Fatalf("cross-profile alias error = %v", err)
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
	changed.WorkflowRuns = append([]WorkflowRunEvidence(nil), first.WorkflowRuns...)
	changed.WorkflowRuns[0].RunnerName = first.RunnerLabel + "-20260810-000000-003"
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
		{name: "missing workflow runner", edit: func(value *PlatformAcceptance) { value.WorkflowRuns[0].RunnerName = "" }, want: "workflow runner name"},
		{name: "reused ephemeral runner", edit: func(value *PlatformAcceptance) { value.WorkflowRuns[1].RunnerName = value.WorkflowRuns[0].RunnerName }, want: "distinct ephemeral runner"},
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

func TestAcceptanceRetainsSchemaOneReadCompatibility(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	acceptance := validAcceptance(digest, "linux/amd64", 101, 102)
	acceptance.SchemaVersion = legacyAcceptanceRecordSchemaVersion
	acceptance.RunnerName = acceptance.RunnerLabel + "-20260810-000000-001"
	for i := range acceptance.WorkflowRuns {
		acceptance.WorkflowRuns[i].RunnerName = ""
	}
	if err := acceptance.Validate(); err != nil {
		t.Fatalf("legacy acceptance rejected: %v", err)
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
		SchemaVersion: EntrySchemaVersion, ArtifactKind: CatalogArtifactKind, Profile: profile, Channel: ChannelStable, Status: status,
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

func catalogWithCompatibleActiveSource(t *testing.T, profile, digest string, completedAt, promotedAt time.Time) Catalog {
	t.Helper()
	entry := validEntry(profile, strings.TrimPrefix(digest, "sha256:")[:1], StatusActive)
	entry.PackageIndexDigest = digest
	entry.PackageReference = DefaultPackageRepository + "@" + digest
	entry.Source.IndexDigest = digest
	entry.Source.Reference = entry.Source.Repository + "@" + digest
	for platform := range entry.Source.PlatformDigests {
		entry.Source.PlatformDigests[platform] = digest
	}
	for i := range entry.Platforms {
		entry.Platforms[i].SourceManifestDigest = digest
	}
	catalog := Catalog{SchemaVersion: CatalogSchemaVersion, ArtifactKind: CatalogArtifactKind, PackageRepository: DefaultPackageRepository, Policies: map[string]ProfilePolicy{profile: {Enabled: true, AutoAdvance: true}}, Entries: []Entry{entry}, Aliases: map[string]Alias{}}
	if _, err := catalog.AppendCompatiblePromotion(digest, "", "fresh upstream and hosted gates", validUpstreamEvidence(profile, completedAt), promotedAt); err != nil {
		t.Fatal(err)
	}
	tag, _ := AliasTag(profile)
	catalog.Aliases[profile] = Alias{Profile: profile, Tag: tag, Reference: DefaultPackageRepository + ":" + tag, PackageIndexDigest: digest, Channel: ChannelStable, Status: StatusActive, UpdatedAt: promotedAt.UTC()}
	return catalog
}

func catalogWithCompatibleActiveDistinctSource(t *testing.T, profile, packageDigest, sourceIndexDigest, sourceAMD64Digest, sourceARM64Digest string, completedAt, promotedAt time.Time) Catalog {
	t.Helper()
	entry := validEntry(profile, strings.TrimPrefix(packageDigest, "sha256:")[:1], StatusActive)
	entry.PackageIndexDigest = packageDigest
	entry.PackageReference = DefaultPackageRepository + "@" + packageDigest
	entry.Source.IndexDigest = sourceIndexDigest
	entry.Source.Reference = entry.Source.Repository + "@" + sourceIndexDigest
	entry.Source.PlatformDigests["linux/amd64"] = sourceAMD64Digest
	entry.Source.PlatformDigests["linux/arm64"] = sourceARM64Digest
	for i := range entry.Platforms {
		switch NormalizePlatform(entry.Platforms[i].Platform) {
		case "linux/amd64":
			entry.Platforms[i].SourceManifestDigest = sourceAMD64Digest
		case "linux/arm64":
			entry.Platforms[i].SourceManifestDigest = sourceARM64Digest
		}
	}
	catalog := Catalog{SchemaVersion: CatalogSchemaVersion, ArtifactKind: CatalogArtifactKind, PackageRepository: DefaultPackageRepository, Policies: map[string]ProfilePolicy{profile: {Enabled: true, AutoAdvance: true}}, Entries: []Entry{entry}, Aliases: map[string]Alias{}}
	if _, err := catalog.AppendCompatiblePromotion(packageDigest, "", "fresh upstream and hosted gates", validUpstreamEvidence(profile, completedAt), promotedAt); err != nil {
		t.Fatal(err)
	}
	tag, _ := AliasTag(profile)
	catalog.Aliases[profile] = Alias{Profile: profile, Tag: tag, Reference: DefaultPackageRepository + ":" + tag, PackageIndexDigest: packageDigest, Channel: ChannelStable, Status: StatusActive, UpdatedAt: promotedAt.UTC()}
	return catalog
}

func validAcceptance(digest, platform string, playwrightRun, dockerHubRun int64) PlatformAcceptance {
	return validAcceptanceForProfile(ProfileAct, digest, platform, playwrightRun, dockerHubRun)
}

func validAcceptanceForProfile(profile, digest, platform string, playwrightRun, dockerHubRun int64) PlatformAcceptance {
	arch := strings.TrimPrefix(platform, "linux/")
	label := "epar-prebuilt-" + profile + "-" + strings.TrimPrefix(digest, "sha256:")[:12] + "-" + arch
	return PlatformAcceptance{
		SchemaVersion: AcceptanceRecordSchemaVersion, Profile: profile, PackageIndexDigest: digest, Platform: platform,
		RunnerGroup: "epar-dev-test", RunnerLabel: label,
		ReceiptSHA256: "sha256:" + strings.Repeat("f", 64), ImportReadback: true, RuntimeValidated: true, CleanupValidated: true,
		WorkflowRuns: []WorkflowRunEvidence{
			{Repository: "solutionforest/ephemeral-action-runner-test", Workflow: "playwright-docker.yml", RunID: playwrightRun, URL: "https://github.com/solutionforest/ephemeral-action-runner-test/actions/runs/" + fmt.Sprint(playwrightRun), Conclusion: "success", RunnerName: label + "-20260810-000000-001"},
			{Repository: "solutionforest/ephemeral-action-runner-test", Workflow: "dockerhub-private-pull.yml", RunID: dockerHubRun, URL: "https://github.com/solutionforest/ephemeral-action-runner-test/actions/runs/" + fmt.Sprint(dockerHubRun), Conclusion: "success", RunnerName: label + "-20260810-000000-002"},
		},
		ReviewedBy: "reviewer", AcceptedAt: time.Unix(2, 0),
	}
}

type fakeResolver struct{ resolved ResolvedReference }

func (f fakeResolver) Resolve(context.Context, string) (ResolvedReference, error) {
	return f.resolved, nil
}
