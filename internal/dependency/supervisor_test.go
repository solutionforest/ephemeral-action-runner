package dependency

import (
	"context"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParsePolicy(t *testing.T) {
	for _, test := range []struct {
		value      string
		kind       PolicyKind
		duration   time.Duration
		shouldFail bool
	}{
		{"", PolicyOff, 0, false},
		{"off", PolicyOff, 0, false},
		{"continuous", PolicyContinuous, 0, false},
		{"4h", PolicyBounded, 4 * time.Hour, false},
		{"0", "", 0, true},
		{"0s", "", 0, true},
		{"-1s", "", 0, true},
		{"bounded", "", 0, true},
	} {
		t.Run(test.value, func(t *testing.T) {
			policy, err := ParsePolicy(test.value)
			if test.shouldFail {
				if err == nil {
					t.Fatalf("ParsePolicy(%q) unexpectedly succeeded", test.value)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if policy.EffectiveKind() != test.kind || policy.Duration != test.duration {
				t.Fatalf("ParsePolicy(%q) = %#v, want kind=%s duration=%s", test.value, policy, test.kind, test.duration)
			}
		})
	}
}

func TestFailureClassification(t *testing.T) {
	for _, status := range []int{http.StatusRequestTimeout, http.StatusTooManyRequests, 500, 502, 503, 599} {
		if !NewHTTPFailure("service", "operation", HTTPMetadata{StatusCode: status}, errors.New("failed")).Retryable {
			t.Fatalf("HTTP %d was not retryable", status)
		}
	}
	if NewHTTPFailure("service", "operation", HTTPMetadata{StatusCode: http.StatusForbidden}, errors.New("forbidden")).Retryable {
		t.Fatal("ordinary HTTP 403 was retryable")
	}
	if !NewHTTPFailure("service", "operation", HTTPMetadata{StatusCode: http.StatusForbidden, RateLimitKnown: true, RateLimitRemain: 0}, errors.New("rate limited")).Retryable {
		t.Fatal("rate-limited HTTP 403 was terminal")
	}
	if !IsRetryable(&net.DNSError{Err: "no such host", Name: "api.example.test"}) {
		t.Fatal("DNS failure was terminal")
	}
	if IsTypedRetryable(&net.DNSError{Err: "no such host", Name: "api.example.test"}) {
		t.Fatal("bare DNS failure crossed the typed supervision boundary")
	}
	if !IsTypedRetryable(NewFailure("github", "request", errors.New("offline"), WithRetryable(true))) {
		t.Fatal("structured transient failure was not supervision eligible")
	}
	if !IsRetryable(context.DeadlineExceeded) {
		t.Fatal("attempt timeout was terminal")
	}
	if IsRetryable(x509.UnknownAuthorityError{}) {
		t.Fatal("x509 trust failure was retryable")
	}
	if IsRetryable(&url.Error{Op: "parse", URL: "://bad", Err: errors.New("missing protocol scheme")}) {
		t.Fatal("malformed URL/configuration failure was retryable")
	}
}

func TestBackoffHonorsServerHintBeyondCapAndDeadline(t *testing.T) {
	now := time.Date(2026, 8, 7, 1, 0, 0, 0, time.UTC)
	settings := BackoffSettings{Initial: 15 * time.Second, Maximum: 30 * time.Second, Multiplier: 2, Jitter: 0, Minimum: time.Second}
	delay := CalculateDelay(settings, 10, ServerHint{RetryAfter: 2 * time.Minute}, now)
	if delay != 2*time.Minute {
		t.Fatalf("delay = %s, want server hint 2m", delay)
	}
	clamped, retry := ClampDelayToDeadline(delay, now, now.Add(time.Minute))
	if clamped != time.Minute || retry {
		t.Fatalf("deadline clamp = %s retry=%t, want 1m false", clamped, retry)
	}
}

func TestSupervisorPersistsOriginalIncidentAcrossRestartAndExhaustsWithoutAnotherRequest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "external-outage.json")
	now := time.Date(2026, 8, 7, 1, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	policy := NewPolicy(PolicyBounded, 80*time.Millisecond)
	settings := BackoffSettings{Initial: time.Second, Maximum: time.Second, Multiplier: 2, Minimum: time.Second, Random: func() float64 { return 0.5 }}
	supervisor, err := NewSupervisor(SupervisorOptions{Policy: policy, Backoff: settings, Path: path, Now: clock, PID: os.Getpid()})
	if err != nil {
		t.Fatal(err)
	}
	failure := NewFailure("github", "list runners", errors.New(`TOKEN=secret-value Authorization: Bearer bearer-secret {"token":"json-secret","authorization":"Bearer json-bearer"} https://user:password@example.test unavailable`), WithRetryable(true), WithRequestID("request-123"))
	decision, err := supervisor.Schedule("initial-reconciliation", failure)
	if err != nil {
		t.Fatal(err)
	}
	if decision.RetryPermitted || decision.Deadline.IsZero() || decision.NextRetryAt != decision.Deadline {
		t.Fatalf("decision = %#v, want wait-to-deadline without another attempt", decision)
	}
	persisted, err := ReadIncidentState(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(persisted.LastSafeError, "secret-value") || strings.Contains(persisted.LastSafeError, "bearer-secret") || strings.Contains(persisted.LastSafeError, "json-secret") || strings.Contains(persisted.LastSafeError, "json-bearer") || strings.Contains(persisted.LastSafeError, "user:password") || !strings.Contains(persisted.LastSafeError, "[REDACTED]") {
		t.Fatalf("persisted error was not redacted: %q", persisted.LastSafeError)
	}
	if rendered := failure.Error(); strings.Contains(rendered, "json-secret") || strings.Contains(rendered, "json-bearer") {
		t.Fatalf("typed failure rendered credentials: %q", rendered)
	}
	if persisted.RequestID != "request-123" || persisted.IncidentStartedAt.IsZero() {
		t.Fatalf("persisted state = %#v", persisted)
	}

	shorter := NewPolicy(PolicyBounded, 30*time.Millisecond)
	now = persisted.IncidentStartedAt.Add(shorter.Duration)
	restarted, err := NewSupervisor(SupervisorOptions{Policy: shorter, Backoff: settings, Path: path, Now: clock, PID: os.Getpid()})
	if err != nil {
		t.Fatal(err)
	}
	err = restarted.WaitUntilReady(context.Background())
	var exhausted *ExhaustedError
	if !errors.As(err, &exhausted) {
		t.Fatalf("WaitUntilReady() error = %v, want exhausted", err)
	}
	wantDeadline := persisted.IncidentStartedAt.Add(shorter.Duration)
	if !exhausted.State.Deadline.Equal(wantDeadline) {
		t.Fatalf("restart deadline = %s, want original incident deadline %s", exhausted.State.Deadline, wantDeadline)
	}
	finalState, err := ReadIncidentState(path)
	if err != nil {
		t.Fatal(err)
	}
	if finalState.Active || finalState.Outcome != "exhausted" || finalState.ExhaustedAt.IsZero() {
		t.Fatalf("final state = %#v", finalState)
	}
	latched, err := NewSupervisor(SupervisorOptions{Policy: shorter, Backoff: settings, Path: path, Now: clock, PID: os.Getpid()})
	if err != nil {
		t.Fatal(err)
	}
	err = latched.WaitUntilReady(context.Background())
	if !errors.As(err, &exhausted) {
		t.Fatalf("persisted exhausted deadline was not latched: err=%v", err)
	}
	off, err := NewSupervisor(SupervisorOptions{Policy: NewPolicy(PolicyOff, 0), Backoff: settings, Path: path, Now: clock, PID: os.Getpid()})
	if err != nil {
		t.Fatal(err)
	}
	if err := off.MarkRecovered(); err != nil {
		t.Fatal(err)
	}
	fresh, err := NewSupervisor(SupervisorOptions{Policy: shorter, Backoff: settings, Path: path, Now: clock, PID: os.Getpid()})
	if err != nil {
		t.Fatal(err)
	}
	if err := fresh.WaitUntilReady(context.Background()); err != nil {
		t.Fatalf("observed recovery did not permit a fresh later incident: %v", err)
	}
}

func TestSupervisorRecoveryEndsIncidentAndNextFailureStartsFreshWindow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "external-outage.json")
	supervisor, err := NewSupervisor(SupervisorOptions{Policy: NewPolicy(PolicyContinuous, 0), Backoff: DefaultBackoffSettings(), Path: path})
	if err != nil {
		t.Fatal(err)
	}
	failure := NewTransient("list runners", errors.New("offline"))
	first, err := supervisor.Schedule("reconcile", failure)
	if err != nil {
		t.Fatal(err)
	}
	if err := supervisor.MarkRecovered(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)
	second, err := supervisor.Schedule("registration", failure)
	if err != nil {
		t.Fatal(err)
	}
	state := supervisor.State()
	if !first.IncidentStarted || !second.IncidentStarted || state.Attempt != 1 || !state.IncidentStartedAt.After(first.NextRetryAt.Add(-first.Delay)) {
		t.Fatalf("fresh incident was not started: first=%#v second=%#v state=%#v", first, second, state)
	}
}
