package image

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

const catthehackerUbuntuRepository = "ghcr.io/catthehacker/ubuntu"

var probeAnonymousOCIReference = func(ctx context.Context, reference string, transport http.RoundTripper) error {
	ref, err := name.ParseReference(reference)
	if err != nil {
		return err
	}
	_, err = remote.Get(ref, remote.WithContext(ctx), remote.WithAuth(authn.Anonymous), remote.WithTransport(transport))
	return err
}

func (m *Coordinator) explainBuiltInCatthehackerAuthFailure(ctx context.Context, reference string, cause error) error {
	if cause == nil || !isCatthehackerReference(reference) || !isRegistryAuthorizationFailure(cause) {
		return cause
	}
	buildTrust, err := m.resolveBuildTrust(ctx)
	if err != nil {
		return cause
	}
	client, err := buildTrustHTTPClient(buildTrust)
	if err != nil {
		return cause
	}
	return explainBuiltInCatthehackerAuthFailureWithProbe(ctx, reference, cause, client.Transport, probeAnonymousOCIReference)
}

func explainBuiltInCatthehackerAuthFailureWithProbe(ctx context.Context, reference string, cause error, transport http.RoundTripper, probe func(context.Context, string, http.RoundTripper) error) error {
	if cause == nil || !isCatthehackerReference(reference) || !isRegistryAuthorizationFailure(cause) {
		return cause
	}
	if err := probe(ctx, reference, transport); err != nil {
		return cause
	}
	return fmt.Errorf("%w; EPAR separately confirmed that this built-in Catthehacker source is anonymously readable. Docker Buildx may be using a stale, revoked, wrong-account, or insufficiently scoped ghcr.io credential. EPAR did not alter credentials or retry the failed operation anonymously. For public-only usage, remove or refresh that credential yourself; removing it may also remove access to private GHCR packages. For private GHCR usage, sign in again with a valid credential that has package pull access", cause)
}

func isCatthehackerReference(reference string) bool {
	ref, err := name.ParseReference(strings.TrimSpace(reference))
	return err == nil && ref.Context().Name() == catthehackerUbuntuRepository
}

func isRegistryAuthorizationFailure(err error) bool {
	message := strings.ToLower(err.Error())
	authorizationShaped := strings.Contains(message, "failed to authorize") || strings.Contains(message, "failed to fetch oauth token") || strings.Contains(message, "unauthorized")
	statusShaped := strings.Contains(message, "401 unauthorized") || strings.Contains(message, "401: unauthorized") || strings.Contains(message, "403 forbidden") || strings.Contains(message, "403: forbidden")
	return authorizationShaped && statusShaped
}
