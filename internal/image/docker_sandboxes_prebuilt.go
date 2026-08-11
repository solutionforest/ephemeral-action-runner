package image

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/solutionforest/ephemeral-action-runner/internal/config"
	"github.com/solutionforest/ephemeral-action-runner/internal/prebuilt"
	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
	"github.com/solutionforest/ephemeral-action-runner/internal/storage"
	storagecatalog "github.com/solutionforest/ephemeral-action-runner/internal/storage/catalog"
)

const (
	dockerSandboxesPrebuiltDerivativeSchema    = 1
	dockerSandboxesPrebuiltMaterializeAttempts = 2
)

type dockerSandboxesPrebuiltImageFetcher func(context.Context, name.Digest, http.RoundTripper) (v1.Image, error)
type dockerSandboxesPrebuiltArchiveWriter func(string, name.Tag, v1.Image) error

func fetchDockerSandboxesPrebuiltImage(ctx context.Context, ref name.Digest, transport http.RoundTripper) (v1.Image, error) {
	return remote.Image(ref, remote.WithContext(ctx), remote.WithAuth(authn.Anonymous), remote.WithTransport(transport))
}

func dockerSandboxesPrebuiltHTTP1RetryTransport(base http.RoundTripper) (http.RoundTripper, bool) {
	transport, ok := base.(*http.Transport)
	if !ok {
		return base, false
	}
	clone := transport.Clone()
	clone.ForceAttemptHTTP2 = false
	clone.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	if clone.TLSClientConfig == nil {
		clone.TLSClientConfig = &tls.Config{NextProtos: []string{"http/1.1"}}
	} else {
		clone.TLSClientConfig = clone.TLSClientConfig.Clone()
		clone.TLSClientConfig.NextProtos = []string{"http/1.1"}
	}
	return clone, true
}

func dockerSandboxesPrebuiltInitialMaterializationTransport(profile string, base http.RoundTripper) (http.RoundTripper, bool) {
	if profile != prebuilt.ProfileFull {
		return base, false
	}
	return dockerSandboxesPrebuiltHTTP1RetryTransport(base)
}

// DockerSandboxesPrebuiltResolver is the narrow trust boundary between the
// image lifecycle and the registry catalog/Sigstore implementation. A result
// is accepted only after the coordinator independently revalidates catalog,
// package, platform, pin, status, and manifest relationships below.
type DockerSandboxesPrebuiltResolver interface {
	ResolveAndVerify(ctx context.Context, packageAlias, optionalDigest, platform string) (VerifiedDockerSandboxesPrebuilt, error)
}

type dockerSandboxesPrebuiltAcceptanceResolver interface {
	ResolveCandidate(ctx context.Context, packageAlias, packageDigest, platform, catalogReference, evidenceRef string) (VerifiedDockerSandboxesPrebuilt, error)
}

type dockerSandboxesPrebuiltStatusResolver interface {
	ResolveStatus(ctx context.Context, packageDigest string) (prebuilt.CatalogStatus, error)
}

type productionDockerSandboxesPrebuiltResolver struct {
	mu       sync.Mutex
	resolver prebuilt.CatalogResolver
}

func newProductionDockerSandboxesPrebuiltResolver() DockerSandboxesPrebuiltResolver {
	return &productionDockerSandboxesPrebuiltResolver{}
}

func (r *productionDockerSandboxesPrebuiltResolver) initialize(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.resolver.Registry != nil && r.resolver.Evidence != nil {
		return nil
	}
	verifier, err := prebuilt.NewSigstoreEvidenceVerifier(ctx)
	if err != nil {
		return err
	}
	r.resolver = prebuilt.CatalogResolver{
		Registry:          prebuilt.NewRemoteCatalogRegistry(),
		Evidence:          verifier,
		PackageRepository: prebuilt.DefaultPackageRepository,
		EvidencePolicy: prebuilt.EvidencePolicy{
			Issuer: prebuilt.GitHubActionsIssuer, Repository: "solutionforest/ephemeral-action-runner", Workflow: "docker-sandboxes-images.yml", Ref: "refs/heads/main", AllowedEvents: []string{"schedule", "workflow_dispatch", "push"},
		},
	}
	return nil
}

func (r *productionDockerSandboxesPrebuiltResolver) ResolveAndVerify(ctx context.Context, packageAlias, optionalDigest, platform string) (VerifiedDockerSandboxesPrebuilt, error) {
	if err := r.initialize(ctx); err != nil {
		return VerifiedDockerSandboxesPrebuilt{}, err
	}
	profile, err := dockerSandboxesPrebuiltProfileForAlias(packageAlias)
	if err != nil {
		return VerifiedDockerSandboxesPrebuilt{}, err
	}
	registry := r.resolver.Registry
	var catalog prebuilt.Catalog
	var catalogDigest string
	var entry prebuilt.Entry
	var immutableDescriptor prebuilt.ResolvedReference
	effectiveStatus := prebuilt.StatusActive
	if optionalDigest != "" {
		status, err := r.resolver.ResolveStatus(ctx, optionalDigest)
		if err != nil {
			return VerifiedDockerSandboxesPrebuilt{}, err
		}
		if status.EffectiveStatus != prebuilt.StatusActive && status.EffectiveStatus != prebuilt.StatusSuperseded {
			return VerifiedDockerSandboxesPrebuilt{}, &DockerSandboxesPrebuiltStatusError{Digest: status.PackageDigest, Status: status.EffectiveStatus, Reason: status.Entry.RevocationReason}
		}
		resolved, err := r.resolver.ResolvePackage(ctx, optionalDigest)
		if err != nil {
			return VerifiedDockerSandboxesPrebuilt{}, err
		}
		catalog, catalogDigest, entry, immutableDescriptor, effectiveStatus = resolved.Catalog, resolved.CatalogDigest, resolved.Entry, resolved.Package, resolved.EffectiveStatus
	} else {
		resolved, err := r.resolver.Resolve(ctx, profile)
		if err != nil {
			return VerifiedDockerSandboxesPrebuilt{}, err
		}
		aliasDescriptor, err := registry.Resolve(ctx, packageAlias)
		if err != nil {
			return VerifiedDockerSandboxesPrebuilt{}, err
		}
		immutableDescriptor, err = registry.Resolve(ctx, resolved.Entry.PackageReference)
		if err != nil {
			return VerifiedDockerSandboxesPrebuilt{}, err
		}
		if aliasDescriptor.Digest != resolved.Entry.PackageIndexDigest || immutableDescriptor.Digest != resolved.Entry.PackageIndexDigest || aliasDescriptor.Digest != immutableDescriptor.Digest {
			return VerifiedDockerSandboxesPrebuilt{}, errors.New("signed catalog, moving package alias, and immutable package descriptor disagree")
		}
		catalog, catalogDigest, entry = resolved.Catalog, resolved.CatalogDigest, resolved.Entry
	}
	if entry.Profile != profile {
		return VerifiedDockerSandboxesPrebuilt{}, fmt.Errorf("prebuilt package profile %q does not match configured alias profile %q", entry.Profile, profile)
	}
	selected, err := prebuilt.ResolvePlatform(immutableDescriptor, platform)
	if err != nil {
		return VerifiedDockerSandboxesPrebuilt{}, err
	}
	if selected.Size <= 0 {
		return VerifiedDockerSandboxesPrebuilt{}, errors.New("verified prebuilt platform descriptor omitted compressed size")
	}
	return VerifiedDockerSandboxesPrebuilt{Catalog: catalog, CatalogDigest: catalogDigest, Entry: entry, Package: immutableDescriptor, Platform: selected, EffectiveStatus: effectiveStatus, VerifiedAt: time.Now().UTC(), CompressedBytes: uint64(selected.Size)}, nil
}

func (r *productionDockerSandboxesPrebuiltResolver) ResolveCandidate(ctx context.Context, packageAlias, packageDigest, platform, catalogReference, evidenceRef string) (VerifiedDockerSandboxesPrebuilt, error) {
	if err := r.initialize(ctx); err != nil {
		return VerifiedDockerSandboxesPrebuilt{}, err
	}
	profile, err := dockerSandboxesPrebuiltProfileForAlias(packageAlias)
	if err != nil {
		return VerifiedDockerSandboxesPrebuilt{}, err
	}
	packageDigest, err = prebuilt.NormalizeDigest(packageDigest)
	if err != nil {
		return VerifiedDockerSandboxesPrebuilt{}, err
	}
	evidenceRef = strings.TrimSpace(evidenceRef)
	resolver := r.resolver
	resolver.EvidencePolicy.Ref = evidenceRef
	verifiedCatalog, err := resolver.VerifyCatalogReference(ctx, strings.TrimSpace(catalogReference))
	if err != nil {
		return VerifiedDockerSandboxesPrebuilt{}, err
	}
	catalog := verifiedCatalog.Artifact.Catalog
	status, err := catalog.EffectiveStatus(packageDigest)
	if err != nil {
		return VerifiedDockerSandboxesPrebuilt{}, err
	}
	if status != prebuilt.StatusCandidate {
		entry, _ := catalog.EntryByDigest(packageDigest)
		return VerifiedDockerSandboxesPrebuilt{}, &DockerSandboxesPrebuiltStatusError{Digest: packageDigest, Status: status, Reason: entry.RevocationReason}
	}
	entry, ok := catalog.EntryByDigest(packageDigest)
	if !ok {
		return VerifiedDockerSandboxesPrebuilt{}, fmt.Errorf("package digest %s is not present in immutable candidate catalog", packageDigest)
	}
	if entry.Profile != profile {
		return VerifiedDockerSandboxesPrebuilt{}, fmt.Errorf("candidate package profile %q does not match configured alias profile %q", entry.Profile, profile)
	}
	verifiedPackage, err := resolver.VerifyPackage(ctx, packageDigest, entry)
	if err != nil {
		return VerifiedDockerSandboxesPrebuilt{}, err
	}
	selected, err := prebuilt.ResolvePlatform(verifiedPackage.Package, platform)
	if err != nil {
		return VerifiedDockerSandboxesPrebuilt{}, err
	}
	if selected.Size <= 0 {
		return VerifiedDockerSandboxesPrebuilt{}, errors.New("verified prebuilt platform descriptor omitted compressed size")
	}
	return VerifiedDockerSandboxesPrebuilt{
		Catalog: catalog, CatalogDigest: verifiedCatalog.Artifact.CanonicalDigest, CatalogReference: verifiedCatalog.Artifact.Reference,
		EvidenceRef: evidenceRef, Acceptance: true, Entry: entry, Package: verifiedPackage.Package, Platform: selected,
		EffectiveStatus: status, VerifiedAt: time.Now().UTC(), CompressedBytes: uint64(selected.Size),
	}, nil
}

func dockerSandboxesPrebuiltProfileForAlias(packageAlias string) (string, error) {
	packageAlias = strings.TrimSpace(packageAlias)
	for _, profile := range []string{prebuilt.ProfileFull, prebuilt.ProfileAct} {
		alias, err := prebuilt.AliasTag(profile)
		if err != nil {
			return "", err
		}
		if packageAlias == prebuilt.DefaultPackageRepository+":"+alias {
			return profile, nil
		}
	}
	return "", fmt.Errorf("unsupported prebuilt package alias %q", packageAlias)
}

func (r *productionDockerSandboxesPrebuiltResolver) ResolveStatus(ctx context.Context, packageDigest string) (prebuilt.CatalogStatus, error) {
	if err := r.initialize(ctx); err != nil {
		return prebuilt.CatalogStatus{}, err
	}
	return r.resolver.ResolveStatus(ctx, packageDigest)
}

type DockerSandboxesPrebuiltStatusError struct {
	Digest string
	Status string
	Reason string
}

func (e *DockerSandboxesPrebuiltStatusError) Error() string {
	if e.Reason == "" {
		return fmt.Sprintf("prebuilt package %s has effective status %s", e.Digest, e.Status)
	}
	return fmt.Sprintf("prebuilt package %s has effective status %s: %s", e.Digest, e.Status, e.Reason)
}

type VerifiedDockerSandboxesPrebuilt struct {
	Catalog          prebuilt.Catalog            `json:"catalog"`
	CatalogDigest    string                      `json:"catalogDigest"`
	CatalogReference string                      `json:"catalogReference,omitempty"`
	EvidenceRef      string                      `json:"evidenceRef,omitempty"`
	Acceptance       bool                        `json:"acceptance,omitempty"`
	Entry            prebuilt.Entry              `json:"entry"`
	Package          prebuilt.ResolvedReference  `json:"package"`
	Platform         prebuilt.PlatformDescriptor `json:"platform"`
	EffectiveStatus  string                      `json:"effectiveStatus"`
	VerifiedAt       time.Time                   `json:"verifiedAt"`
	CompressedBytes  uint64                      `json:"compressedBytes"`
}

type dockerSandboxesPrebuiltReceiptEvidence struct {
	CatalogDigest         string         `json:"catalogDigest"`
	CatalogReference      string         `json:"catalogReference,omitempty"`
	EvidenceRef           string         `json:"evidenceRef,omitempty"`
	Acceptance            bool           `json:"acceptance,omitempty"`
	Entry                 prebuilt.Entry `json:"entry"`
	PackageReference      string         `json:"packageReference"`
	PackageIndexDigest    string         `json:"packageIndexDigest"`
	PackagePlatformDigest string         `json:"packagePlatformDigest"`
	EffectiveStatus       string         `json:"effectiveStatus"`
	VerifiedAt            time.Time      `json:"verifiedAt"`
	BaseArchiveSHA256     string         `json:"baseArchiveSha256"`
	BaseArchiveBytes      uint64         `json:"baseArchiveBytes"`
	Derivative            bool           `json:"derivative"`
}

type dockerSandboxesPrebuiltAcquisition struct {
	SchemaVersion         int       `json:"schemaVersion"`
	PackageReference      string    `json:"packageReference"`
	PackageIndexDigest    string    `json:"packageIndexDigest"`
	PackagePlatformDigest string    `json:"packagePlatformDigest"`
	ImageConfigDigest     string    `json:"imageConfigDigest"`
	CatalogDigest         string    `json:"catalogDigest"`
	ArchiveSHA256         string    `json:"archiveSha256"`
	ArchiveBytes          uint64    `json:"archiveBytes"`
	Platform              string    `json:"platform"`
	AcquiredAt            time.Time `json:"acquiredAt"`
}

type dockerSandboxesPrebuiltMaterialization struct {
	ArchivePath string
	ArtifactTag string
	ImageDigest string
	Acquisition dockerSandboxesPrebuiltAcquisition
}

func selectDockerSandboxesPrebuiltMaterialization(derivative bool, verified VerifiedDockerSandboxesPrebuilt, derivativeTag string, now time.Time, acquire func() (string, dockerSandboxesPrebuiltAcquisition, error), buildDerivative func() (string, string, error)) (dockerSandboxesPrebuiltMaterialization, error) {
	if derivative {
		archivePath, imageDigest, err := buildDerivative()
		if err != nil {
			return dockerSandboxesPrebuiltMaterialization{}, err
		}
		return dockerSandboxesPrebuiltMaterialization{
			ArchivePath: archivePath,
			ArtifactTag: derivativeTag,
			ImageDigest: imageDigest,
			Acquisition: dockerSandboxesPrebuiltAcquisition{
				SchemaVersion: dockerSandboxesPrebuiltDerivativeSchema, PackageReference: verified.Entry.PackageReference, PackageIndexDigest: verified.Entry.PackageIndexDigest,
				PackagePlatformDigest: verified.Platform.Digest, ImageConfigDigest: imageDigest, CatalogDigest: verified.CatalogDigest, Platform: verified.Platform.Platform, AcquiredAt: now.UTC(),
			},
		}, nil
	}
	archivePath, acquisition, err := acquire()
	if err != nil {
		return dockerSandboxesPrebuiltMaterialization{}, err
	}
	return dockerSandboxesPrebuiltMaterialization{ArchivePath: archivePath, ArtifactTag: dockerSandboxesPrebuiltBaseTag(verified), ImageDigest: acquisition.ImageConfigDigest, Acquisition: acquisition}, nil
}

func (m *Coordinator) restoreDockerSandboxesPrebuiltAdmissionBlock(state UpdatePolicyState) {
	controller, ok := m.Lifecycle.(provider.TemplateAdmissionController)
	if !ok {
		return
	}
	if state.AdmissionBlockedReason != "" {
		controller.SetTemplateAdmissionBlock(state.AdmissionBlockedReason)
	} else {
		controller.ClearTemplateAdmissionBlock()
	}
}

func (m *Coordinator) recordDockerSandboxesPrebuiltResolutionFailure(state *UpdatePolicyState, failure error) bool {
	var statusErr *DockerSandboxesPrebuiltStatusError
	if !errors.As(failure, &statusErr) || statusErr.Status != prebuilt.StatusCriticalRevoked {
		return false
	}
	state.AdmissionBlockedDigest = statusErr.Digest
	state.AdmissionBlockedReason = statusErr.Error()
	m.restoreDockerSandboxesPrebuiltAdmissionBlock(*state)
	return true
}

func (m *Coordinator) clearDockerSandboxesPrebuiltAdmissionBlock(state *UpdatePolicyState) {
	state.AdmissionBlockedDigest = ""
	state.AdmissionBlockedReason = ""
	m.restoreDockerSandboxesPrebuiltAdmissionBlock(*state)
}

func (m *Coordinator) dockerSandboxesDistribution() string {
	distribution := strings.ToLower(strings.TrimSpace(m.Config.Image.Distribution))
	if distribution == "" {
		return config.ImageDistributionLocalBuild
	}
	return distribution
}

func (m *Coordinator) dockerSandboxesPrebuiltPlatform() (string, error) {
	platform := strings.TrimSpace(m.Config.Image.SourcePlatform)
	if platform == "" {
		platform = strings.TrimSpace(m.Config.Provider.Platform)
	}
	if platform == "" {
		architecture := runtime.GOARCH
		if architecture == "386" {
			architecture = "amd64"
		}
		platform = "linux/" + architecture
	}
	platform = prebuilt.NormalizePlatform(platform)
	if platform != "linux/amd64" && platform != "linux/arm64" {
		return "", fmt.Errorf("prebuilt Docker Sandboxes packages do not support platform %q", platform)
	}
	return platform, nil
}

func (m *Coordinator) resolveVerifiedDockerSandboxesPrebuilt(ctx context.Context) (VerifiedDockerSandboxesPrebuilt, error) {
	if m.PrebuiltResolver == nil {
		return VerifiedDockerSandboxesPrebuilt{}, errors.New("prebuilt Docker Sandboxes verification is unavailable; EPAR will not fall back to a local build")
	}
	platform, err := m.dockerSandboxesPrebuiltPlatform()
	if err != nil {
		return VerifiedDockerSandboxesPrebuilt{}, err
	}
	reference := strings.TrimSpace(m.Config.Image.PrebuiltReference)
	pin := strings.TrimSpace(m.Config.Image.PrebuiltDigest)
	var verified VerifiedDockerSandboxesPrebuilt
	if m.Config.Image.PrebuiltAcceptance {
		resolver, ok := m.PrebuiltResolver.(dockerSandboxesPrebuiltAcceptanceResolver)
		if !ok {
			return VerifiedDockerSandboxesPrebuilt{}, errors.New("prebuilt Docker Sandboxes candidate verification is unavailable; EPAR will not follow a moving package alias or fall back to a local build")
		}
		verified, err = resolver.ResolveCandidate(ctx, reference, pin, platform, strings.TrimSpace(m.Config.Image.PrebuiltCatalogReference), strings.TrimSpace(m.Config.Image.PrebuiltEvidenceRef))
	} else {
		verified, err = m.PrebuiltResolver.ResolveAndVerify(ctx, reference, pin, platform)
	}
	if err != nil {
		return VerifiedDockerSandboxesPrebuilt{}, fmt.Errorf("resolve and verify Docker Sandboxes prebuilt package: %w", err)
	}
	if err := verified.Catalog.Validate(); err != nil {
		return VerifiedDockerSandboxesPrebuilt{}, fmt.Errorf("validate verified prebuilt catalog: %w", err)
	}
	catalogDigest, err := verified.Catalog.CatalogDigest()
	if err != nil {
		return VerifiedDockerSandboxesPrebuilt{}, err
	}
	if verified.CatalogDigest != catalogDigest || !validSHA256(verified.CatalogDigest) {
		return VerifiedDockerSandboxesPrebuilt{}, errors.New("verified prebuilt catalog digest does not match canonical catalog content")
	}
	entry, found := verified.Catalog.EntryByDigest(verified.Entry.PackageIndexDigest)
	if !found {
		return VerifiedDockerSandboxesPrebuilt{}, errors.New("verified prebuilt entry is absent from its catalog")
	}
	gotEntry, _ := json.Marshal(verified.Entry)
	wantEntry, _ := json.Marshal(entry)
	if string(gotEntry) != string(wantEntry) {
		return VerifiedDockerSandboxesPrebuilt{}, errors.New("verified prebuilt entry differs from its immutable catalog entry")
	}
	status, err := verified.Catalog.EffectiveStatus(entry.PackageIndexDigest)
	if err != nil {
		return VerifiedDockerSandboxesPrebuilt{}, err
	}
	allowedStatus := status == prebuilt.StatusActive || (pin != "" && status == prebuilt.StatusSuperseded)
	if m.Config.Image.PrebuiltAcceptance {
		allowedStatus = status == prebuilt.StatusCandidate && verified.Acceptance && verified.CatalogReference == strings.TrimSpace(m.Config.Image.PrebuiltCatalogReference) && verified.EvidenceRef == strings.TrimSpace(m.Config.Image.PrebuiltEvidenceRef)
	}
	if !allowedStatus {
		return VerifiedDockerSandboxesPrebuilt{}, &DockerSandboxesPrebuiltStatusError{Digest: entry.PackageIndexDigest, Status: status, Reason: entry.RevocationReason}
	}
	if err := m.validateDockerSandboxesPrebuiltRecipe(entry); err != nil {
		return VerifiedDockerSandboxesPrebuilt{}, err
	}
	if verified.EffectiveStatus != status || verified.Package.Digest != entry.PackageIndexDigest || verified.Package.Repository != entry.PackageRepository || verified.Platform.Platform != platform || verified.Platform.Digest == "" {
		return VerifiedDockerSandboxesPrebuilt{}, errors.New("verified prebuilt package identity is internally inconsistent")
	}
	wantPlatform := ""
	for _, publication := range entry.Platforms {
		if prebuilt.NormalizePlatform(publication.Platform) == platform {
			if wantPlatform != "" {
				return VerifiedDockerSandboxesPrebuilt{}, fmt.Errorf("prebuilt catalog contains multiple %s publications", platform)
			}
			wantPlatform = publication.PackageManifestDigest
		}
	}
	if wantPlatform == "" || verified.Platform.Digest != wantPlatform {
		return VerifiedDockerSandboxesPrebuilt{}, fmt.Errorf("verified prebuilt platform digest does not match the catalog")
	}
	if pin != "" && pin != entry.PackageIndexDigest {
		return VerifiedDockerSandboxesPrebuilt{}, fmt.Errorf("prebuilt package moved from configured pin %s to %s", pin, entry.PackageIndexDigest)
	}
	if verified.VerifiedAt.IsZero() || verified.CompressedBytes == 0 {
		return VerifiedDockerSandboxesPrebuilt{}, errors.New("verified prebuilt package omitted verification time or compressed size evidence")
	}
	return verified, nil
}

func (m *Coordinator) validateDockerSandboxesPrebuiltRecipe(entry prebuilt.Entry) error {
	if entry.Recipe.RuntimeContract != "docker-sandboxes-v1" || entry.Recipe.TemplateSchema != 2 {
		return fmt.Errorf("prebuilt package requires unsupported runtime contract %q or template schema %d", entry.Recipe.RuntimeContract, entry.Recipe.TemplateSchema)
	}
	sourceLockPath := filepath.Join(m.ProjectRoot, "templates", "docker-sandboxes", "sources.lock.json")
	sourceLock, err := os.ReadFile(sourceLockPath)
	if err != nil {
		return fmt.Errorf("read supported prebuilt source lock: %w", err)
	}
	sourceDigest := sha256.Sum256(sourceLock)
	wantSourceDigest := "sha256:" + hex.EncodeToString(sourceDigest[:])
	wantToolDigest, err := dockerSandboxesSupportedPrebuiltToolDigest(sourceLock)
	if err != nil {
		return err
	}
	wantRecipeDigest, err := dockerSandboxesSupportedPrebuiltRecipeDigest(m.ProjectRoot)
	if err != nil {
		return err
	}
	if entry.Recipe.SourceLockDigest != wantSourceDigest {
		return fmt.Errorf("prebuilt package source lock identity %s is not supported by this controller (expected %s)", entry.Recipe.SourceLockDigest, wantSourceDigest)
	}
	if entry.Recipe.ToolDigest != wantToolDigest {
		return fmt.Errorf("prebuilt package tool identity %s is not supported by this controller (expected %s)", entry.Recipe.ToolDigest, wantToolDigest)
	}
	if entry.Recipe.Digest != wantRecipeDigest {
		return fmt.Errorf("prebuilt package recipe identity %s is not supported by this controller (expected %s)", entry.Recipe.Digest, wantRecipeDigest)
	}
	return nil
}

func dockerSandboxesSupportedPrebuiltToolDigest(sourceLock []byte) (string, error) {
	var lock map[string]json.RawMessage
	if err := json.Unmarshal(sourceLock, &lock); err != nil {
		return "", fmt.Errorf("parse supported prebuilt source lock: %w", err)
	}
	toolMaterial := make(map[string]any)
	for _, key := range []string{"dockerfileFrontend", "sbomGenerator", "goBuilder", "emulation", "tini"} {
		value, ok := lock[key]
		if !ok {
			return "", fmt.Errorf("supported prebuilt source lock omitted %s", key)
		}
		var canonical any
		if err := json.Unmarshal(value, &canonical); err != nil {
			return "", fmt.Errorf("canonicalize supported prebuilt tool identity %s: %w", key, err)
		}
		toolMaterial[key] = canonical
	}
	toolJSON, err := json.Marshal(toolMaterial)
	if err != nil {
		return "", err
	}
	// The publisher defines this identity with `jq -cS | sha256sum`; jq emits
	// one trailing newline. Preserve that byte so every controller platform
	// verifies the same already-attested tool identity.
	toolJSON = append(toolJSON, '\n')
	toolDigest := sha256.Sum256(toolJSON)
	return "sha256:" + hex.EncodeToString(toolDigest[:]), nil
}

func dockerSandboxesSupportedPrebuiltRecipeDigest(projectRoot string) (string, error) {
	var paths []string
	for _, root := range []string{filepath.Join(projectRoot, "templates", "docker-sandboxes"), filepath.Join(projectRoot, "scripts", "docker-sandboxes"), filepath.Join(projectRoot, "internal", "prebuilt")} {
		err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() {
				return nil
			}
			relative, err := filepath.Rel(projectRoot, path)
			if err != nil {
				return err
			}
			relative = filepath.ToSlash(relative)
			if strings.HasPrefix(relative, "templates/docker-sandboxes/") {
				included := relative == "templates/docker-sandboxes/Dockerfile.prebuilt" || relative == "templates/docker-sandboxes/helpers.sha256" || relative == "templates/docker-sandboxes/prebuilt.lock.json"
				for _, prefix := range []string{"templates/docker-sandboxes/prebuilt/", "templates/docker-sandboxes/guest/", "templates/docker-sandboxes/hook-launcher/", "templates/docker-sandboxes/egress-bridge/", "templates/docker-sandboxes/profiles/"} {
					included = included || strings.HasPrefix(relative, prefix)
				}
				if !included {
					return nil
				}
			}
			paths = append(paths, relative)
			return nil
		})
		if err != nil {
			return "", fmt.Errorf("enumerate supported prebuilt recipe: %w", err)
		}
	}
	sort.Strings(paths)
	outer := sha256.New()
	for _, relative := range paths {
		content, err := os.ReadFile(filepath.Join(projectRoot, filepath.FromSlash(relative)))
		if err != nil {
			return "", err
		}
		digest := sha256.Sum256(content)
		fmt.Fprintf(outer, "%s  %s\n", hex.EncodeToString(digest[:]), relative)
	}
	return "sha256:" + hex.EncodeToString(outer.Sum(nil)), nil
}

func (m *Coordinator) dockerSandboxesPrebuiltLocalManifest() (Manifest, error) {
	platform, err := m.dockerSandboxesPrebuiltPlatform()
	if err != nil {
		return Manifest{}, err
	}
	customScripts, err := m.customInstallScriptDigests()
	if err != nil {
		return Manifest{}, err
	}
	return Manifest{
		SchemaVersion:        ManifestSchemaVersion,
		Distribution:         dockerSandboxesDistributionPrebuilt,
		ProviderType:         "docker-sandboxes",
		ProviderPlatform:     platform,
		SourceType:           config.ImageSourceDockerImage,
		SourceImage:          strings.TrimSpace(m.Config.Image.SourceImage),
		SourcePlatform:       platform,
		OutputImage:          "docker-sandboxes-template",
		RunnerSelector:       normalizedRunnerSelector(m.Config.Image.RunnerVersion),
		CustomInstallScripts: customScripts,
		Prebuilt: &PrebuiltManifestMetadata{
			Reference:        strings.TrimSpace(m.Config.Image.PrebuiltReference),
			Pinned:           strings.TrimSpace(m.Config.Image.PrebuiltDigest) != "",
			Acceptance:       m.Config.Image.PrebuiltAcceptance,
			ConfiguredDigest: strings.TrimSpace(m.Config.Image.PrebuiltDigest),
			CatalogReference: strings.TrimSpace(m.Config.Image.PrebuiltCatalogReference),
			EvidenceRef:      strings.TrimSpace(m.Config.Image.PrebuiltEvidenceRef),
		},
	}, nil
}

func (m *Coordinator) dockerSandboxesPrebuiltDesiredManifest(ctx context.Context) (Manifest, ResolvedDockerSource, VerifiedDockerSandboxesPrebuilt, error) {
	manifest, err := m.dockerSandboxesPrebuiltLocalManifest()
	if err != nil {
		return Manifest{}, ResolvedDockerSource{}, VerifiedDockerSandboxesPrebuilt{}, err
	}
	verified, err := m.resolveVerifiedDockerSandboxesPrebuilt(ctx)
	if err != nil {
		return Manifest{}, ResolvedDockerSource{}, VerifiedDockerSandboxesPrebuilt{}, err
	}
	entry := verified.Entry
	platform := verified.Platform.Platform
	configuredSource, err := NormalizeCatthehackerSource(strings.TrimSpace(m.Config.Image.SourceImage))
	if err != nil {
		return Manifest{}, ResolvedDockerSource{}, VerifiedDockerSandboxesPrebuilt{}, err
	}
	if entry.Source.Repository != catthehackerUbuntuRepository || entry.Source.Repository+":"+entry.Source.SourceTag != configuredSource || entry.Source.Reference != entry.Source.Repository+"@"+entry.Source.IndexDigest {
		return Manifest{}, ResolvedDockerSource{}, VerifiedDockerSandboxesPrebuilt{}, errors.New("verified prebuilt catalog source does not match the configured canonical Catthehacker source")
	}
	assetDigest := entry.Runner.AssetDigests[platform]
	if !validSHA256(assetDigest) {
		return Manifest{}, ResolvedDockerSource{}, VerifiedDockerSandboxesPrebuilt{}, fmt.Errorf("prebuilt catalog omitted the Actions runner digest for %s", platform)
	}
	architecture := "x64"
	if platform == "linux/arm64" {
		architecture = "arm64"
	}
	manifest.SourceImage = entry.Source.Reference
	manifest.SourceDigest = entry.Source.IndexDigest
	manifest.SourcePlatformDigest = entry.Source.PlatformDigests[platform]
	manifest.RunnerSelector = entry.Runner.Selector
	manifest.RunnerVersion = entry.Runner.Version
	manifest.RunnerAssetName = fmt.Sprintf("actions-runner-linux-%s-%s.tar.gz", architecture, entry.Runner.Version)
	manifest.RunnerAssetURL = fmt.Sprintf("https://github.com/actions/runner/releases/download/v%s/%s", entry.Runner.Version, manifest.RunnerAssetName)
	manifest.RunnerAssetDigest = assetDigest
	manifest.Prebuilt = &PrebuiltManifestMetadata{
		Reference:             entry.PackageRepository + "@" + entry.PackageIndexDigest,
		Pinned:                strings.TrimSpace(m.Config.Image.PrebuiltDigest) != "",
		Acceptance:            m.Config.Image.PrebuiltAcceptance,
		ConfiguredDigest:      strings.TrimSpace(m.Config.Image.PrebuiltDigest),
		CatalogReference:      verified.CatalogReference,
		EvidenceRef:           verified.EvidenceRef,
		PackageIndexDigest:    entry.PackageIndexDigest,
		PackagePlatformDigest: verified.Platform.Digest,
		CatalogDigest:         verified.CatalogDigest,
		RecipeDigest:          entry.Recipe.Digest,
		RuntimeContract:       entry.Recipe.RuntimeContract,
		EffectiveStatus:       verified.EffectiveStatus,
	}
	source := ResolvedDockerSource{
		Reference:            entry.Source.Repository + ":" + entry.Source.SourceTag,
		ImmutableReference:   entry.Source.Reference,
		IndexDigest:          entry.Source.IndexDigest,
		PlatformDigest:       entry.Source.PlatformDigests[platform],
		Platform:             platform,
		CompressedLayerBytes: verified.CompressedBytes,
	}
	if !validSHA256(source.IndexDigest) || !validSHA256(source.PlatformDigest) {
		return Manifest{}, ResolvedDockerSource{}, VerifiedDockerSandboxesPrebuilt{}, errors.New("prebuilt catalog source digest evidence is incomplete")
	}
	return manifest, source, verified, nil
}

func (m *Coordinator) ensureDockerSandboxesPrebuiltResolved(ctx context.Context, force bool, manifest Manifest, source ResolvedDockerSource, verified VerifiedDockerSandboxesPrebuilt) error {
	runtimeProvider, ok := m.Lifecycle.(provider.TemplateArtifactRuntime)
	if !ok {
		return errors.New("docker-sandboxes provider is missing required template artifact integration")
	}
	if err := m.recoverDockerSandboxesActivation(ctx, runtimeProvider); err != nil {
		return fmt.Errorf("reconcile interrupted Docker Sandboxes prebuilt activation: %w", err)
	}
	manifestHash, err := ManifestHash(manifest)
	if err != nil {
		return err
	}
	rootDisk, err := m.effectiveDockerSandboxesRootDisk(source)
	if err != nil {
		return err
	}
	if !force {
		receipt, receiptErr := m.readDockerSandboxesReceipt()
		if receiptErr == nil && receipt.Distribution == dockerSandboxesDistributionPrebuilt && receipt.ManifestHash == manifestHash && receipt.Artifact.RootDisk == rootDisk && receipt.Prebuilt != nil && receipt.Prebuilt.PackageIndexDigest == verified.Entry.PackageIndexDigest && receipt.Prebuilt.PackagePlatformDigest == verified.Platform.Digest && receipt.Prebuilt.CatalogDigest == verified.CatalogDigest {
			if err := runtimeProvider.VerifyImportedTemplate(ctx, receipt.Artifact); err == nil {
				if err := activateCommittedDockerSandboxesTemplate(runtimeProvider, receipt.Artifact); err != nil {
					return err
				}
				if err := m.recordCurrentSandboxArtifact(ctx, receipt.Artifact, manifestHash, receipt.ActivatedAt); err != nil {
					return err
				}
				return m.cleanupSupersededCatalog(ctx)
			} else if !errors.Is(err, provider.ErrTemplateNotFound) {
				return fmt.Errorf("measure configured Docker Sandboxes prebuilt artifact availability: %w", err)
			}
		}
		adopted := false
		if err := m.withSandboxBackendLock(ctx, func() error {
			var adoptErr error
			adopted, adoptErr = m.adoptReusableDockerSandboxesTemplateLocked(ctx, manifest, source, manifestHash, rootDisk, runtimeProvider)
			return adoptErr
		}); err != nil {
			return err
		}
		if adopted {
			return m.cleanupSupersededCatalog(ctx)
		}
	}
	if m.DryRun {
		m.infof("[dry-run] would acquire and import verified Docker Sandboxes prebuilt package %s@%s\n", verified.Entry.PackageRepository, verified.Entry.PackageIndexDigest)
		return nil
	}
	return m.buildDockerSandboxesPrebuiltTemplate(ctx, manifest, source, verified, manifestHash, rootDisk, runtimeProvider)
}

func (m *Coordinator) activateCurrentDockerSandboxesPrebuilt(ctx context.Context, manifest Manifest) (bool, error) {
	runtimeProvider, ok := m.Lifecycle.(provider.TemplateArtifactRuntime)
	if !ok {
		return false, errors.New("docker-sandboxes provider is missing required template artifact integration")
	}
	receipt, err := m.readDockerSandboxesReceipt()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	wantHash, err := ManifestHash(manifest)
	if err != nil {
		return false, err
	}
	if receipt.Distribution != dockerSandboxesDistributionPrebuilt || receipt.ManifestHash != wantHash || receipt.Prebuilt == nil {
		return false, nil
	}
	if err := runtimeProvider.VerifyImportedTemplate(ctx, receipt.Artifact); err != nil {
		if errors.Is(err, provider.ErrTemplateNotFound) {
			return false, nil
		}
		return false, err
	}
	if err := activateCommittedDockerSandboxesTemplate(runtimeProvider, receipt.Artifact); err != nil {
		return false, err
	}
	if err := m.recordCurrentSandboxArtifact(ctx, receipt.Artifact, receipt.ManifestHash, receipt.ActivatedAt); err != nil {
		return false, err
	}
	return true, m.cleanupSupersededCatalog(ctx)
}

func (m *Coordinator) ensureDockerSandboxesPrebuiltWithPolicy(ctx context.Context, forceRemote bool) error {
	localManifest, err := m.dockerSandboxesPrebuiltLocalManifest()
	if err != nil {
		return err
	}
	localHash, err := ManifestHash(localManifest)
	if err != nil {
		return err
	}
	now := m.now()
	state, err := m.readUpdatePolicyState()
	if err != nil {
		m.warnf("ignoring stale prebuilt update state and performing an immediate check: %v\n", err)
		state = UpdatePolicyState{SchemaVersion: updatePolicyStateSchemaVersion}
	}
	m.restoreDockerSandboxesPrebuiltAdmissionBlock(state)
	receipt, receiptErr := m.readDockerSandboxesReceipt()
	if receiptErr == nil && receipt.Distribution == dockerSandboxesDistributionPrebuilt && receipt.Prebuilt != nil {
		if state.LocalInputHash == "" {
			state.LocalInputHash = localHash
			state.LastResolvedManifest = &receipt.Manifest
			state.LastResolvedSource = &receipt.Source
			if err := scheduleNextSuccess(&state, m.Config.Image, receipt.ActivatedAt.In(now.Location())); err != nil {
				return err
			}
			if err := m.writeUpdatePolicyState(state); err != nil {
				return err
			}
		}
	} else if receiptErr != nil && !errors.Is(receiptErr, os.ErrNotExist) {
		m.warnf("ignoring invalid Docker Sandboxes prebuilt receipt: %v\n", receiptErr)
	}
	if recalculateScheduleForTimeZone(&state, m.Config.Image, now.Location()) {
		if err := m.writeUpdatePolicyState(state); err != nil {
			return err
		}
	}
	localChanged := state.LocalInputHash != "" && state.LocalInputHash != localHash
	currentVerified := false
	if !localChanged && state.LastResolvedManifest != nil {
		currentVerified, err = m.activateCurrentDockerSandboxesPrebuilt(ctx, *state.LastResolvedManifest)
		if err != nil {
			return err
		}
	}
	if !forceRemote && currentVerified && !updateCheckDue(state, m.Config.Image, now) {
		return nil
	}
	if currentVerified && receiptErr == nil && receipt.Prebuilt != nil {
		statusResolver, ok := m.PrebuiltResolver.(dockerSandboxesPrebuiltStatusResolver)
		if !ok {
			statusErr := errors.New("prebuilt verifier cannot inspect the signed status of the active package digest")
			scheduleUpdateFailure(&state, now, statusErr)
			_ = m.writeUpdatePolicyState(state)
			if !forceRemote {
				m.warnf("scheduled active-package status check failed; retaining the current verified template: %v\n", statusErr)
				return nil
			}
			return statusErr
		}
		status, statusErr := statusResolver.ResolveStatus(ctx, receipt.Prebuilt.PackageIndexDigest)
		if statusErr != nil {
			scheduleUpdateFailure(&state, now, statusErr)
			_ = m.writeUpdatePolicyState(state)
			if !forceRemote {
				m.warnf("scheduled active-package status check failed; retaining the current verified template: %v\n", statusErr)
				return nil
			}
			return statusErr
		}
		switch status.EffectiveStatus {
		case prebuilt.StatusCriticalRevoked:
			critical := &DockerSandboxesPrebuiltStatusError{Digest: status.PackageDigest, Status: status.EffectiveStatus, Reason: status.Entry.RevocationReason}
			m.recordDockerSandboxesPrebuiltResolutionFailure(&state, critical)
			_ = m.writeUpdatePolicyState(state)
		case prebuilt.StatusRevoked:
			m.warnf("active prebuilt package %s is revoked; existing verified instances may continue while a replacement is resolved\n", status.PackageDigest)
		case prebuilt.StatusActive, prebuilt.StatusSuperseded:
		default:
			return fmt.Errorf("active prebuilt package %s has unsupported signed status %s", status.PackageDigest, status.EffectiveStatus)
		}
	}
	state.LastAttemptAt = now.UTC()
	manifest, source, verified, resolveErr := m.dockerSandboxesPrebuiltDesiredManifest(ctx)
	if resolveErr != nil {
		critical := m.recordDockerSandboxesPrebuiltResolutionFailure(&state, resolveErr)
		scheduleUpdateFailure(&state, now, resolveErr)
		_ = m.writeUpdatePolicyState(state)
		if !forceRemote && !localChanged && currentVerified {
			if critical {
				m.warnf("critical prebuilt revocation blocks new Docker Sandboxes admissions while existing instances drain: %v\n", resolveErr)
			} else {
				m.warnf("scheduled prebuilt catalog check failed; continuing with the current verified template: %v\n", resolveErr)
			}
			return nil
		}
		return resolveErr
	}
	state.LocalInputHash = localHash
	state.PendingManifest = &manifest
	state.PendingSource = &source
	state.DeferredReason = "prebuilt package acquisition and activation pending"
	if err := m.writeUpdatePolicyState(state); err != nil {
		return err
	}
	if err := m.ensureDockerSandboxesPrebuiltResolved(ctx, false, manifest, source, verified); err != nil {
		scheduleUpdateFailure(&state, now, err)
		_ = m.writeUpdatePolicyState(state)
		if !forceRemote && !localChanged && currentVerified {
			m.warnf("scheduled prebuilt adoption failed; retaining the current verified template: %v\n", err)
			return nil
		}
		return err
	}
	state.LastResolvedManifest = &manifest
	state.LastResolvedSource = &source
	state.PendingManifest = nil
	state.PendingSource = nil
	m.clearDockerSandboxesPrebuiltAdmissionBlock(&state)
	if err := scheduleNextSuccess(&state, m.Config.Image, m.now()); err != nil {
		return err
	}
	return m.writeUpdatePolicyState(state)
}

func dockerSandboxesPrebuiltBaseTag(verified VerifiedDockerSandboxesPrebuilt) string {
	digest := strings.TrimPrefix(verified.Platform.Digest, "sha256:")
	return "docker.io/library/epar-docker-sandboxes-prebuilt-base:" + digest
}

func (m *Coordinator) dockerSandboxesPrebuiltAcquisitionRoot(verified VerifiedDockerSandboxesPrebuilt) (string, error) {
	configID, err := storagecatalog.ConfigID(m.ProjectRoot, m.effectiveConfigPath())
	if err != nil {
		return "", err
	}
	return filepath.Join(m.ProjectRoot, ".local", "state", "image", configID, "prebuilt", strings.TrimPrefix(verified.Entry.PackageIndexDigest, "sha256:"), strings.ReplaceAll(verified.Platform.Platform, "/", "-")), nil
}

func (m *Coordinator) acquireDockerSandboxesPrebuiltArchive(ctx context.Context, verified VerifiedDockerSandboxesPrebuilt) (string, dockerSandboxesPrebuiltAcquisition, error) {
	localTag := dockerSandboxesPrebuiltBaseTag(verified)
	root, err := m.dockerSandboxesPrebuiltAcquisitionRoot(verified)
	if err != nil {
		return "", dockerSandboxesPrebuiltAcquisition{}, err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", dockerSandboxesPrebuiltAcquisition{}, err
	}
	archivePath := filepath.Join(root, "base-template.tar")
	metadataPath := filepath.Join(root, "acquisition.json")
	var stored dockerSandboxesPrebuiltAcquisition
	if err := readJSONFile(metadataPath, &stored); err == nil {
		if stored.SchemaVersion == dockerSandboxesPrebuiltDerivativeSchema && stored.PackageReference == verified.Entry.PackageReference && stored.PackageIndexDigest == verified.Entry.PackageIndexDigest && stored.PackagePlatformDigest == verified.Platform.Digest && stored.Platform == verified.Platform.Platform && validSHA256(stored.ImageConfigDigest) && validSHA256(stored.ArchiveSHA256) {
			if verifyErr := m.verifyDockerSandboxesPrebuiltBaseArchive(archivePath, localTag, verified, stored); verifyErr == nil {
				if stored.CatalogDigest != verified.CatalogDigest {
					stored.CatalogDigest = verified.CatalogDigest
					if err := writeJSONFile(metadataPath, stored); err != nil {
						return "", dockerSandboxesPrebuiltAcquisition{}, err
					}
				}
				if err := m.recordCurrentPrebuiltArchive(archivePath, verified, stored, m.now().UTC()); err != nil {
					return "", dockerSandboxesPrebuiltAcquisition{}, err
				}
				m.infof("reusing verified Docker Sandboxes prebuilt archive %s\n", verified.Entry.PackageIndexDigest)
				return archivePath, stored, nil
			}
		}
	}
	partialPath := archivePath + ".partial"
	if err := os.Remove(partialPath); err != nil && !os.IsNotExist(err) {
		return "", dockerSandboxesPrebuiltAcquisition{}, fmt.Errorf("remove incomplete prebuilt Docker archive: %w", err)
	}
	partialPublished := false
	defer func() {
		if !partialPublished {
			_ = os.Remove(partialPath)
		}
	}()
	platformReference := verified.Entry.PackageRepository + "@" + verified.Platform.Digest
	ref, err := name.NewDigest(platformReference)
	if err != nil {
		return "", dockerSandboxesPrebuiltAcquisition{}, fmt.Errorf("parse verified prebuilt platform reference: %w", err)
	}
	buildTrust, err := m.resolveBuildTrust(ctx)
	if err != nil {
		return "", dockerSandboxesPrebuiltAcquisition{}, err
	}
	client, err := buildTrustHTTPClient(buildTrust)
	if err != nil {
		return "", dockerSandboxesPrebuiltAcquisition{}, err
	}
	fetch := m.dockerSandboxesPrebuiltImageFetcher
	if fetch == nil {
		fetch = fetchDockerSandboxesPrebuiltImage
	}
	tag, err := name.NewTag(localTag)
	if err != nil {
		return "", dockerSandboxesPrebuiltAcquisition{}, err
	}
	writeArchive := m.dockerSandboxesPrebuiltArchiveWriter
	if writeArchive == nil {
		writeArchive = func(path string, tag name.Tag, image v1.Image) error {
			return tarball.WriteToFile(path, tag, image)
		}
	}
	var configDigest v1.Hash
	materializeTransport := client.Transport
	if fullTransport, ok := dockerSandboxesPrebuiltInitialMaterializationTransport(verified.Entry.Profile, client.Transport); ok {
		materializeTransport = fullTransport
		m.infof("using HTTP/1.1 for the large Docker Sandboxes prebuilt Full archive transfer\n")
	}
	progress := m.startDockerSandboxesPrebuiltArchiveProgress(partialPath, archivePath, verified.Entry.Profile, verified.Platform.Platform)
	defer progress.finish(false)
	for attempt := 1; attempt <= dockerSandboxesPrebuiltMaterializeAttempts; attempt++ {
		progress.setPhase(fmt.Sprintf("downloading/materializing (attempt %d/%d)", attempt, dockerSandboxesPrebuiltMaterializeAttempts))
		attemptCtx, cancel := boundedImageAttempt(ctx, dockerPullAttemptTimeout)
		image, fetchErr := fetch(attemptCtx, ref, materializeTransport)
		if fetchErr != nil {
			cancel()
			classified := classifyImageDependencyFailure(ref.Context().RegistryStr(), "acquire verified prebuilt platform", fetchErr)
			if !isTransientImageDependencyError(fetchErr) || attempt == dockerSandboxesPrebuiltMaterializeAttempts {
				return "", dockerSandboxesPrebuiltAcquisition{}, classified
			}
			if fallback, ok := dockerSandboxesPrebuiltHTTP1RetryTransport(client.Transport); ok {
				materializeTransport = fallback
				m.warnf("verified Docker Sandboxes prebuilt package fetch hit a transient registry failure; retrying once over HTTP/1.1 from the same immutable platform digest: %v\n", fetchErr)
			} else {
				m.warnf("verified Docker Sandboxes prebuilt package fetch hit a transient registry failure; retrying once from the same immutable platform digest: %v\n", fetchErr)
			}
			continue
		}
		manifestDigest, digestErr := image.Digest()
		if digestErr != nil || manifestDigest.String() != verified.Platform.Digest {
			cancel()
			return "", dockerSandboxesPrebuiltAcquisition{}, fmt.Errorf("acquired prebuilt platform digest changed from %s", verified.Platform.Digest)
		}
		configDigest, err = image.ConfigName()
		if err != nil || !validSHA256(configDigest.String()) {
			cancel()
			return "", dockerSandboxesPrebuiltAcquisition{}, errors.New("acquired prebuilt package omitted an exact image config digest")
		}
		writeErr := writeArchive(partialPath, tag, image)
		cancel()
		if writeErr == nil {
			break
		}
		if removeErr := os.Remove(partialPath); removeErr != nil && !os.IsNotExist(removeErr) {
			return "", dockerSandboxesPrebuiltAcquisition{}, fmt.Errorf("clean incomplete prebuilt Docker archive after materialization failure: %w", removeErr)
		}
		classified := classifyImageDependencyFailure(ref.Context().RegistryStr(), "materialize verified prebuilt platform", writeErr)
		if !isTransientImageDependencyError(writeErr) || attempt == dockerSandboxesPrebuiltMaterializeAttempts {
			return "", dockerSandboxesPrebuiltAcquisition{}, fmt.Errorf("materialize verified prebuilt Docker archive: %w", classified)
		}
		if fallback, ok := dockerSandboxesPrebuiltHTTP1RetryTransport(client.Transport); ok {
			materializeTransport = fallback
			m.warnf("verified Docker Sandboxes prebuilt archive materialization hit a transient registry stream failure; retrying once over HTTP/1.1 from the same immutable platform digest: %v\n", writeErr)
		} else {
			m.warnf("verified Docker Sandboxes prebuilt archive materialization hit a transient registry stream failure; retrying once from the same immutable platform digest: %v\n", writeErr)
		}
	}
	progress.setPhase("hashing")
	archiveSHA, archiveBytes, err := hashFile(partialPath)
	if err != nil {
		return "", dockerSandboxesPrebuiltAcquisition{}, err
	}
	stored = dockerSandboxesPrebuiltAcquisition{
		SchemaVersion:         dockerSandboxesPrebuiltDerivativeSchema,
		PackageReference:      verified.Entry.PackageReference,
		PackageIndexDigest:    verified.Entry.PackageIndexDigest,
		PackagePlatformDigest: verified.Platform.Digest,
		ImageConfigDigest:     configDigest.String(),
		CatalogDigest:         verified.CatalogDigest,
		ArchiveSHA256:         archiveSHA,
		ArchiveBytes:          archiveBytes,
		Platform:              verified.Platform.Platform,
		AcquiredAt:            m.now().UTC(),
	}
	progress.setPhase("verifying structure and identity")
	if err := m.verifyDockerSandboxesPrebuiltBaseArchive(partialPath, localTag, verified, stored); err != nil {
		return "", dockerSandboxesPrebuiltAcquisition{}, err
	}
	progress.setPhase("publishing evidence")
	if err := os.Rename(partialPath, archivePath); err != nil {
		return "", dockerSandboxesPrebuiltAcquisition{}, fmt.Errorf("publish verified prebuilt archive: %w", err)
	}
	partialPublished = true
	if err := writeJSONFile(metadataPath, stored); err != nil {
		return "", dockerSandboxesPrebuiltAcquisition{}, err
	}
	if err := m.recordCurrentPrebuiltArchive(archivePath, verified, stored, m.now().UTC()); err != nil {
		return "", dockerSandboxesPrebuiltAcquisition{}, err
	}
	progress.finish(true)
	return archivePath, stored, nil
}

func (m *Coordinator) verifyDockerSandboxesPrebuiltBaseArchive(path, localTag string, verified VerifiedDockerSandboxesPrebuilt, acquisition dockerSandboxesPrebuiltAcquisition) error {
	result, err := verifyDockerSandboxesArchive(path, localTag, verified.Platform.Platform, acquisition.ImageConfigDigest, dockerSandboxesPrebuiltBaseLabels(verified))
	if err != nil {
		return fmt.Errorf("verify prebuilt Docker Sandboxes archive: %w", err)
	}
	if result.ArchiveSHA256 != acquisition.ArchiveSHA256 || result.ArchiveBytes != acquisition.ArchiveBytes {
		return errors.New("prebuilt Docker Sandboxes archive changed during verification")
	}
	return nil
}

func dockerSandboxesPrebuiltBaseLabels(verified VerifiedDockerSandboxesPrebuilt) map[string]string {
	sourcePlatformDigest := verified.Entry.Source.PlatformDigests[verified.Platform.Platform]
	return map[string]string{
		"io.solutionforest.epar.artifact.kind":           "docker-sandboxes-template-base",
		"io.solutionforest.epar.public-base":             "true",
		"io.solutionforest.epar.template.schema-version": "2",
		"io.solutionforest.epar.runtime.contract":        verified.Entry.Recipe.RuntimeContract,
		"io.solutionforest.epar.recipe.digest":           verified.Entry.Recipe.Digest,
		"io.solutionforest.epar.template.profile":        verified.Entry.Profile,
		"io.solutionforest.epar.template.platform":       verified.Platform.Platform,
		"io.solutionforest.epar.source.index-digest":     verified.Entry.Source.IndexDigest,
		"io.solutionforest.epar.source.platform-digest":  sourcePlatformDigest,
		"io.solutionforest.epar.runner.selector":         verified.Entry.Runner.Selector,
		"io.solutionforest.epar.runner.version":          verified.Entry.Runner.Version,
		"io.solutionforest.epar.runner.asset-digest":     verified.Entry.Runner.AssetDigests[verified.Platform.Platform],
		"org.opencontainers.image.base.name":             verified.Entry.Source.Repository + "@" + sourcePlatformDigest,
	}
}

func (m *Coordinator) recordCurrentPrebuiltArchive(path string, verified VerifiedDockerSandboxesPrebuilt, acquisition dockerSandboxesPrebuiltAcquisition, now time.Time) error {
	target, err := storage.SnapshotFilesystemTarget(filepath.Dir(path))
	if err != nil {
		return err
	}
	if target.Kind != storage.TargetDirectory {
		return errors.New("verified prebuilt archive filesystem identity changed before catalog publication")
	}
	store, err := m.hostCatalog()
	if err != nil {
		return err
	}
	_, err = store.WithLock(now, func(value *storagecatalog.Catalog) error {
		configRecord, err := storagecatalog.RegisterConfig(value, m.ProjectRoot, m.effectiveConfigPath(), now)
		if err != nil {
			return err
		}
		if err := m.applyCatalogConfigSettings(value, configRecord.ID); err != nil {
			return err
		}
		resource := storagecatalog.Resource{
			BackendID: "filesystem:" + filepath.VolumeName(target.Locator), Kind: catalogPrebuiltPackageArchiveKind, Provider: "docker-sandboxes", Role: "verified-package-archive",
			Locator: target.Locator, Identity: target.Identity, Fingerprint: target.Fingerprint, Custody: storagecatalog.CustodyAcquired,
			ManifestHash: verified.Entry.PackageIndexDigest, State: storagecatalog.StateCurrent, CreatedAt: now, LastSeenAt: now,
			InstallationIDs: []string{configRecord.InstallationID},
		}
		resource.Key = storagecatalog.ResourceKey(resource.BackendID, resource.Kind, resource.Identity)
		if err := storagecatalog.UpsertResource(value, resource); err != nil {
			return err
		}
		storagecatalog.ReplaceConfigRoleReferences(value, configRecord.ID, "prebuilt-package", map[string]storagecatalog.Reference{resource.Key: {ManifestHash: verified.Entry.PackageIndexDigest}}, now)
		return nil
	})
	return err
}

// buildDockerSandboxesPrebuiltTemplate materializes the verified package
// directly when no custom script is configured. Custom scripts create a small
// local derivative from the immutable package platform; build trust is passed
// only through a temporary BuildKit secret and is not part of the public base.
func (m *Coordinator) buildDockerSandboxesPrebuiltTemplate(ctx context.Context, manifest Manifest, source ResolvedDockerSource, verified VerifiedDockerSandboxesPrebuilt, manifestHash, rootDisk string, runtime provider.TemplateArtifactRuntime) error {
	generation, workspace, buildLock, err := m.beginDockerSandboxesBuild(manifestHash)
	if err != nil {
		return err
	}
	defer buildLock.Close()
	architecture := strings.TrimPrefix(verified.Platform.Platform, "linux/")
	tag := dockerSandboxesGenerationTag("prebuilt-"+verified.Entry.Profile, manifestHash, architecture, generation.ConfigID, generation.Generation)
	localTag := "docker.io/library/" + tag
	if err := m.recordSandboxWorkspace(ctx, workspace, manifestHash, storagecatalog.StateStaging, m.now().UTC()); err != nil {
		return err
	}
	derivative := len(manifest.CustomInstallScripts) != 0
	materialized, err := selectDockerSandboxesPrebuiltMaterialization(
		derivative,
		verified,
		localTag,
		m.now(),
		func() (string, dockerSandboxesPrebuiltAcquisition, error) {
			return m.acquireDockerSandboxesPrebuiltArchive(ctx, verified)
		},
		func() (string, string, error) {
			return m.buildDockerSandboxesPrebuiltDerivative(ctx, workspace, localTag, manifest, verified)
		},
	)
	if err != nil {
		return err
	}
	archivePath, artifactTag, imageDigest, acquisition := materialized.ArchivePath, materialized.ArtifactTag, materialized.ImageDigest, materialized.Acquisition
	rechecked, err := m.resolveVerifiedDockerSandboxesPrebuilt(ctx)
	if err != nil {
		return fmt.Errorf("recheck verified prebuilt package before activation: %w", err)
	}
	if rechecked.CatalogDigest != verified.CatalogDigest || rechecked.Entry.PackageIndexDigest != verified.Entry.PackageIndexDigest || rechecked.Platform.Digest != verified.Platform.Digest {
		return errors.New("prebuilt package or signed catalog changed between verification and activation")
	}
	archiveSHA, archiveBytes, err := hashFile(archivePath)
	if err != nil {
		return err
	}
	artifact := provider.TemplateArtifact{Reference: artifactTag, Digest: imageDigest, Platform: verified.Platform.Platform, RootDisk: rootDisk}
	receipt := dockerSandboxesReceipt{
		SchemaVersion: dockerSandboxesReceiptSchema, Distribution: dockerSandboxesDistributionPrebuilt, ManifestHash: manifestHash, Manifest: manifest, Source: source, Artifact: artifact,
		ArchiveSHA256: archiveSHA, ArchiveBytes: archiveBytes, ActivatedAt: m.now().UTC(),
		Prebuilt: &dockerSandboxesPrebuiltReceiptEvidence{
			CatalogDigest: verified.CatalogDigest, CatalogReference: verified.CatalogReference, EvidenceRef: verified.EvidenceRef, Acceptance: verified.Acceptance,
			Entry: verified.Entry, PackageReference: verified.Package.Reference, PackageIndexDigest: verified.Entry.PackageIndexDigest,
			PackagePlatformDigest: verified.Platform.Digest, EffectiveStatus: verified.EffectiveStatus, VerifiedAt: verified.VerifiedAt,
			BaseArchiveSHA256: acquisition.ArchiveSHA256, BaseArchiveBytes: acquisition.ArchiveBytes, Derivative: derivative,
		},
	}
	activatedAt := time.Time{}
	if err := m.withSandboxBackendLock(ctx, func() error {
		var activationErr error
		activatedAt, activationErr = m.activateDockerSandboxesCandidateWithFinalizerLocked(ctx, receipt, archivePath, true, false, runtime, func(candidate *dockerSandboxesReceipt) error {
			if err := m.writeDockerSandboxesPrebuiltEvidence(workspace, manifest, source, verified, candidate.Artifact, archivePath, archiveSHA, archiveBytes, acquisition, derivative); err != nil {
				return err
			}
			evidence, err := m.persistDockerSandboxesCompactEvidence(manifestHash, candidate.Artifact.CacheID, workspace)
			if err != nil {
				return err
			}
			candidate.Evidence = evidence
			candidate.MetadataSHA256 = evidence["templateMetadata"].SHA256
			return nil
		})
		return activationErr
	}); err != nil {
		return err
	}
	if err := m.finishDockerSandboxesTemplateActivationWorkspace(ctx, workspace, manifestHash, activatedAt); err != nil {
		return err
	}
	m.infof("activated verified prebuilt Docker Sandboxes template %s@%s\n", artifact.Reference, artifact.Digest)
	return nil
}

func (m *Coordinator) buildDockerSandboxesPrebuiltDerivative(ctx context.Context, workspace, localTag string, manifest Manifest, verified VerifiedDockerSandboxesPrebuilt) (string, string, error) {
	manifestHash, err := ManifestHash(manifest)
	if err != nil {
		return "", "", err
	}
	lock, err := loadDockerSandboxesSourceLock(m.ProjectRoot, verified.Platform.Platform)
	if err != nil {
		return "", "", fmt.Errorf("load locked prebuilt derivative inputs: %w", err)
	}
	contextRoot := filepath.Join(workspace, "prebuilt-derivative")
	if err := os.MkdirAll(contextRoot, 0o700); err != nil {
		return "", "", err
	}
	if err := m.prepareDockerSandboxesCustomScripts(contextRoot); err != nil {
		return "", "", err
	}
	trust, err := m.resolveBuildTrust(ctx)
	if err != nil {
		return "", "", err
	}
	var bundle strings.Builder
	for _, certificate := range trust.Certificates {
		bundle.Write(certificate.PEM)
		if len(certificate.PEM) != 0 && certificate.PEM[len(certificate.PEM)-1] != '\n' {
			bundle.WriteByte('\n')
		}
	}
	runCustomInstall := "RUN bash /opt/epar/custom-install/run.sh && rm -rf /opt/epar/custom-install"
	var secretArgs []string
	if bundle.Len() != 0 {
		secretRoot, err := os.MkdirTemp("", "epar-prebuilt-build-trust-")
		if err != nil {
			return "", "", err
		}
		defer os.RemoveAll(secretRoot)
		secretPath := filepath.Join(secretRoot, "ca-bundle.pem")
		if err := writeAtomicFile(secretPath, []byte(bundle.String()), 0o600); err != nil {
			return "", "", err
		}
		runCustomInstall = "RUN --mount=type=secret,id=epar-build-ca-bundle,required=true SSL_CERT_FILE=/run/secrets/epar-build-ca-bundle bash /opt/epar/custom-install/run.sh && rm -rf /opt/epar/custom-install"
		secretArgs = []string{"--secret", "id=epar-build-ca-bundle,src=" + secretPath}
	}
	dockerfile := `# syntax=` + lock.DockerfileFrontend.Reference + `
ARG BASE_IMAGE
ARG TEMPLATE_PLATFORM
FROM --platform=${TEMPLATE_PLATFORM} ${BASE_IMAGE}
USER root
COPY custom-install/ /opt/epar/custom-install/
` + runCustomInstall + `
USER agent
`
	if err := writeAtomicFile(filepath.Join(contextRoot, "Dockerfile"), []byte(dockerfile), 0o600); err != nil {
		return "", "", err
	}
	baseReference := verified.Entry.PackageRepository + "@" + verified.Platform.Digest
	builder, err := m.ensureBuildxBuilder(ctx, []string{baseReference, lock.DockerfileFrontend.Reference})
	if err != nil {
		return "", "", err
	}
	stopBuilder := !m.DryRun
	defer func() {
		if !stopBuilder {
			return
		}
		stopContext, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if stopErr := m.stopBuildxBuilder(stopContext, builder, "release prebuilt derivative builder memory"); stopErr != nil {
			m.warnf("EPAR prebuilt derivative builder shutdown warning: %v\n", stopErr)
		}
	}()
	partialArchive := filepath.Join(workspace, "runner-template.tar.partial")
	archivePath := filepath.Join(workspace, "runner-template.tar")
	metadataPath := filepath.Join(workspace, "derivative-build-metadata.json")
	args := []string{
		"buildx", "build", "--builder", builder, "--platform", verified.Platform.Platform, "--progress", "plain",
		"--output", "type=docker,dest=" + partialArchive, "--provenance=false", "--sbom=false", "--metadata-file", metadataPath, "--tag", strings.TrimPrefix(localTag, "docker.io/library/"),
	}
	args = append(args, secretArgs...)
	args = append(args,
		"--build-arg", "BASE_IMAGE="+baseReference, "--build-arg", "TEMPLATE_PLATFORM="+verified.Platform.Platform,
		"--label", "io.solutionforest.epar.schema=1", "--label", "io.solutionforest.epar.provider=docker-sandboxes", "--label", "io.solutionforest.epar.role=template-staging", "--label", "io.solutionforest.epar.manifest="+manifestHash,
		"--file", filepath.Join(contextRoot, "Dockerfile"), contextRoot,
	)
	logPath := m.buildLogPath("docker-sandboxes-prebuilt-" + strings.TrimPrefix(verified.Platform.Digest, "sha256:")[:16] + ".docker-build.log")
	defer m.releaseTranscript(logPath)
	if err := resetLogs(logPath); err != nil {
		return "", "", err
	}
	if err := m.runHostBuildxLogged(ctx, logPath, "docker", args...); err != nil {
		return "", "", fmt.Errorf("build Docker Sandboxes prebuilt derivative: %w%s", err, boundedRedactedLogTail(logPath, 32*1024))
	}
	var metadata dockerSandboxesBuildMetadata
	if err := readJSONFile(metadataPath, &metadata); err != nil {
		return "", "", err
	}
	expectedLabels := dockerSandboxesPrebuiltBaseLabels(verified)
	expectedLabels["io.solutionforest.epar.schema"] = "1"
	expectedLabels["io.solutionforest.epar.provider"] = "docker-sandboxes"
	expectedLabels["io.solutionforest.epar.role"] = "template-staging"
	expectedLabels["io.solutionforest.epar.manifest"] = manifestHash
	verification, err := verifyDockerSandboxesArchive(partialArchive, localTag, verified.Platform.Platform, metadata.ImageDigest, expectedLabels)
	if err != nil {
		return "", "", fmt.Errorf("verify Docker Sandboxes prebuilt derivative archive: %w", err)
	}
	if err := os.Rename(partialArchive, archivePath); err != nil {
		return "", "", err
	}
	stopBuilder = false
	if err := m.stopBuildxBuilder(ctx, builder, "release prebuilt derivative builder memory"); err != nil {
		return "", "", err
	}
	return archivePath, verification.ImageDigest, nil
}

func (m *Coordinator) writeDockerSandboxesPrebuiltEvidence(root string, manifest Manifest, source ResolvedDockerSource, verified VerifiedDockerSandboxesPrebuilt, artifact provider.TemplateArtifact, archivePath, archiveSHA string, archiveBytes uint64, acquisition dockerSandboxesPrebuiltAcquisition, derivative bool) error {
	manifestHash, err := ManifestHash(manifest)
	if err != nil {
		return err
	}
	summary := map[string]any{"schemaVersion": 1, "catalogDigest": verified.CatalogDigest, "packageIndexDigest": verified.Entry.PackageIndexDigest, "packagePlatformDigest": verified.Platform.Digest, "evidence": verified.Entry.Evidence, "gates": verified.Entry.Gates, "effectiveStatus": verified.EffectiveStatus}
	for filename, value := range map[string]any{
		"attestation-metadata.json": summary,
		"build-metadata.json":       map[string]any{"containerimage.digest": artifact.Digest, "distribution": dockerSandboxesDistributionPrebuilt, "derivative": derivative},
		"provenance.json":           map[string]any{"verifiedDigest": verified.Entry.Evidence.ProvenanceDigest, "subject": verified.Entry.PackageIndexDigest},
		"sbom.intoto.json":          map[string]any{"verifiedDigest": verified.Entry.Evidence.SBOMDigest, "subject": verified.Entry.PackageIndexDigest},
		"compatibility.json":        map[string]any{"runtimeContract": verified.Entry.Recipe.RuntimeContract, "templateSchema": verified.Entry.Recipe.TemplateSchema, "platform": verified.Platform.Platform},
	} {
		if err := writeJSONFile(filepath.Join(root, filename), value); err != nil {
			return err
		}
	}
	if err := writeAtomicFile(filepath.Join(root, "software-inventory.txt"), []byte("verified-prebuilt-package "+verified.Entry.PackageIndexDigest+"\n"), 0o600); err != nil {
		return err
	}
	metadata := dockerSandboxesTemplateMetadata{SchemaVersion: dockerSandboxesMetadataSchema, Profile: verified.Entry.Profile, Platform: verified.Platform.Platform, ManifestHash: manifestHash, Source: source, Artifacts: map[string]artifactEvidence{}}
	metadata.Template.Tag = artifact.Reference
	metadata.Template.Digest = artifact.Digest
	metadata.Template.CacheID = artifact.CacheID
	metadata.Template.RootDisk = artifact.RootDisk
	metadata.Template.Archive = filepath.Base(archivePath)
	metadata.Template.ArchiveSHA256 = archiveSHA
	metadata.Template.ArchiveBytes = archiveBytes
	metadata.Compatibility.TemplateSchemaVersion = verified.Entry.Recipe.TemplateSchema
	metadata.Compatibility.RunnerExecution = "direct-actions-listener"
	metadata.Compatibility.DockerDaemonOwner = "docker-sandboxes-runtime"
	metadata.Compatibility.ExpectedDockerDaemonCount = 1
	metadata.Compatibility.EmulationBackend = "qemu"
	metadata.Compatibility.EmulationPolicy = "configured-best-effort-required-or-native-only"
	metadata.Compatibility.EmulationRelease = "catalog-verified"
	metadata.Compatibility.EmulationSourceDigest = source.IndexDigest
	metadata.Compatibility.EmulationManifestDigest = source.PlatformDigest
	metadata.Compatibility.QEMUVersion = "catalog-verified"
	for name, filename := range map[string]string{"buildMetadata": "build-metadata.json", "attestationMetadata": "attestation-metadata.json", "provenance": "provenance.json", "sbom": "sbom.intoto.json", "softwareInventory": "software-inventory.txt", "compatibility": "compatibility.json"} {
		digest, _, err := hashFile(filepath.Join(root, filename))
		if err != nil {
			return err
		}
		metadata.Artifacts[name] = artifactEvidence{Path: filename, SHA256: digest}
	}
	if err := writeJSONFile(filepath.Join(root, "template-metadata.json"), metadata); err != nil {
		return err
	}
	_ = acquisition
	return nil
}
