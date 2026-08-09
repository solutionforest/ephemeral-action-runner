package prebuilt

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/fulcio/certificate"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/tuf"
	"github.com/sigstore/sigstore-go/pkg/verify"
	structpb "google.golang.org/protobuf/types/known/structpb"
)

const (
	GitHubActionsIssuer = "https://token.actions.githubusercontent.com"
	SLSAProvenanceV02   = "https://slsa.dev/provenance/v0.2"
	SLSAProvenanceV1    = "https://slsa.dev/provenance/v1"
	SPDXPredicate       = "https://spdx.dev/Document"
)

var commitPattern = regexp.MustCompile(`^(?:[0-9a-fA-F]{40}|[0-9a-fA-F]{64})$`)

// EvidencePolicy is the exact GitHub Actions identity allowed to publish an
// EPAR package or catalog. All fields are required; broad repository-only
// identity matching is deliberately unsupported.
type EvidencePolicy struct {
	Issuer        string   `json:"issuer"`
	Repository    string   `json:"repository"`
	Workflow      string   `json:"workflow"`
	Ref           string   `json:"ref"`
	Commit        string   `json:"commit,omitempty"`
	Event         string   `json:"event,omitempty"`
	AllowedEvents []string `json:"allowedEvents,omitempty"`
}

func (p EvidencePolicy) Validate() error {
	if strings.TrimSpace(p.Issuer) == "" {
		return errors.New("evidence issuer is required")
	}
	if strings.TrimSpace(p.Repository) == "" || strings.ContainsAny(p.Repository, " @") {
		return errors.New("evidence repository is required")
	}
	if strings.TrimSpace(p.Workflow) == "" || strings.ContainsAny(p.Workflow, " @") {
		return errors.New("evidence workflow is required")
	}
	if strings.TrimSpace(p.Ref) == "" || !strings.HasPrefix(p.Ref, "refs/") {
		return errors.New("evidence ref must be a full refs/* value")
	}
	if strings.TrimSpace(p.Commit) != "" && !commitPattern.MatchString(strings.TrimSpace(p.Commit)) {
		return errors.New("evidence commit must be a hexadecimal git object id")
	}
	if strings.TrimSpace(p.Event) != "" && strings.ContainsAny(p.Event, " @") {
		return errors.New("evidence event is invalid")
	}
	if len(p.AllowedEvents) == 0 && strings.TrimSpace(p.Event) == "" {
		return errors.New("evidence event or allowed events are required")
	}
	for _, event := range p.AllowedEvents {
		if strings.TrimSpace(event) == "" || strings.ContainsAny(event, " @") {
			return errors.New("evidence allowed event is invalid")
		}
	}
	return nil
}

func (p EvidencePolicy) workflowURI() string {
	workflow := strings.TrimSpace(p.Workflow)
	if strings.HasPrefix(workflow, "https://") {
		return workflow
	}
	workflow = strings.TrimPrefix(workflow, "/")
	if !strings.HasPrefix(workflow, ".github/workflows/") {
		workflow = ".github/workflows/" + workflow
	}
	return "https://github.com/" + strings.TrimSpace(p.Repository) + "/" + workflow
}

func (p EvidencePolicy) buildConfigURI() string {
	return p.workflowURI() + "@" + p.Ref
}

// CertificateIdentity builds the Sigstore certificate identity matcher. The
// SAN and modern Fulcio extensions are all exact, preventing a valid
// certificate from another workflow/ref/commit from being accepted.
func (p EvidencePolicy) CertificateIdentity() (verify.CertificateIdentity, error) {
	if err := p.Validate(); err != nil {
		return verify.CertificateIdentity{}, err
	}
	buildConfigURI := p.buildConfigURI()
	san, err := verify.NewSANMatcher(buildConfigURI, "")
	if err != nil {
		return verify.CertificateIdentity{}, err
	}
	issuer, err := verify.NewIssuerMatcher(p.Issuer, "")
	if err != nil {
		return verify.CertificateIdentity{}, err
	}
	ext := certificate.Extensions{
		SourceRepositoryURI:      "https://github.com/" + p.Repository,
		SourceRepositoryRef:      p.Ref,
		GithubWorkflowRepository: p.Repository,
		GithubWorkflowRef:        p.Ref,
		BuildConfigURI:           buildConfigURI,
	}
	if p.Commit != "" {
		ext.SourceRepositoryDigest = p.Commit
		ext.GithubWorkflowSHA = p.Commit
		ext.BuildConfigDigest = p.Commit
	}
	if p.Event != "" {
		ext.BuildTrigger = p.Event
	}
	return verify.NewCertificateIdentity(san, issuer, ext)
}

// EvidenceVerifier verifies signed referrers and classifies their in-toto
// predicates. Implementations must verify cryptographic signatures before
// returning a result; callers must not use structural-only parsers here.
type EvidenceVerifier interface {
	Verify(ctx context.Context, subjectDigest string, referrers []RegistryReferrer, policy EvidencePolicy) (EvidenceResult, error)
}

// CatalogEvidenceVerifier is the catalog-specific verification boundary.
// Catalog attestations sign the append-only ledger and are not required to
// carry the package-only EPAR SLSA/SPDX tuple; identity, subject, trust-root,
// transparency, and a non-empty predicate remain mandatory.
type CatalogEvidenceVerifier interface {
	VerifyCatalog(ctx context.Context, subjectDigest string, referrers []RegistryReferrer, policy EvidencePolicy) (EvidenceResult, error)
}

// EvidenceResult records the immutable referrer descriptors accepted for a
// package/catalog subject.
type EvidenceResult struct {
	Provenance RegistryDescriptor
	SBOM       RegistryDescriptor
	Signature  RegistryDescriptor
	Claims     EvidenceClaims
}

// EvidenceClaims are the EPAR-specific claims carried in the signed SLSA
// predicate and SPDX document. The runtime compares these with the catalog
// entry before accepting a package; a valid signature over an unrelated or
// empty predicate is therefore insufficient.
type EvidenceClaims struct {
	SourceIndexDigest         string            `json:"sourceIndexDigest"`
	SourcePlatformDigests     map[string]string `json:"sourcePlatformDigests"`
	RecipeDigest              string            `json:"recipeDigest"`
	RecipeRevision            string            `json:"recipeRevision"`
	RuntimeContract           string            `json:"runtimeContract"`
	TemplateSchema            int               `json:"templateSchema"`
	RunnerVersion             string            `json:"runnerVersion"`
	RunnerAssetDigests        map[string]string `json:"runnerAssetDigests"`
	ToolDigests               map[string]string `json:"toolDigests"`
	PlatformPackageDigests    map[string]string `json:"platformPackageDigests"`
	ResolvedDependencyDigests []string          `json:"resolvedDependencyDigests"`
	SPDXNamespace             string            `json:"spdxNamespace"`
	SPDXPackageChecksums      map[string]string `json:"spdxPackageChecksums"`
	SubjectDigest             string            `json:"subjectDigest"`
}

// SigstoreEvidenceVerifier is the production verifier backed by sigstore-go.
// A trusted root loaded from Sigstore TUF and transparency/SCT thresholds are
// required; callers cannot construct an unsafe verifier with missing roots.
type SigstoreEvidenceVerifier struct {
	verifier *verify.Verifier
}

// NewSigstoreEvidenceVerifier loads the Sigstore public-good trusted root via
// TUF and configures SCT, Rekor inclusion, and observer timestamp checks.
// TUF refresh is networked and should be performed by the runtime's normal
// registry-refresh path, not while a user is selecting a provider.
func NewSigstoreEvidenceVerifier(ctx context.Context) (*SigstoreEvidenceVerifier, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	client, err := tuf.DefaultClient()
	if err != nil {
		return nil, fmt.Errorf("load Sigstore TUF root: %w", err)
	}
	trustedMaterial, err := root.GetTrustedRoot(client)
	if err != nil {
		return nil, fmt.Errorf("load Sigstore trusted root: %w", err)
	}
	return NewSigstoreEvidenceVerifierWithTrustedMaterial(trustedMaterial)
}

// NewSigstoreEvidenceVerifierWithTrustedMaterial allows an application to
// provide an enterprise or cached Sigstore trusted root while retaining all
// required verification thresholds.
func NewSigstoreEvidenceVerifierWithTrustedMaterial(material root.TrustedMaterial) (*SigstoreEvidenceVerifier, error) {
	if material == nil {
		return nil, errors.New("Sigstore trusted material is required")
	}
	verifier, err := verify.NewVerifier(material,
		verify.WithSignedCertificateTimestamps(1),
		verify.WithTransparencyLog(1),
		verify.WithObserverTimestamps(1),
	)
	if err != nil {
		return nil, fmt.Errorf("configure Sigstore verifier: %w", err)
	}
	return &SigstoreEvidenceVerifier{verifier: verifier}, nil
}

func (v *SigstoreEvidenceVerifier) Verify(ctx context.Context, subjectDigest string, referrers []RegistryReferrer, policy EvidencePolicy) (EvidenceResult, error) {
	if v == nil || v.verifier == nil {
		return EvidenceResult{}, errors.New("Sigstore verifier is not configured")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return EvidenceResult{}, ctx.Err()
	default:
	}
	digest, err := parseSHA256Digest(subjectDigest)
	if err != nil {
		return EvidenceResult{}, err
	}
	identity, err := policy.CertificateIdentity()
	if err != nil {
		return EvidenceResult{}, err
	}
	if len(referrers) == 0 {
		return EvidenceResult{}, errors.New("no signed referrers were returned")
	}
	result := EvidenceResult{}
	result.Claims.SubjectDigest = strings.ToLower(subjectDigest)
	for _, referrer := range referrers {
		if len(referrer.Payload) == 0 {
			return EvidenceResult{}, fmt.Errorf("referrer %s has no Sigstore bundle", referrer.Descriptor.Digest)
		}
		var signedBundle bundle.Bundle
		if err := signedBundle.UnmarshalJSON(referrer.Payload); err != nil {
			return EvidenceResult{}, fmt.Errorf("parse Sigstore bundle %s: %w", referrer.Descriptor.Digest, err)
		}
		verification, err := v.verifier.Verify(&signedBundle, verify.NewPolicy(
			verify.WithArtifactDigest("sha256", digest),
			verify.WithCertificateIdentity(identity),
		))
		if err != nil {
			return EvidenceResult{}, fmt.Errorf("verify Sigstore bundle %s: %w", referrer.Descriptor.Digest, err)
		}
		if err := validateVerifiedStatement(verification, subjectDigest, policy); err != nil {
			return EvidenceResult{}, fmt.Errorf("validate signed statement %s: %w", referrer.Descriptor.Digest, err)
		}
		if verification.Statement == nil {
			return EvidenceResult{}, fmt.Errorf("Sigstore bundle %s omitted statement", referrer.Descriptor.Digest)
		}
		entry := referrer.Descriptor
		switch verification.Statement.PredicateType {
		case SLSAProvenanceV02, SLSAProvenanceV1:
			claims, err := parseSLSAClaims(verification.Statement.Predicate)
			if err != nil {
				return EvidenceResult{}, fmt.Errorf("parse SLSA predicate %s: %w", referrer.Descriptor.Digest, err)
			}
			if result.Provenance.Digest != "" {
				return EvidenceResult{}, errors.New("multiple provenance referrers are not allowed")
			}
			result.Provenance = entry
			result.Claims.SourceIndexDigest = claims.SourceIndexDigest
			result.Claims.RecipeDigest = claims.RecipeDigest
			result.Claims.RuntimeContract = claims.RuntimeContract
			result.Claims.TemplateSchema = claims.TemplateSchema
			result.Claims.RunnerVersion = claims.RunnerVersion
			result.Claims.RunnerAssetDigests = claims.RunnerAssetDigests
			result.Claims.ToolDigests = claims.ToolDigests
			result.Claims.PlatformPackageDigests = claims.PlatformPackageDigests
			result.Claims.SourcePlatformDigests = claims.SourcePlatformDigests
			result.Claims.ResolvedDependencyDigests = claims.ResolvedDependencyDigests
		case SPDXPredicate:
			namespace, checksums, err := parseSPDXClaims(verification.Statement.Predicate)
			if err != nil {
				return EvidenceResult{}, fmt.Errorf("parse SPDX predicate %s: %w", referrer.Descriptor.Digest, err)
			}
			if result.SBOM.Digest != "" {
				return EvidenceResult{}, errors.New("multiple SPDX referrers are not allowed")
			}
			result.SBOM = entry
			result.Claims.SPDXNamespace = namespace
			result.Claims.SPDXPackageChecksums = checksums
		default:
			return EvidenceResult{}, fmt.Errorf("unsupported attestation predicate %q", verification.Statement.PredicateType)
		}
		// AttestationDigest is defined as the signed provenance bundle digest;
		// OCI referrer order must not change the catalog evidence identity.
		if result.Provenance.Digest != "" {
			result.Signature = result.Provenance
		}
	}
	if result.Provenance.Digest == "" {
		return EvidenceResult{}, errors.New("signed SLSA provenance referrer is missing")
	}
	if result.SBOM.Digest == "" {
		return EvidenceResult{}, errors.New("signed SPDX SBOM referrer is missing")
	}
	if result.Signature.Digest == "" {
		return EvidenceResult{}, errors.New("signed provenance referrer is missing")
	}
	return result, nil
}

// VerifyCatalog verifies an OCI catalog attestation without interpreting it
// as a package SLSA/SPDX pair. It performs the same Sigstore trust-root,
// transparency, certificate identity, and exact in-toto subject checks.
func (v *SigstoreEvidenceVerifier) VerifyCatalog(ctx context.Context, subjectDigest string, referrers []RegistryReferrer, policy EvidencePolicy) (EvidenceResult, error) {
	if v == nil || v.verifier == nil {
		return EvidenceResult{}, errors.New("Sigstore verifier is not configured")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return EvidenceResult{}, ctx.Err()
	default:
	}
	digest, err := parseSHA256Digest(subjectDigest)
	if err != nil {
		return EvidenceResult{}, err
	}
	identity, err := policy.CertificateIdentity()
	if err != nil {
		return EvidenceResult{}, err
	}
	if len(referrers) == 0 {
		return EvidenceResult{}, errors.New("no signed catalog referrers were returned")
	}
	result := EvidenceResult{Claims: EvidenceClaims{SubjectDigest: strings.ToLower(subjectDigest)}}
	for _, referrer := range referrers {
		if len(referrer.Payload) == 0 {
			return EvidenceResult{}, fmt.Errorf("catalog referrer %s has no Sigstore bundle", referrer.Descriptor.Digest)
		}
		var signedBundle bundle.Bundle
		if err := signedBundle.UnmarshalJSON(referrer.Payload); err != nil {
			return EvidenceResult{}, fmt.Errorf("parse catalog Sigstore bundle %s: %w", referrer.Descriptor.Digest, err)
		}
		verification, err := v.verifier.Verify(&signedBundle, verify.NewPolicy(
			verify.WithArtifactDigest("sha256", digest),
			verify.WithCertificateIdentity(identity),
		))
		if err != nil {
			return EvidenceResult{}, fmt.Errorf("verify catalog Sigstore bundle %s: %w", referrer.Descriptor.Digest, err)
		}
		if err := validateVerifiedStatement(verification, subjectDigest, policy); err != nil {
			return EvidenceResult{}, fmt.Errorf("validate signed catalog statement %s: %w", referrer.Descriptor.Digest, err)
		}
		if verification.Statement.PredicateType == "" || verification.Statement.Predicate == nil {
			return EvidenceResult{}, fmt.Errorf("catalog Sigstore bundle %s has an empty predicate", referrer.Descriptor.Digest)
		}
		if result.Signature.Digest == "" || referrer.Descriptor.Digest < result.Signature.Digest {
			// Catalogs do not yet carry an evidence ledger. Re-attestation of an
			// unchanged subject is therefore accepted; select one deterministically
			// so OCI referrer ordering cannot change the result.
			result.Signature = referrer.Descriptor
		}
	}
	if result.Signature.Digest == "" {
		return EvidenceResult{}, errors.New("signed catalog attestation is missing")
	}
	return result, nil
}

func validateVerifiedStatement(result *verify.VerificationResult, subjectDigest string, policy EvidencePolicy) error {
	if result == nil || result.Statement == nil {
		return errors.New("verification result has no in-toto statement")
	}
	if result.VerifiedIdentity == nil || result.VerifiedIdentity.Issuer.Issuer != policy.Issuer {
		return errors.New("verified certificate issuer does not match policy")
	}
	if result.Signature == nil || result.Signature.Certificate == nil {
		return errors.New("verified certificate summary is missing")
	}
	summary := result.Signature.Certificate
	if summary.Issuer != policy.Issuer {
		return fmt.Errorf("certificate issuer mismatch: got %q", summary.Issuer)
	}
	if summary.GithubWorkflowRepository != policy.Repository || summary.GithubWorkflowRef != policy.Ref {
		return errors.New("certificate workflow repository/ref/commit mismatch")
	}
	if summary.SourceRepositoryURI != "https://github.com/"+policy.Repository || summary.SourceRepositoryRef != policy.Ref || !commitPattern.MatchString(summary.SourceRepositoryDigest) {
		return errors.New("certificate source repository/ref/commit mismatch")
	}
	if policy.Commit != "" && (summary.SourceRepositoryDigest != policy.Commit || summary.GithubWorkflowSHA != policy.Commit || summary.BuildConfigDigest != policy.Commit) {
		return errors.New("certificate commit mismatch")
	}
	if summary.BuildConfigURI != policy.buildConfigURI() || !commitPattern.MatchString(summary.BuildConfigDigest) {
		return errors.New("certificate workflow/event identity mismatch")
	}
	if policy.Event != "" && summary.BuildTrigger != policy.Event {
		return errors.New("certificate event mismatch")
	}
	if len(policy.AllowedEvents) > 0 {
		allowed := false
		for _, event := range policy.AllowedEvents {
			if summary.BuildTrigger == event {
				allowed = true
				break
			}
		}
		if !allowed {
			return fmt.Errorf("certificate event %q is not allowlisted", summary.BuildTrigger)
		}
	}
	if len(result.Statement.Subject) == 0 {
		return errors.New("in-toto statement has no subject")
	}
	want := strings.TrimPrefix(strings.ToLower(subjectDigest), "sha256:")
	found := false
	for _, subject := range result.Statement.Subject {
		if subject == nil {
			continue
		}
		digest, ok := subject.Digest["sha256"]
		if !ok || strings.ToLower(digest) != want {
			return errors.New("in-toto subject digest does not match OCI subject")
		}
		if found {
			return errors.New("in-toto statement contains multiple matching subjects")
		}
		found = true
	}
	if !found {
		return errors.New("in-toto statement subject is missing sha256 digest")
	}
	return nil
}

func parseSHA256Digest(value string) ([]byte, error) {
	normalized, err := NormalizeDigest(value)
	if err != nil {
		return nil, err
	}
	digest, err := hex.DecodeString(strings.TrimPrefix(normalized, "sha256:"))
	if err != nil {
		return nil, fmt.Errorf("decode subject digest: %w", err)
	}
	return digest, nil
}

func parseSLSAClaims(predicate *structpb.Struct) (EvidenceClaims, error) {
	if predicate == nil {
		return EvidenceClaims{}, errors.New("predicate is empty")
	}
	root := predicate.AsMap()
	buildDefinition, err := requiredMap(root, "buildDefinition")
	if err != nil {
		return EvidenceClaims{}, err
	}
	external, err := requiredMap(buildDefinition, "externalParameters")
	if err != nil {
		return EvidenceClaims{}, err
	}
	source, err := requiredMap(external, "source")
	if err != nil {
		return EvidenceClaims{}, err
	}
	recipe, err := requiredMap(external, "recipe")
	if err != nil {
		return EvidenceClaims{}, err
	}
	runner, err := requiredMap(external, "runner")
	if err != nil {
		return EvidenceClaims{}, err
	}
	tools, err := requiredMap(external, "tools")
	if err != nil {
		return EvidenceClaims{}, err
	}
	platforms, err := requiredMap(external, "platforms")
	if err != nil {
		return EvidenceClaims{}, err
	}
	claims := EvidenceClaims{}
	claims.SourceIndexDigest, err = requiredString(source, "indexDigest")
	if err != nil {
		return EvidenceClaims{}, err
	}
	if _, err := NormalizeDigest(claims.SourceIndexDigest); err != nil {
		return EvidenceClaims{}, fmt.Errorf("source.indexDigest: %w", err)
	}
	claims.SourcePlatformDigests, err = digestMap(source, "platformDigests")
	if err != nil {
		return EvidenceClaims{}, err
	}
	claims.RecipeDigest, err = requiredString(recipe, "digest")
	if err != nil {
		return EvidenceClaims{}, err
	}
	if _, err := NormalizeDigest(claims.RecipeDigest); err != nil {
		return EvidenceClaims{}, fmt.Errorf("recipe.digest: %w", err)
	}
	claims.RecipeRevision, err = requiredString(recipe, "revision")
	if err != nil {
		claims.RecipeRevision, err = requiredString(recipe, "recipeRevision")
	}
	if err != nil || !commitPattern.MatchString(claims.RecipeRevision) {
		if err == nil {
			err = errors.New("recipe revision must be a 40- or 64-character hexadecimal commit")
		}
		return EvidenceClaims{}, err
	}
	claims.RuntimeContract, err = requiredString(recipe, "runtimeContract")
	if err != nil {
		return EvidenceClaims{}, err
	}
	claims.TemplateSchema, err = requiredInt(recipe, "templateSchema")
	if err != nil || claims.TemplateSchema < 1 {
		if err == nil {
			err = errors.New("templateSchema must be positive")
		}
		return EvidenceClaims{}, err
	}
	claims.RunnerVersion, err = requiredString(runner, "version")
	if err != nil {
		return EvidenceClaims{}, err
	}
	claims.RunnerAssetDigests, err = digestMap(runner, "assetDigests")
	if err != nil {
		return EvidenceClaims{}, err
	}
	claims.ToolDigests, err = toolDigestMap(tools)
	if err != nil {
		return EvidenceClaims{}, err
	}
	claims.PlatformPackageDigests, err = platformDigestMap(platforms)
	if err != nil {
		return EvidenceClaims{}, err
	}
	resolved, ok := buildDefinition["resolvedDependencies"].([]any)
	if !ok {
		// Keep a narrow compatibility read for the pre-v1 fixture shape while
		// requiring the standard buildDefinition location for new publications.
		resolved, ok = root["resolvedDependencies"].([]any)
	}
	if !ok || len(resolved) == 0 {
		return EvidenceClaims{}, errors.New("buildDefinition.resolvedDependencies must contain at least one dependency")
	}
	claims.ResolvedDependencyDigests = make([]string, 0, len(resolved))
	for index, raw := range resolved {
		dependency, ok := raw.(map[string]any)
		if !ok {
			return EvidenceClaims{}, fmt.Errorf("resolvedDependencies[%d] must be an object", index)
		}
		if digestValues, ok := dependency["digest"].(map[string]any); ok {
			if gitCommit, hasGitCommit := digestValues["gitCommit"]; hasGitCommit {
				commit, valid := gitCommit.(string)
				uri, uriValid := dependency["uri"].(string)
				if len(digestValues) != 1 || !valid || !commitPattern.MatchString(commit) || !strings.EqualFold(commit, claims.RecipeRevision) {
					return EvidenceClaims{}, fmt.Errorf("resolvedDependencies[%d] gitCommit must exactly match recipe revision", index)
				}
				if !uriValid || !strings.HasPrefix(uri, "git+https://github.com/") || !strings.Contains(uri, "@refs/") {
					return EvidenceClaims{}, fmt.Errorf("resolvedDependencies[%d] GitHub workflow source URI is invalid", index)
				}
				continue
			}
		}
		digest, err := resourceDigest(dependency, "digest")
		if err != nil {
			return EvidenceClaims{}, fmt.Errorf("resolvedDependencies[%d]: %w", index, err)
		}
		if _, err := NormalizeDigest(digest); err != nil {
			return EvidenceClaims{}, fmt.Errorf("resolvedDependencies[%d]: %w", index, err)
		}
		claims.ResolvedDependencyDigests = append(claims.ResolvedDependencyDigests, strings.ToLower(digest))
	}
	for _, required := range append([]string{claims.SourceIndexDigest}, mapValues(claims.SourcePlatformDigests)...) {
		if !containsString(claims.ResolvedDependencyDigests, required) {
			return EvidenceClaims{}, fmt.Errorf("resolvedDependencies do not include source digest %s", required)
		}
	}
	for _, required := range append(mapValues(claims.RunnerAssetDigests), mapValues(claims.ToolDigests)...) {
		if !containsString(claims.ResolvedDependencyDigests, required) {
			return EvidenceClaims{}, fmt.Errorf("resolvedDependencies do not include required tooling digest %s", required)
		}
	}
	return claims, nil
}

// resourceDigest parses the standard SLSA ResourceDescriptor digest map. A
// single SHA-256 value is required; accepting an arbitrary string here would
// permit an ambiguous or non-standard dependency identity into the claims.
func resourceDigest(parent map[string]any, key string) (string, error) {
	raw, ok := parent[key]
	if !ok {
		return "", fmt.Errorf("predicate field %s is missing", key)
	}
	values, ok := raw.(map[string]any)
	if !ok || len(values) != 1 {
		return "", fmt.Errorf("predicate field %s must contain exactly one sha256 digest", key)
	}
	value, ok := values["sha256"].(string)
	if !ok || len(strings.TrimSpace(value)) != 64 {
		return "", fmt.Errorf("predicate field %s.sha256 must be a 64-character hex digest", key)
	}
	if _, err := hex.DecodeString(strings.TrimSpace(value)); err != nil {
		return "", fmt.Errorf("predicate field %s.sha256 is not hexadecimal: %w", key, err)
	}
	return "sha256:" + strings.ToLower(strings.TrimSpace(value)), nil
}

func parseSPDXClaims(predicate *structpb.Struct) (string, map[string]string, error) {
	if predicate == nil {
		return "", nil, errors.New("predicate is empty")
	}
	root := predicate.AsMap()
	version, err := requiredString(root, "spdxVersion")
	if err != nil {
		return "", nil, err
	}
	if !strings.HasPrefix(strings.ToUpper(version), "SPDX-") {
		return "", nil, fmt.Errorf("unsupported SPDX version %q", version)
	}
	namespace, err := requiredString(root, "documentNamespace")
	if err != nil {
		return "", nil, err
	}
	packages, ok := root["packages"].([]any)
	if !ok || len(packages) == 0 {
		return "", nil, errors.New("SPDX packages are missing")
	}
	checksums := make(map[string]string)
	for index, raw := range packages {
		pkg, ok := raw.(map[string]any)
		if !ok {
			return "", nil, fmt.Errorf("SPDX package %d is not an object", index)
		}
		name, err := requiredString(pkg, "name")
		if err != nil {
			return "", nil, fmt.Errorf("SPDX package %d: %w", index, err)
		}
		values, ok := pkg["checksums"].([]any)
		if !ok || len(values) == 0 {
			return "", nil, fmt.Errorf("SPDX package %q checksums are missing", name)
		}
		found := ""
		for _, rawChecksum := range values {
			checksum, ok := rawChecksum.(map[string]any)
			if !ok {
				continue
			}
			algorithm, _ := checksum["algorithm"].(string)
			value, _ := checksum["checksumValue"].(string)
			if strings.EqualFold(algorithm, "SHA256") && len(value) == 64 {
				if _, err := hex.DecodeString(value); err == nil {
					found = strings.ToLower(value)
					break
				}
			}
		}
		if found == "" {
			return "", nil, fmt.Errorf("SPDX package %q has no SHA256 checksum", name)
		}
		checksums[name] = found
	}
	return namespace, checksums, nil
}

func requiredMap(parent map[string]any, key string) (map[string]any, error) {
	value, ok := parent[key]
	if !ok {
		return nil, fmt.Errorf("predicate field %s is missing", key)
	}
	result, ok := value.(map[string]any)
	if !ok || len(result) == 0 {
		return nil, fmt.Errorf("predicate field %s must be a non-empty object", key)
	}
	return result, nil
}

func requiredString(parent map[string]any, key string) (string, error) {
	value, ok := parent[key].(string)
	if !ok || strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("predicate field %s must be a non-empty string", key)
	}
	return strings.TrimSpace(value), nil
}

func requiredInt(parent map[string]any, key string) (int, error) {
	value, ok := parent[key].(float64)
	if !ok {
		return 0, fmt.Errorf("predicate field %s must be an integer", key)
	}
	if value < 0 || value != float64(int64(value)) || value > float64(^uint(0)>>1) {
		return 0, fmt.Errorf("predicate field %s must be a finite integral value", key)
	}
	return int(value), nil
}

func digestMap(parent map[string]any, key string) (map[string]string, error) {
	value, ok := parent[key].(map[string]any)
	if !ok || len(value) == 0 {
		return nil, fmt.Errorf("predicate field %s must be a non-empty digest map", key)
	}
	result := make(map[string]string, len(value))
	for name, raw := range value {
		digest, ok := raw.(string)
		if !ok {
			return nil, fmt.Errorf("predicate field %s[%s] is not a string", key, name)
		}
		if _, err := NormalizeDigest(digest); err != nil {
			return nil, fmt.Errorf("predicate field %s[%s]: %w", key, name, err)
		}
		result[name] = strings.ToLower(digest)
	}
	return result, nil
}

func toolDigestMap(parent map[string]any) (map[string]string, error) {
	result := make(map[string]string, len(parent))
	for name, raw := range parent {
		entry, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("tool %s must be an object", name)
		}
		digest, err := requiredString(entry, "digest")
		if err != nil {
			return nil, fmt.Errorf("tool %s: %w", name, err)
		}
		if _, err := NormalizeDigest(digest); err != nil {
			return nil, fmt.Errorf("tool %s digest: %w", name, err)
		}
		result[name] = strings.ToLower(digest)
	}
	if len(result) == 0 {
		return nil, errors.New("predicate tools are missing")
	}
	return result, nil
}

func platformDigestMap(parent map[string]any) (map[string]string, error) {
	result := make(map[string]string, len(parent))
	for platform, raw := range parent {
		entry, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("platform %s must be an object", platform)
		}
		digest, err := requiredString(entry, "packageManifestDigest")
		if err != nil {
			return nil, fmt.Errorf("platform %s: %w", platform, err)
		}
		if _, err := NormalizeDigest(digest); err != nil {
			return nil, fmt.Errorf("platform %s digest: %w", platform, err)
		}
		result[platform] = strings.ToLower(digest)
	}
	if len(result) == 0 {
		return nil, errors.New("predicate platforms are missing")
	}
	return result, nil
}

func mapValues(values map[string]string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		result = append(result, value)
	}
	return result
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if strings.EqualFold(value, want) {
			return true
		}
	}
	return false
}

// MarshalEvidencePolicy emits stable JSON for workflow inputs and audit
// receipts. It is kept here rather than in the CLI so runtime and publisher
// share the exact identity fields.
func (p EvidencePolicy) MarshalJSON() ([]byte, error) {
	type alias EvidencePolicy
	return json.Marshal(alias(p))
}
