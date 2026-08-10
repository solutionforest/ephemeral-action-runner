package prebuilt

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestCatalogReferenceConventions(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	if got, err := CatalogMovingReference(DefaultPackageRepository); err != nil || got != DefaultPackageRepository+":catalog-v1" {
		t.Fatalf("moving reference = %q, %v", got, err)
	}
	if got, err := CatalogReference(DefaultPackageRepository, digest); err != nil || got != DefaultPackageRepository+":catalog-v1-pkg-"+strings.Repeat("a", 64) {
		t.Fatalf("immutable reference = %q, %v", got, err)
	}
}

func TestEvidencePolicyRejectsMissingCommitAndAllowsCatalogScheduleIdentity(t *testing.T) {
	policy := EvidencePolicy{Issuer: GitHubActionsIssuer, Repository: "solutionforest/ephemeral-action-runner", Workflow: "docker-sandboxes-images.yml", Ref: "refs/heads/main", AllowedEvents: []string{"schedule", "workflow_dispatch", "push"}}
	if err := policy.Validate(); err != nil {
		t.Fatal(err)
	}
	identity, err := policy.CertificateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if identity.Issuer.Issuer != GitHubActionsIssuer || !strings.Contains(identity.SubjectAlternativeName.SubjectAlternativeName, "docker-sandboxes-images.yml@refs/heads/main") {
		t.Fatalf("identity = %#v", identity)
	}
}

func TestCatalogResolveStatusPreservesCriticalRevocation(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	catalog := Catalog{SchemaVersion: CatalogSchemaVersion, ArtifactKind: CatalogArtifactKind, PackageRepository: DefaultPackageRepository, Entries: []Entry{validEntry(ProfileAct, "a", StatusCandidate)}, Transitions: []StatusTransition{{PackageIndexDigest: digest, FromStatus: StatusCandidate, ToStatus: StatusActive, Reason: "promoted", At: time.Unix(1, 0)}, {PackageIndexDigest: digest, FromStatus: StatusActive, ToStatus: StatusCriticalRevoked, Reason: "compromised", At: time.Unix(2, 0)}}}
	canonicalDigest, err := catalog.CatalogDigest()
	if err != nil {
		t.Fatal(err)
	}
	artifact := CatalogArtifact{Catalog: catalog, Reference: DefaultPackageRepository + ":catalog-v1", ManifestDigest: "sha256:" + strings.Repeat("b", 64), CanonicalDigest: canonicalDigest}
	registry := fakeCatalogRegistry{moving: artifact, immutable: artifact}
	verifier := fakeEvidenceVerifier{}
	resolver := CatalogResolver{Registry: registry, Evidence: verifier, PackageRepository: DefaultPackageRepository, EvidencePolicy: EvidencePolicy{Issuer: GitHubActionsIssuer, Repository: "solutionforest/ephemeral-action-runner", Workflow: "docker-sandboxes-images.yml", Ref: "refs/heads/main", AllowedEvents: []string{"schedule"}}}
	status, err := resolver.ResolveStatus(context.Background(), digest)
	if err != nil {
		t.Fatal(err)
	}
	if status.EffectiveStatus != StatusCriticalRevoked {
		t.Fatalf("effective status = %q", status.EffectiveStatus)
	}
}

func TestCatalogResolveStatusRejectsMissingCatalogEvidence(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	catalog := Catalog{SchemaVersion: CatalogSchemaVersion, ArtifactKind: CatalogArtifactKind, PackageRepository: DefaultPackageRepository, Entries: []Entry{validEntry(ProfileAct, "a", StatusCandidate)}}
	canonicalDigest, err := catalog.CatalogDigest()
	if err != nil {
		t.Fatal(err)
	}
	artifact := CatalogArtifact{Catalog: catalog, ManifestDigest: "sha256:" + strings.Repeat("b", 64), CanonicalDigest: canonicalDigest}
	registry := fakeCatalogRegistry{moving: artifact, immutable: artifact}
	resolver := CatalogResolver{Registry: registry, Evidence: fakeEvidenceVerifier{err: errors.New("missing referrer")}, PackageRepository: DefaultPackageRepository, EvidencePolicy: EvidencePolicy{Issuer: GitHubActionsIssuer, Repository: "solutionforest/ephemeral-action-runner", Workflow: "docker-sandboxes-images.yml", Ref: "refs/heads/main", AllowedEvents: []string{"schedule"}}}
	if _, err := resolver.ResolveStatus(context.Background(), digest); err == nil || !strings.Contains(err.Error(), "missing referrer") {
		t.Fatalf("missing evidence error = %v", err)
	}
}

func TestVerifyCatalogReferenceRejectsMovingAlias(t *testing.T) {
	resolver := CatalogResolver{Registry: fakeCatalogRegistry{}, Evidence: fakeEvidenceVerifier{}, PackageRepository: DefaultPackageRepository, EvidencePolicy: EvidencePolicy{Issuer: GitHubActionsIssuer, Repository: "solutionforest/ephemeral-action-runner", Workflow: "docker-sandboxes-images.yml", Ref: "refs/heads/main", AllowedEvents: []string{"schedule"}}}
	if _, err := resolver.VerifyCatalogReference(context.Background(), DefaultPackageRepository+":catalog-v1"); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("moving catalog reference error = %v", err)
	}
}

func TestVerifyCatalogReferenceRejectsUnsignedExactCatalog(t *testing.T) {
	catalog := Catalog{SchemaVersion: CatalogSchemaVersion, ArtifactKind: CatalogArtifactKind, PackageRepository: DefaultPackageRepository, Entries: []Entry{validEntry(ProfileAct, "a", StatusCandidate)}}
	canonicalDigest, err := catalog.CatalogDigest()
	if err != nil {
		t.Fatal(err)
	}
	artifact := CatalogArtifact{Catalog: catalog, ManifestDigest: "sha256:" + strings.Repeat("b", 64), CanonicalDigest: canonicalDigest}
	ref, err := CatalogReference(DefaultPackageRepository, canonicalDigest)
	if err != nil {
		t.Fatal(err)
	}
	resolver := CatalogResolver{Registry: fakeCatalogRegistry{moving: artifact, immutable: artifact}, Evidence: fakeEvidenceVerifier{err: errors.New("missing catalog attestation")}, PackageRepository: DefaultPackageRepository, EvidencePolicy: EvidencePolicy{Issuer: GitHubActionsIssuer, Repository: "solutionforest/ephemeral-action-runner", Workflow: "docker-sandboxes-images.yml", Ref: "refs/heads/main", AllowedEvents: []string{"schedule"}}}
	if _, err := resolver.VerifyCatalogReference(context.Background(), ref); err == nil || !strings.Contains(err.Error(), "missing catalog attestation") {
		t.Fatalf("unsigned catalog error = %v", err)
	}
}

func TestVerifyCatalogReferenceBootstrapsWithoutMovingCatalog(t *testing.T) {
	catalog := Catalog{SchemaVersion: CatalogSchemaVersion, ArtifactKind: CatalogArtifactKind, PackageRepository: DefaultPackageRepository, Entries: []Entry{validEntry(ProfileAct, "a", StatusCandidate)}}
	canonicalDigest, err := catalog.CatalogDigest()
	if err != nil {
		t.Fatal(err)
	}
	artifact := CatalogArtifact{Catalog: catalog, ManifestDigest: "sha256:" + strings.Repeat("b", 64), CanonicalDigest: canonicalDigest}
	ref, err := CatalogReference(DefaultPackageRepository, canonicalDigest)
	if err != nil {
		t.Fatal(err)
	}
	registry := immutableOnlyCatalogRegistry{artifact: artifact}
	resolver := CatalogResolver{Registry: registry, Evidence: fakeEvidenceVerifier{}, PackageRepository: DefaultPackageRepository, EvidencePolicy: EvidencePolicy{Issuer: GitHubActionsIssuer, Repository: "solutionforest/ephemeral-action-runner", Workflow: "docker-sandboxes-images.yml", Ref: "refs/heads/feature/prebuilt_img", AllowedEvents: []string{"push"}}}
	verified, err := resolver.VerifyCatalogReference(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	status, err := verified.Artifact.Catalog.EffectiveStatus("sha256:" + strings.Repeat("a", 64))
	if err != nil || status != StatusCandidate {
		t.Fatalf("candidate bootstrap status = %q, %v", status, err)
	}
}

func TestResolvePackageUsesExactDigestAfterAliasMove(t *testing.T) {
	oldDigest := "sha256:" + strings.Repeat("a", 64)
	newDigest := "sha256:" + strings.Repeat("b", 64)
	oldEntry := validEntry(ProfileAct, "a", StatusActive)
	newEntry := validEntry(ProfileAct, "b", StatusCandidate)
	catalog := Catalog{SchemaVersion: CatalogSchemaVersion, ArtifactKind: CatalogArtifactKind, PackageRepository: DefaultPackageRepository, Aliases: map[string]Alias{}}
	if _, err := catalog.AppendEntry(oldEntry); err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.AppendEntry(newEntry); err != nil {
		t.Fatal(err)
	}
	if err := catalog.MoveAlias(ProfileAct, DefaultPackageRepository+":act-latest", oldDigest, ChannelStable, "", time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	if err := catalog.MoveAlias(ProfileAct, DefaultPackageRepository+":act-latest", newDigest, ChannelStable, oldDigest, time.Unix(2, 0)); err != nil {
		t.Fatal(err)
	}
	if status, err := catalog.EffectiveStatus(oldDigest); err != nil || status != StatusSuperseded {
		t.Fatalf("old entry status = %q, %v", status, err)
	}
	canonicalDigest, err := catalog.CatalogDigest()
	if err != nil {
		t.Fatal(err)
	}
	artifact := CatalogArtifact{Catalog: catalog, Reference: DefaultPackageRepository + ":catalog-v1", ManifestDigest: "sha256:" + strings.Repeat("d", 64), CanonicalDigest: canonicalDigest}
	resolver := CatalogResolver{Registry: exactPackageRegistry{artifact: artifact}, Evidence: exactPackageEvidence{entry: oldEntry}, PackageRepository: DefaultPackageRepository, EvidencePolicy: EvidencePolicy{Issuer: GitHubActionsIssuer, Repository: "solutionforest/ephemeral-action-runner", Workflow: "docker-sandboxes-images.yml", Ref: "refs/heads/main", AllowedEvents: []string{"schedule"}}}
	resolved, err := resolver.ResolvePackage(context.Background(), oldDigest)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Entry.PackageIndexDigest != oldDigest || resolved.EffectiveStatus != StatusSuperseded {
		t.Fatalf("resolved old package = %s status %s", resolved.Entry.PackageIndexDigest, resolved.EffectiveStatus)
	}
	if resolved.Package.Digest != oldDigest {
		t.Fatalf("resolved descriptor digest = %s", resolved.Package.Digest)
	}
	if _, err := resolver.ResolvePackage(context.Background(), newDigest); err == nil {
		t.Fatal("new alias target unexpectedly resolved with mismatched package evidence")
	}
}

func TestSelectEvidenceReferrersDeduplicatesRerunHistory(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	sbom := "sha256:" + strings.Repeat("b", 64)
	expected := EvidenceDescriptor{ProvenanceDigest: digest, SBOMDigest: sbom, AttestationDigest: digest}
	refs, err := selectEvidenceReferrers([]RegistryReferrer{{Descriptor: RegistryDescriptor{Digest: sbom}, Payload: []byte("sbom")}, {Descriptor: RegistryDescriptor{Digest: digest}, Payload: []byte("prov")}, {Descriptor: RegistryDescriptor{Digest: digest}, Payload: []byte("prov")}, {Descriptor: RegistryDescriptor{Digest: "sha256:" + strings.Repeat("c", 64)}, Payload: []byte("unrelated")}}, expected)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 2 || refs[0].Descriptor.Digest != digest || refs[1].Descriptor.Digest != sbom {
		t.Fatalf("selected referrers = %#v", refs)
	}
}

func TestSelectEvidenceReferrersRejectsConflictingDuplicate(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	expected := EvidenceDescriptor{ProvenanceDigest: digest, SBOMDigest: "sha256:" + strings.Repeat("b", 64), AttestationDigest: digest}
	if _, err := selectEvidenceReferrers([]RegistryReferrer{{Descriptor: RegistryDescriptor{Digest: digest}, Payload: []byte("one")}, {Descriptor: RegistryDescriptor{Digest: digest}, Payload: []byte("two")}}, expected); err == nil {
		t.Fatal("conflicting duplicate referrer unexpectedly accepted")
	}
}

type exactPackageRegistry struct {
	artifact CatalogArtifact
}

func (f exactPackageRegistry) Resolve(_ context.Context, reference string) (ResolvedReference, error) {
	digest, err := referenceDigest(reference)
	if err != nil {
		return ResolvedReference{}, err
	}
	return ResolvedReference{Reference: reference, Repository: DefaultPackageRepository, Digest: digest, MediaType: "application/vnd.oci.image.index.v1+json"}, nil
}

func (f exactPackageRegistry) FetchCatalog(_ context.Context, reference string) (CatalogArtifact, error) {
	artifact := f.artifact
	artifact.Reference = reference
	return artifact, nil
}

func (f exactPackageRegistry) Referrers(context.Context, string) ([]RegistryReferrer, error) {
	return []RegistryReferrer{{Descriptor: RegistryDescriptor{Digest: "sha256:" + strings.Repeat("a", 64)}, Payload: []byte("verified")}, {Descriptor: RegistryDescriptor{Digest: "sha256:" + strings.Repeat("b", 64)}, Payload: []byte("verified")}, {Descriptor: RegistryDescriptor{Digest: "sha256:" + strings.Repeat("c", 64)}, Payload: []byte("verified")}}, nil
}

type exactPackageEvidence struct {
	entry Entry
}

func (f exactPackageEvidence) Verify(_ context.Context, subjectDigest string, _ []RegistryReferrer, _ EvidencePolicy) (EvidenceResult, error) {
	claims := EvidenceClaims{SubjectDigest: subjectDigest, SourceIndexDigest: f.entry.Source.IndexDigest, SourcePlatformDigests: f.entry.Source.PlatformDigests, RecipeDigest: f.entry.Recipe.Digest, RecipeRevision: f.entry.Recipe.RecipeRevision, RuntimeContract: f.entry.Recipe.RuntimeContract, TemplateSchema: f.entry.Recipe.TemplateSchema, RunnerVersion: f.entry.Runner.Version, RunnerAssetDigests: f.entry.Runner.AssetDigests, PlatformPackageDigests: map[string]string{}, ResolvedDependencyDigests: append([]string{f.entry.Source.IndexDigest}, mapValues(f.entry.Source.PlatformDigests)...), SPDXNamespace: "https://spdx.example/catalog", SPDXPackageChecksums: map[string]string{}}
	for name, digest := range expectedSPDXChecksums(f.entry) {
		claims.SPDXPackageChecksums[name] = strings.TrimPrefix(digest, "sha256:")
	}
	for _, platform := range f.entry.Platforms {
		claims.PlatformPackageDigests[platform.Platform] = platform.PackageManifestDigest
	}
	claims.ToolDigests = map[string]string{}
	for _, tool := range f.entry.Tools {
		claims.ToolDigests[tool.Name] = tool.Digest
		claims.ResolvedDependencyDigests = append(claims.ResolvedDependencyDigests, tool.Digest)
	}
	for _, digest := range f.entry.Runner.AssetDigests {
		claims.ResolvedDependencyDigests = append(claims.ResolvedDependencyDigests, digest)
	}
	return EvidenceResult{Provenance: RegistryDescriptor{Digest: f.entry.Evidence.ProvenanceDigest}, SBOM: RegistryDescriptor{Digest: f.entry.Evidence.SBOMDigest}, Signature: RegistryDescriptor{Digest: f.entry.Evidence.AttestationDigest}, Claims: claims}, nil
}

type fakeCatalogRegistry struct {
	moving    CatalogArtifact
	immutable CatalogArtifact
}

type immutableOnlyCatalogRegistry struct {
	artifact CatalogArtifact
}

func (f immutableOnlyCatalogRegistry) Resolve(context.Context, string) (ResolvedReference, error) {
	return ResolvedReference{}, errors.New("unexpected descriptor resolution")
}

func (f immutableOnlyCatalogRegistry) FetchCatalog(_ context.Context, reference string) (CatalogArtifact, error) {
	if strings.HasSuffix(reference, ":"+CatalogMovingTag) {
		return CatalogArtifact{}, errors.New("moving catalog does not exist")
	}
	artifact := f.artifact
	artifact.Reference = reference
	return artifact, nil
}

func (f immutableOnlyCatalogRegistry) Referrers(context.Context, string) ([]RegistryReferrer, error) {
	return []RegistryReferrer{{Descriptor: RegistryDescriptor{Digest: "sha256:" + strings.Repeat("c", 64)}, Payload: []byte("verified")}}, nil
}

func (f fakeCatalogRegistry) Resolve(context.Context, string) (ResolvedReference, error) {
	return ResolvedReference{}, nil
}

func (f fakeCatalogRegistry) FetchCatalog(_ context.Context, reference string) (CatalogArtifact, error) {
	if strings.Contains(reference, "catalog-v1-pkg-") {
		return f.immutable, nil
	}
	return f.moving, nil
}

func (f fakeCatalogRegistry) Referrers(context.Context, string) ([]RegistryReferrer, error) {
	return []RegistryReferrer{{Descriptor: RegistryDescriptor{Digest: "sha256:" + strings.Repeat("c", 64)}, Payload: []byte("verified")}}, nil
}

type fakeEvidenceVerifier struct {
	err error
}

func (f fakeEvidenceVerifier) Verify(context.Context, string, []RegistryReferrer, EvidencePolicy) (EvidenceResult, error) {
	if f.err != nil {
		return EvidenceResult{}, f.err
	}
	digest := "sha256:" + strings.Repeat("c", 64)
	return EvidenceResult{Provenance: RegistryDescriptor{Digest: digest}, SBOM: RegistryDescriptor{Digest: digest}, Signature: RegistryDescriptor{Digest: digest}}, nil
}
