package pool

import (
	"fmt"
	"sort"
	"strings"
	"time"

	gh "github.com/solutionforest/ephemeral-action-runner/internal/github"
)

const (
	unownedRunnerWarningInterval  = 30 * time.Minute
	unownedRunnerWarningListLimit = 8
)

type unownedRunnerIdentity struct {
	name string
	id   int64
}

type unownedRunnerWarningState struct {
	firstSeen        time.Time
	lastWarning      time.Time
	status           string
	busy             bool
	observationCount uint64
	present          bool
	episodeAnnounced bool
}

type unownedRunnerWarningReporter struct {
	observed map[unownedRunnerIdentity]unownedRunnerWarningState
}

type unownedRunnerWarningEvent struct {
	identity unownedRunnerIdentity
	state    unownedRunnerWarningState
	reason   string
}

func newUnownedRunnerWarningReporter() *unownedRunnerWarningReporter {
	return &unownedRunnerWarningReporter{observed: make(map[unownedRunnerIdentity]unownedRunnerWarningState)}
}

// observeSuccessful records one complete GitHub reconciliation. Callers must
// not invoke it after a partial inventory, ownership failure, or cancellation:
// absence is meaningful only when the entire remote pass succeeded.
func (r *unownedRunnerWarningReporter) observeSuccessful(now time.Time, runners []gh.Runner) (warning, resolved string) {
	if r == nil {
		return "", ""
	}
	if r.observed == nil {
		r.observed = make(map[unownedRunnerIdentity]unownedRunnerWarningState)
	}

	current := make(map[unownedRunnerIdentity]gh.Runner, len(runners))
	for _, runner := range sortedUniqueUnownedRunners(runners) {
		identity := unownedRunnerIdentity{name: runner.Name, id: runner.ID}
		current[identity] = runner
	}

	due := make([]unownedRunnerWarningEvent, 0, len(current))
	identities := sortedUnownedRunnerIdentities(current)
	for _, identity := range identities {
		runner := current[identity]
		state, found := r.observed[identity]
		reason := ""
		if !found {
			state = unownedRunnerWarningState{firstSeen: now}
			reason = "first observation"
		} else if !state.present {
			state.firstSeen = now
			state.observationCount = 0
			state.episodeAnnounced = false
			if !now.Before(state.lastWarning.Add(unownedRunnerWarningInterval)) {
				reason = "cooldown elapsed after reappearance"
			}
		} else if !now.Before(state.lastWarning.Add(unownedRunnerWarningInterval)) {
			reason = "periodic reminder"
		}
		if state.observationCount < ^uint64(0) {
			state.observationCount++
		}
		state.status = runner.Status
		state.busy = runner.Busy
		state.present = true
		if reason != "" {
			state.lastWarning = now
			state.episodeAnnounced = true
		}
		r.observed[identity] = state
		if reason != "" {
			due = append(due, unownedRunnerWarningEvent{identity: identity, state: state, reason: reason})
		}
	}

	disappeared := make([]unownedRunnerWarningEvent, 0)
	for identity, state := range r.observed {
		if _, found := current[identity]; found {
			continue
		}
		if state.present {
			if state.episodeAnnounced {
				disappeared = append(disappeared, unownedRunnerWarningEvent{identity: identity, state: state})
			}
			state.present = false
			state.episodeAnnounced = false
			state.firstSeen = time.Time{}
			state.observationCount = 0
		}
		if !now.Before(state.lastWarning.Add(unownedRunnerWarningInterval)) {
			delete(r.observed, identity)
			continue
		}
		r.observed[identity] = state
	}
	sort.Slice(disappeared, func(i, j int) bool {
		return unownedRunnerIdentityLess(disappeared[i].identity, disappeared[j].identity)
	})

	if len(due) > 0 {
		warning = formatUnownedRunnerWarning(now, due, len(current))
	}
	if len(disappeared) > 0 {
		resolved = formatUnownedRunnerResolution(now, disappeared)
	}
	return warning, resolved
}

func sortedUniqueUnownedRunners(runners []gh.Runner) []gh.Runner {
	sorted := append([]gh.Runner(nil), runners...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Name != sorted[j].Name {
			return sorted[i].Name < sorted[j].Name
		}
		if sorted[i].ID != sorted[j].ID {
			return sorted[i].ID < sorted[j].ID
		}
		if sorted[i].Status != sorted[j].Status {
			return sorted[i].Status < sorted[j].Status
		}
		return !sorted[i].Busy && sorted[j].Busy
	})
	unique := sorted[:0]
	for _, runner := range sorted {
		if len(unique) > 0 && unique[len(unique)-1].Name == runner.Name && unique[len(unique)-1].ID == runner.ID {
			continue
		}
		unique = append(unique, runner)
	}
	return unique
}

func sortedUnownedRunnerIdentities(current map[unownedRunnerIdentity]gh.Runner) []unownedRunnerIdentity {
	identities := make([]unownedRunnerIdentity, 0, len(current))
	for identity := range current {
		identities = append(identities, identity)
	}
	sort.Slice(identities, func(i, j int) bool {
		return unownedRunnerIdentityLess(identities[i], identities[j])
	})
	return identities
}

func unownedRunnerIdentityLess(left, right unownedRunnerIdentity) bool {
	if left.name != right.name {
		return left.name < right.name
	}
	return left.id < right.id
}

func formatUnownedRunnerWarning(now time.Time, events []unownedRunnerWarningEvent, total int) string {
	listed := len(events)
	if listed > unownedRunnerWarningListLimit {
		listed = unownedRunnerWarningListLimit
	}
	details := make([]string, 0, listed)
	for _, event := range events[:listed] {
		details = append(details, fmt.Sprintf("name=%q id=%d status=%q busy=%t observations=%d duration=%s reason=%q", event.identity.name, event.identity.id, displayRunnerStatus(event.state.status), event.state.busy, event.state.observationCount, nonNegativeDuration(now, event.state.firstSeen), event.reason))
	}
	omitted := ""
	if len(events) > listed {
		omitted = fmt.Sprintf("; %d additional reportable registration(s) omitted", len(events)-listed)
	}
	return fmt.Sprintf("reconciliation: reporting %d of %d currently observed unowned GitHub runner registration(s) matching the pool prefix: %s%s; repeat observations of the same name and id are suppressed for %s; prefix alone does not authorize deletion; these registrations do not consume local pool capacity", len(events), total, strings.Join(details, ", "), omitted, unownedRunnerWarningInterval)
}

func formatUnownedRunnerResolution(now time.Time, events []unownedRunnerWarningEvent) string {
	listed := len(events)
	if listed > unownedRunnerWarningListLimit {
		listed = unownedRunnerWarningListLimit
	}
	details := make([]string, 0, listed)
	for _, event := range events[:listed] {
		details = append(details, fmt.Sprintf("name=%q id=%d observations=%d duration=%s", event.identity.name, event.identity.id, event.state.observationCount, nonNegativeDuration(now, event.state.firstSeen)))
	}
	omitted := ""
	if len(events) > listed {
		omitted = fmt.Sprintf("; %d additional registration(s) omitted", len(events)-listed)
	}
	return fmt.Sprintf("reconciliation: %d previously reported unowned GitHub runner registration(s) are no longer classified as prefix-only report-only after a complete reconciliation: %s%s; this does not assert that the GitHub registration was deleted", len(events), strings.Join(details, ", "), omitted)
}

func displayRunnerStatus(status string) string {
	if status == "" {
		return "unknown"
	}
	return status
}

func nonNegativeDuration(now, start time.Time) time.Duration {
	if now.Before(start) {
		return 0
	}
	return now.Sub(start)
}
