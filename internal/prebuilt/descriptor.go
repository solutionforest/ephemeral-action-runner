package prebuilt

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// PlatformDescriptor is the registry descriptor selected from an OCI index.
// Digest is obtained from the descriptor itself, never by hashing formatted
// `imagetools inspect --raw` output.
type PlatformDescriptor struct {
	Platform  string `json:"platform"`
	Digest    string `json:"digest"`
	MediaType string `json:"mediaType,omitempty"`
	Size      int64  `json:"size,omitempty"`
}

// ResolvedReference is an immutable observation of a registry reference.
// Reference may be a tag, but Digest is always the registry-provided content
// digest and is the only field suitable for package identity.
type ResolvedReference struct {
	Reference  string                        `json:"reference"`
	Repository string                        `json:"repository"`
	Digest     string                        `json:"digest"`
	MediaType  string                        `json:"mediaType"`
	Size       int64                         `json:"size"`
	Platforms  map[string]PlatformDescriptor `json:"platforms,omitempty"`
}

// DescriptorResolver is deliberately small so publisher tests can inject a
// deterministic fake and the runtime can share the same source verification
// boundary without depending on a shell command.
type DescriptorResolver interface {
	Resolve(ctx context.Context, reference string) (ResolvedReference, error)
}

// RemoteDescriptorResolver resolves OCI descriptors using the registry HTTP
// API through go-containerregistry. Authentication and transport are
// injectable for CI, private mirrors, and tests.
type RemoteDescriptorResolver struct {
	Authenticator authn.Authenticator
	Transport     http.RoundTripper
}

func (r RemoteDescriptorResolver) Resolve(ctx context.Context, reference string) (ResolvedReference, error) {
	trimmed := strings.TrimSpace(reference)
	if trimmed == "" {
		return ResolvedReference{}, fmt.Errorf("registry reference is required")
	}
	ref, err := name.ParseReference(trimmed)
	if err != nil {
		return ResolvedReference{}, fmt.Errorf("parse OCI reference %q: %w", reference, err)
	}
	authenticator := r.Authenticator
	if authenticator == nil {
		// Public Catthehacker and EPAR GHCR packages must resolve without
		// consulting a host Docker credential store. Hosts may have an
		// inaccessible or malformed config; private use can inject an explicit
		// authenticator through RemoteDescriptorResolver.Authenticator.
		authenticator = authn.Anonymous
	}
	options := []remote.Option{remote.WithContext(ctx), remote.WithAuth(authenticator)}
	if r.Transport != nil {
		options = append(options, remote.WithTransport(r.Transport))
	}
	descriptor, err := remote.Get(ref, options...)
	if err != nil {
		return ResolvedReference{}, fmt.Errorf("resolve OCI descriptor %s: %w", trimmed, err)
	}
	digest := descriptor.Digest.String()
	if _, err := NormalizeDigest(digest); err != nil {
		return ResolvedReference{}, fmt.Errorf("registry returned invalid digest for %s: %w", trimmed, err)
	}
	result := ResolvedReference{
		Reference:  trimmed,
		Repository: ref.Context().Name(),
		Digest:     digest,
		MediaType:  string(descriptor.MediaType),
		Size:       descriptor.Size,
	}
	raw, err := descriptor.RawManifest()
	if err != nil {
		return result, fmt.Errorf("read OCI descriptor manifest %s: %w", trimmed, err)
	}
	index, parseErr := v1.ParseIndexManifest(strings.NewReader(string(raw)))
	if parseErr != nil {
		// A single-platform image is a valid OCI artifact, but has no platform
		// map. Callers that require a multi-platform package should enforce that
		// policy explicitly.
		return result, nil
	}
	result.Platforms = make(map[string]PlatformDescriptor, len(index.Manifests))
	for _, manifest := range index.Manifests {
		if manifest.Platform == nil || manifest.Platform.OS == "" || manifest.Platform.Architecture == "" {
			continue
		}
		platform := NormalizePlatform(manifest.Platform.OS + "/" + manifest.Platform.Architecture + func() string {
			if manifest.Platform.Variant == "" {
				return ""
			}
			return "/" + manifest.Platform.Variant
		}())
		if _, exists := result.Platforms[platform]; exists {
			return result, fmt.Errorf("OCI index %s contains duplicate platform %s", trimmed, platform)
		}
		result.Platforms[platform] = PlatformDescriptor{Platform: platform, Digest: manifest.Digest.String(), MediaType: string(manifest.MediaType), Size: manifest.Size}
	}
	return result, nil
}

// ResolvePlatform requires a unique platform descriptor in an immutable
// registry observation. It is used by publication gates before Buildx and
// again immediately before alias promotion.
func ResolvePlatform(resolved ResolvedReference, platform string) (PlatformDescriptor, error) {
	platform = NormalizePlatform(platform)
	if platform == "" {
		return PlatformDescriptor{}, fmt.Errorf("platform is required")
	}
	selected, ok := resolved.Platforms[platform]
	if !ok {
		return PlatformDescriptor{}, fmt.Errorf("OCI reference %s does not provide platform %s", resolved.Reference, platform)
	}
	if _, err := NormalizeDigest(selected.Digest); err != nil {
		return PlatformDescriptor{}, fmt.Errorf("OCI reference %s platform %s digest: %w", resolved.Reference, platform, err)
	}
	return selected, nil
}

// NormalizePlatform maps the common arm64/v8 OCI spelling to EPAR's
// platform contract. Other variants remain explicit instead of being
// silently coerced.
func NormalizePlatform(platform string) string {
	platform = strings.ToLower(strings.TrimSpace(platform))
	if platform == "linux/arm64/v8" {
		return "linux/arm64"
	}
	return platform
}

// RecheckUnchanged resolves a mutable selector again and rejects a TOCTOU
// move. A caller can retain the first observation as a candidate but must not
// advance a moving alias from it.
func RecheckUnchanged(ctx context.Context, resolver DescriptorResolver, reference, expectedDigest string) (ResolvedReference, error) {
	if resolver == nil {
		return ResolvedReference{}, fmt.Errorf("descriptor resolver is required")
	}
	observed, err := resolver.Resolve(ctx, reference)
	if err != nil {
		return ResolvedReference{}, err
	}
	expectedDigest, err = NormalizeDigest(expectedDigest)
	if err != nil {
		return ResolvedReference{}, err
	}
	if observed.Digest != expectedDigest {
		return observed, fmt.Errorf("mutable OCI reference %s moved from %s to %s", reference, expectedDigest, observed.Digest)
	}
	return observed, nil
}
