package image

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestExplainBuiltInCatthehackerAuthFailureWithSuccessfulAnonymousProbe(t *testing.T) {
	for _, status := range []string{"401 Unauthorized", "403 Forbidden"} {
		t.Run(status, func(t *testing.T) {
			cause := errors.New("docker buildx imagetools inspect failed: failed to authorize: failed to fetch oauth token: " + status)
			called := false
			err := explainBuiltInCatthehackerAuthFailureWithProbe(context.Background(), catthehackerUbuntuRepository+":full-latest", cause, http.DefaultTransport, func(_ context.Context, reference string, transport http.RoundTripper) error {
				called = true
				if reference != catthehackerUbuntuRepository+":full-latest" {
					t.Fatalf("probe reference = %q", reference)
				}
				if transport != http.DefaultTransport {
					t.Fatal("probe did not receive the configured transport")
				}
				return nil
			})
			if !called {
				t.Fatal("anonymous probe was not called")
			}
			for _, want := range []string{"anonymously readable", "did not alter credentials", "did not", "retry", "private GHCR packages", "package pull access"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("diagnostic omitted %q: %v", want, err)
				}
			}
			if !errors.Is(err, cause) {
				t.Fatal("diagnostic did not preserve the original failure")
			}
		})
	}
}

func TestExplainBuiltInCatthehackerAuthFailurePreservesUnprovenFailure(t *testing.T) {
	cause := errors.New("failed to authorize: 403 Forbidden")
	probeErr := errors.New("anonymous access unavailable")
	err := explainBuiltInCatthehackerAuthFailureWithProbe(context.Background(), catthehackerUbuntuRepository+":full-latest", cause, http.DefaultTransport, func(context.Context, string, http.RoundTripper) error {
		return probeErr
	})
	if err != cause {
		t.Fatalf("unproven diagnostic changed the original error: %v", err)
	}
}

func TestExplainBuiltInCatthehackerAuthFailureDoesNotProbeOtherFailures(t *testing.T) {
	tests := []struct {
		name      string
		reference string
		cause     error
	}{
		{name: "other repository", reference: "ghcr.io/example/private:latest", cause: errors.New("failed to authorize: 403 Forbidden")},
		{name: "non-auth failure", reference: catthehackerUbuntuRepository + ":full-latest", cause: errors.New("manifest unknown: 404 Not Found")},
		{name: "bare forbidden", reference: catthehackerUbuntuRepository + ":full-latest", cause: errors.New("403 Forbidden")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			called := false
			err := explainBuiltInCatthehackerAuthFailureWithProbe(context.Background(), test.reference, test.cause, http.DefaultTransport, func(context.Context, string, http.RoundTripper) error {
				called = true
				return nil
			})
			if called {
				t.Fatal("anonymous probe was called")
			}
			if err != test.cause {
				t.Fatalf("error changed: %v", err)
			}
		})
	}
}

func TestExplainBuiltInCatthehackerAuthFailureProbesExactDigestReference(t *testing.T) {
	digestReference := catthehackerUbuntuRepository + "@sha256:245c8981fbf4ac268db015463c6c446b9411481f7e0001537128dc384d46dd0c"
	cause := errors.New("failed to fetch oauth token: 403 Forbidden")
	var observed string
	_ = explainBuiltInCatthehackerAuthFailureWithProbe(context.Background(), digestReference, cause, http.DefaultTransport, func(_ context.Context, reference string, _ http.RoundTripper) error {
		observed = reference
		return nil
	})
	if observed != digestReference {
		t.Fatalf("probe reference = %q, want %q", observed, digestReference)
	}
}
