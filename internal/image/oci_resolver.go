package image

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// resolveTartOCIReference observes only the registry descriptor. Tart remains
// responsible for pulling its VM artifact; EPAR records and clones the exact
// immutable OCI identity selected by the schedule.
func (m *Coordinator) resolveTartOCIReference(ctx context.Context, reference string) (string, error) {
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
	descriptor, err := remote.Get(ref, remote.WithContext(ctx), remote.WithAuth(authenticator), remote.WithTransport(client.Transport))
	if err != nil {
		return "", fmt.Errorf("resolve Tart OCI source %s: %w", reference, err)
	}
	digest := descriptor.Digest.String()
	if !validSHA256(digest) {
		return "", fmt.Errorf("Tart OCI source %s returned an invalid immutable digest", reference)
	}
	return ref.Context().Name() + "@" + digest, nil
}
