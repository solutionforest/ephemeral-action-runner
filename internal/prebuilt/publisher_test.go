package prebuilt

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestPublisherAutoAdvancesOnlyForSourceOnlyChange(t *testing.T) {
	oldDigest := "sha256:" + strings.Repeat("a", 64)
	newDigest := "sha256:" + strings.Repeat("b", 64)
	resolver := &sequenceResolver{observations: []ResolvedReference{sourceObservation(newDigest), sourceObservation(newDigest)}}
	previous := validEntry(ProfileAct, "a", StatusActive)
	previous.PackageIndexDigest = oldDigest
	previous.PackageReference = DefaultPackageRepository + "@" + oldDigest
	previous.Source.IndexDigest = oldDigest
	previous.Source.Reference = previous.Source.Repository + "@" + oldDigest
	catalog := Catalog{SchemaVersion: CatalogSchemaVersion, ArtifactKind: CatalogArtifactKind, PackageRepository: DefaultPackageRepository, Policies: map[string]ProfilePolicy{ProfileAct: {Enabled: true, AutoAdvance: true}}, Entries: []Entry{previous}, Aliases: map[string]Alias{ProfileAct: {Profile: ProfileAct, PackageIndexDigest: oldDigest, Tag: "act-latest", Reference: DefaultPackageRepository + ":act-latest", Channel: ChannelStable, Status: StatusActive}}}
	input := publicationInput(newDigest)
	input.SourceReference = resolver.observations[0].Reference
	input.SourceTag = "act-latest"
	plan, err := (Publisher{Resolver: resolver, Now: func() time.Time { return time.Unix(2, 0) }}).Plan(context.Background(), catalog, input)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Action != PlanAdvanceAlias {
		t.Fatalf("action = %q, reason %s", plan.Action, plan.Reason)
	}
	if err := (Publisher{Resolver: resolver, Now: func() time.Time { return time.Unix(2, 0) }}).Promote(context.Background(), &catalog, plan); err != nil {
		t.Fatal(err)
	}
	if got := catalog.Aliases[ProfileAct].PackageIndexDigest; got != newDigest {
		t.Fatalf("alias digest = %s, want %s", got, newDigest)
	}
	if got, ok := catalog.EntryByDigest(newDigest); !ok {
		t.Fatalf("promoted entry missing: %#v", got)
	} else if status, statusErr := catalog.EffectiveStatus(newDigest); statusErr != nil || status != StatusActive {
		t.Fatalf("promoted effective status = %q, %v", status, statusErr)
	}
}

func TestPublisherNoopForCompleteTuple(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	previous := validEntry(ProfileAct, "a", StatusActive)
	catalog := Catalog{SchemaVersion: CatalogSchemaVersion, ArtifactKind: CatalogArtifactKind, PackageRepository: DefaultPackageRepository, Policies: map[string]ProfilePolicy{ProfileAct: {Enabled: true, AutoAdvance: true}}, Entries: []Entry{previous}, Aliases: map[string]Alias{ProfileAct: {Profile: ProfileAct, PackageIndexDigest: digest, Tag: "act-latest", Reference: DefaultPackageRepository + ":act-latest", Channel: ChannelStable, Status: StatusActive}}}
	input := publicationInput(digest)
	input.SourceReference = "ghcr.io/catthehacker/ubuntu:act-latest"
	input.SourceTag = "act-latest"
	resolver := &sequenceResolver{observations: []ResolvedReference{sourceObservation(digest)}}
	plan, err := (Publisher{Resolver: resolver}).Plan(context.Background(), catalog, input)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Action != PlanNoop {
		t.Fatalf("action = %q, reason %s", plan.Action, plan.Reason)
	}
}

func TestPublisherAutoAdvancesWhenUpstreamIndexChanges(t *testing.T) {
	oldDigest := "sha256:" + strings.Repeat("a", 64)
	newDigest := "sha256:" + strings.Repeat("b", 64)
	previous := validEntry(ProfileAct, "a", StatusActive)
	previous.PackageIndexDigest = oldDigest
	previous.PackageReference = DefaultPackageRepository + "@" + oldDigest
	previous.Source.IndexDigest = oldDigest
	previous.Source.Reference = previous.Source.Repository + "@" + oldDigest
	input := publicationInput(newDigest)
	input.SourceReference = "ghcr.io/catthehacker/ubuntu:act-latest"
	input.SourceTag = "act-latest"
	resolver := &sequenceResolver{observations: []ResolvedReference{sourceObservation(newDigest)}}
	catalog := Catalog{SchemaVersion: CatalogSchemaVersion, ArtifactKind: CatalogArtifactKind, PackageRepository: DefaultPackageRepository, Policies: map[string]ProfilePolicy{ProfileAct: {Enabled: true, AutoAdvance: true}}, Entries: []Entry{previous}, Aliases: map[string]Alias{ProfileAct: {Profile: ProfileAct, PackageIndexDigest: oldDigest, Tag: "act-latest", Reference: DefaultPackageRepository + ":act-latest", Channel: ChannelStable, Status: StatusActive}}}
	plan, err := (Publisher{Resolver: resolver}).Plan(context.Background(), catalog, input)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Action != PlanAdvanceAlias {
		t.Fatalf("upstream index change action = %q, reason %s", plan.Action, plan.Reason)
	}
}

func TestPublisherLeavesRecipeChangeAsCandidate(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	previous := validEntry(ProfileAct, "a", StatusActive)
	catalog := Catalog{SchemaVersion: CatalogSchemaVersion, ArtifactKind: CatalogArtifactKind, PackageRepository: DefaultPackageRepository, Policies: map[string]ProfilePolicy{ProfileAct: {Enabled: true, AutoAdvance: true}}, Entries: []Entry{previous}, Aliases: map[string]Alias{ProfileAct: {Profile: ProfileAct, PackageIndexDigest: digest, Tag: "act-latest", Reference: DefaultPackageRepository + ":act-latest", Channel: ChannelStable, Status: StatusActive}}}
	resolver := &sequenceResolver{observations: []ResolvedReference{sourceObservation(digest)}}
	input := publicationInput(digest)
	input.SourceReference = resolver.observations[0].Reference
	input.SourceTag = "act-latest"
	input.Recipe.Digest = "sha256:" + strings.Repeat("b", 64)
	input.PackageIndexDigest = "sha256:" + strings.Repeat("c", 64)
	input.PackageReference = DefaultPackageRepository + "@" + input.PackageIndexDigest
	plan, err := (Publisher{Resolver: resolver}).Plan(context.Background(), catalog, input)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Action != PlanCandidate {
		t.Fatalf("action = %q, want candidate", plan.Action)
	}
}

func TestPublisherProtectedPromotionAdvancesRecipeCandidate(t *testing.T) {
	oldDigest := "sha256:" + strings.Repeat("a", 64)
	newDigest := "sha256:" + strings.Repeat("b", 64)
	previous := validEntry(ProfileAct, "a", StatusActive)
	previous.PackageIndexDigest = oldDigest
	previous.PackageReference = DefaultPackageRepository + "@" + oldDigest
	previous.Source.IndexDigest = oldDigest
	previous.Source.Reference = previous.Source.Repository + "@" + oldDigest
	resolver := &sequenceResolver{observations: []ResolvedReference{sourceObservation(newDigest), sourceObservation(newDigest)}}
	catalog := Catalog{SchemaVersion: CatalogSchemaVersion, ArtifactKind: CatalogArtifactKind, PackageRepository: DefaultPackageRepository, Policies: map[string]ProfilePolicy{ProfileAct: {Enabled: true}}, Entries: []Entry{previous}, Aliases: map[string]Alias{ProfileAct: {Profile: ProfileAct, PackageIndexDigest: oldDigest, Tag: "act-latest", Reference: DefaultPackageRepository + ":act-latest", Channel: ChannelStable, Status: StatusActive}}}
	input := publicationInput(newDigest)
	input.SourceReference = resolver.observations[0].Reference
	input.SourceTag = "act-latest"
	input.Recipe.Digest = "sha256:" + strings.Repeat("c", 64)
	publisher := Publisher{Resolver: resolver, Now: func() time.Time { return time.Unix(2, 0) }}
	plan, err := publisher.Plan(context.Background(), catalog, input)
	if err != nil || plan.Action != PlanCandidate {
		t.Fatalf("candidate plan = %#v, %v", plan, err)
	}
	if err := publisher.PromoteProtected(context.Background(), &catalog, plan); err != nil {
		t.Fatal(err)
	}
	if got := catalog.Aliases[ProfileAct].PackageIndexDigest; got != newDigest {
		t.Fatalf("protected alias digest = %s", got)
	}
}

func TestPublisherProtectedPromotionRejectsDisabledFull(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	resolver := &sequenceResolver{observations: []ResolvedReference{sourceObservation(digest)}}
	catalog := Catalog{SchemaVersion: CatalogSchemaVersion, ArtifactKind: CatalogArtifactKind, PackageRepository: DefaultPackageRepository, Policies: map[string]ProfilePolicy{ProfileFull: {Enabled: false}}}
	input := publicationInput(digest)
	input.Profile = ProfileFull
	input.SourceReference = resolver.observations[0].Reference
	input.SourceTag = "full-latest"
	plan, err := (Publisher{Resolver: resolver}).Plan(context.Background(), catalog, input)
	if err != nil {
		t.Fatal(err)
	}
	if err := (Publisher{Resolver: resolver}).PromoteProtected(context.Background(), &catalog, plan); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("disabled Full promotion error = %v", err)
	}
}

func TestPublisherProtectedPromotionRejectsIncompleteCandidate(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	resolver := &sequenceResolver{observations: []ResolvedReference{sourceObservation(digest)}}
	catalog := Catalog{SchemaVersion: CatalogSchemaVersion, ArtifactKind: CatalogArtifactKind, PackageRepository: DefaultPackageRepository, Policies: map[string]ProfilePolicy{ProfileAct: {Enabled: true}}}
	input := publicationInput(digest)
	input.SourceReference = resolver.observations[0].Reference
	input.SourceTag = "act-latest"
	input.Gates.RuntimeValidated = false
	plan, err := (Publisher{Resolver: resolver}).Plan(context.Background(), catalog, input)
	if err != nil {
		t.Fatal(err)
	}
	if err := (Publisher{Resolver: resolver}).PromoteProtected(context.Background(), &catalog, plan); err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("incomplete candidate error = %v", err)
	}
}

func TestPublisherRejectsRebuildWithSameCompleteTuple(t *testing.T) {
	oldDigest := "sha256:" + strings.Repeat("a", 64)
	newDigest := "sha256:" + strings.Repeat("b", 64)
	previous := validEntry(ProfileAct, "a", StatusActive)
	previous.PackageIndexDigest = oldDigest
	previous.PackageReference = DefaultPackageRepository + "@" + oldDigest
	previous.Source.IndexDigest = oldDigest
	previous.Source.Reference = previous.Source.Repository + "@" + oldDigest
	for platform := range previous.Source.PlatformDigests {
		previous.Source.PlatformDigests[platform] = oldDigest
	}
	catalog := Catalog{SchemaVersion: CatalogSchemaVersion, ArtifactKind: CatalogArtifactKind, PackageRepository: DefaultPackageRepository, Policies: map[string]ProfilePolicy{ProfileAct: {Enabled: true, AutoAdvance: true}}, Entries: []Entry{previous}, Aliases: map[string]Alias{ProfileAct: {Profile: ProfileAct, PackageIndexDigest: oldDigest, Tag: "act-latest", Reference: DefaultPackageRepository + ":act-latest", Channel: ChannelStable, Status: StatusActive}}}
	input := publicationInput(newDigest)
	resolver := &sequenceResolver{observations: []ResolvedReference{sourceObservation(oldDigest)}}
	if _, err := (Publisher{Resolver: resolver}).Plan(context.Background(), catalog, input); err == nil || !strings.Contains(err.Error(), "complete source and EPAR tuple") {
		t.Fatalf("same-tuple rebuild error = %v", err)
	}
}

func TestPublisherRerunOfUnpromotedCandidateIsNoop(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	input := publicationInput(digest)
	resolver := &sequenceResolver{observations: []ResolvedReference{sourceObservation(digest), sourceObservation(digest)}}
	publisher := Publisher{Resolver: resolver, Now: func() time.Time { return time.Unix(2, 0) }}
	catalog := Catalog{SchemaVersion: CatalogSchemaVersion, ArtifactKind: CatalogArtifactKind, PackageRepository: DefaultPackageRepository}
	first, err := publisher.Plan(context.Background(), catalog, input)
	if err != nil || first.Action != PlanCandidate {
		t.Fatalf("first plan = %#v, %v", first, err)
	}
	if err := publisher.Promote(context.Background(), &catalog, first); err != nil {
		t.Fatal(err)
	}
	second, err := publisher.Plan(context.Background(), catalog, input)
	if err != nil {
		t.Fatal(err)
	}
	if second.Action != PlanNoop {
		t.Fatalf("rerun action = %q, reason %s", second.Action, second.Reason)
	}
}

func TestPublisherRerunSameDigestDifferentEvidenceFails(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	input := publicationInput(digest)
	resolver := &sequenceResolver{observations: []ResolvedReference{sourceObservation(digest), sourceObservation(digest)}}
	publisher := Publisher{Resolver: resolver, Now: func() time.Time { return time.Unix(2, 0) }}
	catalog := Catalog{SchemaVersion: CatalogSchemaVersion, ArtifactKind: CatalogArtifactKind, PackageRepository: DefaultPackageRepository}
	first, err := publisher.Plan(context.Background(), catalog, input)
	if err != nil {
		t.Fatal(err)
	}
	if err := publisher.Promote(context.Background(), &catalog, first); err != nil {
		t.Fatal(err)
	}
	input.Evidence.SBOMDigest = "sha256:" + strings.Repeat("b", 64)
	if _, err := publisher.Plan(context.Background(), catalog, input); err == nil || !strings.Contains(err.Error(), "different source") {
		t.Fatalf("different evidence error = %v", err)
	}
}

func TestPublisherRejectsRepublicationOfRevokedDigest(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	for _, tc := range []struct {
		name     string
		critical bool
		status   string
	}{
		{name: "revoked", status: StatusRevoked},
		{name: "critical-revoked", critical: true, status: StatusCriticalRevoked},
	} {
		t.Run(tc.name, func(t *testing.T) {
			catalog := Catalog{SchemaVersion: CatalogSchemaVersion, ArtifactKind: CatalogArtifactKind, PackageRepository: DefaultPackageRepository, Entries: []Entry{validEntry(ProfileAct, "a", StatusActive)}}
			if _, err := catalog.Revoke(digest, "test revocation", tc.critical, time.Unix(3, 0)); err != nil {
				t.Fatal(err)
			}
			input := publicationInput(digest)
			resolver := &sequenceResolver{observations: []ResolvedReference{sourceObservation(digest)}}
			_, err := (Publisher{Resolver: resolver}).Plan(context.Background(), catalog, input)
			if err == nil || !strings.Contains(err.Error(), tc.status) || !strings.Contains(err.Error(), "cannot be republished") {
				t.Fatalf("republish error = %v, want %s rejection", err, tc.status)
			}
		})
	}
}

func TestPublisherPromotionRejectsSourceMove(t *testing.T) {
	oldDigest := "sha256:" + strings.Repeat("a", 64)
	newDigest := "sha256:" + strings.Repeat("b", 64)
	resolver := &sequenceResolver{observations: []ResolvedReference{sourceObservation(newDigest), sourceObservation(oldDigest)}}
	previous := validEntry(ProfileAct, "a", StatusActive)
	previous.PackageIndexDigest = oldDigest
	previous.PackageReference = DefaultPackageRepository + "@" + oldDigest
	previous.Source.IndexDigest = oldDigest
	previous.Source.Reference = previous.Source.Repository + "@" + oldDigest
	catalog := Catalog{SchemaVersion: CatalogSchemaVersion, ArtifactKind: CatalogArtifactKind, PackageRepository: DefaultPackageRepository, Policies: map[string]ProfilePolicy{ProfileAct: {Enabled: true, AutoAdvance: true}}, Entries: []Entry{previous}, Aliases: map[string]Alias{ProfileAct: {Profile: ProfileAct, PackageIndexDigest: oldDigest, Tag: "act-latest", Reference: DefaultPackageRepository + ":act-latest", Channel: ChannelStable, Status: StatusActive}}}
	input := publicationInput(newDigest)
	input.SourceReference = resolver.observations[0].Reference
	input.SourceTag = "act-latest"
	plan, err := (Publisher{Resolver: resolver}).Plan(context.Background(), catalog, input)
	if err != nil {
		t.Fatal(err)
	}
	if err := (Publisher{Resolver: resolver}).Promote(context.Background(), &catalog, plan); err == nil {
		t.Fatal("source race unexpectedly promoted alias")
	}
	if got := catalog.Aliases[ProfileAct].PackageIndexDigest; got != oldDigest {
		t.Fatalf("alias moved despite source race: %s", got)
	}
	if _, ok := catalog.EntryByDigest(newDigest); ok {
		t.Fatal("source-raced candidate became an active catalog entry")
	}
}

func TestPublisherAliasRaceLeavesCatalogUnchanged(t *testing.T) {
	oldDigest := "sha256:" + strings.Repeat("a", 64)
	newDigest := "sha256:" + strings.Repeat("b", 64)
	resolver := &sequenceResolver{observations: []ResolvedReference{sourceObservation(newDigest), sourceObservation(newDigest)}}
	previous := validEntry(ProfileAct, "a", StatusActive)
	previous.PackageIndexDigest = oldDigest
	previous.PackageReference = DefaultPackageRepository + "@" + oldDigest
	previous.Source.IndexDigest = oldDigest
	previous.Source.Reference = previous.Source.Repository + "@" + oldDigest
	catalog := Catalog{SchemaVersion: CatalogSchemaVersion, ArtifactKind: CatalogArtifactKind, PackageRepository: DefaultPackageRepository, Policies: map[string]ProfilePolicy{ProfileAct: {Enabled: true, AutoAdvance: true}}, Entries: []Entry{previous}, Aliases: map[string]Alias{ProfileAct: {Profile: ProfileAct, PackageIndexDigest: oldDigest, Tag: "act-latest", Reference: DefaultPackageRepository + ":act-latest", Channel: ChannelStable, Status: StatusActive}}}
	input := publicationInput(newDigest)
	input.SourceReference = resolver.observations[0].Reference
	input.SourceTag = "act-latest"
	plan, err := (Publisher{Resolver: resolver}).Plan(context.Background(), catalog, input)
	if err != nil {
		t.Fatal(err)
	}
	// Another publisher wins the alias between planning and promotion.
	racedAlias := catalog.Aliases[ProfileAct]
	racedAlias.PackageIndexDigest = "sha256:" + strings.Repeat("c", 64)
	catalog.Aliases[ProfileAct] = racedAlias
	before := len(catalog.Entries)
	if err := (Publisher{Resolver: resolver}).Promote(context.Background(), &catalog, plan); err == nil {
		t.Fatal("alias race unexpectedly promoted")
	}
	if len(catalog.Entries) != before {
		t.Fatal("alias race appended an active candidate to the original catalog")
	}
	if _, ok := catalog.EntryByDigest(newDigest); ok {
		t.Fatal("alias race left a candidate in the original catalog")
	}
}

func TestStatusTransitionsKeepImmutableEntryAndExposeEffectiveStatus(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	catalog := Catalog{SchemaVersion: CatalogSchemaVersion, ArtifactKind: CatalogArtifactKind, PackageRepository: DefaultPackageRepository, Entries: []Entry{validEntry(ProfileAct, "a", StatusCandidate)}}
	if _, err := catalog.AppendStatusTransition(digest, StatusActive, "promoted", time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.Revoke(digest, "security issue", true, time.Unix(2, 0)); err != nil {
		t.Fatal(err)
	}
	status, err := catalog.EffectiveStatus(digest)
	if err != nil || status != StatusCriticalRevoked {
		t.Fatalf("effective status = %q, %v", status, err)
	}
	entry, _ := catalog.EntryByDigest(digest)
	if entry.Status != StatusCandidate {
		t.Fatalf("immutable entry status mutated to %q", entry.Status)
	}
}

func publicationInput(packageDigest string) PublicationInput {
	return PublicationInput{
		Profile: ProfileAct, Channel: ChannelStable, SourceReference: "ghcr.io/catthehacker/ubuntu:act-latest", SourceTag: "act-latest", PackageRepository: DefaultPackageRepository, PackageReference: DefaultPackageRepository + "@" + packageDigest, PackageIndexDigest: packageDigest,
		PackagePlatforms: []PlatformPublication{{Platform: "linux/amd64", PackageManifestDigest: packageDigest, SourceManifestDigest: packageDigest, Validated: true}, {Platform: "linux/arm64", PackageManifestDigest: packageDigest, SourceManifestDigest: packageDigest, Validated: true}}, Recipe: validEntry(ProfileAct, "a", StatusActive).Recipe, Runner: validEntry(ProfileAct, "a", StatusActive).Runner, Tools: validEntry(ProfileAct, "a", StatusActive).Tools, Evidence: validEntry(ProfileAct, "a", StatusActive).Evidence, Gates: validEntry(ProfileAct, "a", StatusActive).Gates, PublishedAt: time.Unix(2, 0),
	}
}

type sequenceResolver struct {
	observations []ResolvedReference
	index        int
}

func (r *sequenceResolver) Resolve(context.Context, string) (ResolvedReference, error) {
	if len(r.observations) == 0 {
		return ResolvedReference{}, nil
	}
	value := r.observations[r.index]
	if r.index+1 < len(r.observations) {
		r.index++
	}
	return value, nil
}

func sourceObservation(digest string) ResolvedReference {
	return ResolvedReference{Reference: "ghcr.io/catthehacker/ubuntu:act-latest", Repository: "ghcr.io/catthehacker/ubuntu", Digest: digest, Platforms: map[string]PlatformDescriptor{
		"linux/amd64": {Platform: "linux/amd64", Digest: digest},
		"linux/arm64": {Platform: "linux/arm64", Digest: digest},
	}}
}
