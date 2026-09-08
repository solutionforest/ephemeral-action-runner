package prebuilt

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"time"
)

const (
	PlanCandidate    = "candidate"
	PlanAdvanceAlias = "advance-alias"
	PlanNoop         = "noop"
)

// PublicationInput contains the immutable identities supplied by the build
// workflow. The publisher does not run Buildx or push a package; those actions
// remain explicit workflow steps. This boundary verifies that the resulting
// package and evidence can be represented safely in the catalog.
type PublicationInput struct {
	Profile             string                   `json:"profile"`
	Channel             string                   `json:"channel"`
	SourceReference     string                   `json:"sourceReference"`
	SourceTag           string                   `json:"sourceTag"`
	PackageRepository   string                   `json:"packageRepository"`
	PackageReference    string                   `json:"packageReference"`
	PackageIndexDigest  string                   `json:"packageIndexDigest"`
	PackagePlatforms    []PlatformPublication    `json:"packagePlatforms"`
	Recipe              RecipeDescriptor         `json:"recipe"`
	Runner              RunnerDescriptor         `json:"runner"`
	Tools               []ToolDescriptor         `json:"tools"`
	Evidence            EvidenceDescriptor       `json:"evidence"`
	Gates               GateResults              `json:"gates"`
	Upstream            UpstreamWorkflowEvidence `json:"upstream"`
	RebuildSourceDigest string                   `json:"rebuildSourceDigest,omitempty"`
	CandidateID         string                   `json:"candidateId,omitempty"`
	PublishedAt         time.Time                `json:"publishedAt"`
}

// PublicationPlan is a deterministic decision after observing the source
// selector and comparing the new tuple with the catalog. A plan never mutates
// the catalog until Promote is called.
type PublicationPlan struct {
	Entry                Entry                    `json:"entry"`
	Action               string                   `json:"action"`
	Reason               string                   `json:"reason"`
	ExpectedSourceDigest string                   `json:"expectedSourceDigest"`
	ExpectedAliasDigest  string                   `json:"expectedAliasDigest,omitempty"`
	SourceReference      string                   `json:"sourceReference"`
	Upstream             UpstreamWorkflowEvidence `json:"upstream"`
	RebuildSourceDigest  string                   `json:"rebuildSourceDigest,omitempty"`
	ProtectedPromotion   bool                     `json:"protectedPromotion,omitempty"`
}

type Publisher struct {
	Resolver DescriptorResolver
	Now      func() time.Time
}

func (p Publisher) now() time.Time {
	if p.Now != nil {
		return p.Now().UTC()
	}
	return time.Now().UTC()
}

// Plan resolves a mutable upstream source once, validates the package output,
// and decides whether a catalog entry is a candidate or can auto-advance the
// profile alias. Exact recipe/tool/runner identities are provenance. A package
// may auto-advance when it preserves the supported runtime contract and both
// upstream and hosted gates are complete.
func (p Publisher) Plan(ctx context.Context, catalog Catalog, input PublicationInput) (PublicationPlan, error) {
	if p.Resolver == nil {
		return PublicationPlan{}, fmt.Errorf("publisher descriptor resolver is required")
	}
	if catalog.SchemaVersion != 0 {
		if err := catalog.Validate(); err != nil {
			return PublicationPlan{}, fmt.Errorf("validate publication catalog: %w", err)
		}
	}
	profile, err := NormalizeProfile(input.Profile)
	if err != nil {
		return PublicationPlan{}, err
	}
	if input.Channel != ChannelPreview && input.Channel != ChannelStable {
		return PublicationPlan{}, fmt.Errorf("unsupported publication channel %q", input.Channel)
	}
	if strings.TrimSpace(input.SourceReference) == "" || strings.TrimSpace(input.SourceTag) == "" {
		return PublicationPlan{}, fmt.Errorf("source reference and source tag are required")
	}
	if input.PackageRepository == "" {
		input.PackageRepository = catalog.PackageRepository
	}
	if input.PackageRepository == "" {
		input.PackageRepository = DefaultPackageRepository
	}
	if input.PackageRepository != DefaultPackageRepository || (catalog.PackageRepository != "" && input.PackageRepository != catalog.PackageRepository) {
		return PublicationPlan{}, fmt.Errorf("publication package repository must be canonical GHCR package %s", DefaultPackageRepository)
	}
	source, err := p.Resolver.Resolve(ctx, input.SourceReference)
	if err != nil {
		return PublicationPlan{}, err
	}
	if source.Digest == "" {
		return PublicationPlan{}, fmt.Errorf("source %s returned no immutable digest", input.SourceReference)
	}
	if _, err := NormalizeDigest(source.Digest); err != nil {
		return PublicationPlan{}, err
	}
	now := p.now()
	entry, err := entryFromInput(input, profile, source, now)
	if err != nil {
		return PublicationPlan{}, err
	}
	rebuildSourceDigest := strings.TrimSpace(input.RebuildSourceDigest)
	if rebuildSourceDigest != "" {
		selection, err := catalog.SelectRebuildSource(profile, rebuildSourceDigest)
		if err != nil {
			return PublicationPlan{}, err
		}
		baseline, _ := catalog.EntryByDigest(selection.SourcePackageIndexDigest)
		if !SourceIdentityEqual(baseline, entry) || baseline.Source.Repository != entry.Source.Repository || baseline.Source.SourceTag != entry.Source.SourceTag || baseline.Source.Reference != entry.Source.Reference {
			return PublicationPlan{}, fmt.Errorf("rebuild source package %s does not match the resolved source index and platform digests", selection.SourcePackageIndexDigest)
		}
		if selection.Upstream != input.Upstream {
			return PublicationPlan{}, fmt.Errorf("rebuild source package %s upstream evidence does not match its historical compatible promotion", selection.SourcePackageIndexDigest)
		}
		rebuildSourceDigest = selection.SourcePackageIndexDigest
	}
	plan := PublicationPlan{Entry: entry, Action: PlanCandidate, Reason: "new immutable package candidate", ExpectedSourceDigest: source.Digest, SourceReference: input.SourceReference, Upstream: input.Upstream, RebuildSourceDigest: rebuildSourceDigest}
	policy, policyOK := catalog.Policies[profile]
	upstreamErr := validateUpstreamWorkflowEvidence(profile, input.Upstream, now)
	autoEligible := policyOK && policy.Enabled && policy.AutoAdvance && entry.Recipe.RuntimeContract == compatibleRuntimeContract && entry.Recipe.TemplateSchema == 2 && entry.Gates.HostedPass() && upstreamErr == nil
	if rebuildSourceDigest != "" && upstreamErr != nil {
		plan.Reason = "pinned-source rebuild historical upstream evidence is no longer fresh; candidate retained"
	}
	if existing, ok := catalog.EntryByDigest(entry.PackageIndexDigest); ok {
		status, statusErr := catalog.EffectiveStatus(entry.PackageIndexDigest)
		if statusErr != nil {
			return PublicationPlan{}, fmt.Errorf("cannot evaluate effective status for package index digest %s: %w", entry.PackageIndexDigest, statusErr)
		}
		if status == StatusRevoked || status == StatusCriticalRevoked {
			return PublicationPlan{}, fmt.Errorf("package index digest %s is %s and cannot be republished; use a new package digest with a changed source or EPAR tuple", entry.PackageIndexDigest, status)
		}
		if publicationEntriesEqual(existing, entry) {
			if alias, aliasOK := catalog.Aliases[profile]; aliasOK {
				plan.ExpectedAliasDigest = alias.PackageIndexDigest
				if alias.PackageIndexDigest == entry.PackageIndexDigest {
					plan.Action = PlanNoop
					plan.Reason = "package index digest is already active"
					return plan, nil
				}
			}
			if autoEligible {
				plan.Action = PlanAdvanceAlias
				if rebuildSourceDigest != "" {
					plan.Reason = "existing compatible package rebuilt from a signed active source with still-fresh historical upstream and hosted gates"
				} else {
					plan.Reason = "existing compatible package passed fresh upstream and hosted gates"
				}
			} else {
				plan.Action = PlanNoop
				plan.Reason = "immutable package candidate is already recorded"
			}
			return plan, nil
		}
		return PublicationPlan{}, fmt.Errorf("package index digest %s is already recorded with different source, EPAR tuple, evidence, or gates", entry.PackageIndexDigest)
	}
	for _, existing := range catalog.Entries {
		if existing.PackageIndexDigest != entry.PackageIndexDigest && SourceTupleUnchanged(existing, entry) && SourceIdentityEqual(existing, entry) {
			return PublicationPlan{}, fmt.Errorf("package index digest %s differs from recorded package %s while the complete source and EPAR tuple is unchanged; unexplained package nondeterminism is not promotable", entry.PackageIndexDigest, existing.PackageIndexDigest)
		}
	}
	alias, hasAlias := catalog.Aliases[profile]
	if !hasAlias {
		if autoEligible {
			plan.Action = PlanAdvanceAlias
			plan.Reason = "first compatible package passed fresh upstream and hosted gates"
		} else {
			plan.Reason = "first publication is not eligible for compatible automatic promotion"
		}
		return plan, nil
	}
	plan.ExpectedAliasDigest = alias.PackageIndexDigest
	previous, found := catalog.EntryByDigest(alias.PackageIndexDigest)
	if !found {
		return PublicationPlan{}, fmt.Errorf("alias %s points to missing catalog entry %s", profile, alias.PackageIndexDigest)
	}
	if previous.PackageIndexDigest == entry.PackageIndexDigest {
		if !SourceTupleUnchanged(previous, entry) || !SourceIdentityEqual(previous, entry) {
			return PublicationPlan{}, fmt.Errorf("package index digest %s is already recorded with a different source or EPAR tuple", entry.PackageIndexDigest)
		}
		plan.Action = PlanNoop
		plan.Reason = "package index digest is already active"
		return plan, nil
	}
	if SourceTupleUnchanged(previous, entry) && SourceIdentityEqual(previous, entry) {
		return PublicationPlan{}, fmt.Errorf("package index digest %s differs while the complete source and EPAR tuple is unchanged; protected rebuild is required", entry.PackageIndexDigest)
	}
	if autoEligible && previous.Recipe.RuntimeContract == entry.Recipe.RuntimeContract {
		plan.Action = PlanAdvanceAlias
		if rebuildSourceDigest != "" {
			plan.Reason = "compatible package rebuilt from a signed active source with still-fresh historical upstream and hosted gates"
		} else {
			plan.Reason = "compatible package passed fresh upstream and hosted gates"
		}
	}
	return plan, nil
}

func publicationEntriesEqual(a, b Entry) bool {
	// Status, publication timestamp, candidate id, supersession, and
	// revocation reason are catalog-ledger projections. Every build identity,
	// evidence digest, and gate result is immutable and must match on rerun.
	a.Status, b.Status = "", ""
	a.PublishedAt, b.PublishedAt = time.Time{}, time.Time{}
	a.CandidateID, b.CandidateID = "", ""
	a.Supersedes, b.Supersedes = "", ""
	a.RevocationReason, b.RevocationReason = "", ""
	return reflect.DeepEqual(a, b)
}

// Promote appends the candidate and, only for an advance-alias plan, rechecks
// the mutable source selector before moving its alias. A source movement
// between build and promotion fails closed and leaves the existing alias.
func (p Publisher) Promote(ctx context.Context, catalog *Catalog, plan PublicationPlan) error {
	if catalog == nil {
		return fmt.Errorf("nil prebuilt catalog")
	}
	entry := plan.Entry
	if err := entry.Validate(catalog.PackageRepository); err != nil {
		return err
	}
	if plan.Action == PlanNoop {
		return nil
	}
	if plan.Action != PlanAdvanceAlias {
		_, err := catalog.AppendEntry(entry)
		return err
	}
	if p.Resolver == nil {
		return fmt.Errorf("publisher descriptor resolver is required")
	}
	policy, ok := catalog.Policies[entry.Profile]
	if !ok || !policy.Enabled || !policy.AutoAdvance {
		return fmt.Errorf("profile %s is no longer enabled for compatible automatic promotion", entry.Profile)
	}
	if entry.Recipe.RuntimeContract != compatibleRuntimeContract || entry.Recipe.TemplateSchema != 2 || !entry.Gates.HostedPass() {
		return fmt.Errorf("package %s is not eligible for compatible automatic promotion", entry.PackageIndexDigest)
	}
	if plan.ExpectedSourceDigest != entry.Source.IndexDigest {
		return fmt.Errorf("promotion plan source digest does not match package provenance")
	}
	if plan.RebuildSourceDigest != "" {
		selection, err := catalog.SelectRebuildSource(entry.Profile, plan.RebuildSourceDigest)
		if err != nil {
			return fmt.Errorf("rebuild source authorization: %w", err)
		}
		baseline, _ := catalog.EntryByDigest(selection.SourcePackageIndexDigest)
		if !SourceIdentityEqual(baseline, entry) || baseline.Source.Repository != entry.Source.Repository || baseline.Source.SourceTag != entry.Source.SourceTag || baseline.Source.Reference != entry.Source.Reference || selection.Upstream != plan.Upstream || plan.ExpectedAliasDigest != selection.SourcePackageIndexDigest {
			return fmt.Errorf("rebuild source authorization changed after planning")
		}
	}
	if err := validateUpstreamWorkflowEvidence(entry.Profile, plan.Upstream, p.now()); err != nil {
		return fmt.Errorf("upstream promotion evidence: %w", err)
	}
	if _, err := RecheckUnchanged(ctx, p.Resolver, plan.SourceReference, plan.ExpectedSourceDigest); err != nil {
		return err
	}
	// Build a complete candidate state first. A source or alias race must not
	// leave an active entry that the moving alias never references.
	clone := cloneCatalog(*catalog)
	if existing, found := clone.EntryByDigest(entry.PackageIndexDigest); found {
		if !publicationEntriesEqual(existing, entry) {
			return fmt.Errorf("existing package %s differs from compatible promotion plan", entry.PackageIndexDigest)
		}
	} else {
		if _, err := clone.AppendEntry(entry); err != nil {
			return err
		}
	}
	profile := entry.Profile
	previousDigest := plan.ExpectedAliasDigest
	if _, err := clone.AppendCompatiblePromotion(entry.PackageIndexDigest, previousDigest, plan.Reason, plan.Upstream, p.now()); err != nil {
		return err
	}
	tag, _ := AliasTag(profile)
	if err := clone.MoveAlias(profile, entry.PackageRepository+":"+tag, entry.PackageIndexDigest, entry.Channel, previousDigest, p.now()); err != nil {
		return err
	}
	*catalog = clone
	return nil
}

// PromoteProtected performs an explicitly approved manual promotion of a
// candidate produced by Plan. It remains the runtime-major and break-glass
// path and requires factual Docker Sandboxes acceptance. The same mutable-
// source recheck and alias CAS rules apply. Legacy catalogs may still use the
// Full bootstrap behavior below.
func (p Publisher) PromoteProtected(ctx context.Context, catalog *Catalog, plan PublicationPlan) error {
	if catalog == nil {
		return fmt.Errorf("nil prebuilt catalog")
	}
	if plan.Action != PlanCandidate {
		return fmt.Errorf("protected promotion requires a candidate plan")
	}
	entry := plan.Entry
	if err := entry.Validate(catalog.PackageRepository); err != nil {
		return err
	}
	policy, ok := catalog.Policies[entry.Profile]
	bootstrapFull := ok && entry.Profile == ProfileFull && !policy.Enabled
	if !ok || !policy.Enabled && !bootstrapFull {
		return fmt.Errorf("profile %s is disabled for protected promotion", entry.Profile)
	}
	gates, err := catalog.EffectiveGates(entry.PackageIndexDigest)
	if err != nil {
		return err
	}
	if !gates.AllPass() {
		return fmt.Errorf("candidate %s has incomplete publication gates", entry.PackageIndexDigest)
	}
	if !catalog.HasCompletePlatformAcceptance(entry.PackageIndexDigest) {
		return fmt.Errorf("candidate %s is missing reviewed amd64 and arm64 acceptance records", entry.PackageIndexDigest)
	}
	if p.Resolver == nil {
		return fmt.Errorf("publisher descriptor resolver is required")
	}
	if _, err := RecheckUnchanged(ctx, p.Resolver, plan.SourceReference, plan.ExpectedSourceDigest); err != nil {
		return err
	}
	clone := cloneCatalog(*catalog)
	if bootstrapFull {
		policy.Enabled = true
		policy.WizardDefault = true
		policy.AutoAdvance = true
		policy.Reason = "Full completed protected amd64 and arm64 EPAR acceptance"
		clone.Policies[ProfileFull] = policy
		if actPolicy, exists := clone.Policies[ProfileAct]; exists {
			actPolicy.WizardDefault = false
			clone.Policies[ProfileAct] = actPolicy
		}
	}
	if _, err := clone.AppendEntry(entry); err != nil {
		return err
	}
	previousDigest := plan.ExpectedAliasDigest
	if previousDigest == "" {
		if alias, exists := clone.Aliases[entry.Profile]; exists {
			previousDigest = alias.PackageIndexDigest
		}
	}
	tag, err := AliasTag(entry.Profile)
	if err != nil {
		return err
	}
	if err := clone.MoveAlias(entry.Profile, entry.PackageRepository+":"+tag, entry.PackageIndexDigest, entry.Channel, previousDigest, p.now()); err != nil {
		return err
	}
	*catalog = clone
	return nil
}

func entryFromInput(input PublicationInput, profile string, source ResolvedReference, publishedAt time.Time) (Entry, error) {
	packageDigest, err := NormalizeDigest(input.PackageIndexDigest)
	if err != nil {
		return Entry{}, fmt.Errorf("package index digest: %w", err)
	}
	if input.PackageReference == "" {
		input.PackageReference = input.PackageRepository + "@" + packageDigest
	}
	if input.PublishedAt.IsZero() {
		input.PublishedAt = publishedAt
	}
	sourceDescriptor := SourceDescriptor{Repository: source.Repository, SourceTag: input.SourceTag, Reference: source.Repository + "@" + source.Digest, IndexDigest: source.Digest, PlatformDigests: map[string]string{}}
	resolvedPlatforms := make(map[string]PlatformDescriptor, len(source.Platforms))
	for platform, descriptor := range source.Platforms {
		resolvedPlatforms[NormalizePlatform(platform)] = descriptor
	}
	for _, publication := range input.PackagePlatforms {
		platform := NormalizePlatform(publication.Platform)
		descriptor, ok := resolvedPlatforms[platform]
		if !ok {
			return Entry{}, fmt.Errorf("source descriptor is missing published platform %s", platform)
		}
		if !strings.EqualFold(descriptor.Digest, publication.SourceManifestDigest) {
			return Entry{}, fmt.Errorf("published platform %s source digest does not match resolved source", platform)
		}
		sourceDescriptor.PlatformDigests[platform] = descriptor.Digest
	}
	entry := Entry{
		SchemaVersion: EntrySchemaVersion, ArtifactKind: CatalogArtifactKind, Profile: profile, Channel: input.Channel, Status: StatusCandidate,
		PackageRepository: input.PackageRepository, PackageReference: input.PackageReference, PackageIndexDigest: packageDigest,
		Source: sourceDescriptor, Recipe: input.Recipe, Runner: input.Runner, Tools: input.Tools, Platforms: input.PackagePlatforms,
		Evidence: input.Evidence, Gates: input.Gates, PublishedAt: input.PublishedAt, CandidateID: input.CandidateID,
	}
	if err := entry.Validate(input.PackageRepository); err != nil {
		return Entry{}, err
	}
	return entry, nil
}

// EntryByDigest returns the immutable catalog entry addressed by a package
// index digest.
func (c Catalog) EntryByDigest(digest string) (Entry, bool) {
	for _, entry := range c.Entries {
		if entry.PackageIndexDigest == digest {
			return entry, true
		}
	}
	return Entry{}, false
}
