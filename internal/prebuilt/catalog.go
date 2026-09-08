// Package prebuilt defines the public contract for EPAR's registry-published
// Docker Sandboxes runner templates.  The package deliberately contains no
// provider lifecycle or sandbox import code; it is the small, shared
// vocabulary used by the publisher, catalog tooling, and (later) the runtime
// consumer.
package prebuilt

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	// LegacyCatalogSchemaVersion remains readable so existing signed catalogs
	// and source-only promotions retain their canonical identity.
	LegacyCatalogSchemaVersion = 1
	// CatalogSchemaVersion adds compatible-v1 hosted promotion records. Older
	// readers must fail closed instead of silently dropping that authority.
	CatalogSchemaVersion = 2
	// EntrySchemaVersion is independent of catalog-ledger semantics.
	EntrySchemaVersion = 1

	// CatalogArtifactKind identifies the OCI artifact represented by an entry.
	CatalogArtifactKind = "docker-sandboxes-template"

	// DefaultPackageRepository is the public EPAR package repository.  Keep the
	// repository separate from Catthehacker's source repository: a package is a
	// verified EPAR recipe result, not an unqualified source mirror.
	DefaultPackageRepository = "ghcr.io/solutionforest/ephemeral-action-runner/docker-sandboxes-template"
	// CatalogMovingTag is the single moving catalog selector consumed by
	// production. It is intentionally separate from profile aliases so a
	// catalog update can carry status transitions without retagging packages.
	CatalogMovingTag = "catalog-v1"
	// CatalogArtifactMediaType identifies the JSON layer in the catalog OCI
	// artifact. The manifest itself is addressed by the registry descriptor;
	// this media type identifies the content to a consumer without relying on
	// a formatted JSON hash.
	CatalogArtifactMediaType = "application/vnd.epar.prebuilt.catalog.v1+json"
	// CatalogArtifactType and CatalogConfigMediaType make a catalog OCI
	// manifest unambiguous. A generic image carrying a JSON layer is not a
	// catalog and must be rejected by runtime resolution.
	CatalogArtifactType    = "application/vnd.epar.prebuilt.catalog.v1"
	CatalogConfigMediaType = "application/vnd.epar.prebuilt.catalog.config.v1+json"

	ProfileAct  = "act"
	ProfileFull = "full"

	ChannelPreview = "preview"
	ChannelStable  = "stable"

	StatusCandidate       = "candidate"
	StatusActive          = "active"
	StatusSuperseded      = "superseded"
	StatusRevoked         = "revoked"
	StatusCriticalRevoked = "critical-revoked"
)

var (
	digestPattern   = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	profilePattern  = regexp.MustCompile(`^[a-z][a-z0-9.-]{0,31}$`)
	revisionPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

const (
	compatibleRuntimeContract = "docker-sandboxes-v1"
	upstreamSourceRepository  = "ghcr.io/catthehacker/ubuntu"
	upstreamRepository        = "catthehacker/docker_images"
	upstreamFullWorkflow      = ".github/workflows/copy-full-image.yml"
	upstreamActWorkflow       = ".github/workflows/build-ubuntu.yml"
	upstreamActGateJob        = "Build base 24.04"
	upstreamFull24Job         = "copy image ghcr.io/christopherhx/runner-images:ubuntu24-runner-large-latest"
	upstreamFull22Job         = "copy image ghcr.io/christopherhx/runner-images:ubuntu22-runner-large-latest"
	upstreamFull20Job         = "copy image ghcr.io/christopherhx/runner-images:ubuntu20-runner-large-latest"
	upstreamEvidenceMaxAge    = 10 * 24 * time.Hour
)

// ProfileEnabled reports whether a profile is supported by the publication
// contract. Catalog policy, complete platform acceptance, and protected
// promotion still gate whether a supported profile can advance an alias.
func ProfileEnabled(profile string) bool {
	profile = strings.ToLower(strings.TrimSpace(profile))
	return profile == ProfileAct || profile == ProfileFull
}

// NormalizeProfile validates the profile key used in aliases, tags, and
// catalog entries.
func NormalizeProfile(profile string) (string, error) {
	profile = strings.ToLower(strings.TrimSpace(profile))
	if !profilePattern.MatchString(profile) {
		return "", fmt.Errorf("invalid prebuilt profile %q", profile)
	}
	if profile != ProfileAct && profile != ProfileFull {
		return "", fmt.Errorf("unsupported prebuilt profile %q", profile)
	}
	return profile, nil
}

// NormalizeDigest accepts the canonical digest form used by OCI references.
func NormalizeDigest(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if !digestPattern.MatchString(value) {
		return "", fmt.Errorf("invalid sha256 digest %q", value)
	}
	return value, nil
}

// CanonicalPackageTag returns the write-once package tag for an OCI index
// digest.  The full digest is intentionally retained: a short prefix would
// create a collision domain for independent package publications.
func CanonicalPackageTag(profile, indexDigest string) (string, error) {
	profile, err := NormalizeProfile(profile)
	if err != nil {
		return "", err
	}
	indexDigest, err = NormalizeDigest(indexDigest)
	if err != nil {
		return "", err
	}
	return profile + "-latest-pkg-" + strings.TrimPrefix(indexDigest, "sha256:"), nil
}

// AliasTag is the moving, upstream-aligned selector.  It is never an
// artifact identity and must not be persisted as the sole receipt identity.
func AliasTag(profile string) (string, error) {
	profile, err := NormalizeProfile(profile)
	if err != nil {
		return "", err
	}
	return profile + "-latest", nil
}

// CatalogMovingReference returns the moving catalog reference. The caller
// must resolve it to an OCI descriptor before using any catalog data.
func CatalogMovingReference(repository string) (string, error) {
	repository = strings.TrimSpace(repository)
	if repository == "" || strings.ContainsAny(repository, " @") {
		return "", fmt.Errorf("invalid catalog repository %q", repository)
	}
	return repository + ":" + CatalogMovingTag, nil
}

// CanonicalCatalogTag returns the write-once catalog tag. Its suffix is the
// SHA-256 digest of canonical catalog JSON, not a registry manifest digest or
// a hash of a pretty-printed representation.
func CanonicalCatalogTag(catalogDigest string) (string, error) {
	catalogDigest, err := NormalizeDigest(catalogDigest)
	if err != nil {
		return "", err
	}
	return CatalogMovingTag + "-pkg-" + strings.TrimPrefix(catalogDigest, "sha256:"), nil
}

// CatalogReference returns an immutable catalog reference derived from the
// canonical catalog digest.
func CatalogReference(repository, catalogDigest string) (string, error) {
	tag, err := CanonicalCatalogTag(catalogDigest)
	if err != nil {
		return "", err
	}
	repository = strings.TrimSpace(repository)
	if repository == "" || strings.ContainsAny(repository, " @") {
		return "", fmt.Errorf("invalid catalog repository %q", repository)
	}
	return repository + ":" + tag, nil
}

// SourceDescriptor identifies the exact upstream OCI source observed by the
// publisher. SourceTag is retained for provenance/discovery only; the digest
// pair is authoritative.
type SourceDescriptor struct {
	Repository      string            `json:"repository"`
	SourceTag       string            `json:"sourceTag"`
	Reference       string            `json:"reference"`
	IndexDigest     string            `json:"indexDigest"`
	PlatformDigests map[string]string `json:"platformDigests"`
	SourceRevision  string            `json:"sourceRevision,omitempty"`
}

// RunnerDescriptor captures the runner artifact used by the recipe or by a
// required local derivative. It is explicit even when the published base
// intentionally leaves host-specific overlays out of the image.
type RunnerDescriptor struct {
	Selector        string            `json:"selector"`
	Version         string            `json:"version"`
	AssetDigests    map[string]string `json:"assetDigests"`
	OverlayRequired bool              `json:"overlayRequired"`
}

// ToolDescriptor is an immutable EPAR-owned build input. Name is stable in
// the catalog, while Digest is the exact OCI/content digest or source hash.
type ToolDescriptor struct {
	Name      string `json:"name"`
	Reference string `json:"reference,omitempty"`
	Digest    string `json:"digest"`
}

// RecipeDescriptor binds package output to the committed EPAR recipe and
// runtime contract. Exact recipe and tool identities are provenance; runtime
// compatibility is defined by RuntimeContract and TemplateSchema.
type RecipeDescriptor struct {
	Digest           string `json:"digest"`
	RuntimeContract  string `json:"runtimeContract"`
	TemplateSchema   int    `json:"templateSchema"`
	RecipeRevision   string `json:"recipeRevision,omitempty"`
	SourceLockDigest string `json:"sourceLockDigest"`
	ToolDigest       string `json:"toolDigest"`
}

// EvidenceDescriptor records immutable evidence attached to the package
// subject. The verifier treats missing provenance/SBOM/attestation as a hard
// publication failure, not as a warning.
type EvidenceDescriptor struct {
	ProvenanceDigest  string `json:"provenanceDigest"`
	SBOMDigest        string `json:"sbomDigest"`
	AttestationDigest string `json:"attestationDigest"`
	SignatureDigest   string `json:"signatureDigest,omitempty"`
	CatalogDigest     string `json:"catalogDigest,omitempty"`
}

// PlatformPublication binds one package index to a platform-specific
// manifest. It deliberately records the source platform digest separately
// from the package platform digest.
type PlatformPublication struct {
	Platform              string `json:"platform"`
	PackageManifestDigest string `json:"packageManifestDigest"`
	SourceManifestDigest  string `json:"sourceManifestDigest"`
	Validated             bool   `json:"validated"`
}

// GateResults is the promotion checklist. An active alias may only point at
// an entry for which every required gate is true. Keeping this in the
// catalog makes a publication auditable without trusting a workflow log.
type GateResults struct {
	SourceResolved      bool `json:"sourceResolved"`
	SourceRechecked     bool `json:"sourceRechecked"`
	BuildSucceeded      bool `json:"buildSucceeded"`
	PlatformsValidated  bool `json:"platformsValidated"`
	ImportReadback      bool `json:"importReadback"`
	RuntimeValidated    bool `json:"runtimeValidated"`
	ProvenanceGenerated bool `json:"provenanceGenerated"`
	SBOMGenerated       bool `json:"sbomGenerated"`
	AttestationVerified bool `json:"attestationVerified"`
}

func (g GateResults) AllPass() bool {
	return g.SourceResolved && g.SourceRechecked && g.BuildSucceeded && g.PlatformsValidated && g.ImportReadback && g.RuntimeValidated && g.ProvenanceGenerated && g.SBOMGenerated && g.AttestationVerified
}

func (g GateResults) HostedPass() bool {
	return g.SourceResolved && g.SourceRechecked && g.BuildSucceeded && g.PlatformsValidated && g.ProvenanceGenerated && g.SBOMGenerated && g.AttestationVerified
}

// ProfilePolicy controls compatible promotion and wizard exposure for each
// profile. Runtime-major candidates still require protected promotion.
type ProfilePolicy struct {
	Enabled       bool   `json:"enabled"`
	WizardDefault bool   `json:"wizardDefault"`
	AutoAdvance   bool   `json:"autoAdvance"`
	Reason        string `json:"reason,omitempty"`
}

// Entry is one immutable package publication. Entries are append-only; an
// alias move creates a new catalog state pointing at an existing or newly
// appended entry rather than mutating an entry's identity.
type Entry struct {
	SchemaVersion      int                   `json:"schemaVersion"`
	ArtifactKind       string                `json:"artifactKind"`
	Profile            string                `json:"profile"`
	Channel            string                `json:"channel"`
	Status             string                `json:"status"`
	PackageRepository  string                `json:"packageRepository"`
	PackageReference   string                `json:"packageReference"`
	PackageIndexDigest string                `json:"packageIndexDigest"`
	Source             SourceDescriptor      `json:"source"`
	Recipe             RecipeDescriptor      `json:"recipe"`
	Runner             RunnerDescriptor      `json:"runner"`
	Tools              []ToolDescriptor      `json:"tools"`
	Platforms          []PlatformPublication `json:"platforms"`
	Evidence           EvidenceDescriptor    `json:"evidence"`
	Gates              GateResults           `json:"gates"`
	PublishedAt        time.Time             `json:"publishedAt"`
	Supersedes         string                `json:"supersedes,omitempty"`
	RevocationReason   string                `json:"revocationReason,omitempty"`
	CandidateID        string                `json:"candidateId,omitempty"`
}

// Alias points a moving profile selector at one immutable package entry.
type Alias struct {
	Profile            string    `json:"profile"`
	Tag                string    `json:"tag"`
	Reference          string    `json:"reference"`
	PackageIndexDigest string    `json:"packageIndexDigest"`
	Channel            string    `json:"channel"`
	Status             string    `json:"status"`
	UpdatedAt          time.Time `json:"updatedAt"`
}

// AliasReconciliation describes the idempotent repair needed when a signed
// catalog pointer was committed but its public profile alias was not. Registry
// tag moves are not atomic across two tags, so the signed catalog is the
// durable authority used to complete an interrupted promotion on the next run.
type AliasReconciliation struct {
	Profile        string `json:"profile"`
	AliasReference string `json:"aliasReference,omitempty"`
	TargetDigest   string `json:"targetDigest,omitempty"`
	ObservedDigest string `json:"observedDigest,omitempty"`
	NeedsRepair    bool   `json:"needsRepair"`
}

// StatusTransition is an append-only tombstone/status ledger. Entry.Status
// records the status at first publication; later supersession and revocation
// never mutate that immutable entry.
type StatusTransition struct {
	PackageIndexDigest string    `json:"packageIndexDigest"`
	FromStatus         string    `json:"fromStatus"`
	ToStatus           string    `json:"toStatus"`
	Reason             string    `json:"reason,omitempty"`
	At                 time.Time `json:"at"`
}

const (
	legacyAcceptanceRecordSchemaVersion = 1
	profilelessAcceptanceSchemaVersion  = 2
	AcceptanceRecordSchemaVersion       = 3
)

// WorkflowRunEvidence is human-reviewed evidence from the private EPAR test
// repository. The publisher intentionally has no cross-repository credential;
// a protected-environment reviewer verifies these immutable run URLs before
// the signed catalog is published.
type WorkflowRunEvidence struct {
	Repository string `json:"repository"`
	Workflow   string `json:"workflow"`
	RunID      int64  `json:"runId"`
	URL        string `json:"url"`
	Conclusion string `json:"conclusion"`
	RunnerName string `json:"runnerName,omitempty"`
}

// PlatformAcceptance records one real Docker-Sandboxes acceptance performed
// by an EPAR-managed ephemeral runner. It is append-only and never changes the
// immutable package Entry that was created by hosted publication.
type PlatformAcceptance struct {
	SchemaVersion      int                   `json:"schemaVersion"`
	Profile            string                `json:"profile,omitempty"`
	PackageIndexDigest string                `json:"packageIndexDigest"`
	Platform           string                `json:"platform"`
	RunnerGroup        string                `json:"runnerGroup"`
	RunnerLabel        string                `json:"runnerLabel"`
	RunnerName         string                `json:"runnerName,omitempty"`
	ReceiptSHA256      string                `json:"receiptSha256"`
	ImportReadback     bool                  `json:"importReadback"`
	RuntimeValidated   bool                  `json:"runtimeValidated"`
	CleanupValidated   bool                  `json:"cleanupValidated"`
	WorkflowRuns       []WorkflowRunEvidence `json:"workflowRuns"`
	ReviewedBy         string                `json:"reviewedBy"`
	AcceptedAt         time.Time             `json:"acceptedAt"`
}

// SourceOnlyPromotion records the deliberate reuse of an already accepted
// EPAR recipe/runtime/runner/tool tuple when only the upstream source identity
// changed. It never authorizes an EPAR-controlled tuple change.
type SourceOnlyPromotion struct {
	PackageIndexDigest string    `json:"packageIndexDigest"`
	AcceptedFromDigest string    `json:"acceptedFromDigest"`
	Reason             string    `json:"reason"`
	At                 time.Time `json:"at"`
}

// UpstreamWorkflowEvidence records the public upstream workflow observation
// that authorized one compatible hosted promotion. It is kept in the signed
// catalog so the profile alias does not depend on an ephemeral workflow log.
type UpstreamWorkflowEvidence struct {
	Repository     string    `json:"repository"`
	Workflow       string    `json:"workflow"`
	RunID          int64     `json:"runId"`
	RunAttempt     int       `json:"runAttempt"`
	Event          string    `json:"event"`
	Branch         string    `json:"branch"`
	HeadSHA        string    `json:"headSha"`
	RunConclusion  string    `json:"runConclusion,omitempty"`
	Conclusion     string    `json:"conclusion"`
	GateJob        string    `json:"gateJob,omitempty"`
	TestConclusion string    `json:"testConclusion,omitempty"`
	CompletedAt    time.Time `json:"completedAt"`
}

// CompatiblePromotion records automatic movement of a stable profile alias
// after hosted gates pass for a supported runtime contract. It deliberately
// does not imply Docker Sandboxes import readback or runtime validation.
type CompatiblePromotion struct {
	PackageIndexDigest string                   `json:"packageIndexDigest"`
	PreviousDigest     string                   `json:"previousDigest,omitempty"`
	RuntimeContract    string                   `json:"runtimeContract"`
	Upstream           UpstreamWorkflowEvidence `json:"upstream"`
	Reason             string                   `json:"reason"`
	At                 time.Time                `json:"at"`
}

// RebuildSourceSelection is the immutable source authorization exported to a
// workflow after the containing catalog has been signature verified.
type RebuildSourceSelection struct {
	Profile                  string                   `json:"profile"`
	SourcePackageIndexDigest string                   `json:"sourcePackageIndexDigest"`
	Source                   SourceDescriptor         `json:"source"`
	Upstream                 UpstreamWorkflowEvidence `json:"upstream"`
}

// ValidateResolution verifies that registry readback resolved the exact index
// and two platform descriptors authorized by this selection.
func (s RebuildSourceSelection) ValidateResolution(reference, indexDigest, amd64Digest, arm64Digest string) error {
	profile, err := NormalizeProfile(s.Profile)
	if err != nil {
		return err
	}
	if _, err := NormalizeDigest(s.SourcePackageIndexDigest); err != nil {
		return fmt.Errorf("rebuild source package digest: %w", err)
	}
	if s.Source.Repository != upstreamSourceRepository || s.Source.SourceTag != profile+"-latest" || s.Source.Reference != upstreamSourceRepository+"@"+s.Source.IndexDigest {
		return errors.New("rebuild source selection has a non-canonical source identity")
	}
	if reference != s.Source.Reference {
		return fmt.Errorf("rebuild source reference expected %s got %s", s.Source.Reference, reference)
	}
	resolved := map[string]string{
		"index":       indexDigest,
		"linux/amd64": amd64Digest,
		"linux/arm64": arm64Digest,
	}
	expected := map[string]string{
		"index":       s.Source.IndexDigest,
		"linux/amd64": s.Source.PlatformDigests["linux/amd64"],
		"linux/arm64": s.Source.PlatformDigests["linux/arm64"],
	}
	if len(s.Source.PlatformDigests) != 2 {
		return fmt.Errorf("rebuild source selection must contain exactly two platform digests, got %d", len(s.Source.PlatformDigests))
	}
	for name, want := range expected {
		got, err := NormalizeDigest(resolved[name])
		if err != nil {
			return fmt.Errorf("resolved %s digest: %w", name, err)
		}
		if want != got {
			return fmt.Errorf("resolved %s digest expected %s got %s", name, want, got)
		}
	}
	return nil
}

// Catalog is an append-only publication ledger. Aliases are a projection of
// the latest successful promotion and can be regenerated from Entries.
type Catalog struct {
	SchemaVersion        int                      `json:"schemaVersion"`
	ArtifactKind         string                   `json:"artifactKind"`
	PackageRepository    string                   `json:"packageRepository"`
	GeneratedAt          time.Time                `json:"generatedAt"`
	Policies             map[string]ProfilePolicy `json:"policies"`
	Entries              []Entry                  `json:"entries"`
	Aliases              map[string]Alias         `json:"aliases"`
	Transitions          []StatusTransition       `json:"transitions"`
	Acceptances          []PlatformAcceptance     `json:"acceptances,omitempty"`
	SourceOnlyPromotions []SourceOnlyPromotion    `json:"sourceOnlyPromotions,omitempty"`
	CompatiblePromotions []CompatiblePromotion    `json:"compatiblePromotions,omitempty"`
}

// PlanAliasReconciliation compares the observed registry alias with the
// active mapping in a validated signed catalog. An empty observed digest means
// the alias is absent. A catalog without an active alias requires no repair.
func (c Catalog) PlanAliasReconciliation(profile, observedDigest string) (AliasReconciliation, error) {
	profile, err := NormalizeProfile(profile)
	if err != nil {
		return AliasReconciliation{}, err
	}
	if err := c.Validate(); err != nil {
		return AliasReconciliation{}, err
	}
	observedDigest = strings.TrimSpace(observedDigest)
	if observedDigest != "" {
		observedDigest, err = NormalizeDigest(observedDigest)
		if err != nil {
			return AliasReconciliation{}, fmt.Errorf("observed alias digest: %w", err)
		}
	}
	plan := AliasReconciliation{Profile: profile, ObservedDigest: observedDigest}
	alias, exists := c.Aliases[profile]
	if !exists {
		return plan, nil
	}
	status, err := c.EffectiveStatus(alias.PackageIndexDigest)
	if err != nil {
		return AliasReconciliation{}, err
	}
	if status != StatusActive {
		return AliasReconciliation{}, fmt.Errorf("catalog alias %s is not active", profile)
	}
	plan.AliasReference = alias.Reference
	plan.TargetDigest = alias.PackageIndexDigest
	plan.NeedsRepair = observedDigest != alias.PackageIndexDigest
	return plan, nil
}

// SourceTupleUnchanged compares the exact EPAR build/provenance tuple. It is
// retained for legacy source-only records and nondeterminism detection; v1
// compatibility itself is defined only by runtime contract and schema.
func SourceTupleUnchanged(a, b Entry) bool {
	return a.Profile == b.Profile &&
		a.Recipe == b.Recipe &&
		RunnerEqual(a.Runner, b.Runner) &&
		ToolsEqual(a.Tools, b.Tools)
}

// SourceIdentityEqual reports whether the immutable upstream source index and
// every normalized platform descriptor are unchanged. It is kept separate
// from SourceTupleUnchanged so unexplained output nondeterminism for one
// complete tuple can fail closed.
func SourceIdentityEqual(a, b Entry) bool {
	if a.Source.IndexDigest != b.Source.IndexDigest || len(a.Source.PlatformDigests) != len(b.Source.PlatformDigests) {
		return false
	}
	left := make(map[string]string, len(a.Source.PlatformDigests))
	right := make(map[string]string, len(b.Source.PlatformDigests))
	for platform, digest := range a.Source.PlatformDigests {
		left[NormalizePlatform(platform)] = digest
	}
	for platform, digest := range b.Source.PlatformDigests {
		right[NormalizePlatform(platform)] = digest
	}
	if len(left) != len(right) {
		return false
	}
	for platform, digest := range left {
		if right[platform] != digest {
			return false
		}
	}
	return true
}

func RunnerEqual(a, b RunnerDescriptor) bool {
	if a.Selector != b.Selector || a.Version != b.Version || a.OverlayRequired != b.OverlayRequired || len(a.AssetDigests) != len(b.AssetDigests) {
		return false
	}
	for key, value := range a.AssetDigests {
		if b.AssetDigests[key] != value {
			return false
		}
	}
	return true
}

func ToolsEqual(a, b []ToolDescriptor) bool {
	if len(a) != len(b) {
		return false
	}
	a = append([]ToolDescriptor(nil), a...)
	b = append([]ToolDescriptor(nil), b...)
	sort.Slice(a, func(i, j int) bool { return a[i].Name < a[j].Name })
	sort.Slice(b, func(i, j int) bool { return b[i].Name < b[j].Name })
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func validateUpstreamWorkflowEvidence(profile string, evidence UpstreamWorkflowEvidence, promotedAt time.Time) error {
	if evidence.Repository != upstreamRepository || evidence.RunID <= 0 || evidence.RunAttempt <= 0 || evidence.Event != "schedule" || evidence.Branch != "master" || evidence.Conclusion != "success" || !revisionPattern.MatchString(evidence.HeadSHA) || evidence.CompletedAt.IsZero() {
		return errors.New("upstream workflow evidence is incomplete or not a successful scheduled master run")
	}
	switch profile {
	case ProfileAct:
		if evidence.Workflow != upstreamActWorkflow || evidence.GateJob != upstreamActGateJob || (evidence.TestConclusion != "success" && evidence.TestConclusion != "skipped") {
			return errors.New("Act upstream evidence does not identify the tag-producing base job")
		}
	case ProfileFull:
		if evidence.Workflow != upstreamFullWorkflow || evidence.GateJob != "four Full copy jobs" || evidence.RunConclusion != "success" {
			return errors.New("Full upstream evidence does not identify the copy workflow")
		}
	default:
		return fmt.Errorf("unsupported upstream evidence profile %q", profile)
	}
	completedAt := evidence.CompletedAt.UTC()
	promotedAt = promotedAt.UTC()
	if promotedAt.IsZero() || completedAt.After(promotedAt) || promotedAt.Sub(completedAt) > upstreamEvidenceMaxAge {
		return errors.New("upstream workflow evidence is stale or newer than the promotion")
	}
	return nil
}

// SelectRebuildSource returns the exact upstream source identity and historical
// workflow evidence attached to a profile's current active package. Callers
// must obtain the Catalog from a signature-verified catalog resolver before
// trusting the returned values.
func (c Catalog) SelectRebuildSource(profile, packageDigest string) (RebuildSourceSelection, error) {
	profile, err := NormalizeProfile(profile)
	if err != nil {
		return RebuildSourceSelection{}, err
	}
	packageDigest, err = NormalizeDigest(packageDigest)
	if err != nil {
		return RebuildSourceSelection{}, fmt.Errorf("rebuild source package digest: %w", err)
	}
	entry, ok := c.EntryByDigest(packageDigest)
	if !ok {
		return RebuildSourceSelection{}, fmt.Errorf("rebuild source package %s is missing from the verified catalog", packageDigest)
	}
	if entry.Profile != profile {
		return RebuildSourceSelection{}, fmt.Errorf("rebuild source package %s belongs to profile %s, not %s", packageDigest, entry.Profile, profile)
	}
	wantSourceTag := profile + "-latest"
	if entry.Source.Repository != upstreamSourceRepository || entry.Source.SourceTag != wantSourceTag || entry.Source.Reference != upstreamSourceRepository+"@"+entry.Source.IndexDigest {
		return RebuildSourceSelection{}, fmt.Errorf("rebuild source package %s does not use the canonical %s source identity", packageDigest, profile)
	}
	status, err := c.EffectiveStatus(packageDigest)
	if err != nil {
		return RebuildSourceSelection{}, err
	}
	if status != StatusActive {
		return RebuildSourceSelection{}, fmt.Errorf("rebuild source package %s has effective status %s, not active", packageDigest, status)
	}
	alias, ok := c.Aliases[profile]
	if !ok || alias.PackageIndexDigest != packageDigest {
		return RebuildSourceSelection{}, fmt.Errorf("rebuild source package %s is not the current %s alias target", packageDigest, profile)
	}
	var upstream UpstreamWorkflowEvidence
	found := false
	for _, promotion := range c.CompatiblePromotions {
		if promotion.PackageIndexDigest != packageDigest {
			continue
		}
		if err := c.validateCompatiblePromotion(promotion); err != nil {
			return RebuildSourceSelection{}, fmt.Errorf("rebuild source package %s has invalid historical upstream evidence: %w", packageDigest, err)
		}
		if found {
			return RebuildSourceSelection{}, fmt.Errorf("rebuild source package %s has ambiguous historical upstream evidence", packageDigest)
		}
		upstream = promotion.Upstream
		found = true
	}
	if !found {
		return RebuildSourceSelection{}, fmt.Errorf("rebuild source package %s has no compatible-promotion upstream evidence", packageDigest)
	}
	return RebuildSourceSelection{Profile: profile, SourcePackageIndexDigest: packageDigest, Source: entry.Source, Upstream: upstream}, nil
}

func (c Catalog) validateCompatiblePromotion(promotion CompatiblePromotion) error {
	targetDigest, err := NormalizeDigest(promotion.PackageIndexDigest)
	if err != nil {
		return err
	}
	target, ok := c.EntryByDigest(targetDigest)
	if !ok {
		return errors.New("compatible promotion references unknown package digest")
	}
	if target.Recipe.RuntimeContract != compatibleRuntimeContract || target.Recipe.TemplateSchema != 2 || promotion.RuntimeContract != target.Recipe.RuntimeContract {
		return errors.New("compatible promotion requires docker-sandboxes-v1 and template schema 2")
	}
	if !target.Gates.HostedPass() {
		return errors.New("compatible promotion target has incomplete hosted gates")
	}
	if strings.TrimSpace(promotion.Reason) == "" || promotion.At.IsZero() {
		return errors.New("compatible promotion requires a reason and timestamp")
	}
	if promotion.PreviousDigest != "" {
		previousDigest, normalizeErr := NormalizeDigest(promotion.PreviousDigest)
		if normalizeErr != nil {
			return normalizeErr
		}
		if previousDigest == targetDigest {
			return errors.New("compatible promotion requires distinct package digests")
		}
		previous, previousOK := c.EntryByDigest(previousDigest)
		if !previousOK || previous.Profile != target.Profile || previous.Recipe.RuntimeContract != target.Recipe.RuntimeContract {
			return errors.New("compatible promotion previous package is missing or runtime-incompatible")
		}
	}
	return validateUpstreamWorkflowEvidence(target.Profile, promotion.Upstream, promotion.At)
}

func (c Catalog) hasCompatiblePromotion(packageDigest string) bool {
	for _, promotion := range c.CompatiblePromotions {
		if promotion.PackageIndexDigest == packageDigest && c.validateCompatiblePromotion(promotion) == nil {
			return true
		}
	}
	return false
}

func (c Catalog) stableAliasEligible(packageDigest string) (bool, error) {
	gates, err := c.EffectiveGates(packageDigest)
	if err != nil {
		return false, err
	}
	if gates.AllPass() {
		return true, nil
	}
	return gates.HostedPass() && c.hasCompatiblePromotion(packageDigest), nil
}

// Validate performs structural and policy validation. It does not perform
// network or signature checks; those are intentionally separate publisher
// gates.
func (c Catalog) Validate() error {
	if c.SchemaVersion != LegacyCatalogSchemaVersion && c.SchemaVersion != CatalogSchemaVersion {
		return fmt.Errorf("unsupported prebuilt catalog schema %d", c.SchemaVersion)
	}
	if c.SchemaVersion == LegacyCatalogSchemaVersion && len(c.CompatiblePromotions) != 0 {
		return errors.New("compatible promotions require prebuilt catalog schema 2")
	}
	if c.ArtifactKind != CatalogArtifactKind {
		return fmt.Errorf("unexpected prebuilt catalog artifact kind %q", c.ArtifactKind)
	}
	if strings.TrimSpace(c.PackageRepository) == "" {
		return errors.New("prebuilt catalog package repository is required")
	}
	if c.PackageRepository != DefaultPackageRepository {
		return fmt.Errorf("prebuilt catalog package repository must be canonical GHCR package %s", DefaultPackageRepository)
	}
	for profile, policy := range c.Policies {
		if _, err := NormalizeProfile(profile); err != nil {
			return fmt.Errorf("catalog policy %q: %w", profile, err)
		}
		if policy.WizardDefault && !policy.Enabled {
			return fmt.Errorf("catalog policy %q cannot default a disabled profile", profile)
		}
	}
	seen := make(map[string]struct{}, len(c.Entries))
	for i := range c.Entries {
		entry := c.Entries[i]
		if err := entry.Validate(c.PackageRepository); err != nil {
			return fmt.Errorf("catalog entry %d: %w", i, err)
		}
		if _, exists := seen[entry.PackageIndexDigest]; exists {
			return fmt.Errorf("duplicate package index digest %s", entry.PackageIndexDigest)
		}
		seen[entry.PackageIndexDigest] = struct{}{}
	}
	for i, transition := range c.Transitions {
		if _, err := NormalizeDigest(transition.PackageIndexDigest); err != nil {
			return fmt.Errorf("catalog transition %d digest: %w", i, err)
		}
		if !validStatus(transition.FromStatus) || !validStatus(transition.ToStatus) {
			return fmt.Errorf("catalog transition %d has unsupported status", i)
		}
		if !validTransition(transition.FromStatus, transition.ToStatus) {
			return fmt.Errorf("catalog transition %d cannot move %s to %s", i, transition.FromStatus, transition.ToStatus)
		}
		if transition.At.IsZero() {
			return fmt.Errorf("catalog transition %d has no timestamp", i)
		}
		if _, exists := seen[transition.PackageIndexDigest]; !exists {
			return fmt.Errorf("catalog transition %d references unknown package digest %s", i, transition.PackageIndexDigest)
		}
	}
	acceptanceKeys := make(map[string]struct{}, len(c.Acceptances))
	for i, acceptance := range c.Acceptances {
		if err := acceptance.Validate(); err != nil {
			return fmt.Errorf("catalog acceptance %d: %w", i, err)
		}
		if _, exists := seen[acceptance.PackageIndexDigest]; !exists {
			return fmt.Errorf("catalog acceptance %d references unknown package digest %s", i, acceptance.PackageIndexDigest)
		}
		key := acceptance.PackageIndexDigest + "|" + NormalizePlatform(acceptance.Platform)
		if _, exists := acceptanceKeys[key]; exists {
			return fmt.Errorf("duplicate platform acceptance for %s", key)
		}
		acceptanceKeys[key] = struct{}{}
	}
	for i, promotion := range c.SourceOnlyPromotions {
		if err := c.validateSourceOnlyPromotion(promotion); err != nil {
			return fmt.Errorf("catalog source-only promotion %d: %w", i, err)
		}
		gates, err := c.EffectiveGates(promotion.PackageIndexDigest)
		if err != nil {
			return fmt.Errorf("catalog source-only promotion %d effective gates: %w", i, err)
		}
		if !gates.AllPass() {
			return fmt.Errorf("catalog source-only promotion %d has incomplete effective gates", i)
		}
	}
	compatiblePromotionKeys := make(map[string]struct{}, len(c.CompatiblePromotions))
	for i, promotion := range c.CompatiblePromotions {
		if err := c.validateCompatiblePromotion(promotion); err != nil {
			return fmt.Errorf("catalog compatible promotion %d: %w", i, err)
		}
		if _, exists := compatiblePromotionKeys[promotion.PackageIndexDigest]; exists {
			return fmt.Errorf("duplicate compatible promotion for %s", promotion.PackageIndexDigest)
		}
		compatiblePromotionKeys[promotion.PackageIndexDigest] = struct{}{}
	}
	for key, alias := range c.Aliases {
		profile, err := NormalizeProfile(key)
		if err != nil {
			return fmt.Errorf("catalog alias %q: %w", key, err)
		}
		if alias.Profile != profile || alias.PackageIndexDigest == "" {
			return fmt.Errorf("catalog alias %q has inconsistent profile or digest", key)
		}
		tag, err := AliasTag(profile)
		if err != nil {
			return fmt.Errorf("catalog alias %q tag: %w", key, err)
		}
		if alias.Tag != tag || alias.Reference != c.PackageRepository+":"+tag {
			return fmt.Errorf("catalog alias %q has inconsistent tag/reference", key)
		}
		if alias.Channel != ChannelStable || alias.Status != StatusActive {
			return fmt.Errorf("catalog alias %q has invalid channel/status", key)
		}
		if _, err := NormalizeDigest(alias.PackageIndexDigest); err != nil {
			return fmt.Errorf("catalog alias %q digest: %w", key, err)
		}
		if _, exists := seen[alias.PackageIndexDigest]; !exists {
			return fmt.Errorf("catalog alias %q points to unknown package digest %s", key, alias.PackageIndexDigest)
		}
		status, err := c.EffectiveStatus(alias.PackageIndexDigest)
		if err != nil {
			return fmt.Errorf("catalog alias %q status: %w", key, err)
		}
		if status != StatusActive {
			return fmt.Errorf("catalog alias %q points to package with status %s", key, status)
		}
		eligible, eligibilityErr := c.stableAliasEligible(alias.PackageIndexDigest)
		if eligibilityErr != nil {
			return fmt.Errorf("catalog alias %q eligibility: %w", key, eligibilityErr)
		}
		if !eligible {
			return fmt.Errorf("catalog alias %q points to package without reviewed or compatible hosted promotion evidence", key)
		}
	}
	return nil
}

func (e Entry) Validate(packageRepository string) error {
	if e.SchemaVersion != EntrySchemaVersion {
		return fmt.Errorf("unsupported entry schema %d", e.SchemaVersion)
	}
	if e.ArtifactKind != CatalogArtifactKind {
		return fmt.Errorf("unexpected artifact kind %q", e.ArtifactKind)
	}
	profile, err := NormalizeProfile(e.Profile)
	if err != nil {
		return err
	}
	if e.Profile != profile {
		return fmt.Errorf("profile is not normalized: %q", e.Profile)
	}
	if e.Channel != ChannelPreview && e.Channel != ChannelStable {
		return fmt.Errorf("unsupported channel %q", e.Channel)
	}
	switch e.Status {
	case StatusCandidate, StatusActive, StatusSuperseded, StatusRevoked, StatusCriticalRevoked:
	default:
		return fmt.Errorf("unsupported entry status %q", e.Status)
	}
	if strings.TrimSpace(e.PackageRepository) == "" || e.PackageRepository != packageRepository {
		return fmt.Errorf("package repository mismatch: %q", e.PackageRepository)
	}
	if strings.TrimSpace(e.PackageReference) == "" {
		return errors.New("package reference is required")
	}
	if !strings.HasPrefix(e.PackageReference, packageRepository+"@") {
		return fmt.Errorf("package reference must use canonical repository %s", packageRepository)
	}
	if _, err := NormalizeDigest(e.PackageIndexDigest); err != nil {
		return fmt.Errorf("package index digest: %w", err)
	}
	if _, err := NormalizeDigest(e.Source.IndexDigest); err != nil {
		return fmt.Errorf("source index digest: %w", err)
	}
	if e.Source.Reference == "" || e.Source.SourceTag == "" || e.Source.Repository == "" {
		return errors.New("source repository, tag, and immutable reference are required")
	}
	sourceReferenceDigest, err := referenceDigest(e.Source.Reference)
	if err != nil || sourceReferenceDigest != e.Source.IndexDigest {
		return errors.New("source reference must be repository@source index digest")
	}
	packageReferenceDigest, err := referenceDigest(e.PackageReference)
	if err != nil || packageReferenceDigest != e.PackageIndexDigest {
		return errors.New("package reference must be repository@package index digest")
	}
	if e.Recipe.Digest == "" || e.Recipe.RuntimeContract == "" || e.Recipe.SourceLockDigest == "" || e.Recipe.ToolDigest == "" {
		return errors.New("recipe digest, runtime contract, source lock digest, and tool digest are required")
	}
	if !commitPattern.MatchString(e.Recipe.RecipeRevision) {
		return errors.New("recipeRevision must be a 40- or 64-character hexadecimal commit")
	}
	if e.Recipe.TemplateSchema <= 0 {
		return errors.New("template schema must be positive")
	}
	if e.Runner.Selector == "" || e.Runner.Version == "" {
		return errors.New("runner selector and version are required")
	}
	if len(e.Platforms) == 0 {
		return errors.New("at least one platform publication is required")
	}
	for _, platform := range e.Platforms {
		if platform.Platform == "" {
			return errors.New("platform name is required")
		}
		if _, err := NormalizeDigest(platform.PackageManifestDigest); err != nil {
			return fmt.Errorf("platform %s package digest: %w", platform.Platform, err)
		}
		if _, err := NormalizeDigest(platform.SourceManifestDigest); err != nil {
			return fmt.Errorf("platform %s source digest: %w", platform.Platform, err)
		}
		if e.Status == StatusActive && !platform.Validated {
			return fmt.Errorf("active entry platform %s is not validated", platform.Platform)
		}
	}
	if e.Status == StatusActive && !e.Gates.AllPass() {
		return errors.New("active entry has incomplete publication gates")
	}
	if e.Status == StatusActive {
		if err := validateActivePlatforms(e); err != nil {
			return err
		}
	}
	if e.Evidence.ProvenanceDigest == "" || e.Evidence.SBOMDigest == "" || e.Evidence.AttestationDigest == "" {
		return errors.New("provenance, SBOM, and attestation evidence are required")
	}
	if e.Evidence.AttestationDigest != e.Evidence.ProvenanceDigest {
		return errors.New("attestationDigest must equal provenanceDigest")
	}
	if e.PublishedAt.IsZero() {
		return errors.New("publishedAt is required")
	}
	return nil
}

func validateActivePlatforms(e Entry) error {
	if len(e.Platforms) != 2 {
		return fmt.Errorf("active %s entry must contain exactly amd64 and arm64 platforms, got %d", e.Profile, len(e.Platforms))
	}
	want := map[string]bool{"linux/amd64": false, "linux/arm64": false}
	for _, platform := range e.Platforms {
		name := NormalizePlatform(platform.Platform)
		if _, ok := want[name]; !ok {
			return fmt.Errorf("active %s entry has unsupported platform %q", e.Profile, platform.Platform)
		}
		if want[name] {
			return fmt.Errorf("active %s entry has duplicate platform %q", e.Profile, name)
		}
		want[name] = true
		if !platform.Validated {
			return fmt.Errorf("active %s entry platform %s is not validated", e.Profile, name)
		}
		if expected := e.Source.PlatformDigests[name]; expected == "" || expected != platform.SourceManifestDigest {
			return fmt.Errorf("active %s platform %s source digest does not match source descriptor", e.Profile, name)
		}
	}
	for platform, found := range want {
		if !found {
			return fmt.Errorf("active %s entry is missing platform %s", e.Profile, platform)
		}
		if _, ok := e.Source.PlatformDigests[platform]; !ok {
			return fmt.Errorf("active %s source platform digest %s is missing", e.Profile, platform)
		}
	}
	if len(e.Source.PlatformDigests) != len(want) {
		return fmt.Errorf("active %s source descriptor has extra platform digests", e.Profile)
	}
	return nil
}

func validStatus(status string) bool {
	switch status {
	case StatusCandidate, StatusActive, StatusSuperseded, StatusRevoked, StatusCriticalRevoked:
		return true
	default:
		return false
	}
}

func validTransition(from, to string) bool {
	switch from {
	case StatusCandidate:
		return to == StatusActive || to == StatusRevoked || to == StatusCriticalRevoked
	case StatusActive:
		return to == StatusSuperseded || to == StatusRevoked || to == StatusCriticalRevoked
	case StatusSuperseded:
		return to == StatusActive || to == StatusRevoked || to == StatusCriticalRevoked
	default:
		return false
	}
}

func (a PlatformAcceptance) Validate() error {
	if a.SchemaVersion != legacyAcceptanceRecordSchemaVersion && a.SchemaVersion != profilelessAcceptanceSchemaVersion && a.SchemaVersion != AcceptanceRecordSchemaVersion {
		return fmt.Errorf("unsupported acceptance schema %d", a.SchemaVersion)
	}
	if _, err := NormalizeDigest(a.PackageIndexDigest); err != nil {
		return err
	}
	platform := NormalizePlatform(a.Platform)
	if platform != "linux/amd64" && platform != "linux/arm64" {
		return fmt.Errorf("unsupported acceptance platform %q", a.Platform)
	}
	if a.Platform != platform {
		return fmt.Errorf("acceptance platform is not normalized: %q", a.Platform)
	}
	if a.RunnerGroup != "epar-dev-test" {
		return errors.New("acceptance requires epar-dev-test runner group")
	}
	profile := ProfileAct
	if a.SchemaVersion == AcceptanceRecordSchemaVersion {
		var err error
		profile, err = NormalizeProfile(a.Profile)
		if err != nil {
			return fmt.Errorf("acceptance profile: %w", err)
		}
	} else if strings.TrimSpace(a.Profile) != "" && a.Profile != ProfileAct {
		return errors.New("legacy acceptance records may describe only the Act profile")
	}
	wantLabel := "epar-prebuilt-" + profile + "-" + strings.TrimPrefix(a.PackageIndexDigest, "sha256:")[:12] + "-" + strings.TrimPrefix(platform, "linux/")
	if a.RunnerLabel != wantLabel {
		return fmt.Errorf("acceptance runner label must be %s", wantLabel)
	}
	if a.SchemaVersion == legacyAcceptanceRecordSchemaVersion {
		if !strings.HasPrefix(a.RunnerName, wantLabel+"-") {
			return fmt.Errorf("acceptance runner name must be generated from pool prefix %s", wantLabel)
		}
	} else if strings.TrimSpace(a.RunnerName) != "" {
		return errors.New("acceptance schema 2 and later record runner names on individual workflow runs")
	}
	if _, err := NormalizeDigest(a.ReceiptSHA256); err != nil {
		return fmt.Errorf("acceptance receipt digest: %w", err)
	}
	if !a.ImportReadback || !a.RuntimeValidated || !a.CleanupValidated {
		return errors.New("acceptance import/readback, runtime, and cleanup gates must all pass")
	}
	if strings.TrimSpace(a.ReviewedBy) == "" || a.AcceptedAt.IsZero() {
		return errors.New("acceptance reviewer and timestamp are required")
	}
	wantWorkflows := map[string]bool{"playwright-docker.yml": false, "dockerhub-private-pull.yml": false}
	runnerNames := make(map[string]struct{}, len(wantWorkflows))
	if len(a.WorkflowRuns) != len(wantWorkflows) {
		return errors.New("acceptance must contain exactly the Playwright and private Docker Hub workflow runs")
	}
	for _, run := range a.WorkflowRuns {
		if run.Repository != "solutionforest/ephemeral-action-runner-test" || run.RunID <= 0 || run.Conclusion != "success" {
			return errors.New("acceptance workflow run has invalid repository, id, or conclusion")
		}
		if _, ok := wantWorkflows[run.Workflow]; !ok || wantWorkflows[run.Workflow] {
			return fmt.Errorf("unexpected or duplicate acceptance workflow %q", run.Workflow)
		}
		wantURL := fmt.Sprintf("https://github.com/%s/actions/runs/%d", run.Repository, run.RunID)
		if run.URL != wantURL {
			return fmt.Errorf("acceptance workflow URL must be %s", wantURL)
		}
		if a.SchemaVersion >= profilelessAcceptanceSchemaVersion {
			if !strings.HasPrefix(run.RunnerName, wantLabel+"-") {
				return fmt.Errorf("acceptance workflow runner name must be generated from pool prefix %s", wantLabel)
			}
			if _, duplicate := runnerNames[run.RunnerName]; duplicate {
				return errors.New("acceptance workflows must run on distinct ephemeral runner names")
			}
			runnerNames[run.RunnerName] = struct{}{}
		}
		wantWorkflows[run.Workflow] = true
	}
	return nil
}

// AppendAcceptance records one reviewed platform result. An exact duplicate
// is idempotent; a different record for the same digest/platform is rejected.
func (c *Catalog) AppendAcceptance(acceptance PlatformAcceptance) (bool, error) {
	if c == nil {
		return false, errors.New("nil prebuilt catalog")
	}
	if err := acceptance.Validate(); err != nil {
		return false, err
	}
	entry, ok := c.EntryByDigest(acceptance.PackageIndexDigest)
	if !ok {
		return false, fmt.Errorf("acceptance references unknown package digest %s", acceptance.PackageIndexDigest)
	}
	acceptanceProfile := acceptance.Profile
	if acceptance.SchemaVersion < AcceptanceRecordSchemaVersion {
		acceptanceProfile = ProfileAct
	} else {
		acceptanceProfile, _ = NormalizeProfile(acceptance.Profile)
	}
	if entry.Profile != acceptanceProfile {
		return false, fmt.Errorf("acceptance profile %s does not match package profile %s", acceptanceProfile, entry.Profile)
	}
	for _, existing := range c.Acceptances {
		if existing.PackageIndexDigest != acceptance.PackageIndexDigest || existing.Platform != acceptance.Platform {
			continue
		}
		a, _ := json.Marshal(existing)
		b, _ := json.Marshal(acceptance)
		if string(a) == string(b) {
			return false, nil
		}
		return false, fmt.Errorf("acceptance for %s %s is already recorded with different evidence", acceptance.PackageIndexDigest, acceptance.Platform)
	}
	c.Acceptances = append(c.Acceptances, acceptance)
	return true, nil
}

// EffectiveGates overlays reviewed two-platform acceptance onto immutable
// hosted publication gates without rewriting the package Entry.
func (c Catalog) EffectiveGates(digest string) (GateResults, error) {
	return c.effectiveGates(digest, map[string]bool{})
}

func (c Catalog) HasCompletePlatformAcceptance(digest string) bool {
	accepted := map[string]bool{"linux/amd64": false, "linux/arm64": false}
	for _, acceptance := range c.Acceptances {
		if acceptance.PackageIndexDigest == digest && acceptance.Validate() == nil {
			accepted[acceptance.Platform] = true
		}
	}
	return accepted["linux/amd64"] && accepted["linux/arm64"]
}

func (c Catalog) effectiveGates(digest string, visiting map[string]bool) (GateResults, error) {
	if visiting[digest] {
		return GateResults{}, fmt.Errorf("source-only promotion cycle for %s", digest)
	}
	visiting[digest] = true
	defer delete(visiting, digest)
	entry, ok := c.EntryByDigest(digest)
	if !ok {
		return GateResults{}, fmt.Errorf("unknown package digest %s", digest)
	}
	gates := entry.Gates
	accepted := map[string]bool{"linux/amd64": false, "linux/arm64": false}
	for _, acceptance := range c.Acceptances {
		if acceptance.PackageIndexDigest != digest {
			continue
		}
		if err := acceptance.Validate(); err != nil {
			return GateResults{}, err
		}
		accepted[acceptance.Platform] = true
	}
	if accepted["linux/amd64"] && accepted["linux/arm64"] {
		gates.PlatformsValidated = true
		gates.ImportReadback = true
		gates.RuntimeValidated = true
	}
	for _, promotion := range c.SourceOnlyPromotions {
		if promotion.PackageIndexDigest != digest {
			continue
		}
		baseline, err := c.effectiveGates(promotion.AcceptedFromDigest, visiting)
		if err != nil {
			return GateResults{}, err
		}
		if !baseline.AllPass() || !gates.HostedPass() {
			return GateResults{}, fmt.Errorf("source-only promotion %s has incomplete baseline or hosted gates", digest)
		}
		gates.ImportReadback = true
		gates.RuntimeValidated = true
	}
	return gates, nil
}

func (c Catalog) validateSourceOnlyPromotion(promotion SourceOnlyPromotion) error {
	if _, err := NormalizeDigest(promotion.PackageIndexDigest); err != nil {
		return err
	}
	if _, err := NormalizeDigest(promotion.AcceptedFromDigest); err != nil {
		return err
	}
	if promotion.PackageIndexDigest == promotion.AcceptedFromDigest || promotion.At.IsZero() || strings.TrimSpace(promotion.Reason) == "" {
		return errors.New("source-only promotion requires distinct digests, reason, and timestamp")
	}
	target, targetOK := c.EntryByDigest(promotion.PackageIndexDigest)
	baseline, baselineOK := c.EntryByDigest(promotion.AcceptedFromDigest)
	if !targetOK || !baselineOK {
		return errors.New("source-only promotion references unknown package digest")
	}
	if !SourceTupleUnchanged(baseline, target) || SourceIdentityEqual(baseline, target) {
		return errors.New("source-only promotion changed an EPAR tuple or retained the same upstream identity")
	}
	return nil
}

func (c *Catalog) AppendSourceOnlyPromotion(packageDigest, acceptedFromDigest, reason string, at time.Time) (bool, error) {
	if c == nil {
		return false, errors.New("nil prebuilt catalog")
	}
	promotion := SourceOnlyPromotion{PackageIndexDigest: packageDigest, AcceptedFromDigest: acceptedFromDigest, Reason: strings.TrimSpace(reason), At: at.UTC()}
	if err := c.validateSourceOnlyPromotion(promotion); err != nil {
		return false, err
	}
	baselineGates, err := c.EffectiveGates(acceptedFromDigest)
	if err != nil || !baselineGates.AllPass() {
		return false, errors.New("source-only promotion baseline has not passed complete acceptance")
	}
	targetGates, err := c.EffectiveGates(packageDigest)
	if err != nil || !targetGates.HostedPass() {
		return false, errors.New("source-only promotion target has incomplete hosted gates")
	}
	for _, existing := range c.SourceOnlyPromotions {
		if existing.PackageIndexDigest == packageDigest {
			if existing.AcceptedFromDigest == acceptedFromDigest {
				return false, nil
			}
			return false, fmt.Errorf("source-only promotion for %s already uses another baseline", packageDigest)
		}
	}
	c.SourceOnlyPromotions = append(c.SourceOnlyPromotions, promotion)
	return true, nil
}

// AppendCompatiblePromotion records hosted-only automatic promotion for a
// supported runtime contract without claiming Docker Sandboxes readback or
// runtime validation.
func (c *Catalog) AppendCompatiblePromotion(packageDigest, previousDigest, reason string, upstream UpstreamWorkflowEvidence, at time.Time) (bool, error) {
	if c == nil {
		return false, errors.New("nil prebuilt catalog")
	}
	target, ok := c.EntryByDigest(packageDigest)
	if !ok {
		return false, errors.New("compatible promotion references unknown package digest")
	}
	if previousDigest == "" {
		if _, exists := c.Aliases[target.Profile]; exists {
			return false, fmt.Errorf("compatible promotion for %s omitted the current %s alias digest", packageDigest, target.Profile)
		}
	} else {
		alias, exists := c.Aliases[target.Profile]
		if !exists || alias.PackageIndexDigest != previousDigest {
			return false, fmt.Errorf("compatible promotion for %s does not match the current %s alias", packageDigest, target.Profile)
		}
	}
	promotion := CompatiblePromotion{
		PackageIndexDigest: packageDigest,
		PreviousDigest:     previousDigest,
		RuntimeContract:    target.Recipe.RuntimeContract,
		Upstream:           upstream,
		Reason:             strings.TrimSpace(reason),
		At:                 at.UTC(),
	}
	if err := c.validateCompatiblePromotion(promotion); err != nil {
		return false, err
	}
	for _, existing := range c.CompatiblePromotions {
		if existing.PackageIndexDigest == packageDigest {
			if existing.PreviousDigest == previousDigest && existing.Upstream == upstream {
				return false, nil
			}
			return false, fmt.Errorf("compatible promotion for %s already has different evidence", packageDigest)
		}
	}
	c.SchemaVersion = CatalogSchemaVersion
	c.CompatiblePromotions = append(c.CompatiblePromotions, promotion)
	return true, nil
}

// EffectiveStatus applies the append-only transition ledger to an immutable
// entry. Runtime consumers must use this rather than Entry.Status alone.
func (c Catalog) EffectiveStatus(digest string) (string, error) {
	digest, err := NormalizeDigest(digest)
	if err != nil {
		return "", err
	}
	entry, found := c.EntryByDigest(digest)
	if !found {
		return "", fmt.Errorf("unknown package digest %s", digest)
	}
	status := entry.Status
	for _, transition := range c.Transitions {
		if transition.PackageIndexDigest != digest {
			continue
		}
		if transition.FromStatus != status {
			return "", fmt.Errorf("invalid status transition for %s: expected %s, got %s", digest, status, transition.FromStatus)
		}
		status = transition.ToStatus
	}
	return status, nil
}

// AppendStatusTransition records a status change without mutating the
// immutable publication entry. Repeating the exact last transition is
// idempotent; any other transition must start from the effective status.
func (c *Catalog) AppendStatusTransition(digest, toStatus, reason string, at time.Time) (bool, error) {
	if c == nil {
		return false, errors.New("nil prebuilt catalog")
	}
	digest, err := NormalizeDigest(digest)
	if err != nil {
		return false, err
	}
	if !validStatus(toStatus) || toStatus == StatusCandidate {
		return false, fmt.Errorf("unsupported transition target %q", toStatus)
	}
	fromStatus, err := c.EffectiveStatus(digest)
	if err != nil {
		return false, err
	}
	if fromStatus == toStatus {
		return false, nil
	}
	if !validTransition(fromStatus, toStatus) {
		return false, fmt.Errorf("cannot transition %s from %s to %s", digest, fromStatus, toStatus)
	}
	if at.IsZero() {
		return false, errors.New("status transition timestamp is required")
	}
	c.Transitions = append(c.Transitions, StatusTransition{PackageIndexDigest: digest, FromStatus: fromStatus, ToStatus: toStatus, Reason: strings.TrimSpace(reason), At: at.UTC()})
	return true, nil
}

// Revoke appends a normal or security-critical revocation tombstone. Existing
// local artifacts can decide their fail-closed policy from EffectiveStatus.
func (c *Catalog) Revoke(digest, reason string, critical bool, at time.Time) (bool, error) {
	target := StatusRevoked
	if critical {
		target = StatusCriticalRevoked
	}
	return c.AppendStatusTransition(digest, target, reason, at)
}

// AppendEntry appends an immutable entry, rejecting identity reuse with
// different content. Repeating an identical publication is an idempotent
// no-op and returns false.
func (c *Catalog) AppendEntry(entry Entry) (bool, error) {
	if c == nil {
		return false, errors.New("nil prebuilt catalog")
	}
	if c.SchemaVersion == 0 {
		c.SchemaVersion = LegacyCatalogSchemaVersion
	}
	if c.ArtifactKind == "" {
		c.ArtifactKind = CatalogArtifactKind
	}
	if c.PackageRepository == "" {
		c.PackageRepository = entry.PackageRepository
	}
	if err := entry.Validate(c.PackageRepository); err != nil {
		return false, err
	}
	for _, existing := range c.Entries {
		if existing.PackageIndexDigest != entry.PackageIndexDigest {
			continue
		}
		a, _ := json.Marshal(existing)
		b, _ := json.Marshal(entry)
		if string(a) == string(b) {
			return false, nil
		}
		return false, fmt.Errorf("immutable package digest %s is already recorded with different metadata", entry.PackageIndexDigest)
	}
	c.Entries = append(c.Entries, entry)
	return true, nil
}

// MoveAlias atomically updates one in-memory alias projection. expectedDigest
// is an optimistic-concurrency guard: an empty value allows first publication;
// a nonempty value must match the current alias digest.
func (c *Catalog) MoveAlias(profile, reference, packageDigest, channel, expectedDigest string, now time.Time) error {
	if c == nil {
		return errors.New("nil prebuilt catalog")
	}
	clone := cloneCatalog(*c)
	if err := clone.moveAliasInPlace(profile, reference, packageDigest, channel, expectedDigest, now); err != nil {
		return err
	}
	*c = clone
	return nil
}

func cloneCatalog(value Catalog) Catalog {
	clone := value
	clone.Entries = append([]Entry(nil), value.Entries...)
	clone.Transitions = append([]StatusTransition(nil), value.Transitions...)
	clone.Acceptances = append([]PlatformAcceptance(nil), value.Acceptances...)
	clone.SourceOnlyPromotions = append([]SourceOnlyPromotion(nil), value.SourceOnlyPromotions...)
	clone.CompatiblePromotions = append([]CompatiblePromotion(nil), value.CompatiblePromotions...)
	clone.Aliases = make(map[string]Alias, len(value.Aliases))
	for key, alias := range value.Aliases {
		clone.Aliases[key] = alias
	}
	clone.Policies = make(map[string]ProfilePolicy, len(value.Policies))
	for key, policy := range value.Policies {
		clone.Policies[key] = policy
	}
	return clone
}

func (c *Catalog) moveAliasInPlace(profile, reference, packageDigest, channel, expectedDigest string, now time.Time) error {
	profile, err := NormalizeProfile(profile)
	if err != nil {
		return err
	}
	packageDigest, err = NormalizeDigest(packageDigest)
	if err != nil {
		return err
	}
	if channel != ChannelPreview && channel != ChannelStable {
		return fmt.Errorf("unsupported channel %q", channel)
	}
	if profile == ProfileFull && channel == ChannelStable {
		policy, ok := c.Policies[profile]
		if !ok || !policy.Enabled {
			return fmt.Errorf("stable full alias is disabled until independent gates are promoted")
		}
	}
	if expectedDigest != "" {
		if existing, ok := c.Aliases[profile]; !ok || existing.PackageIndexDigest != expectedDigest {
			return fmt.Errorf("alias %s moved concurrently", profile)
		}
	} else if _, exists := c.Aliases[profile]; exists {
		return fmt.Errorf("alias %s appeared concurrently", profile)
	}
	known := false
	for _, entry := range c.Entries {
		if entry.PackageIndexDigest == packageDigest {
			if entry.Profile != profile {
				return fmt.Errorf("alias %s cannot point to package owned by profile %s", profile, entry.Profile)
			}
			status, statusErr := c.EffectiveStatus(packageDigest)
			if statusErr != nil {
				return statusErr
			}
			if status == StatusRevoked || status == StatusCriticalRevoked {
				return fmt.Errorf("alias %s cannot point to revoked package digest %s", profile, packageDigest)
			}
			eligible, eligibilityErr := c.stableAliasEligible(packageDigest)
			if eligibilityErr != nil {
				return eligibilityErr
			}
			if !eligible {
				return fmt.Errorf("alias %s cannot point to package digest %s without reviewed or compatible hosted promotion evidence", profile, packageDigest)
			}
			if status == StatusCandidate || status == StatusSuperseded {
				if _, err := c.AppendStatusTransition(packageDigest, StatusActive, "promoted by alias move", now); err != nil {
					return err
				}
			}
			known = true
			break
		}
	}
	if !known {
		return fmt.Errorf("alias %s points to unpublished package digest %s", profile, packageDigest)
	}
	if c.Aliases == nil {
		c.Aliases = make(map[string]Alias)
	}
	if existing, ok := c.Aliases[profile]; ok && existing.PackageIndexDigest != packageDigest {
		if status, err := c.EffectiveStatus(existing.PackageIndexDigest); err == nil && status == StatusActive {
			if _, err := c.AppendStatusTransition(existing.PackageIndexDigest, StatusSuperseded, "superseded by alias move", now); err != nil {
				return err
			}
		}
	}
	tag, _ := AliasTag(profile)
	c.Aliases[profile] = Alias{Profile: profile, Tag: tag, Reference: reference, PackageIndexDigest: packageDigest, Channel: channel, Status: StatusActive, UpdatedAt: now.UTC()}
	return nil
}

// MarshalCanonical emits stable JSON suitable for hashing and attaching as a
// catalog OCI artifact. Struct field order is explicit; entries and tools are
// normalized before encoding to avoid scheduler-order churn.
func (c Catalog) MarshalCanonical() ([]byte, error) {
	clone := c
	clone.Entries = append([]Entry(nil), c.Entries...)
	clone.Acceptances = append([]PlatformAcceptance(nil), c.Acceptances...)
	clone.SourceOnlyPromotions = append([]SourceOnlyPromotion(nil), c.SourceOnlyPromotions...)
	clone.CompatiblePromotions = append([]CompatiblePromotion(nil), c.CompatiblePromotions...)
	clone.Aliases = make(map[string]Alias, len(c.Aliases))
	for key, value := range c.Aliases {
		clone.Aliases[key] = value
	}
	sort.SliceStable(clone.Entries, func(i, j int) bool {
		return clone.Entries[i].PackageIndexDigest < clone.Entries[j].PackageIndexDigest
	})
	for i := range clone.Entries {
		clone.Entries[i].Tools = append([]ToolDescriptor(nil), clone.Entries[i].Tools...)
		sort.Slice(clone.Entries[i].Tools, func(a, b int) bool { return clone.Entries[i].Tools[a].Name < clone.Entries[i].Tools[b].Name })
		sort.Slice(clone.Entries[i].Platforms, func(a, b int) bool {
			return clone.Entries[i].Platforms[a].Platform < clone.Entries[i].Platforms[b].Platform
		})
	}
	sort.SliceStable(clone.Acceptances, func(i, j int) bool {
		if clone.Acceptances[i].PackageIndexDigest == clone.Acceptances[j].PackageIndexDigest {
			return clone.Acceptances[i].Platform < clone.Acceptances[j].Platform
		}
		return clone.Acceptances[i].PackageIndexDigest < clone.Acceptances[j].PackageIndexDigest
	})
	sort.SliceStable(clone.SourceOnlyPromotions, func(i, j int) bool {
		return clone.SourceOnlyPromotions[i].PackageIndexDigest < clone.SourceOnlyPromotions[j].PackageIndexDigest
	})
	sort.SliceStable(clone.CompatiblePromotions, func(i, j int) bool {
		return clone.CompatiblePromotions[i].PackageIndexDigest < clone.CompatiblePromotions[j].PackageIndexDigest
	})
	return json.MarshalIndent(clone, "", "  ")
}

// CatalogDigest returns a content digest over canonical catalog JSON.
func (c Catalog) CatalogDigest() (string, error) {
	content, err := c.MarshalCanonical()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
