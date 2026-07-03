package github

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
)

func TestJWTShape(t *testing.T) {
	keyPath := writeKey(t)
	token, err := appJWT(123, keyPath, time.Unix(1000, 0))
	if err != nil {
		t.Fatal(err)
	}
	if parts := strings.Split(token, "."); len(parts) != 3 {
		t.Fatalf("jwt has %d parts", len(parts))
	}
}

func TestListRunnersUsesInstallationToken(t *testing.T) {
	keyPath := writeKey(t)
	var sawRunnerList bool
	client := New(config.GitHubConfig{
		AppID:          123,
		Organization:   "example",
		PrivateKeyPath: keyPath,
		APIBaseURL:     "https://api.github.test",
		WebBaseURL:     "https://github.test",
	})
	client.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var body string
		switch r.URL.Path {
		case "/orgs/example/installation":
			body = `{"id":42}`
		case "/app/installations/42/access_tokens":
			body = `{"token":"installation-token","expires_at":"2099-01-01T00:00:00Z"}`
		case "/orgs/example/actions/runners":
			if r.Header.Get("Authorization") != "Bearer installation-token" {
				t.Fatalf("unexpected auth header: %q", r.Header.Get("Authorization"))
			}
			sawRunnerList = true
			body = `{"total_count":1,"runners":[{"id":1,"name":"epar-test-1","os":"linux","status":"online","busy":false}]}`
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		return &http.Response{
			StatusCode: 200,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	})}
	runners, err := client.ListRunners(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !sawRunnerList || len(runners) != 1 {
		t.Fatalf("unexpected runners: %+v", runners)
	}
}

func TestDeleteRunnerIfExistsIgnoresNotFound(t *testing.T) {
	keyPath := writeKey(t)
	client := New(config.GitHubConfig{
		AppID:          123,
		Organization:   "example",
		PrivateKeyPath: keyPath,
		APIBaseURL:     "https://api.github.test",
		WebBaseURL:     "https://github.test",
	})
	client.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		status := 200
		body := `{}`
		switch r.URL.Path {
		case "/orgs/example/installation":
			body = `{"id":42}`
		case "/app/installations/42/access_tokens":
			body = `{"token":"installation-token","expires_at":"2099-01-01T00:00:00Z"}`
		case "/orgs/example/actions/runners/99":
			status = http.StatusNotFound
			body = `{"message":"Not Found"}`
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		return &http.Response{
			StatusCode: status,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	})}
	if err := client.DeleteRunnerIfExists(context.Background(), 99); err != nil {
		t.Fatalf("expected nil for 404, got %v", err)
	}
}

func writeKey(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	block := &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}
	f, err := os.CreateTemp(t.TempDir(), "key-*.pem")
	if err != nil {
		t.Fatal(err)
	}
	if err := pem.Encode(f, block); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return f.Name()
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}
