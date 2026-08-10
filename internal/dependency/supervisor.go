package dependency

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/logging"
	storagecatalog "github.com/solutionforest/ephemeral-action-runner/internal/storage/catalog"
)

const IncidentStateSchemaVersion = 1

type IncidentState struct {
	SchemaVersion     int       `json:"schemaVersion"`
	Active            bool      `json:"active"`
	Outcome           string    `json:"outcome"`
	Policy            string    `json:"policy"`
	IncidentStartedAt time.Time `json:"incidentStartedAt,omitempty"`
	Attempt           int       `json:"attempt,omitempty"`
	Stage             string    `json:"stage,omitempty"`
	Dependency        string    `json:"dependency,omitempty"`
	NextRetryAt       time.Time `json:"nextRetryAt,omitempty"`
	RetryPermitted    bool      `json:"retryPermitted,omitempty"`
	Deadline          time.Time `json:"deadline,omitempty"`
	PID               int       `json:"pid,omitempty"`
	UpdatedAt         time.Time `json:"updatedAt,omitempty"`
	LastSafeError     string    `json:"lastSafeError,omitempty"`
	RequestID         string    `json:"requestId,omitempty"`
	RecoveredAt       time.Time `json:"recoveredAt,omitempty"`
	ExhaustedAt       time.Time `json:"exhaustedAt,omitempty"`
}

type RetryDecision struct {
	IncidentStarted bool
	Attempt         int
	Delay           time.Duration
	NextRetryAt     time.Time
	Deadline        time.Time
	RetryPermitted  bool
}

type SupervisorOptions struct {
	Policy  Policy
	Backoff BackoffSettings
	Path    string
	Now     func() time.Time
	PID     int
}

type Supervisor struct {
	mu      sync.Mutex
	policy  Policy
	backoff BackoffSettings
	path    string
	now     func() time.Time
	pid     int
	state   IncidentState
}

type ExhaustedError struct {
	State IncidentState
}

func (err *ExhaustedError) Error() string {
	if err == nil {
		return "external outage retry deadline exhausted"
	}
	message := "external outage retry deadline exhausted"
	if !err.State.Deadline.IsZero() {
		message += " at " + err.State.Deadline.UTC().Format(time.RFC3339)
	}
	if err.State.LastSafeError != "" {
		message += ": " + err.State.LastSafeError
	}
	return message
}

func ExternalOutageStatePath(projectRoot, configPath string) (string, error) {
	configID, err := storagecatalog.ConfigID(projectRoot, configPath)
	if err != nil {
		return "", err
	}
	return filepath.Join(projectRoot, ".local", "state", "supervision", configID, "external-outage.json"), nil
}

func ReadIncidentState(path string) (IncidentState, error) {
	content, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return IncidentState{}, nil
	}
	if err != nil {
		return IncidentState{}, err
	}
	var state IncidentState
	if err := json.Unmarshal(content, &state); err != nil {
		return IncidentState{}, fmt.Errorf("decode external outage supervision state: %w", err)
	}
	if state.SchemaVersion != IncidentStateSchemaVersion {
		return IncidentState{}, fmt.Errorf("unsupported external outage supervision state schema %d", state.SchemaVersion)
	}
	return state, nil
}

func NewSupervisor(options SupervisorOptions) (*Supervisor, error) {
	if !options.Policy.Valid() {
		return nil, fmt.Errorf("invalid external outage retry policy")
	}
	if strings.TrimSpace(options.Path) == "" {
		return nil, fmt.Errorf("external outage supervision state path is required")
	}
	state, err := ReadIncidentState(options.Path)
	if err != nil {
		return nil, err
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.PID <= 0 {
		options.PID = os.Getpid()
	}
	supervisor := &Supervisor{policy: options.Policy, backoff: options.Backoff.Normalize(), path: options.Path, now: options.Now, pid: options.PID, state: state}
	if supervisor.state.Active {
		supervisor.applyPolicyDeadline()
	} else if supervisor.state.Outcome == "exhausted" && !supervisor.policy.IsOff() {
		supervisor.applyPolicyDeadline()
		now := supervisor.now().UTC()
		if supervisor.policy.IsContinuous() || (!supervisor.state.Deadline.IsZero() && now.Before(supervisor.state.Deadline)) {
			supervisor.state.Active = true
			supervisor.state.Outcome = "retrying"
			supervisor.state.NextRetryAt = time.Time{}
			supervisor.state.RetryPermitted = true
		}
	}
	return supervisor, nil
}

func (supervisor *Supervisor) State() IncidentState {
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	return supervisor.state
}

func (supervisor *Supervisor) Cooldown(now time.Time) (time.Duration, bool) {
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	if !supervisor.state.Active || supervisor.state.NextRetryAt.IsZero() || !now.Before(supervisor.state.NextRetryAt) {
		return 0, false
	}
	return supervisor.state.NextRetryAt.Sub(now), true
}

// AttemptContext bounds a supervised retry attempt by the original incident
// deadline. The first attempt has no incident deadline because the incident
// clock starts only after that attempt returns a typed transient failure.
func (supervisor *Supervisor) AttemptContext(parent context.Context) (context.Context, context.CancelFunc) {
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	if supervisor.state.Active && !supervisor.state.Deadline.IsZero() {
		return context.WithDeadline(parent, supervisor.state.Deadline)
	}
	return context.WithCancel(parent)
}

func (supervisor *Supervisor) Schedule(stage string, failureErr error) (RetryDecision, error) {
	if !IsTypedRetryable(failureErr) {
		return RetryDecision{}, failureErr
	}
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	now := supervisor.now().UTC()
	started := !supervisor.state.Active
	if started {
		supervisor.state = IncidentState{SchemaVersion: IncidentStateSchemaVersion, Active: true, Outcome: "retrying", Policy: supervisor.policy.String(), IncidentStartedAt: now}
	}
	supervisor.applyPolicyDeadline()
	if !supervisor.state.Deadline.IsZero() && !now.Before(supervisor.state.Deadline) {
		return RetryDecision{}, supervisor.exhaustLocked(now)
	}
	failure, _ := AsFailure(failureErr)
	delay := CalculateDelay(supervisor.backoff, supervisor.state.Attempt, HintFromFailure(failureErr), now)
	retryPermitted := true
	if !supervisor.state.Deadline.IsZero() {
		delay, retryPermitted = ClampDelayToDeadline(delay, now, supervisor.state.Deadline)
	}
	supervisor.state.Attempt++
	supervisor.state.Stage = strings.TrimSpace(stage)
	supervisor.state.Dependency = "external"
	if failure != nil {
		if failure.Service != "" {
			supervisor.state.Dependency = failure.Service
		}
		supervisor.state.RequestID = SanitizeRequestID(failure.RequestID)
	} else {
		supervisor.state.RequestID = ""
	}
	supervisor.state.NextRetryAt = now.Add(delay)
	supervisor.state.RetryPermitted = retryPermitted
	supervisor.state.PID = supervisor.pid
	supervisor.state.UpdatedAt = now
	supervisor.state.LastSafeError = safeError(failureErr)
	if err := supervisor.writeLocked(); err != nil {
		return RetryDecision{}, err
	}
	return RetryDecision{IncidentStarted: started, Attempt: supervisor.state.Attempt, Delay: delay, NextRetryAt: supervisor.state.NextRetryAt, Deadline: supervisor.state.Deadline, RetryPermitted: retryPermitted}, nil
}

func (supervisor *Supervisor) WaitUntilReady(ctx context.Context) error {
	supervisor.mu.Lock()
	if supervisor.state.Outcome == "exhausted" && !supervisor.state.Active && !supervisor.policy.IsOff() {
		state := supervisor.state
		supervisor.mu.Unlock()
		return &ExhaustedError{State: state}
	}
	if !supervisor.state.Active {
		supervisor.mu.Unlock()
		return nil
	}
	now := supervisor.now().UTC()
	supervisor.applyPolicyDeadline()
	if !supervisor.state.Deadline.IsZero() && !now.Before(supervisor.state.Deadline) {
		err := supervisor.exhaustLocked(now)
		supervisor.mu.Unlock()
		return err
	}
	wakeAt := supervisor.state.NextRetryAt
	retryPermitted := supervisor.state.RetryPermitted
	deadline := supervisor.state.Deadline
	if !deadline.IsZero() && (wakeAt.IsZero() || deadline.Before(wakeAt)) {
		wakeAt = deadline
		retryPermitted = false
	}
	if wakeAt.IsZero() || !now.Before(wakeAt) {
		supervisor.mu.Unlock()
		return nil
	}
	delay := wakeAt.Sub(now)
	supervisor.state.Policy = supervisor.policy.String()
	supervisor.state.PID = supervisor.pid
	supervisor.state.UpdatedAt = now
	if err := supervisor.writeLocked(); err != nil {
		supervisor.mu.Unlock()
		return err
	}
	supervisor.mu.Unlock()

	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
	}
	if retryPermitted {
		return nil
	}
	supervisor.mu.Lock()
	err := supervisor.exhaustLocked(supervisor.now().UTC())
	supervisor.mu.Unlock()
	return err
}

// CheckDeadline reports exhaustion without waiting. It is used when an
// in-flight attempt returns an otherwise terminal error at the exact bounded
// incident deadline; unrelated terminal failures must not be delayed by the
// supervisor's cooldown.
func (supervisor *Supervisor) CheckDeadline() error {
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	if supervisor.state.Outcome == "exhausted" && !supervisor.state.Active && !supervisor.policy.IsOff() {
		return &ExhaustedError{State: supervisor.state}
	}
	if !supervisor.state.Active || supervisor.state.Deadline.IsZero() {
		return nil
	}
	now := supervisor.now().UTC()
	if now.Before(supervisor.state.Deadline) {
		return nil
	}
	return supervisor.exhaustLocked(now)
}

func (supervisor *Supervisor) MarkRecovered() error {
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	if supervisor.state.IncidentStartedAt.IsZero() || supervisor.state.Outcome == "recovered" {
		return nil
	}
	now := supervisor.now().UTC()
	supervisor.state.Active = false
	supervisor.state.Outcome = "recovered"
	supervisor.state.NextRetryAt = time.Time{}
	supervisor.state.RetryPermitted = false
	supervisor.state.PID = supervisor.pid
	supervisor.state.UpdatedAt = now
	supervisor.state.RecoveredAt = now
	return supervisor.writeLocked()
}

func (supervisor *Supervisor) applyPolicyDeadline() {
	supervisor.state.Policy = supervisor.policy.String()
	if supervisor.policy.IsBounded() {
		supervisor.state.Deadline = supervisor.policy.Deadline(supervisor.state.IncidentStartedAt).UTC()
	} else {
		supervisor.state.Deadline = time.Time{}
	}
}

func (supervisor *Supervisor) exhaustLocked(now time.Time) error {
	supervisor.state.Active = false
	supervisor.state.Outcome = "exhausted"
	supervisor.state.NextRetryAt = time.Time{}
	supervisor.state.RetryPermitted = false
	supervisor.state.PID = supervisor.pid
	supervisor.state.UpdatedAt = now
	supervisor.state.ExhaustedAt = now
	writeErr := supervisor.writeLocked()
	exhaustedErr := &ExhaustedError{State: supervisor.state}
	if writeErr != nil {
		return errors.Join(exhaustedErr, writeErr)
	}
	return exhaustedErr
}

func (supervisor *Supervisor) writeLocked() error {
	if err := os.MkdirAll(filepath.Dir(supervisor.path), 0o700); err != nil {
		return fmt.Errorf("create external outage supervision state directory: %w", err)
	}
	content, err := json.MarshalIndent(supervisor.state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode external outage supervision state: %w", err)
	}
	content = append(content, '\n')
	if err := logging.WritePrivateFileAtomic(supervisor.path, content); err != nil {
		return fmt.Errorf("write external outage supervision state: %w", err)
	}
	return nil
}

var sensitiveStateAssignment = regexp.MustCompile(`(?i)([[:alnum:]_.-]*(?:token|password|secret|authorization|private(?:[_-]?key)?)[[:alnum:]_.-]*=)(?:"[^"\r\n]*"|'[^'\r\n]*'|[^[:space:]]+)`)
var sensitiveStateAuthorization = regexp.MustCompile(`(?i)(authorization:[[:space:]]*(?:bearer|basic)[[:space:]]+)[^[:space:]]+`)
var sensitiveStateJSON = regexp.MustCompile(`(?i)((?:"|')?[[:alnum:]_.-]*(?:token|password|secret|authorization|private(?:[_-]?key)?)[[:alnum:]_.-]*(?:"|')?[[:space:]]*:[[:space:]]*)(?:"[^"\r\n]*"|'[^'\r\n]*'|[^,}\r\n[:space:]]+)`)
var sensitiveStateURLUserinfo = regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://)[^/@[:space:]]+@`)

// SafeError returns a bounded single-line error string with common credential
// forms redacted. It is suitable for durable state and operator logs.
func SafeError(err error) string {
	if err == nil {
		return ""
	}
	return sanitizeErrorText(err.Error())
}

func safeError(err error) string {
	if err == nil {
		return ""
	}
	return sanitizeErrorText(err.Error())
}

func sanitizeErrorText(value string) string {
	text := strings.Join(strings.Fields(value), " ")
	text = sensitiveStateAssignment.ReplaceAllString(text, `${1}[REDACTED]`)
	text = sensitiveStateAuthorization.ReplaceAllString(text, `${1}[REDACTED]`)
	text = sensitiveStateJSON.ReplaceAllString(text, `${1}"[REDACTED]"`)
	text = sensitiveStateURLUserinfo.ReplaceAllString(text, `${1}[REDACTED]@`)
	if len(text) > 2048 {
		text = text[:2048]
	}
	return text
}
