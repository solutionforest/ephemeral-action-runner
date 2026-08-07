package github

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
	"github.com/solutionforest/ephemeral-action-runner/internal/dependency"
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

func TestHTTPErrorDependencyFailureClassification(t *testing.T) {
	now := time.Now()
	for _, test := range []struct {
		name      string
		err       *HTTPError
		retryable bool
	}{
		{name: "request timeout", err: &HTTPError{StatusCode: http.StatusRequestTimeout}, retryable: true},
		{name: "rate limit", err: &HTTPError{StatusCode: http.StatusTooManyRequests}, retryable: true},
		{name: "server error", err: &HTTPError{StatusCode: http.StatusBadGateway}, retryable: true},
		{name: "unauthorized", err: &HTTPError{StatusCode: http.StatusUnauthorized}, retryable: false},
		{name: "ordinary forbidden", err: &HTTPError{StatusCode: http.StatusForbidden}, retryable: false},
		{name: "rate limited forbidden", err: &HTTPError{StatusCode: http.StatusForbidden, RateLimitKnown: true, RateLimitRemaining: 0}, retryable: true},
		{name: "reset hinted forbidden", err: &HTTPError{StatusCode: http.StatusForbidden, RateLimitReset: now.Add(time.Minute)}, retryable: true},
		{name: "retry-after hinted forbidden", err: &HTTPError{StatusCode: http.StatusForbidden, RetryAfter: time.Minute}, retryable: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			failure := test.err.DependencyFailure()
			if failure == nil || failure.Retryable != test.retryable {
				t.Fatalf("failure = %+v, retryable = %t, want %t", failure, failure != nil && failure.Retryable, test.retryable)
			}
			if got := dependency.IsRetryable(test.err); got != test.retryable {
				t.Fatalf("dependency.IsRetryable() = %t, want %t", got, test.retryable)
			}
		})
	}
}

func TestHTTPErrorCarriesStructuredMetadataThroughWrapping(t *testing.T) {
	client := New(config.GitHubConfig{APIBaseURL: "https://api.github.test"})
	client.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		response := &http.Response{
			StatusCode: http.StatusForbidden,
			Header: http.Header{
				"Retry-After":           []string{"120"},
				"X-RateLimit-Reset":     []string{fmt.Sprintf("%d", time.Now().Add(5*time.Minute).Unix())},
				"X-RateLimit-Remaining": []string{"0"},
				"X-Github-Request-Id":   []string{"req-123"},
			},
			Body: io.NopCloser(strings.NewReader(`{"message":"rate limit exceeded"}`)),
		}
		return response, nil
	})}
	err := client.request(context.Background(), http.MethodGet, "/orgs/example/actions/runners", "", nil, nil)
	if err == nil {
		t.Fatal("request() error = nil, want HTTP failure")
	}
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("errors.As(HTTPError) failed: %v", err)
	}
	if httpErr.RequestID != "req-123" || httpErr.RateLimitRemaining != 0 || !httpErr.RateLimitKnown || httpErr.RetryAfter != 2*time.Minute {
		t.Fatalf("HTTP metadata: request=%q remaining=%d known=%t retry=%s reset=%s", httpErr.RequestID, httpErr.RateLimitRemaining, httpErr.RateLimitKnown, httpErr.RetryAfter, httpErr.RateLimitReset)
	}
	var failure *dependency.Failure
	if !errors.As(err, &failure) {
		t.Fatalf("errors.As(dependency.Failure) failed: %v", err)
	}
	if failure.Service != "github" || !strings.Contains(failure.Operation, "GET /orgs/example/actions/runners") || !failure.Retryable || failure.RequestID != "req-123" {
		t.Fatalf("dependency failure = %+v", failure)
	}
	wrapped := fmt.Errorf("list runner groups: %w", err)
	if !errors.As(wrapped, &failure) || failure.RequestID != "req-123" || !failure.Retryable {
		t.Fatalf("wrapped dependency failure = %+v", failure)
	}
}

func TestGitHubTransportFailureIsTypedAndSingleAttempt(t *testing.T) {
	client := New(config.GitHubConfig{APIBaseURL: "https://api.github.test"})
	var calls int
	client.httpClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, &net.DNSError{Err: "temporary DNS failure", IsTemporary: true}
	})}
	err := client.request(context.Background(), http.MethodGet, "/orgs/example/actions/runners", "", nil, nil)
	if calls != 1 {
		t.Fatalf("transport calls = %d, want one attempt", calls)
	}
	var failure *dependency.Failure
	if !errors.As(err, &failure) || !failure.Retryable || failure.Service != "github" {
		t.Fatalf("typed transport failure = %+v (err=%v)", failure, err)
	}
}

func TestGitHubClientTimeoutIsRetryableButCallerDeadlineIsTerminal(t *testing.T) {
	client := New(config.GitHubConfig{APIBaseURL: "https://api.github.test"})
	client.httpClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, &url.Error{Op: http.MethodGet, URL: "https://api.github.test", Err: context.DeadlineExceeded}
	})}
	err := client.request(context.Background(), http.MethodGet, "/timeout", "", nil, nil)
	var failure *dependency.Failure
	if !errors.As(err, &failure) || !failure.Retryable {
		t.Fatalf("client timeout failure = %+v (err=%v), want retryable", failure, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	defer cancel()
	err = client.request(ctx, http.MethodGet, "/timeout", "", nil, nil)
	failure = nil
	if errors.As(err, &failure) && failure.Retryable {
		t.Fatalf("caller deadline failure = %+v, want terminal", failure)
	}
}

func TestListRunnersAbortsPaginationOnFirstTransientFailure(t *testing.T) {
	keyPath := writeKey(t)
	client := New(config.GitHubConfig{AppID: 123, Organization: "example", PrivateKeyPath: keyPath, APIBaseURL: "https://api.github.test"})
	var listCalls int
	client.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/orgs/example/installation":
			return response(http.StatusOK, `{"id":42}`), nil
		case "/app/installations/42/access_tokens":
			return response(http.StatusOK, `{"token":"installation-token","expires_at":"2099-01-01T00:00:00Z"}`), nil
		case "/orgs/example/actions/runners":
			listCalls++
			return response(http.StatusServiceUnavailable, `{"message":"temporarily unavailable"}`), nil
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
			return nil, nil
		}
	})}
	_, err := client.ListRunners(context.Background())
	if err == nil || listCalls != 1 {
		t.Fatalf("ListRunners() err=%v listCalls=%d, want one transient attempt", err, listCalls)
	}
	var failure *dependency.Failure
	if !errors.As(err, &failure) || !failure.Retryable {
		t.Fatalf("ListRunners() failure = %+v", failure)
	}
	_, err = client.WaitRunnerOnline(context.Background(), "epar-test-1", time.Second)
	if err == nil || listCalls != 2 {
		t.Fatalf("WaitRunnerOnline() err=%v listCalls=%d, want one list attempt", err, listCalls)
	}
}

func response(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
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

func TestInstallationRequestRefreshesRejectedCachedTokenOnce(t *testing.T) {
	keyPath := writeKey(t)
	client := New(config.GitHubConfig{AppID: 123, Organization: "example", PrivateKeyPath: keyPath, APIBaseURL: "https://api.github.test"})
	var tokenRequests int
	var runnerRequests int
	client.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/orgs/example/installation":
			return response(http.StatusOK, `{"id":42}`), nil
		case "/app/installations/42/access_tokens":
			tokenRequests++
			return response(http.StatusOK, fmt.Sprintf(`{"token":"installation-token-%d","expires_at":"2099-01-01T00:00:00Z"}`, tokenRequests)), nil
		case "/orgs/example/actions/runners":
			runnerRequests++
			if runnerRequests == 1 {
				if got := r.Header.Get("Authorization"); got != "Bearer installation-token-1" {
					t.Fatalf("first authorization = %q", got)
				}
				return response(http.StatusUnauthorized, `{"message":"Bad credentials"}`), nil
			}
			if got := r.Header.Get("Authorization"); got != "Bearer installation-token-2" {
				t.Fatalf("refreshed authorization = %q", got)
			}
			return response(http.StatusOK, `{"total_count":1,"runners":[{"id":1,"name":"epar-test-1","status":"online"}]}`), nil
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
			return nil, nil
		}
	})}

	runners, err := client.ListRunners(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(runners) != 1 || runners[0].ID != 1 {
		t.Fatalf("runners = %+v", runners)
	}
	if tokenRequests != 2 || runnerRequests != 2 {
		t.Fatalf("token requests=%d runner requests=%d, want 2/2", tokenRequests, runnerRequests)
	}
}

func TestConcurrentUnauthorizedRequestsShareOneRefreshedToken(t *testing.T) {
	keyPath := writeKey(t)
	client := New(config.GitHubConfig{AppID: 123, Organization: "example", PrivateKeyPath: keyPath, APIBaseURL: "https://api.github.test"})
	const callers = 16
	var tokenRequests atomic.Int32
	var rejectedRequests atomic.Int32
	var recoveredRequests atomic.Int32
	allRejectedStarted := make(chan struct{})
	client.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/orgs/example/installation":
			return response(http.StatusOK, `{"id":42}`), nil
		case "/app/installations/42/access_tokens":
			request := tokenRequests.Add(1)
			return response(http.StatusOK, fmt.Sprintf(`{"token":"installation-token-%d","expires_at":"2099-01-01T00:00:00Z"}`, request)), nil
		case "/orgs/example/actions/runners":
			switch r.Header.Get("Authorization") {
			case "Bearer installation-token-1":
				if rejectedRequests.Add(1) == callers {
					close(allRejectedStarted)
				}
				<-allRejectedStarted
				return response(http.StatusUnauthorized, `{"message":"Bad credentials"}`), nil
			case "Bearer installation-token-2":
				recoveredRequests.Add(1)
				return response(http.StatusOK, `{"total_count":0,"runners":[]}`), nil
			default:
				t.Fatalf("unexpected authorization %q", r.Header.Get("Authorization"))
				return nil, nil
			}
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
			return nil, nil
		}
	})}
	if _, err := client.installationToken(context.Background()); err != nil {
		t.Fatal(err)
	}

	errorsCh := make(chan error, callers)
	var wait sync.WaitGroup
	wait.Add(callers)
	for index := 0; index < callers; index++ {
		go func() {
			defer wait.Done()
			_, err := client.ListRunners(context.Background())
			errorsCh <- err
		}()
	}
	wait.Wait()
	close(errorsCh)
	for err := range errorsCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := tokenRequests.Load(); got != 2 {
		t.Fatalf("installation token requests = %d, want initial plus one shared refresh", got)
	}
	if rejectedRequests.Load() != callers || recoveredRequests.Load() != callers {
		t.Fatalf("rejected=%d recovered=%d, want %d/%d", rejectedRequests.Load(), recoveredRequests.Load(), callers, callers)
	}
}

func TestInstallationRequestKeepsSecondUnauthorizedTerminal(t *testing.T) {
	keyPath := writeKey(t)
	client := New(config.GitHubConfig{AppID: 123, Organization: "example", PrivateKeyPath: keyPath, APIBaseURL: "https://api.github.test"})
	var tokenRequests int
	var runnerRequests int
	client.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/orgs/example/installation":
			return response(http.StatusOK, `{"id":42}`), nil
		case "/app/installations/42/access_tokens":
			tokenRequests++
			return response(http.StatusOK, fmt.Sprintf(`{"token":"installation-token-%d","expires_at":"2099-01-01T00:00:00Z"}`, tokenRequests)), nil
		case "/orgs/example/actions/runners":
			runnerRequests++
			return response(http.StatusUnauthorized, `{"message":"Bad credentials"}`), nil
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
			return nil, nil
		}
	})}

	_, err := client.ListRunners(context.Background())
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusUnauthorized || dependency.IsRetryable(err) {
		t.Fatalf("error = %v, want terminal second HTTP 401", err)
	}
	if tokenRequests != 2 || runnerRequests != 2 {
		t.Fatalf("token requests=%d runner requests=%d, want exactly 2/2", tokenRequests, runnerRequests)
	}
}

func TestInstallationRequestPropagatesTransientRefreshFailure(t *testing.T) {
	keyPath := writeKey(t)
	client := New(config.GitHubConfig{AppID: 123, Organization: "example", PrivateKeyPath: keyPath, APIBaseURL: "https://api.github.test"})
	var tokenRequests int
	var runnerRequests int
	client.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/orgs/example/installation":
			return response(http.StatusOK, `{"id":42}`), nil
		case "/app/installations/42/access_tokens":
			tokenRequests++
			if tokenRequests == 1 {
				return response(http.StatusOK, `{"token":"installation-token-1","expires_at":"2099-01-01T00:00:00Z"}`), nil
			}
			return response(http.StatusServiceUnavailable, `{"message":"temporarily unavailable"}`), nil
		case "/orgs/example/actions/runners":
			runnerRequests++
			return response(http.StatusUnauthorized, `{"message":"Bad credentials"}`), nil
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
			return nil, nil
		}
	})}

	_, err := client.ListRunners(context.Background())
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusServiceUnavailable || !dependency.IsTypedRetryable(err) {
		t.Fatalf("error = %v, want typed transient token-refresh failure", err)
	}
	if tokenRequests != 2 || runnerRequests != 1 {
		t.Fatalf("token requests=%d runner requests=%d, want 2/1", tokenRequests, runnerRequests)
	}
}

func TestInstallationTokenCacheIsConcurrentAndSingleFlight(t *testing.T) {
	keyPath := writeKey(t)
	client := New(config.GitHubConfig{
		AppID:          123,
		Organization:   "example",
		PrivateKeyPath: keyPath,
		APIBaseURL:     "https://api.github.test",
		WebBaseURL:     "https://github.test",
	})
	var installationRequests atomic.Int32
	var tokenRequests atomic.Int32
	client.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var body string
		switch r.URL.Path {
		case "/orgs/example/installation":
			installationRequests.Add(1)
			body = `{"id":42}`
		case "/app/installations/42/access_tokens":
			tokenRequests.Add(1)
			body = `{"token":"installation-token","expires_at":"2099-01-01T00:00:00Z"}`
		default:
			return nil, fmt.Errorf("unexpected path %s", r.URL.Path)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	})}

	const callers = 32
	errorsCh := make(chan error, callers)
	var wait sync.WaitGroup
	wait.Add(callers)
	for index := 0; index < callers; index++ {
		go func() {
			defer wait.Done()
			token, err := client.installationToken(context.Background())
			if err == nil && token != "installation-token" {
				err = fmt.Errorf("installationToken() = %q", token)
			}
			errorsCh <- err
		}()
	}
	wait.Wait()
	close(errorsCh)
	for err := range errorsCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := installationRequests.Load(); got != 1 {
		t.Fatalf("installation lookup requests = %d, want 1", got)
	}
	if got := tokenRequests.Load(); got != 1 {
		t.Fatalf("access-token requests = %d, want 1", got)
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
