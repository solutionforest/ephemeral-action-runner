package github

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
)

func TestRunnerGroupRepositoryAccessUsesMaximumBreadth(t *testing.T) {
	for _, test := range []struct {
		maximum string
		actual  string
		allowed bool
	}{
		{maximum: "selected", actual: "selected", allowed: true},
		{maximum: "selected", actual: "private", allowed: false},
		{maximum: "selected", actual: "all", allowed: false},
		{maximum: "private", actual: "selected", allowed: true},
		{maximum: "private", actual: "private", allowed: true},
		{maximum: "private", actual: "all", allowed: false},
		{maximum: "all", actual: "selected", allowed: true},
		{maximum: "all", actual: "private", allowed: true},
		{maximum: "all", actual: "all", allowed: true},
	} {
		t.Run(test.maximum+"_allows_"+test.actual, func(t *testing.T) {
			if got := repositoryAccessAllows(test.maximum, test.actual); got != test.allowed {
				t.Fatalf("repositoryAccessAllows(%q, %q) = %t, want %t", test.maximum, test.actual, got, test.allowed)
			}
		})
	}
}

func TestEvaluateRunnerGroupPolicy(t *testing.T) {
	strict := config.Default().Security.RunnerGroup
	for _, test := range []struct {
		name          string
		group         RunnerGroup
		repositories  []RunnerGroupRepository
		mutate        func(*config.RunnerGroupSecurityConfig)
		wantAllowed   bool
		wantViolation string
		wantAdvisory  string
	}{
		{
			name:         "strict selected private repositories",
			group:        RunnerGroup{ID: 1, Name: "restricted", Visibility: "selected"},
			repositories: []RunnerGroupRepository{{FullName: "example/private", Private: true}},
			wantAllowed:  true,
		},
		{
			name:          "default rejected when required",
			group:         RunnerGroup{ID: 1, Name: "Default", Visibility: "selected", Default: true},
			wantViolation: "requireNonDefaultGroup is true",
		},
		{
			name:  "default allowed with advisory",
			group: RunnerGroup{ID: 1, Name: "Default", Visibility: "all", Default: true},
			mutate: func(policy *config.RunnerGroupSecurityConfig) {
				policy.RequireNonDefaultGroup = false
				policy.RequiredRepositoryAccess = "all"
			},
			wantAllowed:  true,
			wantAdvisory: "default group",
		},
		{
			name:          "broader access rejected",
			group:         RunnerGroup{ID: 1, Name: "broad", Visibility: "private"},
			wantViolation: "broader than",
		},
		{
			name:          "public group rejected",
			group:         RunnerGroup{ID: 1, Name: "public", Visibility: "selected", AllowsPublicRepositories: true},
			wantViolation: "allows public repositories",
		},
		{
			name:          "public repository defensively rejected",
			group:         RunnerGroup{ID: 1, Name: "inconsistent", Visibility: "selected"},
			repositories:  []RunnerGroupRepository{{FullName: "example/public", Private: false}},
			wantViolation: "includes public repository",
		},
		{
			name:         "public exception remains advisory",
			group:        RunnerGroup{ID: 1, Name: "public", Visibility: "selected", AllowsPublicRepositories: true},
			mutate:       func(policy *config.RunnerGroupSecurityConfig) { policy.RequirePublicRepositoriesDisabled = false },
			wantAllowed:  true,
			wantAdvisory: "untrusted public or fork workflows",
		},
		{
			name:         "inherited group allowed with advisory",
			group:        RunnerGroup{ID: 1, Name: "enterprise", Visibility: "selected", Inherited: true},
			wantAllowed:  true,
			wantAdvisory: "enterprise level",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			policy := strict
			if test.mutate != nil {
				test.mutate(&policy)
			}
			result := EvaluateRunnerGroupPolicy(test.group, test.repositories, policy)
			if result.Allowed() != test.wantAllowed {
				t.Fatalf("Allowed() = %t, violations=%#v", result.Allowed(), result.Violations)
			}
			if test.wantViolation != "" && !containsText(result.Violations, test.wantViolation) {
				t.Fatalf("violations = %#v, want text %q", result.Violations, test.wantViolation)
			}
			if test.wantAdvisory != "" && !containsText(result.Advisories, test.wantAdvisory) {
				t.Fatalf("advisories = %#v, want text %q", result.Advisories, test.wantAdvisory)
			}
		})
	}
}

func TestResolveRunnerGroupRequiresExactOrDefault(t *testing.T) {
	groups := []RunnerGroup{{ID: 1, Name: "Restricted", Visibility: "selected"}, {ID: 2, Name: "Default", Visibility: "all", Default: true}}
	policy := config.Default().Security.RunnerGroup
	if result := ResolveRunnerGroup("restricted", policy, groups); result.Allowed() || !containsText(result.Violations, "was not found") {
		t.Fatalf("case-mismatched group result = %+v, want not found", result)
	}
	policy.RequireExplicitGroup = false
	policy.RequireNonDefaultGroup = false
	policy.RequiredRepositoryAccess = "all"
	result := ResolveRunnerGroup("", policy, groups)
	if !result.Allowed() || !result.Resolved || result.Group.Name != "Default" || !containsText(result.Advisories, "runner.group is empty") {
		t.Fatalf("default result = %+v", result)
	}
}

func TestListRunnerGroupsAndRepositoriesPaginates(t *testing.T) {
	keyPath := writeKey(t)
	client := New(config.GitHubConfig{AppID: 123, Organization: "example", PrivateKeyPath: keyPath, APIBaseURL: "https://api.github.test"})
	client.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body := "{}"
		switch r.URL.Path {
		case "/orgs/example/installation":
			body = `{"id":42}`
		case "/app/installations/42/access_tokens":
			body = `{"token":"installation-token","expires_at":"2099-01-01T00:00:00Z"}`
		case "/orgs/example/actions/runner-groups":
			page := r.URL.Query().Get("page")
			if page == "1" {
				items := make([]string, 100)
				for i := range items {
					items[i] = fmt.Sprintf(`{"id":%d,"name":"group-%d","visibility":"selected"}`, i+1, i+1)
				}
				body = `{"total_count":101,"runner_groups":[` + strings.Join(items, ",") + `]}`
			} else {
				body = `{"total_count":101,"runner_groups":[{"id":101,"name":"group-101","visibility":"selected"}]}`
			}
		case "/orgs/example/actions/runner-groups/1/repositories":
			page := r.URL.Query().Get("page")
			if page == "1" {
				items := make([]string, 100)
				for i := range items {
					items[i] = fmt.Sprintf(`{"id":%d,"name":"repo-%d","full_name":"example/repo-%d","private":true}`, i+1, i+1, i+1)
				}
				body = `{"total_count":101,"repositories":[` + strings.Join(items, ",") + `]}`
			} else {
				body = `{"total_count":101,"repositories":[{"id":101,"name":"repo-101","full_name":"example/repo-101","private":true}]}`
			}
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	groups, err := client.ListRunnerGroups(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 101 {
		t.Fatalf("groups = %d, want 101", len(groups))
	}
	repositories, err := client.ListRunnerGroupRepositories(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(repositories) != 101 {
		t.Fatalf("repositories = %d, want 101", len(repositories))
	}
}

func TestLiveRunnerGroupPolicy(t *testing.T) {
	configPath := os.Getenv("EPAR_LIVE_CONFIG")
	safeGroup := os.Getenv("EPAR_LIVE_SAFE_GROUP")
	publicGroup := os.Getenv("EPAR_LIVE_PUBLIC_GROUP")
	if configPath == "" || safeGroup == "" || publicGroup == "" {
		t.Skip("set EPAR_LIVE_CONFIG, EPAR_LIVE_SAFE_GROUP, and EPAR_LIVE_PUBLIC_GROUP to run live GitHub policy validation")
	}
	repositoryRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(configPath) {
		configPath = config.ProjectPath(repositoryRoot, configPath)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(cfg.GitHub.PrivateKeyPath) {
		cfg.GitHub.PrivateKeyPath = config.ProjectPath(repositoryRoot, cfg.GitHub.PrivateKeyPath)
	}
	client := New(cfg.GitHub)
	strict := config.Default().Security.RunnerGroup
	strict.Enforcement = config.RunnerGroupEnforcementEnforce

	safeResult, err := client.EvaluateRunnerGroupPolicy(context.Background(), safeGroup, strict)
	if err != nil {
		t.Fatal(err)
	}
	if !safeResult.Allowed() {
		t.Fatalf("safe group violations = %#v", safeResult.Violations)
	}

	groups, err := client.ListRunnerGroups(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var defaultName string
	for _, group := range groups {
		if group.Default {
			defaultName = group.Name
			break
		}
	}
	if defaultName == "" {
		t.Fatal("GitHub returned no default runner group")
	}
	defaultStrict, err := client.EvaluateRunnerGroupPolicy(context.Background(), defaultName, strict)
	if err != nil {
		t.Fatal(err)
	}
	if defaultStrict.Allowed() {
		t.Fatalf("default group unexpectedly passed strict policy: %+v", defaultStrict)
	}
	defaultPolicy := strict
	defaultPolicy.RequireNonDefaultGroup = false
	defaultPolicy.RequiredRepositoryAccess = config.RunnerGroupRepositoryAccessAll
	defaultAllowed, err := client.EvaluateRunnerGroupPolicy(context.Background(), defaultName, defaultPolicy)
	if err != nil {
		t.Fatal(err)
	}
	if !defaultAllowed.Allowed() || !containsText(defaultAllowed.Advisories, "default group") {
		t.Fatalf("default group deliberate policy result = %+v", defaultAllowed)
	}

	publicStrict, err := client.EvaluateRunnerGroupPolicy(context.Background(), publicGroup, strict)
	if err != nil {
		t.Fatal(err)
	}
	if publicStrict.Allowed() || !containsText(publicStrict.Violations, "allows public repositories") {
		t.Fatalf("public group strict result = %+v", publicStrict)
	}
	publicPolicy := strict
	publicPolicy.RequirePublicRepositoriesDisabled = false
	publicAllowed, err := client.EvaluateRunnerGroupPolicy(context.Background(), publicGroup, publicPolicy)
	if err != nil {
		t.Fatal(err)
	}
	if !publicAllowed.Allowed() || !containsText(publicAllowed.Advisories, "untrusted public or fork workflows") {
		t.Fatalf("public group deliberate policy result = %+v", publicAllowed)
	}
}

func containsText(values []string, text string) bool {
	for _, value := range values {
		if strings.Contains(value, text) {
			return true
		}
	}
	return false
}
