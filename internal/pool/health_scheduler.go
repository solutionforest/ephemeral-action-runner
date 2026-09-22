package pool

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
)

// Health evidence is advisory, never a host-trust lease. Bound its age even
// when a large or unavailable pool takes multiple supervisor ticks to visit.
const healthEvidenceLifetime = 60 * time.Second
const healthWarningInterval = 5 * time.Minute

type healthIdentity struct {
	name, providerID, trust string
	runnerID                int64
}

func runnerHealthIdentity(vm ProvisionedInstance) healthIdentity {
	return healthIdentity{vm.Name, vm.ProviderID, vm.HostTrustGeneration, vm.RunnerID}
}

type healthProgress struct {
	identity   healthIdentity
	stage      int
	done       bool
	admittedAt time.Time
}

type healthEpisode struct {
	first, last time.Time
	attempts    int
}

// Owned exclusively by RunPool. No probes or lifecycle mutations outlive a tick.
type healthScheduler struct {
	progress          map[string]*healthProgress
	episodes          map[healthIdentity]*healthEpisode
	lastWarnings      map[healthIdentity]time.Time
	lastSuccess       map[healthIdentity]time.Time
	globalAt          time.Time
	generation        uint64
	cursor            string
	globalDuration    time.Duration
	instanceDurations map[healthIdentity]map[string]time.Duration
	trustGeneration   string
	resume            string
}

func (s *healthScheduler) sync(active map[string]ProvisionedInstance, generation uint64, now time.Time, inactive map[string]int, trust ...string) {
	if s.progress == nil {
		s.progress = make(map[string]*healthProgress)
	}
	if s.episodes == nil {
		s.episodes = make(map[healthIdentity]*healthEpisode)
	}
	if s.lastWarnings == nil {
		s.lastWarnings = make(map[healthIdentity]time.Time)
	}
	if s.lastSuccess == nil {
		s.lastSuccess = make(map[healthIdentity]time.Time)
	}
	trustChanged := len(trust) > 0 && trust[0] != "" && trust[0] != s.trustGeneration
	reset := trustChanged || generation != s.generation
	if reset {
		s.globalAt = time.Time{}
		s.resume = ""
		for _, p := range s.progress {
			p.stage = 0
			p.done = false
			p.admittedAt = time.Time{}
		}
		if trustChanged || generation != s.generation {
			clear(inactive)
		}
	}
	// Refresh global admission independently: renewing a shared observation
	// must not erase exact-instance work already completed by a slow pool.
	if !s.globalAt.IsZero() && now.Sub(s.globalAt) >= healthEvidenceLifetime {
		s.globalAt = time.Time{}
	}
	s.generation = generation
	if len(trust) > 0 && trust[0] != "" {
		s.trustGeneration = trust[0]
	}
	identities := make(map[healthIdentity]bool)
	for name, vm := range active {
		if !shouldProbeRunnerLiveness(vm) {
			delete(s.progress, name)
			delete(inactive, name)
			continue
		}
		id := runnerHealthIdentity(vm)
		identities[id] = true
		if p := s.progress[name]; p == nil || p.identity != id {
			if s.resume == name {
				s.resume = ""
			}
			s.progress[name] = &healthProgress{identity: id}
			delete(inactive, name)
		}
	}
	for name := range s.progress {
		if _, ok := active[name]; !ok {
			delete(s.progress, name)
			delete(inactive, name)
		}
	}
	for id := range s.episodes {
		if !identities[id] {
			delete(s.episodes, id)
		}
	}
	for id := range s.lastWarnings {
		if !identities[id] {
			delete(s.lastWarnings, id)
		}
	}
	for id := range s.lastSuccess {
		if !identities[id] {
			delete(s.lastSuccess, id)
		}
	}
	for id := range s.instanceDurations {
		if !identities[id] {
			delete(s.instanceDurations, id)
		}
	}
	for _, p := range s.progress {
		if p.done || (p.stage > 0 && (p.admittedAt.IsZero() || now.Sub(p.admittedAt) >= healthEvidenceLifetime)) {
			p.stage = 0
			p.done = false
			p.admittedAt = time.Time{}
		}
	}
}

func (s *healthScheduler) next(excluded ...map[string]bool) (string, *healthProgress) {
	// Finish a successful admission's short remaining chain before starting
	// another expensive admission. Errors yield this preference immediately;
	// the round-robin cursor then gives every other identity its turn.
	if p := s.progress[s.resume]; p != nil && !p.done && (len(excluded) == 0 || !excluded[0][s.resume]) {
		s.cursor = s.resume
		return s.resume, p
	}
	names := make([]string, 0, len(s.progress))
	for name, p := range s.progress {
		if !p.done && (len(excluded) == 0 || !excluded[0][name]) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return "", nil
	}
	i := sort.SearchStrings(names, s.cursor)
	if i < len(names) && names[i] == s.cursor {
		i++
	}
	if i == len(names) {
		i = 0
	}
	s.cursor = names[i]
	return names[i], s.progress[names[i]]
}

func (s *healthScheduler) evidenceFresh(p *healthProgress, now time.Time) bool {
	return !s.globalAt.IsZero() && now.Sub(s.globalAt) < healthEvidenceLifetime && !p.admittedAt.IsZero() && now.Sub(p.admittedAt) < healthEvidenceLifetime
}

func (s *healthScheduler) phase(p *healthProgress) string {
	if s.globalAt.IsZero() {
		return "global-admission"
	}
	return []string{"instance-admission", "github", "process"}[p.stage]
}

// Estimates affect scheduling only; they neither declare failure nor extend a
// deadline. Retain the cursor on deferral so the next tick starts with this work.
func (s *healthScheduler) estimatedDuration(p *healthProgress) time.Duration {
	phase := s.phase(p)
	estimate := map[string]time.Duration{"global-admission": 4 * time.Second, "instance-admission": 9 * time.Second, "github": 3 * time.Second, "process": 6 * time.Second}[phase]
	observed := s.instanceDurations[p.identity][phase]
	if phase == "global-admission" {
		observed = s.globalDuration
	}
	if observed > 0 {
		estimate = observed
	}
	return estimate
}

// A slow sample raises the estimate immediately. Successful faster samples
// release only one eighth of the excess each time; failed/partial probes cannot
// lower it. fits adds headroom and caps budget admission so slow work can retry.
func (s *healthScheduler) observeDuration(id healthIdentity, phase string, elapsed time.Duration, succeeded bool) {
	if elapsed <= 0 {
		return
	}
	update := func(previous time.Duration) time.Duration {
		if elapsed >= previous {
			return elapsed
		}
		if succeeded {
			return previous - (previous-elapsed)/8
		}
		return previous
	}
	if phase == "global-admission" {
		s.globalDuration = update(s.globalDuration)
		return
	}
	if s.instanceDurations == nil {
		s.instanceDurations = make(map[healthIdentity]map[string]time.Duration)
	}
	if s.instanceDurations[id] == nil {
		s.instanceDurations[id] = make(map[string]time.Duration)
	}
	s.instanceDurations[id][phase] = update(s.instanceDurations[id][phase])
}

func (s *healthScheduler) fits(ctx context.Context, p *healthProgress, clock ...time.Time) bool {
	now := time.Now()
	if len(clock) > 0 {
		now = clock[0]
	}
	// Do not knowingly start a phase whose admission evidence will expire
	// before it can finish. Global renewal leaves per-instance progress intact.
	estimate := s.estimatedDuration(p) + time.Second
	if p.stage > 0 && (p.admittedAt.IsZero() || now.Sub(p.admittedAt)+estimate >= healthEvidenceLifetime) {
		p.stage = 0
		p.admittedAt = time.Time{}
		estimate = s.estimatedDuration(p) + time.Second
	}
	if !s.globalAt.IsZero() && now.Sub(s.globalAt)+estimate >= healthEvidenceLifetime {
		s.globalAt = time.Time{}
	}
	deadline, bounded := ctx.Deadline()
	if !bounded {
		return true
	}
	budget := hostTrustRefreshInterval / 2
	required := s.estimatedDuration(p) + time.Second
	if required > budget*9/10 {
		required = budget * 9 / 10
	}
	return deadline.Sub(now) >= required
}

// visit performs only one resumable phase. Negative verdicts are returned now,
// never stored for retirement in a later tick. Observed cancellation prevents
// dispatch; cancellation may still race with dependency entry. A success arriving
// after the deadline remains an unknown result.
func (m *Manager) visitRunnerHealth(ctx context.Context, s *healthScheduler, p *healthProgress, vm ProvisionedInstance) (done, alive bool, reason, stage string, err error) {
	stage = s.phase(p)
	if err = ctx.Err(); err != nil {
		return
	}
	// The host-trust sweep supplies its shorter parent deadline. Without host
	// trust, preserve the ordinary 30-second health-probe allowance.
	stageCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	callerCtx := ctx
	ctx = stageCtx
	contextErr := func() error {
		if err := callerCtx.Err(); err != nil {
			return err
		}
		return ctx.Err()
	}
	started := m.currentTime()
	defer func() {
		if deadlineErr := contextErr(); deadlineErr != nil {
			done, alive, err = false, true, deadlineErr
		}
		if done && !s.evidenceFresh(p, m.currentTime()) {
			done, alive, err = false, true, fmt.Errorf("health admission evidence expired")
		}
		if err != nil || done {
			s.resume = ""
		} else if stage != "global-admission" {
			s.resume = vm.Name
		}
		elapsed := m.currentTime().Sub(started)
		s.observeDuration(runnerHealthIdentity(vm), stage, elapsed, err == nil)
		m.logger().Debug(fmt.Sprintf("runner health phase completed: instance=%s providerID=%s runnerID=%d stage=%s duration=%s finished=%t alive=%t error=%v", vm.Name, vm.ProviderID, vm.RunnerID, stage, elapsed, done, alive, err))
	}()
	if s.globalAt.IsZero() {
		if v, ok := m.Lifecycle.(provider.AdmissionVerifier); ok {
			err = v.VerifyAdmission(ctx)
		}
		if err == nil && contextErr() == nil {
			s.globalAt = m.currentTime()
		}
		return
	}
	switch p.stage {
	case 0:
		var instance provider.Instance
		instance, err = m.providerInstance(ctx, vm.Name)
		if err == nil && instance.ProviderID != vm.ProviderID {
			err = fmt.Errorf("provider identity changed for %s", vm.Name)
		}
		if err == nil && contextErr() == nil {
			if v, ok := m.Lifecycle.(provider.InstanceAdmissionVerifier); ok {
				err = v.VerifyInstanceAdmission(ctx, instance)
			}
		}
	case 1:
		if m.GitHub != nil {
			runner, found, lookupErr := m.GitHub.RunnerByName(ctx, vm.Name)
			err = lookupErr
			if err != nil {
				return
			}
			if contextErr() != nil {
				return
			}
			if found && vm.RunnerID != 0 && runner.ID != vm.RunnerID {
				err = fmt.Errorf("GitHub runner identity changed for %s", vm.Name)
				return
			}
			if !found {
				err = m.recordLifecycleRemoteAbsence(ctx, vm.Name)
				done, reason = err == nil, "GitHub runner record is gone"
				return
			}
			if runner.Busy {
				done, alive = true, true
				return
			}
			if runner.Status != "online" {
				done, reason = true, fmt.Sprintf("GitHub runner status is %q", runner.Status)
				return
			}
		}
	case 2:
		// Resolve and compare immediately before executing against the exact
		// provider handle, rather than resolving the name again inside execGuest.
		var instance provider.Instance
		instance, err = m.providerInstance(ctx, vm.Name)
		if err == nil && instance.ProviderID != vm.ProviderID {
			err = fmt.Errorf("provider identity changed for %s", vm.Name)
		}
		if err != nil || contextErr() != nil {
			return
		}
		var result provider.ExecResult
		result, err = m.providerLifecycle().Exec(ctx, instance, provider.ShellCommand(runnerProcessHealthScript()), provider.ExecOptions{SuppressTranscript: true})
		if err != nil {
			return
		}
		alive, err = parseRunnerProcessHealth(result.Stdout)
		done = err == nil
		if !alive {
			reason = runnerProcessInactiveReason
			// GitHub may have assigned work since the preceding tick. Never
			// retire on an earlier idle observation plus a later process result.
			if err == nil && m.GitHub != nil && contextErr() == nil {
				runner, found, lookupErr := m.GitHub.RunnerByName(ctx, vm.Name)
				if lookupErr != nil {
					done, err = false, lookupErr
					return
				}
				if found && vm.RunnerID != 0 && runner.ID != vm.RunnerID {
					done, err = false, fmt.Errorf("GitHub runner identity changed for %s", vm.Name)
					return
				}
				if found && runner.Busy {
					alive, reason = true, ""
				}
			}
		}
		return
	}
	if err == nil && contextErr() == nil {
		if p.stage == 0 {
			p.admittedAt = m.currentTime()
		}
		p.stage++
	}
	return
}

func (s *healthScheduler) unknown(id healthIdentity, now time.Time) (bool, int, time.Duration) {
	if s.episodes == nil {
		s.episodes = make(map[healthIdentity]*healthEpisode)
	}
	if s.lastWarnings == nil {
		s.lastWarnings = make(map[healthIdentity]time.Time)
	}
	e := s.episodes[id]
	if e == nil {
		e = &healthEpisode{first: now}
		s.episodes[id] = e
	}
	e.attempts++
	emit := s.lastWarnings[id].IsZero() || now.Sub(s.lastWarnings[id]) >= healthWarningInterval
	if emit {
		e.last = now
		s.lastWarnings[id] = now
	}
	return emit, e.attempts, now.Sub(e.first)
}

func (m *Manager) reportUnknownHealth(ctx, probeCtx context.Context, s *healthScheduler, vm ProvisionedInstance, stage string, err error) {
	if ctx.Err() != nil {
		return
	}
	if emit, count, elapsed := s.unknown(runnerHealthIdentity(vm), m.currentTime()); emit {
		owner := "stage/dependency"
		if probeCtx.Err() != nil {
			owner = "sweep-budget"
		}
		lastSuccess := "never"
		if at := s.lastSuccess[runnerHealthIdentity(vm)]; !at.IsZero() {
			lastSuccess = m.currentTime().Sub(at).Round(time.Second).String()
		}
		m.warnf("[%s] runner health is temporarily unknown; keeping the runner and retrying: runnerID=%d providerID=%s attempts=%d duration=%s stage=%s deadlineOwner=%s lastSuccessfulHealthAgo=%s: %v\n", vm.Name, vm.RunnerID, vm.ProviderID, count, elapsed.Round(time.Second), stage, owner, lastSuccess, err)
	}
}

func (m *Manager) reportRecoveredHealth(s *healthScheduler, vm ProvisionedInstance) {
	id := runnerHealthIdentity(vm)
	if e := s.episodes[id]; e != nil {
		// Only announced episodes need a recovery message. Preserve the last
		// warning time across recovery so flapping does not restart the flood.
		if !e.last.IsZero() {
			m.infof("[%s] runner health is known again: runnerID=%d providerID=%s unknownAttempts=%d duration=%s\n", vm.Name, vm.RunnerID, vm.ProviderID, e.attempts, m.currentTime().Sub(e.first).Round(time.Second))
		}
		delete(s.episodes, id)
	}
}
