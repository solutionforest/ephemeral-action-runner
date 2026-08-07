package dependency

import (
	"fmt"
	"strings"
	"time"
)

// PolicyKind identifies the startup/supervision retry mode.
type PolicyKind string

const (
	// PolicyOff disables startup retries (the default and fail-fast mode).
	PolicyOff PolicyKind = "off"
	// PolicyContinuous retries until the process context is canceled.
	PolicyContinuous PolicyKind = "continuous"
	// PolicyBounded retries until Duration after the first failure.
	PolicyBounded PolicyKind = "bounded"
)

// Policy controls whether a supervisor retries an incident.  For bounded
// policies, Duration is measured from the first failure and is not reset by
// each attempt.  Kind and Mode are both populated for compatibility with
// callers that prefer either field name.
type Policy struct {
	Kind     PolicyKind
	Mode     PolicyKind
	Duration time.Duration
}

// ParsePolicy parses the user-facing retry policy syntax.  Empty input and
// "off" normalize to PolicyOff; "continuous" has no deadline; any positive
// Go duration (for example "4h") creates a bounded policy.
func ParsePolicy(value string) (Policy, error) {
	value = strings.TrimSpace(strings.ToLower(value))
	switch value {
	case "", string(PolicyOff):
		return NewPolicy(PolicyOff, 0), nil
	case string(PolicyContinuous):
		return NewPolicy(PolicyContinuous, 0), nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil {
		return Policy{}, fmt.Errorf("invalid dependency retry policy %q: use off, continuous, or a positive Go duration: %w", value, err)
	}
	if duration <= 0 {
		return Policy{}, fmt.Errorf("invalid dependency retry policy %q: duration must be positive", value)
	}
	return NewPolicy(PolicyBounded, duration), nil
}

// NewPolicy constructs a policy while keeping Kind and Mode synchronized.
func NewPolicy(kind PolicyKind, duration time.Duration) Policy {
	if kind == PolicyBounded && duration <= 0 {
		kind = PolicyOff
		duration = 0
	}
	if kind != PolicyOff && kind != PolicyContinuous && kind != PolicyBounded {
		kind = PolicyOff
		duration = 0
	}
	return Policy{Kind: kind, Mode: kind, Duration: duration}
}

// Valid reports whether the policy is internally consistent.
func (policy Policy) Valid() bool {
	kind := policy.Kind
	if kind == "" {
		kind = policy.Mode
	}
	switch kind {
	case PolicyOff, PolicyContinuous:
		return policy.Duration == 0
	case PolicyBounded:
		return policy.Duration > 0
	default:
		return false
	}
}

// EffectiveKind returns Kind, falling back to Mode for values decoded from a
// caller-owned struct that only populated Mode.
func (policy Policy) EffectiveKind() PolicyKind {
	if policy.Kind != "" {
		return policy.Kind
	}
	if policy.Mode != "" {
		return policy.Mode
	}
	return PolicyOff
}

// IsOff reports whether retrying is disabled.
func (policy Policy) IsOff() bool { return policy.EffectiveKind() == PolicyOff }

// IsContinuous reports whether retries have no deadline other than context
// cancellation.
func (policy Policy) IsContinuous() bool { return policy.EffectiveKind() == PolicyContinuous }

// IsBounded reports whether retries have a finite incident deadline.
func (policy Policy) IsBounded() bool {
	return policy.EffectiveKind() == PolicyBounded && policy.Duration > 0
}

// Deadline returns the bounded deadline for an incident that started at.  It
// returns the zero time for off/continuous policies.
func (policy Policy) Deadline(start time.Time) time.Time {
	if !policy.IsBounded() || start.IsZero() {
		return time.Time{}
	}
	return start.Add(policy.Duration)
}

// String returns canonical user-facing syntax.
func (policy Policy) String() string {
	switch policy.EffectiveKind() {
	case PolicyContinuous:
		return string(PolicyContinuous)
	case PolicyBounded:
		if policy.Duration > 0 {
			return policy.Duration.String()
		}
	default:
		return string(PolicyOff)
	}
	return string(PolicyOff)
}
