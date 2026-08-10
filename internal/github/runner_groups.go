package github

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
)

type RunnerGroup struct {
	ID                       int64  `json:"id"`
	Name                     string `json:"name"`
	Visibility               string `json:"visibility"`
	Default                  bool   `json:"default"`
	Inherited                bool   `json:"inherited"`
	AllowsPublicRepositories bool   `json:"allows_public_repositories"`
}

type RunnerGroupRepository struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	FullName string `json:"full_name"`
	Private  bool   `json:"private"`
}

type RunnerGroupPolicyResult struct {
	Group        RunnerGroup
	Resolved     bool
	Repositories []RunnerGroupRepository
	Violations   []string
	Advisories   []string
}

func (r RunnerGroupPolicyResult) Allowed() bool {
	return len(r.Violations) == 0
}

func (c *Client) ListRunnerGroups(ctx context.Context) ([]RunnerGroup, error) {
	var all []RunnerGroup
	for page := 1; ; page++ {
		var response struct {
			TotalCount   int           `json:"total_count"`
			RunnerGroups []RunnerGroup `json:"runner_groups"`
		}
		path := fmt.Sprintf("/orgs/%s/actions/runner-groups?per_page=100&page=%d", url.PathEscape(c.cfg.Organization), page)
		if err := c.installationRequest(ctx, http.MethodGet, path, nil, &response); err != nil {
			return nil, fmt.Errorf("list organization runner groups: %w", err)
		}
		all = append(all, response.RunnerGroups...)
		if len(response.RunnerGroups) < 100 {
			return all, nil
		}
	}
}

func (c *Client) ListRunnerGroupRepositories(ctx context.Context, groupID int64) ([]RunnerGroupRepository, error) {
	var all []RunnerGroupRepository
	for page := 1; ; page++ {
		var response struct {
			TotalCount   int                     `json:"total_count"`
			Repositories []RunnerGroupRepository `json:"repositories"`
		}
		path := fmt.Sprintf("/orgs/%s/actions/runner-groups/%d/repositories?per_page=100&page=%d", url.PathEscape(c.cfg.Organization), groupID, page)
		if err := c.installationRequest(ctx, http.MethodGet, path, nil, &response); err != nil {
			return nil, fmt.Errorf("list repositories for runner group id %d: %w", groupID, err)
		}
		all = append(all, response.Repositories...)
		if len(response.Repositories) < 100 {
			return all, nil
		}
	}
}

func (c *Client) EvaluateRunnerGroupPolicy(ctx context.Context, configuredGroup string, policy config.RunnerGroupSecurityConfig) (RunnerGroupPolicyResult, error) {
	groups, err := c.ListRunnerGroups(ctx)
	if err != nil {
		return RunnerGroupPolicyResult{}, err
	}
	result := ResolveRunnerGroup(configuredGroup, policy, groups)
	if !result.Resolved || result.Group.Visibility != config.RunnerGroupRepositoryAccessSelected {
		return result, nil
	}
	repositories, err := c.ListRunnerGroupRepositories(ctx, result.Group.ID)
	if err != nil {
		return RunnerGroupPolicyResult{}, err
	}
	return EvaluateRunnerGroupPolicy(result.Group, repositories, policy), nil
}

func ResolveRunnerGroup(configuredGroup string, policy config.RunnerGroupSecurityConfig, groups []RunnerGroup) RunnerGroupPolicyResult {
	name := configuredGroup
	if strings.TrimSpace(name) == "" {
		if policy.RequireExplicitGroup {
			return RunnerGroupPolicyResult{Violations: []string{"runner.group is required by security.runnerGroup.requireExplicitGroup"}}
		}
		for _, group := range groups {
			if group.Default {
				result := EvaluateRunnerGroupPolicy(group, nil, policy)
				result.Advisories = append(result.Advisories, "runner.group is empty; GitHub's default runner group will be used")
				return result
			}
		}
		return RunnerGroupPolicyResult{Violations: []string{"runner.group is empty and GitHub did not return a default runner group"}}
	}
	for _, group := range groups {
		if group.Name == name {
			return EvaluateRunnerGroupPolicy(group, nil, policy)
		}
	}
	return RunnerGroupPolicyResult{Violations: []string{fmt.Sprintf("runner group %q was not found in the organization", name)}}
}

func EvaluateRunnerGroupPolicy(group RunnerGroup, repositories []RunnerGroupRepository, policy config.RunnerGroupSecurityConfig) RunnerGroupPolicyResult {
	result := RunnerGroupPolicyResult{
		Group:        group,
		Resolved:     true,
		Repositories: append([]RunnerGroupRepository(nil), repositories...),
	}
	if group.Default {
		if policy.RequireNonDefaultGroup {
			result.Violations = append(result.Violations, fmt.Sprintf("runner group %q is GitHub's default group but security.runnerGroup.requireNonDefaultGroup is true", group.Name))
		} else {
			result.Advisories = append(result.Advisories, fmt.Sprintf("runner group %q is GitHub's default group; repository access may broaden as organization policy changes", group.Name))
		}
	}
	if group.Inherited {
		result.Advisories = append(result.Advisories, fmt.Sprintf("runner group %q is inherited from the enterprise; its policy must be managed at enterprise level", group.Name))
	}
	if !repositoryAccessAllows(policy.RequiredRepositoryAccess, group.Visibility) {
		result.Violations = append(result.Violations, fmt.Sprintf("runner group %q has repository access %q, broader than security.runnerGroup.requiredRepositoryAccess %q", group.Name, group.Visibility, policy.RequiredRepositoryAccess))
	}
	switch group.Visibility {
	case config.RunnerGroupRepositoryAccessPrivate:
		result.Advisories = append(result.Advisories, fmt.Sprintf("runner group %q is available to all private repositories in the organization", group.Name))
	case config.RunnerGroupRepositoryAccessAll:
		result.Advisories = append(result.Advisories, fmt.Sprintf("runner group %q is available to all repositories allowed by its public-repository setting", group.Name))
	case config.RunnerGroupRepositoryAccessSelected:
		if len(repositories) == 0 {
			result.Advisories = append(result.Advisories, fmt.Sprintf("runner group %q has no selected repositories and cannot receive jobs", group.Name))
		}
	default:
		result.Violations = append(result.Violations, fmt.Sprintf("runner group %q returned unsupported repository access %q", group.Name, group.Visibility))
	}
	if policy.RequirePublicRepositoriesDisabled {
		if group.AllowsPublicRepositories {
			result.Violations = append(result.Violations, fmt.Sprintf("runner group %q allows public repositories but security.runnerGroup.requirePublicRepositoriesDisabled is true", group.Name))
		}
		for _, repository := range repositories {
			if !repository.Private {
				name := repository.FullName
				if name == "" {
					name = repository.Name
				}
				result.Violations = append(result.Violations, fmt.Sprintf("runner group %q includes public repository %q while public repositories are required to be disabled", group.Name, name))
			}
		}
	} else if group.AllowsPublicRepositories {
		result.Advisories = append(result.Advisories, fmt.Sprintf("runner group %q allows public repositories; untrusted public or fork workflows may reach self-hosted runners", group.Name))
	}
	return result
}

func repositoryAccessAllows(maximum, actual string) bool {
	rank := map[string]int{
		config.RunnerGroupRepositoryAccessSelected: 0,
		config.RunnerGroupRepositoryAccessPrivate:  1,
		config.RunnerGroupRepositoryAccessAll:      2,
	}
	maximumRank, maximumKnown := rank[maximum]
	actualRank, actualKnown := rank[actual]
	return maximumKnown && actualKnown && actualRank <= maximumRank
}
