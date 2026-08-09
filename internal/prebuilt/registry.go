package prebuilt

// This file contains the registry-facing side of the prebuilt contract. The
// publisher may use mutable source selectors while it plans a build; the
// runtime consumer only accepts OCI descriptors and verified referrers.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

const (
	// ReferrerBundleMediaType is the OCI layer media type emitted by
	// actions/attest when an attestation bundle is pushed to a registry.
	ReferrerBundleMediaType = "application/vnd.dev.sigstore.bundle+json;version=0.3"
	// ReferrerPayloadMediaType is the in-toto statement media type accepted by
	// the strict verifier. The bundle itself remains the signed object.
	ReferrerPayloadMediaType = "application/vnd.in-toto+json"

	maxCatalogBytes  = 16 << 20
	maxReferrerBytes = 64 << 20
	maxManifestBytes = 8 << 20
)

// RegistryDescriptor is the untrusted metadata returned by a registry. The
// digest is authoritative only after the registry client has resolved the
// descriptor and, for blobs, verified the bytes against it.
type RegistryDescriptor struct {
	Repository   string
	Reference    string
	Digest       string
	MediaType    string
	Size         int64
	ArtifactType string
	Annotations  map[string]string
}

// RegistryReferrer is one OCI artifact attached to a package or catalog
// subject. Payload is the exact signed Sigstore bundle bytes from its layer.
type RegistryReferrer struct {
	Descriptor RegistryDescriptor
	Payload    []byte
}

// CatalogArtifact is the catalog JSON plus the descriptor identity of the
// OCI manifest carrying it. ManifestDigest must be used as the referrer
// subject; CanonicalDigest is the immutable catalog tag suffix.
type CatalogArtifact struct {
	Catalog         Catalog
	Reference       string
	ManifestDigest  string
	CanonicalDigest string
}

// CatalogRegistry is intentionally small so runtime tests can use a fake
// registry while production uses RemoteCatalogRegistry. Implementations must
// be anonymous by default for the public GHCR package and must never resolve
// a mutable ref without returning its descriptor digest.
type CatalogRegistry interface {
	Resolve(ctx context.Context, reference string) (ResolvedReference, error)
	FetchCatalog(ctx context.Context, reference string) (CatalogArtifact, error)
	Referrers(ctx context.Context, subject string) ([]RegistryReferrer, error)
}

// RemoteCatalogRegistry implements CatalogRegistry with go-containerregistry.
// Authenticator is nil/anonymous by default; callers needing a private
// registry must inject one explicitly rather than reading Docker config.
type RemoteCatalogRegistry struct {
	Authenticator authn.Authenticator
	Transport     remote.Option
	Resolver      DescriptorResolver
}

// NewRemoteCatalogRegistry returns an anonymous public-registry client.
func NewRemoteCatalogRegistry() *RemoteCatalogRegistry {
	return &RemoteCatalogRegistry{
		Authenticator: authn.Anonymous,
		Resolver:      RemoteDescriptorResolver{Authenticator: authn.Anonymous},
	}
}

func (r *RemoteCatalogRegistry) auth() authn.Authenticator {
	if r != nil && r.Authenticator != nil {
		return r.Authenticator
	}
	return authn.Anonymous
}

func (r *RemoteCatalogRegistry) resolveOptions(ctx context.Context) []remote.Option {
	opts := []remote.Option{remote.WithContext(ctx), remote.WithAuth(r.auth())}
	if r != nil && r.Transport != nil {
		opts = append(opts, r.Transport)
	}
	return opts
}

// Resolve resolves a tag or digest to the registry's descriptor digest. It
// deliberately delegates to RemoteDescriptorResolver so platform digests are
// normalized consistently with publisher source resolution.
func (r *RemoteCatalogRegistry) Resolve(ctx context.Context, reference string) (ResolvedReference, error) {
	if r == nil {
		return ResolvedReference{}, errors.New("nil remote catalog registry")
	}
	resolver := r.Resolver
	if resolver == nil {
		resolver = RemoteDescriptorResolver{Authenticator: r.auth()}
	}
	return resolver.Resolve(ctx, reference)
}

// FetchCatalog fetches the catalog artifact JSON layer, validates the catalog,
// and returns both the manifest descriptor digest and canonical JSON digest.
func (r *RemoteCatalogRegistry) FetchCatalog(ctx context.Context, reference string) (CatalogArtifact, error) {
	if r == nil {
		return CatalogArtifact{}, errors.New("nil remote catalog registry")
	}
	parsed, err := name.ParseReference(reference)
	if err != nil {
		return CatalogArtifact{}, fmt.Errorf("parse catalog reference %q: %w", reference, err)
	}
	desc, err := remote.Get(parsed, r.resolveOptions(ctx)...)
	if err != nil {
		return CatalogArtifact{}, fmt.Errorf("resolve catalog %q: %w", reference, err)
	}
	if desc.MediaType != types.OCIManifestSchema1 {
		return CatalogArtifact{}, fmt.Errorf("catalog %q must be an OCI manifest, got %s", reference, desc.MediaType)
	}
	if len(desc.Manifest) == 0 || len(desc.Manifest) > maxManifestBytes {
		return CatalogArtifact{}, fmt.Errorf("catalog %q manifest is empty or too large", reference)
	}
	manifest, err := v1.ParseManifest(bytes.NewReader(desc.Manifest))
	if err != nil {
		return CatalogArtifact{}, fmt.Errorf("parse catalog manifest: %w", err)
	}
	if manifest.ArtifactType != CatalogArtifactType {
		return CatalogArtifact{}, fmt.Errorf("catalog %q has unexpected artifact type %q", reference, manifest.ArtifactType)
	}
	if string(manifest.Config.MediaType) != CatalogConfigMediaType {
		return CatalogArtifact{}, fmt.Errorf("catalog %q has unexpected config media type %q", reference, manifest.Config.MediaType)
	}
	layers := make([]v1.Descriptor, 0, 1)
	for _, candidate := range manifest.Layers {
		if string(candidate.MediaType) == CatalogArtifactMediaType {
			layers = append(layers, candidate)
		}
	}
	if len(layers) != 1 {
		return CatalogArtifact{}, fmt.Errorf("catalog %q must contain exactly one JSON layer, found %d", reference, len(layers))
	}
	payload, layer, err := r.fetchLayer(ctx, parsed.Context(), layers[0], maxCatalogBytes)
	if err != nil {
		return CatalogArtifact{}, fmt.Errorf("fetch catalog JSON layer: %w", err)
	}
	var catalog Catalog
	if err := json.Unmarshal(payload, &catalog); err != nil {
		return CatalogArtifact{}, fmt.Errorf("decode catalog JSON: %w", err)
	}
	if err := catalog.Validate(); err != nil {
		return CatalogArtifact{}, fmt.Errorf("validate catalog: %w", err)
	}
	digest, err := catalog.CatalogDigest()
	if err != nil {
		return CatalogArtifact{}, fmt.Errorf("canonicalize catalog: %w", err)
	}
	if layer.Size > 0 && int64(len(payload)) != layer.Size {
		return CatalogArtifact{}, fmt.Errorf("catalog JSON layer size mismatch: descriptor %d, received %d", layer.Size, len(payload))
	}
	if tag, ok := parsed.(name.Tag); ok {
		if suffix := strings.TrimPrefix(tag.TagStr(), CatalogMovingTag+"-pkg-"); suffix != tag.TagStr() {
			if digest != "sha256:"+suffix {
				return CatalogArtifact{}, fmt.Errorf("immutable catalog tag %s does not match canonical catalog digest %s", tag.TagStr(), digest)
			}
		}
	}
	return CatalogArtifact{
		Catalog:         catalog,
		Reference:       reference,
		ManifestDigest:  desc.Digest.String(),
		CanonicalDigest: digest,
	}, nil
}

// Referrers returns all registry referrers and loads exactly one signed bundle
// layer from each artifact. Empty or malformed artifacts are hard failures;
// consumers must not silently fall back to unsigned labels or workflow logs.
func (r *RemoteCatalogRegistry) Referrers(ctx context.Context, subject string) ([]RegistryReferrer, error) {
	if r == nil {
		return nil, errors.New("nil remote catalog registry")
	}
	parsed, err := name.ParseReference(subject)
	if err != nil {
		return nil, fmt.Errorf("parse referrer subject %q: %w", subject, err)
	}
	digestRef, ok := parsed.(name.Digest)
	if !ok {
		return nil, fmt.Errorf("referrer subject must be immutable digest, got %q", subject)
	}
	idx, err := remote.Referrers(digestRef, r.resolveOptions(ctx)...)
	if err != nil {
		return nil, fmt.Errorf("fetch referrers for %s: %w", subject, err)
	}
	manifest, err := idx.IndexManifest()
	if err != nil {
		return nil, fmt.Errorf("parse referrer index: %w", err)
	}
	subjectDescriptor, err := remote.Get(digestRef, r.resolveOptions(ctx)...)
	if err != nil {
		return nil, fmt.Errorf("resolve referrer subject %s: %w", subject, err)
	}
	result := make([]RegistryReferrer, 0, len(manifest.Manifests))
	for _, descriptor := range manifest.Manifests {
		artifactRef := digestRef.Context().Digest(descriptor.Digest.String())
		desc, err := remote.Get(artifactRef, r.resolveOptions(ctx)...)
		if err != nil {
			return nil, fmt.Errorf("fetch referrer manifest %s: %w", descriptor.Digest, err)
		}
		if desc.MediaType != types.OCIManifestSchema1 {
			return nil, fmt.Errorf("referrer %s is not an OCI manifest", descriptor.Digest)
		}
		refManifest, err := v1.ParseManifest(bytes.NewReader(desc.Manifest))
		if err != nil {
			return nil, fmt.Errorf("parse referrer %s manifest: %w", descriptor.Digest, err)
		}
		if refManifest.Subject == nil {
			return nil, fmt.Errorf("referrer %s has no OCI subject", descriptor.Digest)
		}
		if refManifest.Subject.Digest.String() != digestRef.DigestStr() ||
			refManifest.Subject.MediaType != subjectDescriptor.MediaType ||
			refManifest.Subject.Size != subjectDescriptor.Size {
			return nil, fmt.Errorf("referrer %s subject descriptor does not exactly match %s", descriptor.Digest, subject)
		}
		if !isSigstoreArtifactType(refManifest.ArtifactType) {
			return nil, fmt.Errorf("referrer %s has unexpected artifact type %q", descriptor.Digest, refManifest.ArtifactType)
		}
		bundleLayers := make([]v1.Descriptor, 0, 1)
		for _, candidate := range refManifest.Layers {
			if isSigstoreBundleMediaType(string(candidate.MediaType)) {
				bundleLayers = append(bundleLayers, candidate)
			}
		}
		if len(bundleLayers) != 1 {
			return nil, fmt.Errorf("referrer %s must contain exactly one Sigstore bundle layer, found %d", descriptor.Digest, len(bundleLayers))
		}
		payload, layer, err := r.fetchLayer(ctx, digestRef.Context(), bundleLayers[0], maxReferrerBytes)
		if err != nil {
			return nil, fmt.Errorf("fetch referrer %s bundle: %w", descriptor.Digest, err)
		}
		result = append(result, RegistryReferrer{
			Descriptor: RegistryDescriptor{
				Repository:   digestRef.Context().RepositoryStr(),
				Reference:    artifactRef.String(),
				Digest:       descriptor.Digest.String(),
				MediaType:    string(descriptor.MediaType),
				Size:         descriptor.Size,
				ArtifactType: descriptor.ArtifactType,
				Annotations:  cloneStringMap(descriptor.Annotations),
			},
			Payload: payload,
		})
		if layer.Size > 0 && int64(len(payload)) != layer.Size {
			return nil, fmt.Errorf("referrer %s bundle size mismatch: descriptor %d, received %d", descriptor.Digest, layer.Size, len(payload))
		}
	}
	return result, nil
}

func (r *RemoteCatalogRegistry) fetchJSONLayer(ctx context.Context, repo name.Repository, layers []v1.Descriptor, mediaType string, maxBytes int64) ([]byte, v1.Descriptor, error) {
	for _, layer := range layers {
		if string(layer.MediaType) != mediaType {
			continue
		}
		return r.fetchLayer(ctx, repo, layer, maxBytes)
	}
	return nil, v1.Descriptor{}, fmt.Errorf("required layer media type %q is missing", mediaType)
}

func (r *RemoteCatalogRegistry) fetchBundleLayer(ctx context.Context, repo name.Repository, layers []v1.Descriptor, maxBytes int64) ([]byte, v1.Descriptor, error) {
	for _, layer := range layers {
		mediaType := string(layer.MediaType)
		if mediaType != ReferrerBundleMediaType && mediaType != ReferrerPayloadMediaType && !strings.Contains(mediaType, "sigstore.bundle") {
			continue
		}
		return r.fetchLayer(ctx, repo, layer, maxBytes)
	}
	return nil, v1.Descriptor{}, errors.New("sigstore bundle layer is missing")
}

func isSigstoreArtifactType(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	return value == "application/vnd.dev.sigstore.bundle.v0.3+json" || value == "application/vnd.dev.sigstore.bundle+json" || strings.HasPrefix(value, "application/vnd.dev.sigstore.bundle+")
}

func isSigstoreBundleMediaType(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	return value == strings.ToLower(ReferrerBundleMediaType) || strings.HasPrefix(value, "application/vnd.dev.sigstore.bundle+")
}

func (r *RemoteCatalogRegistry) fetchLayer(ctx context.Context, repo name.Repository, descriptor v1.Descriptor, maxBytes int64) ([]byte, v1.Descriptor, error) {
	if descriptor.Digest.String() == "" {
		return nil, v1.Descriptor{}, errors.New("layer digest is missing")
	}
	layer, err := remote.Layer(repo.Digest(descriptor.Digest.String()), r.resolveOptions(ctx)...)
	if err != nil {
		return nil, v1.Descriptor{}, err
	}
	reader, err := layer.Uncompressed()
	if err != nil {
		return nil, v1.Descriptor{}, err
	}
	defer reader.Close()
	limited := io.LimitReader(reader, maxBytes+1)
	payload, err := io.ReadAll(limited)
	if err != nil {
		return nil, v1.Descriptor{}, err
	}
	if int64(len(payload)) > maxBytes {
		return nil, v1.Descriptor{}, fmt.Errorf("layer exceeds %d bytes", maxBytes)
	}
	return payload, descriptor, nil
}

func cloneStringMap(input map[string]string) map[string]string {
	if len(input) == 0 {
		return nil
	}
	output := make(map[string]string, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}
