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

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name  string
		value string
		want  time.Duration
	}{
		{name: "seconds", value: "120", want: 2 * time.Minute},
		{name: "http date", value: now.Add(3 * time.Minute).Format(http.TimeFormat), want: 3 * time.Minute},
		{name: "past date", value: now.Add(-time.Minute).Format(http.TimeFormat), want: 0},
		{name: "invalid", value: "later", want: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := parseRetryAfter(test.value, now); got != test.want {
				t.Fatalf("parseRetryAfter(%q) = %s, want %s", test.value, got, test.want)
			}
		})
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

func TestWaitRunnerOnlineAcceptsBusyRunner(t *testing.T) {
	keyPath := writeKey(t)
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
			body = `{"total_count":1,"runners":[{"id":1,"name":"epar-test-1","os":"linux","status":"online","busy":true}]}`
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	})}

	runner, err := client.WaitRunnerOnline(context.Background(), "epar-test-1", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if runner.ID != 1 || !runner.Busy {
		t.Fatalf("runner = %+v, want online busy runner id 1", runner)
	}
}

func TestWaitRunnerOnlineIdleRejectsBusyRunner(t *testing.T) {
	keyPath := writeKey(t)
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
			body = `{"total_count":1,"runners":[{"id":1,"name":"epar-test-1","os":"linux","status":"online","busy":true}]}`
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	})}

	_, err := client.WaitRunnerOnlineIdle(context.Background(), "epar-test-1", 0)
	if err == nil || !strings.Contains(err.Error(), "did not become online and idle") {
		t.Fatalf("WaitRunnerOnlineIdle() error = %v, want busy runner rejected", err)
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

func TestDeleteRunnersByPrefixContinuesAfterFailureAndPreservesBoundary(t *testing.T) {
	keyPath := writeKey(t)
	client := New(config.GitHubConfig{
		AppID:          123,
		Organization:   "example",
		PrivateKeyPath: keyPath,
		APIBaseURL:     "https://api.github.test",
		WebBaseURL:     "https://github.test",
	})
	var deletePaths []string
	client.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		status := http.StatusOK
		body := `{}`
		switch r.URL.Path {
		case "/orgs/example/installation":
			body = `{"id":42}`
		case "/app/installations/42/access_tokens":
			body = `{"token":"installation-token","expires_at":"2099-01-01T00:00:00Z"}`
		case "/orgs/example/actions/runners":
			body = `{"total_count":5,"runners":[` +
				`{"id":1,"name":"epar-core"},` +
				`{"id":2,"name":"epar-core-first"},` +
				`{"id":3,"name":"epar-core-second"},` +
				`{"id":4,"name":"epar-core-third"},` +
				`{"id":5,"name":"epar-corex-unrelated"}]}`
		case "/orgs/example/actions/runners/1", "/orgs/example/actions/runners/2", "/orgs/example/actions/runners/3", "/orgs/example/actions/runners/4":
			deletePaths = append(deletePaths, r.URL.Path)
			status = http.StatusNoContent
			body = ""
			if r.URL.Path == "/orgs/example/actions/runners/2" || r.URL.Path == "/orgs/example/actions/runners/3" {
				status = http.StatusInternalServerError
				body = `{"message":"temporary failure"}`
			}
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		return &http.Response{
			StatusCode: status,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	})}

	deleted, err := client.DeleteRunnersByPrefix(context.Background(), "epar-core")
	if err == nil {
		t.Fatal("DeleteRunnersByPrefix() error = nil, want aggregate delete error")
	}
	if !strings.Contains(err.Error(), `delete runner "epar-core-first" (id=2)`) {
		t.Fatalf("error %q does not identify the failed runner", err)
	}
	if !strings.Contains(err.Error(), `delete runner "epar-core-second" (id=3)`) {
		t.Fatalf("aggregate error %q does not identify every failed runner", err)
	}
	if got, want := strings.Join(deletePaths, ","), strings.Join([]string{
		"/orgs/example/actions/runners/1",
		"/orgs/example/actions/runners/2",
		"/orgs/example/actions/runners/3",
		"/orgs/example/actions/runners/4",
	}, ","); got != want {
		t.Fatalf("delete paths = %q, want %q", got, want)
	}
	if len(deleted) != 2 || deleted[0].ID != 1 || deleted[1].ID != 4 {
		t.Fatalf("deleted runners = %+v, want ids 1 and 4", deleted)
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
