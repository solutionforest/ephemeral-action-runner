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
	"strings"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
)

type Client struct {
	cfg          config.GitHubConfig
	httpClient   *http.Client
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
	for _, runner := range runners {
		if strings.HasPrefix(runner.Name, prefix+"-") || runner.Name == prefix {
			if err := c.DeleteRunner(ctx, runner.ID); err != nil {
				return deleted, err
			}
			deleted = append(deleted, runner)
		}
	}
	return deleted, nil
}

func (c *Client) WaitRunnerOnlineIdle(ctx context.Context, name string, timeout time.Duration) (Runner, error) {
	deadline := time.Now().Add(timeout)
	for {
		runners, err := c.ListRunners(ctx)
		if err != nil {
			return Runner{}, err
		}
		for _, runner := range runners {
			if runner.Name == name && runner.Status == "online" && !runner.Busy {
				return runner, nil
			}
		}
		if time.Now().After(deadline) {
			return Runner{}, fmt.Errorf("runner %q did not become online and idle within %s", name, timeout)
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
			return err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.cfg.APIBaseURL+path, reader)
	if err != nil {
		return err
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
		return err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &HTTPError{
			Method:     method,
			Path:       path,
			StatusCode: resp.StatusCode,
			Body:       strings.TrimSpace(string(respBody)),
		}
	}
	if out == nil || len(respBody) == 0 {
		return nil
	}
	return json.Unmarshal(respBody, out)
}

type HTTPError struct {
	Method     string
	Path       string
	StatusCode int
	Body       string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("github %s %s returned %d: %s", e.Method, e.Path, e.StatusCode, e.Body)
}
