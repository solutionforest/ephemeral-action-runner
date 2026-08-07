package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
	"github.com/solutionforest/ephemeral-action-runner/internal/dependency"
)

type Client struct {
	cfg          config.GitHubConfig
	httpClient   *http.Client
	tokenMu      sync.Mutex
	token        string
	tokenExpires time.Time
}

type RegistrationToken struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

type Runner struct {
	ID     int64         `json:"id"`
	Name   string        `json:"name"`
	OS     string        `json:"os"`
	Status string        `json:"status"`
	Busy   bool          `json:"busy"`
	Labels []RunnerLabel `json:"labels"`
}

type RunnerLabel struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}

func New(cfg config.GitHubConfig) *Client {
	if cfg.APIBaseURL == "" {
		cfg.APIBaseURL = "https://api.github.com"
	}
	if cfg.WebBaseURL == "" {
		cfg.WebBaseURL = "https://github.com"
	}
	cfg.APIBaseURL = strings.TrimRight(cfg.APIBaseURL, "/")
	cfg.WebBaseURL = strings.TrimRight(cfg.WebBaseURL, "/")
	return &Client{
		cfg:        cfg,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *Client) OrganizationURL() string {
	return c.cfg.WebBaseURL + "/" + c.cfg.Organization
}

func (c *Client) RegistrationToken(ctx context.Context) (RegistrationToken, error) {
	var token RegistrationToken
	if err := c.installationRequest(ctx, http.MethodPost, fmt.Sprintf("/orgs/%s/actions/runners/registration-token", url.PathEscape(c.cfg.Organization)), nil, &token); err != nil {
		return token, err
	}
	return token, nil
}

func (c *Client) ListRunners(ctx context.Context) ([]Runner, error) {
	var all []Runner
	for page := 1; ; page++ {
		var response struct {
			TotalCount int      `json:"total_count"`
			Runners    []Runner `json:"runners"`
		}
		path := fmt.Sprintf("/orgs/%s/actions/runners?per_page=100&page=%d", url.PathEscape(c.cfg.Organization), page)
		if err := c.installationRequest(ctx, http.MethodGet, path, nil, &response); err != nil {
			return nil, err
		}
		all = append(all, response.Runners...)
		if len(response.Runners) < 100 {
			return all, nil
		}
	}
}

func (c *Client) DeleteRunner(ctx context.Context, id int64) error {
	return c.installationRequest(ctx, http.MethodDelete, fmt.Sprintf("/orgs/%s/actions/runners/%d", url.PathEscape(c.cfg.Organization), id), nil, nil)
}

func (c *Client) DeleteRunnerIfExists(ctx context.Context, id int64) error {
	err := c.DeleteRunner(ctx, id)
	var httpErr *HTTPError
	if errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusNotFound {
		return nil
	}
	return err
}

func (c *Client) RunnerByName(ctx context.Context, name string) (Runner, bool, error) {
	runners, err := c.ListRunners(ctx)
	if err != nil {
		return Runner{}, false, err
	}
	for _, runner := range runners {
		if runner.Name == name {
			return runner, true, nil
		}
	}
	return Runner{}, false, nil
}

func (c *Client) DeleteRunnersByPrefix(ctx context.Context, prefix string) ([]Runner, error) {
	runners, err := c.ListRunners(ctx)
	if err != nil {
		return nil, err
	}
	var deleted []Runner
	var deleteErrors []error
	for _, runner := range runners {
		if strings.HasPrefix(runner.Name, prefix+"-") || runner.Name == prefix {
			if err := c.DeleteRunnerIfExists(ctx, runner.ID); err != nil {
				deleteErrors = append(deleteErrors, fmt.Errorf("delete runner %q (id=%d): %w", runner.Name, runner.ID, err))
				continue
			}
			deleted = append(deleted, runner)
		}
	}
	return deleted, errors.Join(deleteErrors...)
}

func (c *Client) WaitRunnerOnline(ctx context.Context, name string, timeout time.Duration) (Runner, error) {
	return c.waitRunnerOnline(ctx, name, timeout, false)
}

func (c *Client) WaitRunnerOnlineIdle(ctx context.Context, name string, timeout time.Duration) (Runner, error) {
	return c.waitRunnerOnline(ctx, name, timeout, true)
}

func (c *Client) waitRunnerOnline(ctx context.Context, name string, timeout time.Duration, requireIdle bool) (Runner, error) {
	deadline := time.Now().Add(timeout)
	for {
		runners, err := c.ListRunners(ctx)
		if err != nil {
			return Runner{}, err
		}
		for _, runner := range runners {
			if runner.Name == name && runner.Status == "online" && (!requireIdle || !runner.Busy) {
				return runner, nil
			}
		}
		if time.Now().After(deadline) {
			if requireIdle {
				return Runner{}, fmt.Errorf("runner %q did not become online and idle within %s", name, timeout)
			}
			return Runner{}, fmt.Errorf("runner %q did not become online within %s", name, timeout)
		}
		select {
		case <-ctx.Done():
			return Runner{}, ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

func (c *Client) installationRequest(ctx context.Context, method, path string, body, out any) error {
	token, err := c.installationToken(ctx)
	if err != nil {
		return err
	}
	return c.request(ctx, method, path, "Bearer "+token, body, out)
}

func (c *Client) installationToken(ctx context.Context) (string, error) {
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()

	if c.token != "" && time.Now().Before(c.tokenExpires.Add(-2*time.Minute)) {
		return c.token, nil
	}
	jwt, err := appJWT(c.cfg.AppID, c.cfg.PrivateKeyPath, time.Now())
	if err != nil {
		return "", err
	}
	var installation struct {
		ID int64 `json:"id"`
	}
	if err := c.request(ctx, http.MethodGet, fmt.Sprintf("/orgs/%s/installation", url.PathEscape(c.cfg.Organization)), "Bearer "+jwt, nil, &installation); err != nil {
		return "", err
	}
	var response struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := c.request(ctx, http.MethodPost, fmt.Sprintf("/app/installations/%d/access_tokens", installation.ID), "Bearer "+jwt, nil, &response); err != nil {
		return "", err
	}
	c.token = response.Token
	c.tokenExpires = response.ExpiresAt
	return c.token, nil
}

func (c *Client) request(ctx context.Context, method, path, auth string, body, out any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return dependency.NewFailure("github", requestOperation(method, path), err)
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.cfg.APIBaseURL+path, reader)
	if err != nil {
		return dependency.NewFailure("github", requestOperation(method, path), err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		// GitHub calls deliberately make one HTTP attempt.  The lifecycle
		// supervisor owns retry timing and can therefore coordinate retries
		// across providers without nested pagination/readiness retries.
		return dependency.NewFailure("github", requestOperation(method, path), err,
			dependency.WithRetryable(retryableTransportFailure(ctx, err)))
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return dependency.NewFailure("github", requestOperation(method, path), err,
			dependency.WithRetryable(retryableTransportFailure(ctx, err)),
			dependency.WithRequestID(headerValue(resp.Header, "X-GitHub-Request-Id")))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		httpError := &HTTPError{
			Method:         method,
			Path:           path,
			StatusCode:     resp.StatusCode,
			Body:           strings.TrimSpace(string(respBody)),
			RetryAfter:     parseRetryAfter(headerValue(resp.Header, "Retry-After"), time.Now()),
			RateLimitReset: parseRateLimitReset(headerValue(resp.Header, "X-RateLimit-Reset")),
			RequestID:      dependency.SanitizeRequestID(headerValue(resp.Header, "X-GitHub-Request-Id")),
			Cause:          errors.New(strings.TrimSpace(string(respBody))),
		}
		httpError.RateLimitRemaining, httpError.RateLimitKnown = parseRateLimitRemaining(headerValue(resp.Header, "X-RateLimit-Remaining"))
		return httpError
	}
	if out == nil || len(respBody) == 0 {
		return nil
	}
	if err := json.Unmarshal(respBody, out); err != nil {
		return dependency.NewFailure("github", requestOperation(method, path), err,
			dependency.WithRequestID(headerValue(resp.Header, "X-GitHub-Request-Id")))
	}
	return nil
}

func retryableTransportFailure(ctx context.Context, err error) bool {
	if err == nil {
		return false
	}
	if ctx != nil && ctx.Err() != nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		// http.Client.Timeout and transport-level deadlines wrap
		// context.DeadlineExceeded even though the caller's context remains
		// usable.  Treat those bounded request timeouts as transient.
		return true
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	return dependency.IsRetryable(err)
}

type HTTPError struct {
	Method             string
	Path               string
	StatusCode         int
	Body               string
	RetryAfter         time.Duration
	RateLimitReset     time.Time
	RateLimitRemaining int
	RateLimitKnown     bool
	RateLimited        bool
	RequestID          string
	Cause              error
}

func (e *HTTPError) Error() string {
	if e == nil {
		return "<nil>"
	}
	message := fmt.Sprintf("github %s %s returned %d: %s", e.Method, e.Path, e.StatusCode, e.Body)
	metadata := make([]string, 0, 3)
	if requestID := dependency.SanitizeRequestID(e.RequestID); requestID != "" {
		metadata = append(metadata, "request-id="+requestID)
	}
	if e.RetryAfter > 0 {
		metadata = append(metadata, "retry-after="+e.RetryAfter.String())
	}
	if !e.RateLimitReset.IsZero() {
		metadata = append(metadata, "rate-limit-reset="+e.RateLimitReset.UTC().Format(time.RFC3339))
	}
	if len(metadata) > 0 {
		message += " (" + strings.Join(metadata, ", ") + ")"
	}
	return message
}

// Unwrap exposes the provider-neutral failure while retaining the response
// cause through Failure.Unwrap.  This lets errors.As discover structured
// metadata even after runner-group policy methods add contextual wrapping.
func (e *HTTPError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.DependencyFailure()
}

// DependencyFailure adapts HTTPError to the provider-neutral failure model.
// It intentionally computes the classification lazily so HTTPError values
// constructed in tests or by callers with struct literals retain the same
// status/rate-limit semantics as responses created by request.
func (e *HTTPError) DependencyFailure() *dependency.Failure {
	if e == nil {
		return nil
	}
	return dependency.NewHTTPFailure("github", requestOperation(e.Method, e.Path), dependency.HTTPMetadata{
		StatusCode:      e.StatusCode,
		RetryAfter:      e.RetryAfter,
		RateLimitReset:  e.RateLimitReset,
		RateLimitRemain: e.RateLimitRemaining,
		RateLimitKnown:  e.RateLimitKnown || e.RateLimited,
		RequestID:       e.RequestID,
		Body:            e.Body,
	}, e.Cause)
}

// IsRetryable exposes the shared classification for callers that already
// hold an HTTPError value rather than its wrapped error interface.
func (e *HTTPError) IsRetryable() bool {
	failure := e.DependencyFailure()
	return failure != nil && failure.Retryable
}

func requestOperation(method, path string) string {
	method = strings.TrimSpace(method)
	path = strings.TrimSpace(path)
	if method == "" {
		return path
	}
	if path == "" {
		return method
	}
	return method + " " + path
}

func parseRateLimitReset(value string) time.Time {
	seconds, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil || seconds <= 0 {
		return time.Time{}
	}
	return time.Unix(seconds, 0).UTC()
}

func parseRateLimitRemaining(value string) (int, bool) {
	if strings.TrimSpace(value) == "" {
		return 0, false
	}
	remaining, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return 0, false
	}
	return remaining, true
}

func headerValue(headers http.Header, name string) string {
	if value := headers.Get(name); value != "" {
		return value
	}
	for key, values := range headers {
		if strings.EqualFold(key, name) && len(values) > 0 {
			return values[0]
		}
	}
	return ""
}

func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := time.ParseDuration(value + "s"); err == nil {
		if seconds > 0 {
			return seconds
		}
		return 0
	}
	when, err := http.ParseTime(value)
	if err != nil || !when.After(now) {
		return 0
	}
	return when.Sub(now)
}
