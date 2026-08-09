package prebuilt

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ResolvedCatalog is the runtime-safe result of resolving the moving catalog
// alias and its selected package alias. All statuses were evaluated after
// cryptographic referrer verification.
type ResolvedCatalog struct {
	Catalog          Catalog
	CatalogReference string
	CatalogDigest    string
	CatalogManifest  string
	Alias            Alias
	Entry            Entry
	PackageEvidence  EvidenceResult
	CatalogEvidence  EvidenceResult
}

// CatalogStatus is the signed status projection for an exact package digest.
// It remains available when an alias has been removed after revocation, so a
// runtime due-check cannot mistake a missing alias for permission to continue
// using a critical-revoked receipt.
type CatalogStatus struct {
	Catalog          Catalog
	CatalogReference string
	CatalogDigest    string
	CatalogManifest  string
	PackageDigest    string
	EffectiveStatus  string
	Entry            Entry
}

// ResolvedPackage is the exact-digest runtime result. Unlike Resolve, it does
// not require a moving profile alias to point at the package: an immutable
// receipt may continue to use a superseded package after the alias advances.
// Candidate and revoked entries are rejected; active and superseded entries
// remain usable subject to the caller's normal platform policy.
type ResolvedPackage struct {
	Catalog          Catalog
	CatalogReference string
	CatalogDigest    string
	CatalogManifest  string
	Entry            Entry
	Package          ResolvedReference
	PackageEvidence  EvidenceResult
	CatalogEvidence  EvidenceResult
	EffectiveStatus  string
}

// VerifiedCatalog is the exact immutable catalog artifact and its verified
// Sigstore attestation. Workflows use this before moving catalog-v1 so a
// newly-published catalog cannot be replaced by an unsigned or different
// descriptor during a race.
type VerifiedCatalog struct {
	Artifact CatalogArtifact
	Evidence EvidenceResult
}

// VerifiedPackage is an exact immutable package descriptor plus its strict
// package SLSA/SPDX evidence. It can be produced for a candidate Entry before
// that entry is appended to the signed catalog.
type VerifiedPackage struct {
	Entry    Entry
	Package  ResolvedReference
	Evidence EvidenceResult
}

// CatalogResolver is the production consumer boundary. Registry and Evidence
// must be non-nil; no local catalog, workflow log, or unsigned OCI label is a
// fallback when a registry check fails.
type CatalogResolver struct {
	Registry          CatalogRegistry
	Evidence          EvidenceVerifier
	PackageRepository string
	EvidencePolicy    EvidencePolicy
}

// Resolve resolves one profile's moving package alias from the signed
// catalog. The moving catalog tag is re-resolved first, then the immutable
// catalog tag derived from canonical JSON is fetched and required to point at
// the same manifest descriptor.
func (r CatalogResolver) Resolve(ctx context.Context, profile string) (ResolvedCatalog, error) {
	if r.Registry == nil {
		return ResolvedCatalog{}, errors.New("catalog registry is required")
	}
	if r.Evidence == nil {
		return ResolvedCatalog{}, errors.New("catalog evidence verifier is required")
	}
	repository := strings.TrimSpace(r.PackageRepository)
	if repository == "" {
		repository = DefaultPackageRepository
	}
	profile, err := NormalizeProfile(profile)
	if err != nil {
		return ResolvedCatalog{}, err
	}
	if err := r.EvidencePolicy.Validate(); err != nil {
		return ResolvedCatalog{}, err
	}
	movingRef, err := CatalogMovingReference(repository)
	if err != nil {
		return ResolvedCatalog{}, err
	}
	moving, err := r.Registry.FetchCatalog(ctx, movingRef)
	if err != nil {
		return ResolvedCatalog{}, fmt.Errorf("resolve moving catalog %s: %w", movingRef, err)
	}
	immutableRef, err := CatalogReference(repository, moving.CanonicalDigest)
	if err != nil {
		return ResolvedCatalog{}, err
	}
	immutable, err := r.Registry.FetchCatalog(ctx, immutableRef)
	if err != nil {
		return ResolvedCatalog{}, fmt.Errorf("resolve immutable catalog %s: %w", immutableRef, err)
	}
	if immutable.CanonicalDigest != moving.CanonicalDigest {
		return ResolvedCatalog{}, fmt.Errorf("catalog digest mismatch: moving %s, immutable %s", moving.CanonicalDigest, immutable.CanonicalDigest)
	}
	if immutable.ManifestDigest != moving.ManifestDigest {
		return ResolvedCatalog{}, fmt.Errorf("catalog alias moved between descriptor digests: moving %s, immutable %s", moving.ManifestDigest, immutable.ManifestDigest)
	}
	catalog := immutable.Catalog
	if err := catalog.Validate(); err != nil {
		return ResolvedCatalog{}, fmt.Errorf("validate signed catalog: %w", err)
	}
	alias, ok := catalog.Aliases[profile]
	if !ok {
		return ResolvedCatalog{}, fmt.Errorf("catalog alias %s is missing", profile)
	}
	aliasTag, err := AliasTag(profile)
	if err != nil {
		return ResolvedCatalog{}, err
	}
	if alias.Tag != aliasTag || alias.Reference != repository+":"+aliasTag || alias.Profile != profile || alias.Channel != ChannelStable {
		return ResolvedCatalog{}, fmt.Errorf("catalog alias %s has an unexpected reference", profile)
	}
	status, err := catalog.EffectiveStatus(alias.PackageIndexDigest)
	if err != nil {
		return ResolvedCatalog{}, fmt.Errorf("catalog alias %s status: %w", profile, err)
	}
	if status != StatusActive {
		return ResolvedCatalog{}, fmt.Errorf("catalog alias %s is not active: %s", profile, status)
	}
	entry, ok := catalog.EntryByDigest(alias.PackageIndexDigest)
	if !ok {
		return ResolvedCatalog{}, fmt.Errorf("catalog alias %s points to missing entry %s", profile, alias.PackageIndexDigest)
	}
	if entry.Profile != profile || entry.Channel != ChannelStable {
		return ResolvedCatalog{}, fmt.Errorf("catalog entry %s does not match alias %s", entry.PackageIndexDigest, profile)
	}
	packageReferrers, err := r.Registry.Referrers(ctx, entry.PackageReference)
	if err != nil {
		return ResolvedCatalog{}, fmt.Errorf("resolve package evidence %s: %w", entry.PackageReference, err)
	}
	packageReferrers, err = selectEvidenceReferrers(packageReferrers, entry.Evidence)
	if err != nil {
		return ResolvedCatalog{}, fmt.Errorf("select package evidence %s: %w", entry.PackageIndexDigest, err)
	}
	packagePolicy := r.EvidencePolicy
	packagePolicy.Commit = entry.Recipe.RecipeRevision
	packageEvidence, err := r.Evidence.Verify(ctx, entry.PackageIndexDigest, packageReferrers, packagePolicy)
	if err != nil {
		return ResolvedCatalog{}, fmt.Errorf("verify package evidence %s: %w", entry.PackageIndexDigest, err)
	}
	if err := matchEvidence(entry.Evidence, packageEvidence); err != nil {
		return ResolvedCatalog{}, fmt.Errorf("catalog evidence does not match package referrers: %w", err)
	}
	if err := matchEvidenceClaims(entry, packageEvidence.Claims); err != nil {
		return ResolvedCatalog{}, fmt.Errorf("signed predicate claims do not match catalog entry: %w", err)
	}
	catalogSubject := repository + "@" + moving.ManifestDigest
	catalogReferrers, err := r.Registry.Referrers(ctx, catalogSubject)
	if err != nil {
		return ResolvedCatalog{}, fmt.Errorf("resolve catalog evidence %s: %w", catalogSubject, err)
	}
	catalogEvidence, err := r.verifyCatalogEvidence(ctx, moving.ManifestDigest, catalogReferrers)
	if err != nil {
		return ResolvedCatalog{}, fmt.Errorf("verify catalog evidence %s: %w", moving.ManifestDigest, err)
	}
	return ResolvedCatalog{
		Catalog:          catalog,
		CatalogReference: immutableRef,
		CatalogDigest:    moving.CanonicalDigest,
		CatalogManifest:  moving.ManifestDigest,
		Alias:            alias,
		Entry:            entry,
		PackageEvidence:  packageEvidence,
		CatalogEvidence:  catalogEvidence,
	}, nil
}

// ResolveStatus verifies the signed moving catalog and returns the effective
// append-only status for an exact package index digest, independent of whether
// a profile alias currently points at it. Revoked and critical-revoked states
// are therefore observable and must be enforced by runtime callers.
func (r CatalogResolver) ResolveStatus(ctx context.Context, packageDigest string) (CatalogStatus, error) {
	if r.Registry == nil {
		return CatalogStatus{}, errors.New("catalog registry is required")
	}
	if r.Evidence == nil {
		return CatalogStatus{}, errors.New("catalog evidence verifier is required")
	}
	if _, err := NormalizeDigest(packageDigest); err != nil {
		return CatalogStatus{}, err
	}
	repository := strings.TrimSpace(r.PackageRepository)
	if repository == "" {
		repository = DefaultPackageRepository
	}
	if err := r.EvidencePolicy.Validate(); err != nil {
		return CatalogStatus{}, err
	}
	movingRef, err := CatalogMovingReference(repository)
	if err != nil {
		return CatalogStatus{}, err
	}
	moving, err := r.Registry.FetchCatalog(ctx, movingRef)
	if err != nil {
		return CatalogStatus{}, fmt.Errorf("resolve moving catalog %s: %w", movingRef, err)
	}
	immutableRef, err := CatalogReference(repository, moving.CanonicalDigest)
	if err != nil {
		return CatalogStatus{}, err
	}
	immutable, err := r.Registry.FetchCatalog(ctx, immutableRef)
	if err != nil {
		return CatalogStatus{}, fmt.Errorf("resolve immutable catalog %s: %w", immutableRef, err)
	}
	if immutable.CanonicalDigest != moving.CanonicalDigest || immutable.ManifestDigest != moving.ManifestDigest {
		return CatalogStatus{}, errors.New("moving catalog does not match immutable catalog descriptor")
	}
	if err := immutable.Catalog.Validate(); err != nil {
		return CatalogStatus{}, fmt.Errorf("validate signed catalog: %w", err)
	}
	subject := repository + "@" + moving.ManifestDigest
	referrers, err := r.Registry.Referrers(ctx, subject)
	if err != nil {
		return CatalogStatus{}, fmt.Errorf("resolve catalog evidence %s: %w", subject, err)
	}
	if _, err := r.verifyCatalogEvidence(ctx, moving.ManifestDigest, referrers); err != nil {
		return CatalogStatus{}, fmt.Errorf("verify catalog evidence %s: %w", moving.ManifestDigest, err)
	}
	status, err := immutable.Catalog.EffectiveStatus(packageDigest)
	if err != nil {
		return CatalogStatus{}, err
	}
	entry, ok := immutable.Catalog.EntryByDigest(packageDigest)
	if !ok {
		return CatalogStatus{}, fmt.Errorf("package digest %s is not present in signed catalog", packageDigest)
	}
	return CatalogStatus{
		Catalog:          immutable.Catalog,
		CatalogReference: immutableRef,
		CatalogDigest:    moving.CanonicalDigest,
		CatalogManifest:  moving.ManifestDigest,
		PackageDigest:    packageDigest,
		EffectiveStatus:  status,
		Entry:            entry,
	}, nil
}

// VerifyCatalogReference verifies an immutable catalog tag or repository@
// manifest-digest reference. A moving catalog-v1 tag is intentionally
// rejected; callers that need the moving projection should use Resolve or
// ResolveStatus, which re-fetch and compare its immutable companion.
func (r CatalogResolver) VerifyCatalogReference(ctx context.Context, reference string) (VerifiedCatalog, error) {
	if r.Registry == nil {
		return VerifiedCatalog{}, errors.New("catalog registry is required")
	}
	if r.Evidence == nil {
		return VerifiedCatalog{}, errors.New("catalog evidence verifier is required")
	}
	reference = strings.TrimSpace(reference)
	if reference == "" {
		return VerifiedCatalog{}, errors.New("catalog reference is required")
	}
	if strings.HasSuffix(reference, ":"+CatalogMovingTag) {
		return VerifiedCatalog{}, errors.New("catalog reference must be immutable, not catalog-v1")
	}
	repository := strings.TrimSpace(r.PackageRepository)
	if repository == "" {
		repository = DefaultPackageRepository
	}
	if err := r.EvidencePolicy.Validate(); err != nil {
		return VerifiedCatalog{}, err
	}
	artifact, err := r.Registry.FetchCatalog(ctx, reference)
	if err != nil {
		return VerifiedCatalog{}, fmt.Errorf("fetch immutable catalog %s: %w", reference, err)
	}
	if artifact.Catalog.PackageRepository != repository {
		return VerifiedCatalog{}, fmt.Errorf("catalog package repository expected %s got %s", repository, artifact.Catalog.PackageRepository)
	}
	if strings.Contains(reference, ":"+CatalogMovingTag+"-pkg-") {
		canonicalReference, err := CatalogReference(repository, artifact.CanonicalDigest)
		if err != nil {
			return VerifiedCatalog{}, err
		}
		if reference != canonicalReference {
			return VerifiedCatalog{}, fmt.Errorf("immutable catalog tag %s does not match canonical digest reference %s", reference, canonicalReference)
		}
	}
	if strings.Contains(reference, "@") {
		digest, err := referenceDigest(reference)
		if err != nil {
			return VerifiedCatalog{}, err
		}
		if digest != artifact.ManifestDigest {
			return VerifiedCatalog{}, fmt.Errorf("catalog manifest digest expected %s got %s", digest, artifact.ManifestDigest)
		}
	} else if !strings.Contains(reference, "-pkg-") {
		return VerifiedCatalog{}, errors.New("catalog reference must use an immutable catalog-v1-pkg tag or @sha256 digest")
	}
	subject := repository + "@" + artifact.ManifestDigest
	referrers, err := r.Registry.Referrers(ctx, subject)
	if err != nil {
		return VerifiedCatalog{}, fmt.Errorf("resolve catalog evidence %s: %w", subject, err)
	}
	evidence, err := r.verifyCatalogEvidence(ctx, artifact.ManifestDigest, referrers)
	if err != nil {
		return VerifiedCatalog{}, fmt.Errorf("verify catalog evidence %s: %w", artifact.ManifestDigest, err)
	}
	return VerifiedCatalog{Artifact: artifact, Evidence: evidence}, nil
}

// ResolvePackage verifies one exact package index digest against the signed
// moving catalog and immutable registry descriptor. It is the primitive used
// for receipt pin checks and for candidate package evidence gates; it never
// follows a mutable package alias and therefore remains stable after a later
// alias move.
func (r CatalogResolver) ResolvePackage(ctx context.Context, packageDigest string) (ResolvedPackage, error) {
	if r.Registry == nil {
		return ResolvedPackage{}, errors.New("catalog registry is required")
	}
	if r.Evidence == nil {
		return ResolvedPackage{}, errors.New("catalog evidence verifier is required")
	}
	packageDigest, err := NormalizeDigest(packageDigest)
	if err != nil {
		return ResolvedPackage{}, err
	}
	repository := strings.TrimSpace(r.PackageRepository)
	if repository == "" {
		repository = DefaultPackageRepository
	}
	if err := r.EvidencePolicy.Validate(); err != nil {
		return ResolvedPackage{}, err
	}
	movingRef, err := CatalogMovingReference(repository)
	if err != nil {
		return ResolvedPackage{}, err
	}
	moving, err := r.Registry.FetchCatalog(ctx, movingRef)
	if err != nil {
		return ResolvedPackage{}, fmt.Errorf("resolve moving catalog %s: %w", movingRef, err)
	}
	immutableRef, err := CatalogReference(repository, moving.CanonicalDigest)
	if err != nil {
		return ResolvedPackage{}, err
	}
	immutable, err := r.Registry.FetchCatalog(ctx, immutableRef)
	if err != nil {
		return ResolvedPackage{}, fmt.Errorf("resolve immutable catalog %s: %w", immutableRef, err)
	}
	if immutable.CanonicalDigest != moving.CanonicalDigest || immutable.ManifestDigest != moving.ManifestDigest {
		return ResolvedPackage{}, errors.New("moving catalog does not match immutable catalog descriptor")
	}
	catalog := immutable.Catalog
	if err := catalog.Validate(); err != nil {
		return ResolvedPackage{}, fmt.Errorf("validate signed catalog: %w", err)
	}
	status, err := catalog.EffectiveStatus(packageDigest)
	if err != nil {
		return ResolvedPackage{}, err
	}
	if status != StatusActive && status != StatusSuperseded {
		return ResolvedPackage{}, fmt.Errorf("package %s has effective status %s", packageDigest, status)
	}
	entry, ok := catalog.EntryByDigest(packageDigest)
	if !ok {
		return ResolvedPackage{}, fmt.Errorf("package digest %s is not present in signed catalog", packageDigest)
	}
	verifiedPackage, err := r.VerifyPackage(ctx, packageDigest, entry)
	if err != nil {
		return ResolvedPackage{}, err
	}
	catalogSubject := repository + "@" + moving.ManifestDigest
	catalogReferrers, err := r.Registry.Referrers(ctx, catalogSubject)
	if err != nil {
		return ResolvedPackage{}, fmt.Errorf("resolve catalog evidence %s: %w", catalogSubject, err)
	}
	catalogEvidence, err := r.verifyCatalogEvidence(ctx, moving.ManifestDigest, catalogReferrers)
	if err != nil {
		return ResolvedPackage{}, fmt.Errorf("verify catalog evidence %s: %w", moving.ManifestDigest, err)
	}
	return ResolvedPackage{Catalog: catalog, CatalogReference: immutableRef, CatalogDigest: moving.CanonicalDigest, CatalogManifest: moving.ManifestDigest, Entry: entry, Package: verifiedPackage.Package, PackageEvidence: verifiedPackage.Evidence, CatalogEvidence: catalogEvidence, EffectiveStatus: status}, nil
}

// VerifyPackage validates an exact package descriptor and strict package
// evidence against the supplied entry. The entry may be a candidate and need
// not be present in a catalog yet; callers still must publish it through the
// append-only promotion path before making it discoverable.
func (r CatalogResolver) VerifyPackage(ctx context.Context, packageDigest string, entry Entry) (VerifiedPackage, error) {
	if r.Registry == nil {
		return VerifiedPackage{}, errors.New("catalog registry is required")
	}
	if r.Evidence == nil {
		return VerifiedPackage{}, errors.New("catalog evidence verifier is required")
	}
	packageDigest, err := NormalizeDigest(packageDigest)
	if err != nil {
		return VerifiedPackage{}, err
	}
	repository := strings.TrimSpace(r.PackageRepository)
	if repository == "" {
		repository = DefaultPackageRepository
	}
	if err := r.EvidencePolicy.Validate(); err != nil {
		return VerifiedPackage{}, err
	}
	if err := entry.Validate(repository); err != nil {
		return VerifiedPackage{}, fmt.Errorf("validate package entry: %w", err)
	}
	if entry.PackageIndexDigest != packageDigest {
		return VerifiedPackage{}, fmt.Errorf("entry package digest expected %s got %s", packageDigest, entry.PackageIndexDigest)
	}
	packageReferenceDigest, err := referenceDigest(entry.PackageReference)
	if err != nil {
		return VerifiedPackage{}, fmt.Errorf("catalog package reference is not immutable: %w", err)
	}
	if packageReferenceDigest != packageDigest {
		return VerifiedPackage{}, fmt.Errorf("catalog package reference digest expected %s got %s", packageDigest, packageReferenceDigest)
	}
	packageDescriptor, err := r.Registry.Resolve(ctx, entry.PackageReference)
	if err != nil {
		return VerifiedPackage{}, fmt.Errorf("resolve immutable package %s: %w", entry.PackageReference, err)
	}
	if packageDescriptor.Digest != packageDigest {
		return VerifiedPackage{}, fmt.Errorf("immutable package descriptor expected %s got %s", packageDigest, packageDescriptor.Digest)
	}
	packageReferrers, err := r.Registry.Referrers(ctx, entry.PackageReference)
	if err != nil {
		return VerifiedPackage{}, fmt.Errorf("resolve package evidence %s: %w", entry.PackageReference, err)
	}
	packageReferrers, err = selectEvidenceReferrers(packageReferrers, entry.Evidence)
	if err != nil {
		return VerifiedPackage{}, fmt.Errorf("select package evidence %s: %w", packageDigest, err)
	}
	packagePolicy := r.EvidencePolicy
	packagePolicy.Commit = entry.Recipe.RecipeRevision
	packageEvidence, err := r.Evidence.Verify(ctx, packageDigest, packageReferrers, packagePolicy)
	if err != nil {
		return VerifiedPackage{}, fmt.Errorf("verify package evidence %s: %w", packageDigest, err)
	}
	if err := matchEvidence(entry.Evidence, packageEvidence); err != nil {
		return VerifiedPackage{}, fmt.Errorf("catalog evidence does not match package referrers: %w", err)
	}
	if err := matchEvidenceClaims(entry, packageEvidence.Claims); err != nil {
		return VerifiedPackage{}, fmt.Errorf("signed predicate claims do not match catalog entry: %w", err)
	}
	return VerifiedPackage{Entry: entry, Package: packageDescriptor, Evidence: packageEvidence}, nil
}

func (r CatalogResolver) verifyCatalogEvidence(ctx context.Context, subjectDigest string, referrers []RegistryReferrer) (EvidenceResult, error) {
	if verifier, ok := r.Evidence.(CatalogEvidenceVerifier); ok {
		return verifier.VerifyCatalog(ctx, subjectDigest, referrers, r.EvidencePolicy)
	}
	// Test and enterprise implementations written before the catalog-specific
	// interface remain compatible, but production SigstoreEvidenceVerifier
	// always takes the relaxed catalog path above.
	return r.Evidence.Verify(ctx, subjectDigest, referrers, r.EvidencePolicy)
}

// selectEvidenceReferrers narrows the registry's append-only referrer history
// to the exact bundle identities committed in an Entry. Re-attestation can
// legitimately repeat an immutable bundle descriptor; identical repeats are
// deduplicated, while conflicting payloads for one digest fail closed.
func selectEvidenceReferrers(referrers []RegistryReferrer, expected EvidenceDescriptor) ([]RegistryReferrer, error) {
	want := []string{expected.ProvenanceDigest, expected.SBOMDigest}
	if expected.AttestationDigest != "" && expected.AttestationDigest != expected.ProvenanceDigest && expected.AttestationDigest != expected.SBOMDigest {
		want = append(want, expected.AttestationDigest)
	}
	byDigest := make(map[string]RegistryReferrer, len(want))
	for _, referrer := range referrers {
		for _, digest := range want {
			if referrer.Descriptor.Digest != digest {
				continue
			}
			if prior, ok := byDigest[digest]; ok {
				if string(prior.Payload) != string(referrer.Payload) {
					return nil, fmt.Errorf("referrer %s has conflicting duplicate payloads", digest)
				}
				continue
			}
			byDigest[digest] = referrer
		}
	}
	selected := make([]RegistryReferrer, 0, len(want))
	for _, digest := range want {
		referrer, ok := byDigest[digest]
		if !ok {
			return nil, fmt.Errorf("required evidence referrer %s is missing", digest)
		}
		selected = append(selected, referrer)
	}
	return selected, nil
}

func referenceDigest(reference string) (string, error) {
	idx := strings.LastIndex(strings.TrimSpace(reference), "@")
	if idx <= 0 || idx == len(strings.TrimSpace(reference))-1 {
		return "", errors.New("immutable registry reference must contain repository@digest")
	}
	return NormalizeDigest(strings.TrimSpace(reference)[idx+1:])
}

func matchEvidence(expected EvidenceDescriptor, actual EvidenceResult) error {
	if expected.ProvenanceDigest == "" || expected.SBOMDigest == "" || expected.AttestationDigest == "" {
		return errors.New("catalog entry has incomplete evidence digest metadata")
	}
	if expected.ProvenanceDigest != actual.Provenance.Digest {
		return fmt.Errorf("provenance digest expected %s got %s", expected.ProvenanceDigest, actual.Provenance.Digest)
	}
	if expected.SBOMDigest != actual.SBOM.Digest {
		return fmt.Errorf("SBOM digest expected %s got %s", expected.SBOMDigest, actual.SBOM.Digest)
	}
	if expected.AttestationDigest != actual.Signature.Digest {
		return fmt.Errorf("attestation digest expected %s got %s", expected.AttestationDigest, actual.Signature.Digest)
	}
	return nil
}

func matchEvidenceClaims(entry Entry, claims EvidenceClaims) error {
	if claims.SubjectDigest != entry.PackageIndexDigest {
		return fmt.Errorf("subject digest expected %s got %s", entry.PackageIndexDigest, claims.SubjectDigest)
	}
	if claims.SourceIndexDigest != entry.Source.IndexDigest {
		return fmt.Errorf("source index digest expected %s got %s", entry.Source.IndexDigest, claims.SourceIndexDigest)
	}
	if !stringMapEqual(claims.SourcePlatformDigests, entry.Source.PlatformDigests) {
		return errors.New("source platform claims do not match catalog entry")
	}
	if claims.RecipeDigest != entry.Recipe.Digest || claims.RecipeRevision != entry.Recipe.RecipeRevision || claims.RuntimeContract != entry.Recipe.RuntimeContract || claims.TemplateSchema != entry.Recipe.TemplateSchema {
		return errors.New("recipe/runtime claims do not match catalog entry")
	}
	if claims.RunnerVersion != entry.Runner.Version || !stringMapEqual(claims.RunnerAssetDigests, entry.Runner.AssetDigests) {
		return errors.New("runner claims do not match catalog entry")
	}
	wantTools := make(map[string]string, len(entry.Tools))
	for _, tool := range entry.Tools {
		wantTools[tool.Name] = tool.Digest
	}
	if !stringMapEqual(claims.ToolDigests, wantTools) {
		return errors.New("tool claims do not match catalog entry")
	}
	wantPlatforms := make(map[string]string, len(entry.Platforms))
	for _, platform := range entry.Platforms {
		wantPlatforms[platform.Platform] = platform.PackageManifestDigest
	}
	if !stringMapEqual(claims.PlatformPackageDigests, wantPlatforms) {
		return errors.New("platform claims do not match catalog entry")
	}
	for _, digest := range append([]string{entry.Source.IndexDigest}, mapValues(entry.Source.PlatformDigests)...) {
		if !containsString(claims.ResolvedDependencyDigests, digest) {
			return fmt.Errorf("resolved dependency claims omit %s", digest)
		}
	}
	if claims.SPDXNamespace == "" || len(claims.SPDXPackageChecksums) == 0 {
		return errors.New("SPDX claims are incomplete")
	}
	for name, digest := range expectedSPDXChecksums(entry) {
		actual, ok := claims.SPDXPackageChecksums[name]
		if !ok || !strings.EqualFold(actual, strings.TrimPrefix(digest, "sha256:")) {
			return fmt.Errorf("SPDX package checksum %s does not match catalog identity", name)
		}
	}
	return nil
}

func expectedSPDXChecksums(entry Entry) map[string]string {
	expected := map[string]string{
		"epar-package-index":  entry.PackageIndexDigest,
		"epar-runtime-config": entry.Recipe.Digest,
	}
	for _, platform := range entry.Platforms {
		name := "epar-platform-" + strings.ReplaceAll(NormalizePlatform(platform.Platform), "/", "-")
		expected[name] = platform.PackageManifestDigest
	}
	return expected
}

func stringMapEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for key, value := range a {
		if b[key] != value {
			return false
		}
	}
	return true
}
