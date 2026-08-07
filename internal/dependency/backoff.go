package dependency

import (
	"math"
	"math/rand"
	"time"
)

// BackoffSettings describes the shared exponential retry policy.  Jitter is
// normally a fraction (0.20 means plus or minus 20 percent); values greater
// than one are accepted as percentages for configuration compatibility.  A
// custom Random function makes tests deterministic without global state.
type BackoffSettings struct {
	Initial       time.Duration
	Maximum       time.Duration
	Multiplier    float64
	Jitter        float64
	JitterPercent float64
	Minimum       time.Duration
	Random        func() float64
}

// DefaultBackoffSettings returns EPAR's shared 15-second to 30-minute policy.
func DefaultBackoffSettings() BackoffSettings {
	return BackoffSettings{
		Initial:    15 * time.Second,
		Maximum:    1800 * time.Second,
		Multiplier: 2,
		Jitter:     0.20,
		Minimum:    time.Second,
	}
}

// NewBackoffSettings converts the configuration's seconds and percentage
// representation to BackoffSettings.
func NewBackoffSettings(initialSeconds, maximumSeconds int, multiplier float64, jitterPercent int) BackoffSettings {
	settings := BackoffSettings{
		Initial:       time.Duration(initialSeconds) * time.Second,
		Maximum:       time.Duration(maximumSeconds) * time.Second,
		Multiplier:    multiplier,
		JitterPercent: float64(jitterPercent),
		Minimum:       time.Second,
	}
	return settings.Normalize()
}

// Normalize fills omitted values with safe defaults and clamps malformed
// jitter/multiplier values.  Configuration validation should normally reject
// malformed values before this method is reached, but supervisors must remain
// safe when handed a zero-value settings struct.
func (settings BackoffSettings) Normalize() BackoffSettings {
	defaults := DefaultBackoffSettings()
	if settings.Initial <= 0 {
		settings.Initial = defaults.Initial
	}
	if settings.Maximum <= 0 {
		settings.Maximum = defaults.Maximum
	}
	if settings.Maximum < settings.Initial {
		settings.Maximum = settings.Initial
	}
	if settings.Multiplier < 1 || math.IsNaN(settings.Multiplier) || math.IsInf(settings.Multiplier, 0) {
		settings.Multiplier = defaults.Multiplier
	}
	if settings.JitterPercent != 0 {
		settings.Jitter = settings.JitterPercent / 100
	}
	if settings.Jitter > 1 {
		settings.Jitter /= 100
	}
	if settings.Jitter < 0 || math.IsNaN(settings.Jitter) || math.IsInf(settings.Jitter, 0) {
		settings.Jitter = 0
	}
	if settings.Minimum <= 0 {
		settings.Minimum = time.Second
	}
	return settings
}

// ServerHint carries dependency-provided delay hints.  Hints are applied
// after the local cap, so a server's Retry-After or reset time may defer a
// retry beyond the configured maximum.
type ServerHint struct {
	RetryAfter     time.Duration
	RateLimitReset time.Time
}

// HintFromFailure extracts server hints from a structured failure.
func HintFromFailure(err error) ServerHint {
	failure, ok := AsFailure(err)
	if !ok {
		return ServerHint{}
	}
	return ServerHint{RetryAfter: failure.RetryAfter, RateLimitReset: failure.RateLimitReset}
}

// CalculateDelay calculates the next retry delay for a zero-based attempt.
// It applies exponential backoff and jitter, enforces a positive floor, then
// honors Retry-After/rate-limit-reset hints as lower bounds.
func CalculateDelay(settings BackoffSettings, attempt int, hint ServerHint, now time.Time) time.Duration {
	settings = settings.Normalize()
	if attempt < 0 {
		attempt = 0
	}

	// Compute the nominal delay without allowing a floating-point overflow to
	// wrap a time.Duration.  The cap is applied before jitter as in the legacy
	// replacement policy.
	nominal := float64(settings.Initial)
	if attempt > 0 {
		nominal *= math.Pow(settings.Multiplier, float64(attempt))
	}
	if math.IsInf(nominal, 0) || nominal > float64(settings.Maximum) {
		nominal = float64(settings.Maximum)
	}

	factor := 1.0
	if settings.Jitter > 0 {
		randomValue := rand.Float64()
		if settings.Random != nil {
			randomValue = settings.Random()
		}
		if randomValue < 0 {
			randomValue = 0
		} else if randomValue > 1 {
			randomValue = 1
		}
		factor += ((randomValue * 2) - 1) * settings.Jitter
	}
	delay := time.Duration(nominal * factor)
	minimum := settings.Minimum
	if minimum < time.Second {
		minimum = time.Second
	}
	if delay < minimum {
		delay = minimum
	}
	if delay > settings.Maximum && settings.Maximum >= minimum {
		delay = settings.Maximum
	}

	if hint.RetryAfter > delay {
		delay = hint.RetryAfter
	}
	if !hint.RateLimitReset.IsZero() && hint.RateLimitReset.After(now) {
		if resetDelay := hint.RateLimitReset.Sub(now); resetDelay > delay {
			delay = resetDelay
		}
	}
	return delay
}

// CalculateDelayWithRandom is useful when a caller prefers to supply a
// random sample directly rather than retaining it in BackoffSettings.
func CalculateDelayWithRandom(settings BackoffSettings, attempt int, hint ServerHint, now time.Time, randomValue float64) time.Duration {
	settings.Random = func() float64 { return randomValue }
	return CalculateDelay(settings, attempt, hint, now)
}

// ClampDelayToDeadline clamps a delay to a bounded incident deadline.  The
// bool is false when the next attempt cannot begin before the deadline; the
// caller should return the last failure rather than issuing another request.
func ClampDelayToDeadline(delay time.Duration, now, deadline time.Time) (time.Duration, bool) {
	if deadline.IsZero() {
		return delay, true
	}
	remaining := deadline.Sub(now)
	if remaining <= 0 {
		return 0, false
	}
	if delay >= remaining {
		return remaining, false
	}
	return delay, true
}

// BackoffState tracks attempts for a single logical incident.  It is small
// enough to embed in a durable supervisor state record.
type BackoffState struct {
	Attempt           int
	IncidentStartedAt time.Time
	LastFailureAt     time.Time
	NextRetryAt       time.Time
}

// Active reports whether the state currently defers another attempt.
func (state BackoffState) Active(now time.Time) bool {
	return !state.NextRetryAt.IsZero() && now.Before(state.NextRetryAt)
}

// Remaining reports the current cooldown, rounded to the nearest second for
// operator-facing status.  It returns zero when no cooldown is active.
func (state BackoffState) Remaining(now time.Time) time.Duration {
	if !state.Active(now) {
		return 0
	}
	return state.NextRetryAt.Sub(now).Round(time.Second)
}

// Schedule records a failed attempt and returns its chosen delay.
func (state *BackoffState) Schedule(now time.Time, settings BackoffSettings, hint ServerHint) time.Duration {
	if state.IncidentStartedAt.IsZero() {
		state.IncidentStartedAt = now
	}
	state.LastFailureAt = now
	delay := CalculateDelay(settings, state.Attempt, hint, now)
	state.Attempt++
	state.NextRetryAt = now.Add(delay)
	return delay
}

// ScheduleError records a structured failure's server hints.
func (state *BackoffState) ScheduleError(now time.Time, settings BackoffSettings, err error) time.Duration {
	return state.Schedule(now, settings, HintFromFailure(err))
}

// Reset clears the incident state after a successful dependency operation.
func (state *BackoffState) Reset() {
	*state = BackoffState{}
}
