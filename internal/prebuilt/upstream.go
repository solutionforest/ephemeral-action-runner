package prebuilt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

const defaultGitHubAPI = "https://api.github.com"

type UpstreamGateResult struct {
	Eligible bool                     `json:"eligible"`
	Reason   string                   `json:"reason"`
	Evidence UpstreamWorkflowEvidence `json:"evidence,omitempty"`
}

type UpstreamGateClient struct {
	HTTPClient *http.Client
	BaseURL    string
	Token      string
	Now        func() time.Time
}

type upstreamWorkflowRun struct {
	ID         int64     `json:"id"`
	RunAttempt int       `json:"run_attempt"`
	Event      string    `json:"event"`
	Status     string    `json:"status"`
	Conclusion string    `json:"conclusion"`
	HeadBranch string    `json:"head_branch"`
	HeadSHA    string    `json:"head_sha"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type upstreamWorkflowRunsResponse struct {
	WorkflowRuns []upstreamWorkflowRun `json:"workflow_runs"`
}

type upstreamWorkflowStep struct {
	Name       string `json:"name"`
	Conclusion string `json:"conclusion"`
}

type upstreamWorkflowJob struct {
	Name       string                 `json:"name"`
	Status     string                 `json:"status"`
	Conclusion string                 `json:"conclusion"`
	Steps      []upstreamWorkflowStep `json:"steps"`
}

type upstreamWorkflowJobsResponse struct {
	Jobs []upstreamWorkflowJob `json:"jobs"`
}

func (c UpstreamGateClient) now() time.Time {
	if c.Now != nil {
		return c.Now().UTC()
	}
	return time.Now().UTC()
}

func (c UpstreamGateClient) endpoint(path string) string {
	base := strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
	if base == "" {
		base = defaultGitHubAPI
	}
	return base + path
}

func (c UpstreamGateClient) getJSON(ctx context.Context, path string, target any) error {
	client := c.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	token := strings.TrimSpace(c.Token)
	for attempt := 1; attempt <= 3; attempt++ {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint(path), nil)
		if err != nil {
			return err
		}
		request.Header.Set("Accept", "application/vnd.github+json")
		request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		request.Header.Set("User-Agent", "epar-prebuilt-publisher")
		if token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		response, requestErr := client.Do(request)
		if requestErr != nil {
			if attempt == 3 {
				return requestErr
			}
			continue
		}
		if response.StatusCode == http.StatusOK {
			decodeErr := json.NewDecoder(response.Body).Decode(target)
			response.Body.Close()
			if decodeErr != nil {
				return fmt.Errorf("decode GitHub API %s: %w", path, decodeErr)
			}
			return nil
		}
		status := response.Status
		statusCode := response.StatusCode
		response.Body.Close()
		if token != "" && (statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden) {
			// Public Actions metadata may reject a repository-scoped GITHUB_TOKEN.
			// Retry the same GET anonymously before requiring a dedicated token.
			token = ""
			continue
		}
		if statusCode != http.StatusTooManyRequests && statusCode < http.StatusInternalServerError || attempt == 3 {
			return fmt.Errorf("GitHub API %s returned %s", path, status)
		}
	}
	return fmt.Errorf("GitHub API %s exhausted retries", path)
}

func (c UpstreamGateClient) Evaluate(ctx context.Context, profile string) (UpstreamGateResult, error) {
	profile, err := NormalizeProfile(profile)
	if err != nil {
		return UpstreamGateResult{}, err
	}
	workflow := upstreamActWorkflow
	if profile == ProfileFull {
		workflow = upstreamFullWorkflow
	}
	runsPath := "/repos/" + upstreamRepository + "/actions/workflows/" + url.PathEscape(workflow) + "/runs?branch=master&per_page=20"
	var runs upstreamWorkflowRunsResponse
	if err := c.getJSON(ctx, runsPath, &runs); err != nil {
		return UpstreamGateResult{}, err
	}
	var tagMoving []upstreamWorkflowRun
	for _, run := range runs.WorkflowRuns {
		if run.Event == "schedule" || run.Event == "workflow_dispatch" {
			tagMoving = append(tagMoving, run)
		}
	}
	if len(tagMoving) == 0 {
		return UpstreamGateResult{Reason: "no tag-moving upstream workflow run was found"}, nil
	}
	sort.Slice(tagMoving, func(i, j int) bool { return tagMoving[i].CreatedAt.After(tagMoving[j].CreatedAt) })
	run := tagMoving[0]
	if run.ID <= 0 || run.RunAttempt <= 0 || run.HeadBranch != "master" || !revisionPattern.MatchString(run.HeadSHA) || run.CreatedAt.IsZero() || run.UpdatedAt.IsZero() {
		return UpstreamGateResult{}, errors.New("newest upstream workflow run metadata is malformed")
	}
	if run.Event != "schedule" {
		return UpstreamGateResult{Reason: "newest tag-moving upstream run was not scheduled"}, nil
	}
	completedAt := run.UpdatedAt.UTC()
	if run.Status != "completed" || completedAt.IsZero() || completedAt.After(c.now()) || c.now().Sub(completedAt) > upstreamEvidenceMaxAge {
		return UpstreamGateResult{Reason: "newest scheduled upstream run is incomplete or older than 10 days"}, nil
	}
	jobsPath := fmt.Sprintf("/repos/%s/actions/runs/%d/attempts/%d/jobs?per_page=100", upstreamRepository, run.ID, run.RunAttempt)
	var jobs upstreamWorkflowJobsResponse
	if err := c.getJSON(ctx, jobsPath, &jobs); err != nil {
		return UpstreamGateResult{}, err
	}
	gateJob := ""
	gateConclusion := "success"
	testConclusion := ""
	if profile == ProfileFull {
		if run.Conclusion != "success" || len(jobs.Jobs) != 4 {
			return UpstreamGateResult{Reason: "Full copy workflow or expected four-job matrix was not green"}, nil
		}
		expectedJobs := map[string]int{upstreamFull24Job: 2, upstreamFull22Job: 1, upstreamFull20Job: 1}
		for _, job := range jobs.Jobs {
			if job.Status != "completed" || job.Conclusion != "success" || !stepSucceeded(job.Steps, "Login to GitHub Container Registry") {
				return UpstreamGateResult{Reason: "Full copy workflow contained an incomplete GHCR copy job"}, nil
			}
			expectedJobs[job.Name]--
		}
		for _, remaining := range expectedJobs {
			if remaining != 0 {
				return UpstreamGateResult{Reason: "Full copy workflow job identities did not match the expected full-latest matrix"}, nil
			}
		}
		gateJob = "four Full copy jobs"
	} else {
		matches := 0
		for _, job := range jobs.Jobs {
			if job.Name != upstreamActGateJob {
				continue
			}
			matches++
			if job.Status != "completed" || job.Conclusion != "success" || !stepSucceeded(job.Steps, "Build and push ubuntu:act-24.04") {
				return UpstreamGateResult{Reason: "Act tag-producing job or push step was not green"}, nil
			}
			testConclusion = stepConclusion(job.Steps, "Run cd act/")
			if testConclusion != "success" && testConclusion != "skipped" {
				return UpstreamGateResult{Reason: "Act tag-producing job did not contain its expected test path"}, nil
			}
		}
		if matches != 1 {
			return UpstreamGateResult{Reason: "Act workflow did not contain exactly one tag-producing job"}, nil
		}
		gateJob = upstreamActGateJob
	}
	evidence := UpstreamWorkflowEvidence{
		Repository:     upstreamRepository,
		Workflow:       workflow,
		RunID:          run.ID,
		RunAttempt:     run.RunAttempt,
		Event:          run.Event,
		Branch:         run.HeadBranch,
		HeadSHA:        run.HeadSHA,
		RunConclusion:  run.Conclusion,
		Conclusion:     gateConclusion,
		GateJob:        gateJob,
		TestConclusion: testConclusion,
		CompletedAt:    completedAt,
	}
	if err := validateUpstreamWorkflowEvidence(profile, evidence, c.now()); err != nil {
		return UpstreamGateResult{}, err
	}
	return UpstreamGateResult{Eligible: true, Reason: "fresh upstream tag-producing workflow evidence is green", Evidence: evidence}, nil
}

func stepSucceeded(steps []upstreamWorkflowStep, name string) bool {
	for _, step := range steps {
		if step.Name == name && step.Conclusion == "success" {
			return true
		}
	}
	return false
}

func stepConclusion(steps []upstreamWorkflowStep, name string) string {
	for _, step := range steps {
		if step.Name == name {
			return step.Conclusion
		}
	}
	return ""
}
