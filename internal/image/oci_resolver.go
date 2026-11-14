package image

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/solutionforest/ephemeral-action-runner/internal/config"
	"github.com/solutionforest/ephemeral-action-runner/internal/hosttrust"
)

type TartSource struct {
	Reference          string
	ImmutableReference string
	Platform           string
	CompressedBytes    uint64
	ExpandedBytes      uint64
}

// ResolveTartSource reads metadata only; no VM disk or Docker tool is needed.
func ResolveTartSource(ctx context.Context, reference, platform string) (TartSource, error) {
	client, err := buildTrustHTTPClient(hosttrust.Snapshot{})
	if err != nil {
		return TartSource{}, err
	}
	return resolveTartSource(ctx, reference, platform, client.Transport)
}

func resolveTartSource(ctx context.Context, reference, platform string, transport http.RoundTripper) (TartSource, error) {
	ref, err := name.ParseReference(strings.TrimSpace(reference))
	if err != nil {
		return TartSource{}, fmt.Errorf("parse Tart OCI source: %w", err)
	}
	authenticator, err := authn.DefaultKeychain.Resolve(ref.Context().Registry)
	if err != nil {
		return TartSource{}, fmt.Errorf("resolve Tart registry credentials: %w", err)
	}
	attemptCtx, cancel := boundedImageAttempt(ctx, imageMetadataAttemptTimeout)
	defer cancel()
	descriptor, err := remote.Get(ref, remote.WithContext(attemptCtx), remote.WithAuth(authenticator), remote.WithTransport(transport))
	if err != nil {
		return TartSource{}, classifyImageDependencyFailure(ref.Context().RegistryStr(), "resolve Tart OCI image", err)
	}
	img, err := descriptor.Image()
	if err != nil {
		return TartSource{}, fmt.Errorf("Tart source must be a single VM manifest: %w", err)
	}
	manifest, err := img.Manifest()
	if err != nil {
		return TartSource{}, err
	}
	guest, err := img.ConfigFile()
	if err != nil {
		return TartSource{}, err
	}
	return parseTartSource(ref.Name(), ref.Context().Name()+"@"+descriptor.Digest.String(), platform, manifest, guest)
}

func parseTartSource(reference, immutable, platform string, manifest *v1.Manifest, guest *v1.ConfigFile) (TartSource, error) {
	observed := guest.OS + "/" + guest.Architecture
	if observed != platform {
		return TartSource{}, fmt.Errorf("Tart source platform %s does not match configured %s", observed, platform)
	}
	expanded, err := strconv.ParseUint(manifest.Annotations["org.cirruslabs.tart.uncompressed-disk-size"], 10, 64)
	if err != nil || expanded == 0 {
		return TartSource{}, fmt.Errorf("Tart source omitted its uncompressed disk size")
	}
	var compressed uint64
	disk := false
	for _, layer := range manifest.Layers {
		if layer.Size < 0 || uint64(layer.Size) > math.MaxUint64-compressed {
			return TartSource{}, fmt.Errorf("Tart source layer size overflows")
		}
		compressed += uint64(layer.Size)
		if strings.HasPrefix(string(layer.MediaType), "application/vnd.cirruslabs.tart.disk.") {
			disk = true
		}
	}
	if !disk || compressed == 0 {
		return TartSource{}, fmt.Errorf("Tart source has no VM disk layers")
	}
	return TartSource{Reference: reference, ImmutableReference: immutable, Platform: platform, CompressedBytes: compressed, ExpandedBytes: expanded}, nil
}

// resolveTartOCIReference observes only the registry descriptor. Tart remains
// responsible for pulling its VM artifact; EPAR records and clones the exact
// immutable OCI identity selected by the schedule.
func (m *Coordinator) resolveTartOCIReference(ctx context.Context, reference string) (string, error) {
	if m.Config.Image.SourceType == config.ImageSourceTartImage {
		trust, err := m.resolveBuildTrust(ctx)
		if err != nil {
			return "", err
		}
		client, err := buildTrustHTTPClient(trust)
		if err != nil {
			return "", err
		}
		platform := m.Config.Provider.Platform
		if platform == "" {
			platform = "linux/arm64"
		}
		source, err := resolveTartSource(ctx, reference, platform, client.Transport)
		return source.ImmutableReference, err
	}
	ref, err := name.ParseReference(strings.TrimSpace(reference))
	if err != nil {
		return "", fmt.Errorf("parse Tart OCI source %q: %w", reference, err)
	}
	authenticator, err := authn.DefaultKeychain.Resolve(ref.Context().Registry)
	if err != nil {
		return "", fmt.Errorf("resolve Tart OCI registry credentials: %w", err)
	}
	buildTrust, err := m.resolveBuildTrust(ctx)
	if err != nil {
		return "", err
	}
	client, err := buildTrustHTTPClient(buildTrust)
	if err != nil {
		return "", err
	}
	attemptCtx, cancel := boundedImageAttempt(ctx, imageMetadataAttemptTimeout)
	defer cancel()
	descriptor, err := remote.Get(ref, remote.WithContext(attemptCtx), remote.WithAuth(authenticator), remote.WithTransport(client.Transport))
	if err != nil {
		cause := fmt.Errorf("resolve Tart OCI source %s: %w", reference, err)
		return "", classifyImageDependencyFailure(ref.Context().RegistryStr(), "resolve Tart OCI source", cause)
	}
	digest := descriptor.Digest.String()
	if !validSHA256(digest) {
		return "", fmt.Errorf("Tart OCI source %s returned an invalid immutable digest", reference)
	}
	return ref.Context().Name() + "@" + digest, nil
}
