package prebuilt

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestUpstreamGateActUsesTagProducingJobDespiteUnrelatedFailures(t *testing.T) {
	now := time.Date(2026, 8, 27, 0, 0, 0, 0, time.UTC)
	runs := upstreamRunsFixture(now, "schedule", "completed", "failure")
	jobs := `{"jobs":[{"name":"Build base 24.04","status":"completed","conclusion":"success","steps":[{"name":"Build and push ubuntu:act-24.04","conclusion":"success"},{"name":"Run cd act/","conclusion":"skipped"}]},{"name":"Unrelated flavor","status":"completed","conclusion":"failure","steps":[]}]}`
	client, closeServer := upstreamFixtureClient(t, runs, jobs, http.StatusOK)
	defer closeServer()

	result, err := client.Evaluate(context.Background(), ProfileAct)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Eligible || result.Evidence.RunConclusion != "failure" || result.Evidence.GateJob != upstreamActGateJob {
		t.Fatalf("Act gate result = %#v", result)
	}
}

func TestUpstreamGateFullRequiresGreenFourJobCopyMatrix(t *testing.T) {
	now := time.Date(2026, 8, 27, 0, 0, 0, 0, time.UTC)
	runs := upstreamRunsFixture(now, "schedule", "completed", "success")
	job := func(name string) string {
		return fmt.Sprintf(`{"name":%q,"status":"completed","conclusion":"success","steps":[{"name":"Login to GitHub Container Registry","conclusion":"success"}]}`, name)
	}
	jobs := `{"jobs":[` + strings.Join([]string{job(upstreamFull24Job), job(upstreamFull24Job), job(upstreamFull22Job), job(upstreamFull20Job)}, ",") + `]}`
	client, closeServer := upstreamFixtureClient(t, runs, jobs, http.StatusOK)
	defer closeServer()

	result, err := client.Evaluate(context.Background(), ProfileFull)
	if err != nil || !result.Eligible {
		t.Fatalf("Full gate = %#v, %v", result, err)
	}
}

func TestUpstreamGateNonGreenStatesSkipPublication(t *testing.T) {
	now := time.Date(2026, 8, 27, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		runs string
		jobs string
	}{
		{name: "newer manual", runs: fmt.Sprintf(`{"workflow_runs":[{"id":99,"run_attempt":1,"event":"workflow_dispatch","status":"completed","conclusion":"success","head_branch":"master","head_sha":"%s","created_at":"%s","updated_at":"%s"},{"id":42,"run_attempt":1,"event":"schedule","status":"completed","conclusion":"success","head_branch":"master","head_sha":"%s","created_at":"%s","updated_at":"%s"}]}`, strings.Repeat("a", 40), now.Add(-time.Hour).Format(time.RFC3339), now.Add(-time.Hour).Format(time.RFC3339), strings.Repeat("b", 40), now.Add(-2*time.Hour).Format(time.RFC3339), now.Add(-2*time.Hour).Format(time.RFC3339)), jobs: `{"jobs":[]}`},
		{name: "pending", runs: upstreamRunsFixture(now, "schedule", "in_progress", ""), jobs: `{"jobs":[]}`},
		{name: "stale", runs: upstreamRunsFixture(now.Add(-11*24*time.Hour), "schedule", "completed", "success"), jobs: `{"jobs":[]}`},
		{name: "missing job", runs: upstreamRunsFixture(now, "schedule", "completed", "success"), jobs: `{"jobs":[]}`},
		{name: "missing push step", runs: upstreamRunsFixture(now, "schedule", "completed", "success"), jobs: `{"jobs":[{"name":"Build base 24.04","status":"completed","conclusion":"success","steps":[]}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, closeServer := upstreamFixtureClient(t, tc.runs, tc.jobs, http.StatusOK)
			defer closeServer()
			result, err := client.Evaluate(context.Background(), ProfileAct)
			if err != nil {
				t.Fatal(err)
			}
			if result.Eligible || result.Reason == "" {
				t.Fatalf("non-green gate = %#v", result)
			}
		})
	}
}

func TestUpstreamEvidenceFreshnessBoundary(t *testing.T) {
	now := time.Date(2026, 8, 27, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name      string
		completed time.Time
		wantError bool
	}{
		{name: "exactly ten days", completed: now.Add(-10 * 24 * time.Hour)},
		{name: "just stale", completed: now.Add(-10*24*time.Hour - time.Second), wantError: true},
		{name: "future", completed: now.Add(time.Second), wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateUpstreamWorkflowEvidence(ProfileAct, validUpstreamEvidence(ProfileAct, tc.completed), now)
			if (err != nil) != tc.wantError {
				t.Fatalf("freshness error = %v, wantError=%v", err, tc.wantError)
			}
		})
	}
}

func TestUpstreamGateAPIFailuresFailClosed(t *testing.T) {
	now := time.Date(2026, 8, 27, 0, 0, 0, 0, time.UTC)
	for _, status := range []int{http.StatusForbidden, http.StatusTooManyRequests, http.StatusInternalServerError} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			client, closeServer := upstreamFixtureClient(t, upstreamRunsFixture(now, "schedule", "completed", "success"), `{"jobs":[]}`, status)
			defer closeServer()
			if _, err := client.Evaluate(context.Background(), ProfileAct); err == nil {
				t.Fatalf("HTTP %d unexpectedly passed", status)
			}
		})
	}
	client, closeServer := upstreamFixtureClient(t, `{`, `{"jobs":[]}`, http.StatusOK)
	defer closeServer()
	if _, err := client.Evaluate(context.Background(), ProfileAct); err == nil {
		t.Fatal("malformed response unexpectedly passed")
	}
}

func TestUpstreamGateFallsBackToAnonymousGETForPublicMetadata(t *testing.T) {
	now := time.Date(2026, 8, 27, 0, 0, 0, 0, time.UTC)
	runs := upstreamRunsFixture(now, "schedule", "completed", "failure")
	jobs := `{"jobs":[{"name":"Build base 24.04","status":"completed","conclusion":"success","steps":[{"name":"Build and push ubuntu:act-24.04","conclusion":"success"},{"name":"Run cd act/","conclusion":"skipped"}]}]}`
	authenticated := 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			t.Fatalf("method = %s", request.Method)
		}
		if request.Header.Get("Authorization") != "" {
			authenticated++
			response.WriteHeader(http.StatusForbidden)
			return
		}
		response.Header().Set("Content-Type", "application/json")
		if strings.Contains(request.URL.Path, "/actions/runs/") {
			_, _ = response.Write([]byte(jobs))
		} else {
			_, _ = response.Write([]byte(runs))
		}
	}))
	defer server.Close()
	result, err := (UpstreamGateClient{HTTPClient: server.Client(), BaseURL: server.URL, Token: "repository-scoped", Now: func() time.Time { return now }}).Evaluate(context.Background(), ProfileAct)
	if err != nil || !result.Eligible || authenticated != 2 {
		t.Fatalf("anonymous fallback = %#v, authenticated requests=%d, error=%v", result, authenticated, err)
	}
}

func TestUpstreamGateRetriesTransientTransportFailure(t *testing.T) {
	attempts := 0
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		attempts++
		return nil, context.DeadlineExceeded
	})}
	_, err := (UpstreamGateClient{HTTPClient: client}).Evaluate(context.Background(), ProfileAct)
	if !errors.Is(err, context.DeadlineExceeded) || attempts != 3 {
		t.Fatalf("transport retries = %d, error=%v", attempts, err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func upstreamRunsFixture(completedAt time.Time, event, status, conclusion string) string {
	createdAt := completedAt.Add(-time.Hour)
	return fmt.Sprintf(`{"workflow_runs":[{"id":42,"run_attempt":1,"event":%q,"status":%q,"conclusion":%q,"head_branch":"master","head_sha":"%s","created_at":"%s","updated_at":"%s"}]}`, event, status, conclusion, strings.Repeat("a", 40), createdAt.Format(time.RFC3339), completedAt.Format(time.RFC3339))
}

func upstreamFixtureClient(t *testing.T, runs, jobs string, status int) (UpstreamGateClient, func()) {
	t.Helper()
	now := time.Date(2026, 8, 27, 0, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if status != http.StatusOK {
			response.WriteHeader(status)
			return
		}
		response.Header().Set("Content-Type", "application/json")
		if strings.Contains(request.URL.Path, "/actions/runs/") {
			_, _ = response.Write([]byte(jobs))
			return
		}
		_, _ = response.Write([]byte(runs))
	}))
	return UpstreamGateClient{HTTPClient: server.Client(), BaseURL: server.URL, Now: func() time.Time { return now }}, server.Close
}
