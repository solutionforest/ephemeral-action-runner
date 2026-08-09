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
	Profile            string                `json:"profile"`
	Channel            string                `json:"channel"`
	SourceReference    string                `json:"sourceReference"`
	SourceTag          string                `json:"sourceTag"`
	PackageRepository  string                `json:"packageRepository"`
	PackageReference   string                `json:"packageReference"`
	PackageIndexDigest string                `json:"packageIndexDigest"`
	PackagePlatforms   []PlatformPublication `json:"packagePlatforms"`
	Recipe             RecipeDescriptor      `json:"recipe"`
	Runner             RunnerDescriptor      `json:"runner"`
	Tools              []ToolDescriptor      `json:"tools"`
	Evidence           EvidenceDescriptor    `json:"evidence"`
	Gates              GateResults           `json:"gates"`
	CandidateID        string                `json:"candidateId,omitempty"`
	PublishedAt        time.Time             `json:"publishedAt"`
}

// PublicationPlan is a deterministic decision after observing the source
// selector and comparing the new tuple with the catalog. A plan never mutates
// the catalog until Promote is called.
type PublicationPlan struct {
	Entry                Entry  `json:"entry"`
	Action               string `json:"action"`
	Reason               string `json:"reason"`
	ExpectedSourceDigest string `json:"expectedSourceDigest"`
	ExpectedAliasDigest  string `json:"expectedAliasDigest,omitempty"`
	SourceReference      string `json:"sourceReference"`
	ProtectedPromotion   bool   `json:"protectedPromotion,omitempty"`
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
// profile alias. Recipe/tool/runner changes are always candidates. A source
// move with an unchanged tuple may auto-advance only when the catalog policy
// enables that profile and every gate is complete.
func (p Publisher) Plan(ctx context.Context, catalog Catalog, input PublicationInput) (PublicationPlan, error) {
	if p.Resolver == nil {
		return PublicationPlan{}, fmt.Errorf("publisher descriptor resolver is required")
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
	entry, err := entryFromInput(input, profile, source, p.now())
	if err != nil {
		return PublicationPlan{}, err
	}
	plan := PublicationPlan{Entry: entry, Action: PlanCandidate, Reason: "new immutable package candidate", ExpectedSourceDigest: source.Digest, SourceReference: input.SourceReference}
	if existing, ok := catalog.EntryByDigest(entry.PackageIndexDigest); ok {
		status, statusErr := catalog.EffectiveStatus(entry.PackageIndexDigest)
		if statusErr != nil {
			return PublicationPlan{}, fmt.Errorf("cannot evaluate effective status for package index digest %s: %w", entry.PackageIndexDigest, statusErr)
		}
		if status == StatusRevoked || status == StatusCriticalRevoked {
			return PublicationPlan{}, fmt.Errorf("package index digest %s is %s and cannot be republished; use a new package digest with a changed source or EPAR tuple", entry.PackageIndexDigest, status)
		}
		if publicationEntriesEqual(existing, entry) {
			plan.Action = PlanNoop
			plan.Reason = "immutable package candidate is already recorded"
			if alias, aliasOK := catalog.Aliases[profile]; aliasOK {
				plan.ExpectedAliasDigest = alias.PackageIndexDigest
			}
			return plan, nil
		}
		return PublicationPlan{}, fmt.Errorf("package index digest %s is already recorded with different source, EPAR tuple, evidence, or gates", entry.PackageIndexDigest)
	}
	alias, hasAlias := catalog.Aliases[profile]
	if !hasAlias {
		if policy, ok := catalog.Policies[profile]; ok && policy.Enabled && policy.AutoAdvance && entry.Gates.AllPass() {
			plan.Action = PlanAdvanceAlias
			plan.Reason = "first publication satisfies enabled profile policy"
		} else {
			plan.Reason = "first publication requires protected promotion"
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
	unchangedEPARTuple := SourceTupleUnchanged(previous, entry)
	sourceChanged := !SourceIdentityEqual(previous, entry)
	if unchangedEPARTuple && !sourceChanged {
		return PublicationPlan{}, fmt.Errorf("package index digest %s differs while the complete source and EPAR tuple is unchanged; protected rebuild is required", entry.PackageIndexDigest)
	}
	if policy, ok := catalog.Policies[profile]; ok && policy.Enabled && policy.AutoAdvance && sourceChanged && unchangedEPARTuple && entry.Gates.AllPass() {
		plan.Action = PlanAdvanceAlias
		plan.Reason = "upstream source digest changed while recipe/runtime/runner/tool tuple remained unchanged"
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
	if _, err := RecheckUnchanged(ctx, p.Resolver, plan.SourceReference, plan.ExpectedSourceDigest); err != nil {
		return err
	}
	// Build a complete candidate state first. A source or alias race must not
	// leave an active entry that the moving alias never references.
	clone := cloneCatalog(*catalog)
	if _, err := clone.AppendEntry(entry); err != nil {
		return err
	}
	profile := entry.Profile
	previousDigest := plan.ExpectedAliasDigest
	tag, _ := AliasTag(profile)
	if err := clone.MoveAlias(profile, entry.PackageRepository+":"+tag, entry.PackageIndexDigest, entry.Channel, previousDigest, p.now()); err != nil {
		return err
	}
	*catalog = clone
	return nil
}

// PromoteProtected performs an explicitly approved manual promotion of a
// candidate produced by Plan. It is deliberately separate from Promote so a
// recipe/runner/tool change can never auto-advance an alias merely because all
// build gates happen to pass. The same mutable-source recheck and alias CAS
// rules apply, and disabled profiles (including Full) remain unavailable.
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
	if !ok || !policy.Enabled {
		return fmt.Errorf("profile %s is disabled for protected promotion", entry.Profile)
	}
	if !entry.Gates.AllPass() {
		return fmt.Errorf("candidate %s has incomplete publication gates", entry.PackageIndexDigest)
	}
	if p.Resolver == nil {
		return fmt.Errorf("publisher descriptor resolver is required")
	}
	if _, err := RecheckUnchanged(ctx, p.Resolver, plan.SourceReference, plan.ExpectedSourceDigest); err != nil {
		return err
	}
	clone := cloneCatalog(*catalog)
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
	for platform, descriptor := range source.Platforms {
		sourceDescriptor.PlatformDigests[NormalizePlatform(platform)] = descriptor.Digest
	}
	entry := Entry{
		SchemaVersion: CatalogSchemaVersion, ArtifactKind: CatalogArtifactKind, Profile: profile, Channel: input.Channel, Status: StatusCandidate,
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
