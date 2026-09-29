package pool

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gh "github.com/solutionforest/ephemeral-action-runner/internal/github"
	"github.com/solutionforest/ephemeral-action-runner/internal/logging"
	poolstate "github.com/solutionforest/ephemeral-action-runner/internal/pool/state"
	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
)

func TestUnownedRunnerWarningReporterThrottleBoundaryAndRecovery(t *testing.T) {
	reporter := newUnownedRunnerWarningReporter()
	started := time.Date(2026, time.September, 29, 19, 4, 25, 0, time.UTC)
	runner := gh.Runner{Name: "epar-test-stale", ID: 2450, Status: "offline"}

	warning, recovered := reporter.observeSuccessful(started, []gh.Runner{runner})
	if warning == "" || recovered != "" {
		t.Fatalf("first observation warning = %q, recovered = %q", warning, recovered)
	}
	for _, required := range []string{
		`name="epar-test-stale" id=2450`,
		`observations=1 duration=0s reason="first observation"`,
		"prefix alone does not authorize deletion",
		"do not consume local pool capacity",
	} {
		if !strings.Contains(warning, required) {
			t.Fatalf("first warning omitted %q: %q", required, warning)
		}
	}

	warning, recovered = reporter.observeSuccessful(started.Add(unownedRunnerWarningInterval-time.Nanosecond), []gh.Runner{runner})
	if warning != "" || recovered != "" {
		t.Fatalf("unchanged observation before boundary warning = %q, recovered = %q", warning, recovered)
	}

	warning, recovered = reporter.observeSuccessful(started.Add(unownedRunnerWarningInterval), []gh.Runner{runner})
	if warning == "" || recovered != "" {
		t.Fatalf("boundary observation warning = %q, recovered = %q", warning, recovered)
	}
	for _, required := range []string{`observations=3`, `duration=30m0s`, `reason="periodic reminder"`} {
		if !strings.Contains(warning, required) {
			t.Fatalf("periodic warning omitted %q: %q", required, warning)
		}
	}

	warning, recovered = reporter.observeSuccessful(started.Add(31*time.Minute), nil)
	if warning != "" || !strings.Contains(recovered, `name="epar-test-stale" id=2450 observations=3 duration=31m0s`) {
		t.Fatalf("disappearance warning = %q, recovered = %q", warning, recovered)
	}
	state, found := reporter.observed[unownedRunnerIdentity{name: runner.Name, id: runner.ID}]
	if !found || state.present || state.episodeAnnounced {
		t.Fatalf("disappeared identity state = %#v, want retained cooldown history only", state)
	}

	warning, recovered = reporter.observeSuccessful(started.Add(32*time.Minute), nil)
	if warning != "" || recovered != "" {
		t.Fatalf("repeated absence warning = %q, recovered = %q", warning, recovered)
	}

	warning, recovered = reporter.observeSuccessful(started.Add(33*time.Minute), []gh.Runner{runner})
	if warning != "" || recovered != "" {
		t.Fatalf("suppressed reappearance warning = %q, recovered = %q", warning, recovered)
	}
	warning, recovered = reporter.observeSuccessful(started.Add(34*time.Minute), nil)
	if warning != "" || recovered != "" {
		t.Fatalf("unannounced episode disappearance warning = %q, recovered = %q", warning, recovered)
	}

	warning, recovered = reporter.observeSuccessful(started.Add(60*time.Minute), []gh.Runner{runner})
	if recovered != "" || !strings.Contains(warning, `observations=1 duration=0s reason="cooldown elapsed after reappearance"`) {
		t.Fatalf("post-cooldown reappearance warning = %q, recovered = %q", warning, recovered)
	}
	warning, recovered = reporter.observeSuccessful(started.Add(61*time.Minute), nil)
	if warning != "" || !strings.Contains(recovered, `observations=1 duration=1m0s`) {
		t.Fatalf("announced reappearance disappearance warning = %q, recovered = %q", warning, recovered)
	}
	warning, recovered = reporter.observeSuccessful(started.Add(90*time.Minute), nil)
	if warning != "" || recovered != "" || len(reporter.observed) != 0 {
		t.Fatalf("expired cooldown state = %#v, warning = %q, recovered = %q", reporter.observed, warning, recovered)
	}
}

func TestUnownedRunnerWarningReporterThrottlesStatusAndBusyFlapping(t *testing.T) {
	reporter := newUnownedRunnerWarningReporter()
	started := time.Unix(1_000, 0)
	runner := gh.Runner{Name: "epar-test-stale", ID: 10, Status: "offline"}
	warningCount := 0
	if warning, _ := reporter.observeSuccessful(started, []gh.Runner{runner}); warning != "" {
		warningCount++
	}

	for minute := 1; minute < 30; minute++ {
		if minute%2 == 0 {
			runner.Status = "offline"
			runner.Busy = false
		} else {
			runner.Status = "online"
			runner.Busy = true
		}
		warning, recovered := reporter.observeSuccessful(started.Add(time.Duration(minute)*time.Minute), []gh.Runner{runner})
		if warning != "" || recovered != "" {
			t.Fatalf("flapping minute %d warning = %q, recovered = %q", minute, warning, recovered)
		}
	}

	runner.Status = "online"
	runner.Busy = true
	warning, recovered := reporter.observeSuccessful(started.Add(unownedRunnerWarningInterval), []gh.Runner{runner})
	if warning != "" {
		warningCount++
	}
	if recovered != "" {
		t.Fatalf("periodic flapping recovery = %q", recovered)
	}
	for _, required := range []string{`status="online"`, `busy=true`, `observations=31`, `duration=30m0s`, `reason="periodic reminder"`} {
		if !strings.Contains(warning, required) {
			t.Fatalf("periodic flapping warning omitted %q: %q", required, warning)
		}
	}
	if warningCount != 2 {
		t.Fatalf("warning count = %d, want initial plus one periodic warning", warningCount)
	}
}

func TestUnownedRunnerWarningReporterTreatsNewIDAsNewAndResetsPerRun(t *testing.T) {
	started := time.Unix(2_000, 0)
	firstRun := newUnownedRunnerWarningReporter()
	firstRun.observeSuccessful(started, []gh.Runner{{Name: "epar-test-stale", ID: 10, Status: "offline"}})

	warning, recovered := firstRun.observeSuccessful(started.Add(time.Second), []gh.Runner{{Name: "epar-test-stale", ID: 11, Status: "offline"}})
	if !strings.Contains(warning, `id=11`) || !strings.Contains(warning, `reason="first observation"`) {
		t.Fatalf("new-id warning = %q", warning)
	}
	if !strings.Contains(recovered, `id=10`) {
		t.Fatalf("old-id recovery = %q", recovered)
	}

	secondRun := newUnownedRunnerWarningReporter()
	warning, recovered = secondRun.observeSuccessful(started.Add(2*time.Second), []gh.Runner{{Name: "epar-test-stale", ID: 11, Status: "offline"}})
	if warning == "" || recovered != "" || !strings.Contains(warning, `observations=1`) {
		t.Fatalf("new run warning = %q, recovered = %q", warning, recovered)
	}
}

func TestUnownedRunnerWarningReporterAggregatesDeterministicallyAndCapsDetails(t *testing.T) {
	reporter := newUnownedRunnerWarningReporter()
	runners := make([]gh.Runner, 0, 12)
	for index := 11; index >= 0; index-- {
		runners = append(runners, gh.Runner{Name: fmt.Sprintf("epar-test-%02d", index), ID: int64(100 + index), Status: "offline"})
	}

	warning, _ := reporter.observeSuccessful(time.Unix(3_000, 0), runners)
	if !strings.Contains(warning, "reporting 12 of 12 currently observed unowned GitHub runner registration(s)") {
		t.Fatalf("aggregate count warning = %q", warning)
	}
	if !strings.Contains(warning, "4 additional reportable registration(s) omitted") {
		t.Fatalf("capped warning = %q", warning)
	}
	if strings.Contains(warning, `name="epar-test-08"`) {
		t.Fatalf("warning exceeded detail cap: %q", warning)
	}
	previous := -1
	for index := 0; index < unownedRunnerWarningListLimit; index++ {
		position := strings.Index(warning, fmt.Sprintf(`name="epar-test-%02d"`, index))
		if position <= previous {
			t.Fatalf("warning details are not sorted at index %d: %q", index, warning)
		}
		previous = position
	}
}

func TestUnownedRunnerResolutionDoesNotAssertRemoteDeletionWhenLocalClassificationChanges(t *testing.T) {
	store, err := poolstate.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	name := "epar-test-stale"
	providerDouble := &fakeProvider{}
	githubDouble := &fakeGitHub{listRunners: []gh.Runner{{Name: name, ID: 2450, Status: "offline"}}}
	manager := newRegisteredTestManager(t, providerDouble, githubDouble)
	manager.LifecycleState = store
	current := time.Unix(3_500, 0)
	manager.now = func() time.Time { return current }
	var console bytes.Buffer
	runtime, err := logging.NewRuntime(logging.Options{Directory: t.TempDir(), ManagerSinks: logging.SinkConsole, Stdout: &console, Stderr: &console})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	manager.Logging = runtime
	reporter := newUnownedRunnerWarningReporter()

	if _, err := manager.reconcilePhysicalPoolWithReporter(context.Background(), nil, true, reporter); err != nil {
		t.Fatal(err)
	}
	console.Reset()
	providerDouble.instances = []provider.Instance{{Name: name, ProviderID: "foreign", State: "running"}}
	current = current.Add(time.Minute)
	if _, err := manager.reconcilePhysicalPoolWithReporter(context.Background(), nil, true, reporter); err != nil {
		t.Fatal(err)
	}
	output := console.String()
	for _, required := range []string{
		"no longer classified as prefix-only report-only",
		"does not assert that the GitHub registration was deleted",
	} {
		if !strings.Contains(output, required) {
			t.Fatalf("classification transition output omitted %q: %q", required, output)
		}
	}
	if got := atomic.LoadInt32(&githubDouble.deleteCalls); got != 0 {
		t.Fatalf("GitHub delete calls = %d, want 0", got)
	}
	if len(githubDouble.listRunners) != 1 || githubDouble.listRunners[0].ID != 2450 {
		t.Fatalf("test GitHub inventory changed: %#v", githubDouble.listRunners)
	}
}

func TestUnownedRunnerWarningsChangeOnlyAfterCompleteReconciliation(t *testing.T) {
	store, err := poolstate.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Reserve(context.Background(), poolstate.CreateSpec{Name: "other-pool-runner", ProviderType: "docker-container", GitHub: poolstate.GitHubIdentity{ExactName: "other-pool-runner"}}); err != nil {
		t.Fatal(err)
	}
	validState, err := os.ReadFile(store.Path())
	if err != nil {
		t.Fatal(err)
	}

	providerDouble := &fakeProvider{}
	githubDouble := &fakeGitHub{listRunners: []gh.Runner{{Name: "epar-test-stale", ID: 2450, Status: "offline"}}}
	manager := newRegisteredTestManager(t, providerDouble, githubDouble)
	manager.LifecycleState = store
	current := time.Unix(4_000, 0)
	manager.now = func() time.Time { return current }
	var console bytes.Buffer
	runtime, err := logging.NewRuntime(logging.Options{Directory: t.TempDir(), ManagerSinks: logging.SinkConsole, Stdout: &console, Stderr: &console})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	manager.Logging = runtime
	reporter := newUnownedRunnerWarningReporter()

	if _, err := manager.reconcilePhysicalPoolWithReporter(context.Background(), nil, true, reporter); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&githubDouble.deleteCalls); got != 0 {
		t.Fatalf("GitHub delete calls = %d, want 0 for prefix-only registration", got)
	}
	if len(reporter.observed) != 1 || !strings.Contains(console.String(), "prefix alone does not authorize deletion") {
		t.Fatalf("initial reporter state = %#v, output = %q", reporter.observed, console.String())
	}
	console.Reset()
	current = current.Add(time.Minute)
	if _, err := manager.reconcilePhysicalPoolWithReporter(context.Background(), nil, true, reporter); err != nil {
		t.Fatal(err)
	}
	if console.Len() != 0 {
		t.Fatalf("unchanged successful reconciliation logged %q", console.String())
	}
	for _, state := range reporter.observed {
		if state.observationCount != 2 {
			t.Fatalf("observation count = %d, want 2", state.observationCount)
		}
	}

	githubDouble.listErr = errors.New("GitHub unavailable")
	if _, err := manager.reconcilePhysicalPoolWithReporter(context.Background(), nil, true, reporter); err == nil {
		t.Fatal("API failure error = nil")
	}
	if len(reporter.observed) != 1 || console.Len() != 0 {
		t.Fatalf("API failure changed reporter state = %#v or logged %q", reporter.observed, console.String())
	}

	githubDouble.listErr = nil
	githubDouble.listFunc = func(context.Context) ([]gh.Runner, error) {
		if err := os.WriteFile(store.Path(), []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
		return append([]gh.Runner(nil), githubDouble.listRunners...), nil
	}
	if _, err := manager.reconcilePhysicalPoolWithReporter(context.Background(), nil, true, reporter); err == nil {
		t.Fatal("ownership failure error = nil")
	}
	if len(reporter.observed) != 1 || console.Len() != 0 {
		t.Fatalf("ownership failure changed reporter state = %#v or logged %q", reporter.observed, console.String())
	}
	if err := os.WriteFile(store.Path(), validState, 0o600); err != nil {
		t.Fatal(err)
	}
	githubDouble.listFunc = nil

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := manager.reconcilePhysicalPoolWithReporter(canceled, nil, true, reporter); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled reconciliation error = %v, want context.Canceled", err)
	}
	if len(reporter.observed) != 1 || console.Len() != 0 {
		t.Fatalf("cancellation changed reporter state = %#v or logged %q", reporter.observed, console.String())
	}

	githubDouble.listRunners = nil
	current = current.Add(time.Minute)
	if _, err := manager.reconcilePhysicalPoolWithReporter(context.Background(), nil, true, reporter); err != nil {
		t.Fatal(err)
	}
	if len(reporter.observed) != 1 || !strings.Contains(console.String(), "no longer classified as prefix-only report-only after a complete reconciliation") || !strings.Contains(console.String(), "does not assert that the GitHub registration was deleted") {
		t.Fatalf("successful disappearance state = %#v, output = %q", reporter.observed, console.String())
	}
	for _, state := range reporter.observed {
		if state.present || state.episodeAnnounced {
			t.Fatalf("disappearance retained active episode state = %#v", state)
		}
	}
	if got := atomic.LoadInt32(&githubDouble.deleteCalls); got != 0 {
		t.Fatalf("GitHub delete calls after reconciliation = %d, want 0", got)
	}
}
