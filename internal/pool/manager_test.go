package pool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
	gh "github.com/solutionforest/ephemeral-action-runner/internal/github"
	"github.com/solutionforest/ephemeral-action-runner/internal/hosttrust"
	"github.com/solutionforest/ephemeral-action-runner/internal/logging"
	poolstate "github.com/solutionforest/ephemeral-action-runner/internal/pool/state"
	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
)

func TestRunnerAliveKeepsBusyGitHubRunnerWithoutServiceCheck(t *testing.T) {
	provider := &fakeProvider{execErr: errors.New("inactive")}
	github := &fakeGitHub{
		runner: gh.Runner{Name: "epar-test-1", Status: "online", Busy: true},
		found:  true,
	}
	manager := Manager{Provider: provider, GitHub: github}

	alive, reason, err := manager.runnerAlive(context.Background(), ProvisionedInstance{Name: "epar-test-1"})
	if err != nil {
		t.Fatal(err)
	}
	if !alive {
		t.Fatalf("runnerAlive() alive = false, reason = %q", reason)
	}
	if got := atomic.LoadInt32(&provider.execCalls); got != 0 {
		t.Fatalf("service check ran %d time(s), want 0", got)
	}
}

func TestImageMaintenanceDrainRequiresConsecutiveIdleObservations(t *testing.T) {
	host := &fakeProvider{}
	github := &fakeGitHub{
		runner: gh.Runner{ID: 42, Name: "epar-test-1", Status: "online"},
		found:  true,
	}
	manager := Manager{Provider: host, GitHub: github}
	active := map[string]ProvisionedInstance{
		"epar-test-1": {Name: "epar-test-1", RunnerID: 42},
	}
	confirmedIdle := make(map[string]int)

	remaining, err := manager.drainPoolForImageUpdate(context.Background(), active, confirmedIdle)
	if err != nil {
		t.Fatal(err)
	}
	if remaining != 1 || atomic.LoadInt32(&github.deleteCalls) != 0 || atomic.LoadInt32(&host.deleteCalls) != 0 {
		t.Fatalf("first idle observation retired runner: remaining=%d remoteDeletes=%d localDeletes=%d", remaining, github.deleteCalls, host.deleteCalls)
	}

	github.runner.Busy = true
	remaining, err = manager.drainPoolForImageUpdate(context.Background(), active, confirmedIdle)
	if err != nil {
		t.Fatal(err)
	}
	if remaining != 1 || confirmedIdle["epar-test-1"] != 0 {
		t.Fatalf("busy observation did not reset idle confirmation: remaining=%d checks=%d", remaining, confirmedIdle["epar-test-1"])
	}

	github.runner.Busy = false
	if remaining, err = manager.drainPoolForImageUpdate(context.Background(), active, confirmedIdle); err != nil || remaining != 1 {
		t.Fatalf("idle confirmation after busy = remaining %d, error %v", remaining, err)
	}
	if remaining, err = manager.drainPoolForImageUpdate(context.Background(), active, confirmedIdle); err != nil || remaining != 0 {
		t.Fatalf("second consecutive idle confirmation = remaining %d, error %v", remaining, err)
	}
	if atomic.LoadInt32(&github.deleteCalls) != 1 || atomic.LoadInt32(&host.deleteCalls) != 1 {
		t.Fatalf("confirmed idle retirement calls = remote %d local %d, want 1 each", github.deleteCalls, host.deleteCalls)
	}
}

func TestRunnerGroupPreflightEnforcesAndWarns(t *testing.T) {
	violation := gh.RunnerGroupPolicyResult{Violations: []string{"runner group allows public repositories"}}
	for _, test := range []struct {
		name        string
		enforcement string
		result      gh.RunnerGroupPolicyResult
		apiErr      error
		wantErr     bool
	}{
		{name: "enforce violation", enforcement: config.RunnerGroupEnforcementEnforce, result: violation, wantErr: true},
		{name: "warn violation", enforcement: config.RunnerGroupEnforcementWarn, result: violation},
		{name: "enforce API failure", enforcement: config.RunnerGroupEnforcementEnforce, apiErr: errors.New("forbidden"), wantErr: true},
		{name: "warn API failure", enforcement: config.RunnerGroupEnforcementWarn, apiErr: errors.New("forbidden")},
		{name: "enforce allowed", enforcement: config.RunnerGroupEnforcementEnforce, result: gh.RunnerGroupPolicyResult{Resolved: true, Group: gh.RunnerGroup{Name: "restricted"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.Runner.Group = "restricted"
			cfg.Security.RunnerGroup.Enforcement = test.enforcement
			github := &fakeGitHub{policyResult: test.result, policyErr: test.apiErr}
			manager := Manager{Config: cfg, GitHub: github}
			err := manager.PreflightRunnerGroup(context.Background())
			if (err != nil) != test.wantErr {
				t.Fatalf("PreflightRunnerGroup() error = %v, wantErr=%t", err, test.wantErr)
			}
			if got := atomic.LoadInt32(&github.policyCalls); got != 1 {
				t.Fatalf("policy calls = %d, want 1", got)
			}
			if got := atomic.LoadInt32(&github.registrationCalls); got != 0 {
				t.Fatalf("registration-token calls = %d, want 0", got)
			}
			if got := atomic.LoadInt32(&github.deleteCalls); got != 0 {
				t.Fatalf("runner delete calls = %d, want 0", got)
			}
		})
	}
}

func TestRegistrationPreflightRejectsBeforeTokenRequest(t *testing.T) {
	provider := &fakeProvider{ip: "127.0.0.1"}
	github := &fakeGitHub{policyResult: gh.RunnerGroupPolicyResult{Violations: []string{"unsafe group"}}}
	manager := newRegisteredTestManager(t, provider, github)
	manager.Config.Security = config.Default().Security
	manager.Config.Security.RunnerGroup.Enforcement = config.RunnerGroupEnforcementEnforce
	_, err := manager.provisionOne(context.Background(), "epar-test-policy", true, false)
	if err == nil || !strings.Contains(err.Error(), "unsafe group") {
		t.Fatalf("provisionOne() error = %v, want policy rejection", err)
	}
	if got := atomic.LoadInt32(&github.policyCalls); got != 1 {
		t.Fatalf("policy calls = %d, want 1", got)
	}
	if got := atomic.LoadInt32(&github.registrationCalls); got != 0 {
		t.Fatalf("registration-token calls = %d, want 0", got)
	}
	if got := atomic.LoadInt32(&github.deleteCalls); got != 0 {
		t.Fatalf("remote runner delete calls = %d, want 0", got)
	}
}

func TestRetiredInstanceTranscriptsBecomeRetentionEligibleWhileLiveInstanceStaysProtected(t *testing.T) {
	root := t.TempDir()
	runtime, err := logging.NewRuntime(logging.Options{Directory: root, TranscriptSinks: logging.SinkFile})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	manager := Manager{
		Config: config.Config{
			Logging:  config.LoggingConfig{Directory: root},
			Provider: config.ProviderConfig{Type: "docker-container"},
		},
		ProjectRoot: root,
		Logging:     runtime,
	}
	retired := ProvisionedInstance{
		Name:         "retired-runner",
		LogPath:      filepath.Join(root, "instances", "retired-runner.docker-container.log"),
		GuestLogPath: filepath.Join(root, "instances", "retired-runner.guest.log"),
	}
	livePath := filepath.Join(root, "instances", "live-runner.guest.log")
	for _, item := range []struct {
		path      string
		instance  string
		component string
	}{
		{retired.LogPath, retired.Name, "provider"},
		{retired.GuestLogPath, retired.Name, "guest"},
		{livePath, "live-runner", "guest"},
	} {
		transcript, err := manager.transcript(item.path, item.instance, item.component)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := transcript.Stdout.Write([]byte("old\n")); err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-48 * time.Hour)
		if err := os.Chtimes(item.path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	if err := manager.releaseInstanceTranscripts(retired); err != nil {
		t.Fatal(err)
	}
	report, err := logging.PruneRetention(root, logging.RetentionPolicy{InstanceMaxAge: time.Hour}, false)
	if err != nil {
		t.Fatal(err)
	}
	if report.Deleted != 2 {
		t.Fatalf("deleted = %d, report = %#v", report.Deleted, report)
	}
	for _, path := range []string{retired.LogPath, retired.GuestLogPath} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("retired transcript %s was not deleted: %v", path, err)
		}
	}
	if _, err := os.Stat(livePath); err != nil {
		t.Fatalf("live transcript was not protected: %v", err)
	}
}

func TestRetirementSuccessIsNotReversedByTranscriptCloseFailure(t *testing.T) {
	root := t.TempDir()
	runtime, err := logging.NewRuntime(logging.Options{Directory: root, TranscriptSinks: logging.SinkFile})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	provider := &fakeProvider{}
	manager := Manager{
		Config: config.Config{
			Logging:  config.LoggingConfig{Directory: root},
			Provider: config.ProviderConfig{Type: "docker-container"},
		},
		Provider:    provider,
		ProjectRoot: root,
		Logging:     runtime,
	}
	vm := ProvisionedInstance{Name: "retired-runner", LogPath: filepath.Join(root, "instances", "retired-runner.docker-container.log")}
	transcript, err := manager.transcript(vm.LogPath, vm.Name, "provider")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transcript.Stdout.Write([]byte("line\n")); err != nil {
		t.Fatal(err)
	}
	metadataFiles, err := filepath.Glob(filepath.Join(root, ".epar-control", "active", "*.json"))
	if err != nil || len(metadataFiles) != 1 {
		t.Fatalf("active metadata = %v, err = %v", metadataFiles, err)
	}
	data, err := os.ReadFile(metadataFiles[0])
	if err != nil {
		t.Fatal(err)
	}
	var state map[string]any
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	state["ownerToken"] = "changed-owner"
	data, err = json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metadataFiles[0], data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := manager.retireInstance(context.Background(), vm, "test"); err != nil {
		t.Fatalf("retireInstance returned transcript close failure after successful provider deletion: %v", err)
	}
	if got := atomic.LoadInt32(&provider.deleteCalls); got != 1 {
		t.Fatalf("provider delete calls = %d, want 1", got)
	}
}

func TestRunnerAliveRetiresIdleRunnerWhenServiceIsInactive(t *testing.T) {
	provider := &fakeProvider{execFunc: func(_ context.Context, _ string, command []string, _ provider.ExecOptions) (provider.ExecResult, error) {
		if strings.Contains(strings.Join(command, " "), runnerProcessRunningSentinel) {
			return provider.ExecResult{Stdout: runnerProcessStoppedSentinel + "\n"}, nil
		}
		return provider.ExecResult{}, nil
	}}
	github := &fakeGitHub{
		runner: gh.Runner{Name: "epar-test-1", Status: "online", Busy: false},
		found:  true,
	}
	manager := Manager{Provider: provider, GitHub: github}

	alive, reason, err := manager.runnerAlive(context.Background(), ProvisionedInstance{Name: "epar-test-1"})
	if err != nil {
		t.Fatal(err)
	}
	if alive {
		t.Fatal("runnerAlive() alive = true, want false")
	}
	if reason != runnerProcessInactiveReason {
		t.Fatalf("reason = %q", reason)
	}
	if got := atomic.LoadInt32(&provider.execCalls); got != 1 {
		t.Fatalf("service check ran %d time(s), want 1", got)
	}
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if len(provider.execOptions) != 1 || !provider.execOptions[0].SuppressTranscript {
		t.Fatal("machine-readable health probe was not excluded from the guest transcript")
	}
}

func TestRunnerAlivePreservesRunnerWhenProcessProbeIsUnavailable(t *testing.T) {
	provider := &fakeProvider{execErr: context.DeadlineExceeded}
	github := &fakeGitHub{
		runner: gh.Runner{Name: "epar-test-1", Status: "online", Busy: false},
		found:  true,
	}
	manager := Manager{Provider: provider, GitHub: github}

	alive, reason, err := manager.runnerAlive(context.Background(), ProvisionedInstance{Name: "epar-test-1"})
	if err == nil {
		t.Fatal("runnerAlive() error = nil, want unavailable process-probe error")
	}
	if !alive {
		t.Fatalf("runnerAlive() alive = false, reason = %q; an unavailable probe must preserve the runner", reason)
	}
	if got := atomic.LoadInt32(&provider.deleteCalls); got != 0 {
		t.Fatalf("provider delete calls = %d, want 0", got)
	}
}

func TestRunnerProcessProbeRejectsUnsupportedOutput(t *testing.T) {
	provider := &fakeProvider{execFunc: func(context.Context, string, []string, provider.ExecOptions) (provider.ExecResult, error) {
		return provider.ExecResult{Stdout: "unexpected\n"}, nil
	}}
	manager := Manager{Provider: provider}
	if _, err := manager.probeRunnerProcess(context.Background(), "epar-test-1"); err == nil || !strings.Contains(err.Error(), "unsupported response") {
		t.Fatalf("probeRunnerProcess() error = %v, want unsupported-response error", err)
	}
}

func TestRunnerLivenessRequiresConsecutiveConfirmedInactiveProbes(t *testing.T) {
	counts := make(map[string]int)
	if count, retire := recordRunnerLiveness(counts, "runner-1", false, runnerProcessInactiveReason, nil); count != 1 || retire {
		t.Fatalf("first confirmed inactive probe = count %d retire %t, want 1 false", count, retire)
	}
	if count, retire := recordRunnerLiveness(counts, "runner-1", true, "", context.DeadlineExceeded); count != 0 || retire {
		t.Fatalf("unavailable probe = count %d retire %t, want 0 false", count, retire)
	}
	if count, retire := recordRunnerLiveness(counts, "runner-1", false, runnerProcessInactiveReason, nil); count != 1 || retire {
		t.Fatalf("first probe after uncertainty = count %d retire %t, want 1 false", count, retire)
	}
	if count, retire := recordRunnerLiveness(counts, "runner-1", false, runnerProcessInactiveReason, nil); count != 2 || !retire {
		t.Fatalf("second consecutive confirmed inactive probe = count %d retire %t, want 2 true", count, retire)
	}
	if count, retire := recordRunnerLiveness(counts, "runner-2", false, "GitHub runner record is gone", nil); count != 0 || !retire {
		t.Fatalf("authoritative remote absence = count %d retire %t, want 0 true", count, retire)
	}
}

func TestRunnerAliveFallsBackToServiceCheckWhenGitHubLivenessHasServerError(t *testing.T) {
	provider := &fakeProvider{}
	github := &fakeGitHub{
		runnerErr: &gh.HTTPError{StatusCode: 500},
	}
	manager := Manager{Provider: provider, GitHub: github}

	alive, reason, err := manager.runnerAlive(context.Background(), ProvisionedInstance{Name: "epar-test-1"})
	if err != nil {
		t.Fatal(err)
	}
	if !alive {
		t.Fatalf("runnerAlive() alive = false, reason = %q", reason)
	}
	if got := atomic.LoadInt32(&provider.execCalls); got != 1 {
		t.Fatalf("service check ran %d time(s), want 1", got)
	}
}

func TestRunnerAliveReturnsNonTransientGitHubLivenessError(t *testing.T) {
	provider := &fakeProvider{}
	github := &fakeGitHub{
		runnerErr: &gh.HTTPError{StatusCode: 403},
	}
	manager := Manager{Provider: provider, GitHub: github}

	alive, reason, err := manager.runnerAlive(context.Background(), ProvisionedInstance{Name: "epar-test-1"})
	if err == nil {
		t.Fatal("runnerAlive() error = nil, want GitHub error")
	}
	if !alive {
		t.Fatalf("runnerAlive() alive = false, reason = %q", reason)
	}
	if got := atomic.LoadInt32(&provider.execCalls); got != 0 {
		t.Fatalf("service check ran %d time(s), want 0", got)
	}
}

func TestRetireInstanceDefersLocalDeleteWhenGitHubDeleteFails(t *testing.T) {
	provider := &fakeProvider{}
	github := &fakeGitHub{deleteErr: errors.New("github runner is currently running a job")}
	manager := Manager{Provider: provider, GitHub: github}

	err := manager.retireInstance(context.Background(), ProvisionedInstance{Name: "epar-test-1", RunnerID: 123}, "done")
	if err == nil {
		t.Fatal("retireInstance() error = nil, want GitHub delete error")
	}
	if got := atomic.LoadInt32(&provider.stopCalls); got != 0 {
		t.Fatalf("Stop called %d time(s), want 0", got)
	}
	if got := atomic.LoadInt32(&provider.deleteCalls); got != 0 {
		t.Fatalf("Delete called %d time(s), want 0", got)
	}
}

func TestRunPoolDoesNotReplaceWhenRetirementIsDeferred(t *testing.T) {
	provider := &fakeProvider{ip: "127.0.0.1"}
	github := &fakeGitHub{
		runner:     gh.Runner{Name: "epar-test-1", ID: 123, Status: "offline"},
		found:      true,
		waitRunner: gh.Runner{Name: "epar-test-1", ID: 123, Status: "online"},
		deleteErr:  errors.New("github runner is currently running a job"),
	}
	manager := Manager{
		Config: config.Config{
			Provider: config.ProviderConfig{SourceImage: "image"},
			Pool:     config.PoolConfig{Instances: 1, NamePrefix: "epar-test"},
			Logging:  config.LoggingConfig{Directory: t.TempDir()},
			Runner:   config.RunnerConfig{Labels: []string{"self-hosted"}},
		},
		Provider:    provider,
		GitHub:      github,
		ProjectRoot: t.TempDir(),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()

	if err := manager.RunPool(ctx, RunOptions{
		Instances:        1,
		Register:         true,
		KeepOnExit:       true,
		ReplaceCompleted: true,
		MonitorInterval:  5 * time.Millisecond,
		PoolLockHeld:     true,
	}); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&provider.cloneCalls); got != 1 {
		t.Fatalf("Clone called %d time(s), want 1; deferred retirement should not create replacements", got)
	}
}

func TestRunPoolReplacesCompletedRunnerAfterBusyProvisioning(t *testing.T) {
	provider := &fakeProvider{ip: "127.0.0.1"}
	github := &fakeGitHub{
		waitRunner: gh.Runner{Name: "epar-test-1", ID: 123, Status: "online", Busy: true},
	}
	manager := Manager{
		Config: config.Config{
			Provider: config.ProviderConfig{SourceImage: "image"},
			Pool:     config.PoolConfig{Instances: 1, NamePrefix: "epar-test"},
			Logging:  config.LoggingConfig{Directory: t.TempDir()},
			Runner:   config.RunnerConfig{Labels: []string{"self-hosted"}, Ephemeral: true},
			Security: config.Default().Security,
		},
		Provider:    provider,
		GitHub:      github,
		ProjectRoot: t.TempDir(),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()

	if err := manager.RunPool(ctx, RunOptions{
		Instances:        1,
		Register:         true,
		KeepOnExit:       true,
		ReplaceCompleted: true,
		MonitorInterval:  5 * time.Millisecond,
		PoolLockHeld:     true,
	}); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&provider.cloneCalls); got < 2 {
		t.Fatalf("Clone called %d time(s), want a replacement after the initially busy ephemeral runner disappeared", got)
	}
	if got := atomic.LoadInt32(&provider.deleteCalls); got < 1 {
		t.Fatalf("Delete called %d time(s), want completed runner instance retired", got)
	}
	if got := atomic.LoadInt32(&github.waitOnlineCalls); got < 2 {
		t.Fatalf("WaitRunnerOnline called %d time(s), want initial busy runner and replacement", got)
	}
	if got := atomic.LoadInt32(&github.waitOnlineIdleCalls); got != 0 {
		t.Fatalf("WaitRunnerOnlineIdle called %d time(s), want supervised pool to accept busy runners", got)
	}
}

func TestRunPoolFailsClosedWhenReplacementHostTrustActivationFails(t *testing.T) {
	const activationFailure = "activate provider host-trust runtime: execute in docker sandbox failed: exit status 1: EPAR host-trust relay: activation failed at private-dockerd-contract (exit=1)"
	snapshot := hosttrust.Snapshot{
		Generation:   "g1",
		HostOS:       "windows",
		Scopes:       []string{"system"},
		Certificates: []hosttrust.Certificate{{Name: "root.crt", PEM: []byte("pem")}},
		CollectedAt:  time.Now().UTC(),
	}
	fake := &fakeProvider{ip: "127.0.0.1"}
	marker, err := hostTrustMarkerJSON(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	fake.execFunc = func(_ context.Context, _ string, command []string, _ provider.ExecOptions) (provider.ExecResult, error) {
		commandText := strings.Join(command, " ")
		if commandText == "cat "+hostTrustMarkerGuest {
			return provider.ExecResult{Stdout: string(marker)}, nil
		}
		if strings.Contains(commandText, runnerProcessRunningSentinel) {
			return provider.ExecResult{Stdout: runnerProcessRunningSentinel + "\n"}, nil
		}
		return provider.ExecResult{}, nil
	}
	github := &fakeGitHub{
		waitRunner: gh.Runner{Name: "epar-test-1", ID: 123, Status: "online", Busy: true},
	}
	activator := &activatingLifecycle{Lifecycle: provider.AdaptLegacy(fake, false)}
	activator.onActivate = func(provider.Instance) {
		if activator.calls == 2 {
			activator.err = errors.New(activationFailure)
		}
	}
	manager := newRegisteredTestManager(t, fake, github)
	manager.Config.Provider.Type = "docker-sandboxes"
	manager.Config.Image.HostTrustMode = config.HostTrustModeOverlay
	manager.Config.Image.HostTrustScopes = []string{"system"}
	manager.AllowInsufficientStorage = true
	manager.Lifecycle = activator
	manager.hostTrustResolver = func(context.Context) (hosttrust.Snapshot, error) { return snapshot, nil }
	manager.hostTrustImageEnsurer = func(context.Context) error { return nil }
	state, err := poolstate.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manager.LifecycleState = state

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err = manager.RunPool(ctx, RunOptions{
		Instances:         1,
		Register:          true,
		ReplaceCompleted:  true,
		MonitorInterval:   5 * time.Millisecond,
		PoolLockHeld:      true,
		HostTrustLockHeld: true,
	})
	if err == nil || !strings.Contains(err.Error(), "private-dockerd-contract") {
		t.Fatalf("RunPool() error = %v, want terminal host-trust stage failure", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("RunPool() timed out instead of returning the replacement failure: %v", err)
	}
	if isTransientDependencyError(err) {
		t.Fatalf("replacement failure was classified transient: %v", err)
	}
	if activator.calls != 2 {
		t.Fatalf("host-trust activation calls = %d, want initial activation and one failed replacement", activator.calls)
	}
	if got := atomic.LoadInt32(&fake.cloneCalls); got != 2 {
		t.Fatalf("Clone calls = %d, want exactly two candidates and no retry storm", got)
	}
	if got := atomic.LoadInt32(&github.registrationCalls); got != 1 {
		t.Fatalf("registration token calls = %d, want only the initial runner registered", got)
	}
	if got := atomic.LoadInt32(&fake.deleteCalls); got != 2 {
		t.Fatalf("provider delete calls = %d, want retired runner and failed replacement", got)
	}
	fake.mu.Lock()
	deletedNames := append([]string(nil), fake.deletedNames...)
	remaining := append([]provider.Instance(nil), fake.instances...)
	fake.mu.Unlock()
	if len(remaining) != 0 {
		t.Fatalf("provider inventory after terminal cleanup = %#v, want empty", remaining)
	}
	if len(activator.instances) < 2 {
		t.Fatalf("activated instances = %#v, want the failed replacement candidate", activator.instances)
	}
	replacementName := activator.instances[1].Name
	deletedReplacement := false
	for _, deletedName := range deletedNames {
		if deletedName == replacementName {
			deletedReplacement = true
			break
		}
	}
	if !deletedReplacement {
		t.Fatalf("deleted provider names = %v, want exact failed replacement %q", deletedNames, replacementName)
	}
	records, err := state.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 {
		t.Fatalf("lifecycle records = %#v, want initial and replacement tombstones", records)
	}
	for _, record := range records {
		if record.Phase != poolstate.PhaseTombstoned || !strings.HasPrefix(record.ProviderID, "fake:") {
			t.Fatalf("lifecycle record = %#v, want exact tombstoned provider identity", record)
		}
	}
}

func TestRunPoolAddsCurrentTrustCapacityWhileOldGenerationDrains(t *testing.T) {
	fake := &fakeProvider{ip: "127.0.0.1"}
	github := &fakeGitHub{
		runner:     gh.Runner{Name: "epar-test-1", ID: 123, Status: "online", Busy: true},
		found:      true,
		waitRunner: gh.Runner{Name: "epar-test-1", ID: 123, Status: "online", Busy: true},
	}
	manager := Manager{
		Config: config.Config{
			Provider: config.ProviderConfig{SourceImage: "image", Type: "docker-container"},
			Pool:     config.PoolConfig{Instances: 1, NamePrefix: "epar-test"},
			Logging:  config.LoggingConfig{Directory: t.TempDir()},
			Runner:   config.RunnerConfig{Labels: []string{"self-hosted"}, Ephemeral: true},
			Image: config.ImageConfig{
				HostTrustMode: config.HostTrustModeOverlay, HostTrustScopes: []string{"system"},
			},
		},
		Provider:    fake,
		GitHub:      github,
		ProjectRoot: t.TempDir(),
	}
	snapshot := func(generation string) hosttrust.Snapshot {
		return hosttrust.Snapshot{
			Generation: generation, HostOS: "linux", Scopes: []string{"system"},
			Certificates: []hosttrust.Certificate{{Name: "root.crt", PEM: []byte("pem")}},
			CollectedAt:  time.Now().UTC(),
		}
	}
	manager.hostTrustResolver = func(context.Context) (hosttrust.Snapshot, error) {
		if atomic.LoadInt32(&github.waitOnlineCalls)+atomic.LoadInt32(&github.waitOnlineIdleCalls) == 0 {
			return snapshot("g1"), nil
		}
		return snapshot("g2"), nil
	}
	var imageEnsures int32
	manager.hostTrustImageEnsurer = func(context.Context) error {
		atomic.AddInt32(&imageEnsures, 1)
		return nil
	}
	fake.execFunc = func(_ context.Context, _ string, command []string, _ provider.ExecOptions) (provider.ExecResult, error) {
		if strings.Contains(strings.Join(command, " "), hostTrustMarkerGuest) {
			generation := "g1"
			if atomic.LoadInt32(&fake.cloneCalls) >= 2 {
				generation = "g2"
			}
			marker := fmt.Sprintf(`{"schemaVersion":1,"generation":%q,"hostOS":"linux","mode":"overlay","scopes":["system"],"certificateCount":1}`, generation)
			return provider.ExecResult{Stdout: marker}, nil
		}
		return provider.ExecResult{}, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	if err := manager.RunPool(ctx, RunOptions{
		Instances: 1, Register: true, KeepOnExit: true, ReplaceCompleted: false, MonitorInterval: 5 * time.Millisecond, HostTrustLockHeld: true, PoolLockHeld: true,
	}); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&fake.cloneCalls); got != 1 {
		t.Fatalf("Clone called %d time(s), want strict physical cap while busy G1 drains", got)
	}
	if got := atomic.LoadInt32(&fake.deleteCalls); got != 0 {
		t.Fatalf("busy G1 was deleted %d time(s), want it left draining", got)
	}
	if got := atomic.LoadInt32(&imageEnsures); got == 0 {
		t.Fatal("G2 replacement image was not ensured")
	}
}

func TestRunPoolRefreshesHostTrustTransportOnLeaseCadence(t *testing.T) {
	snapshot := hosttrust.Snapshot{Generation: "g1", HostOS: "windows", Scopes: []string{"system"}, Certificates: []hosttrust.Certificate{{Name: "root.crt", PEM: []byte("pem")}}, CollectedAt: time.Now().UTC()}
	marker, err := hostTrustMarkerJSON(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeProvider{ip: "127.0.0.1"}
	fake.execFunc = func(_ context.Context, _ string, command []string, _ provider.ExecOptions) (provider.ExecResult, error) {
		if strings.Join(command, " ") == "cat "+hostTrustMarkerGuest {
			return provider.ExecResult{Stdout: string(marker)}, nil
		}
		if strings.Contains(strings.Join(command, " "), runnerProcessRunningSentinel) {
			return provider.ExecResult{Stdout: runnerProcessRunningSentinel + "\n"}, nil
		}
		return provider.ExecResult{}, nil
	}
	github := &fakeGitHub{
		runner:     gh.Runner{Name: "epar-test-1", ID: 123, Status: "online"},
		found:      true,
		waitRunner: gh.Runner{Name: "epar-test-1", ID: 123, Status: "online"},
	}
	activation := make(chan struct{}, 4)
	activator := &activatingLifecycle{Lifecycle: provider.AdaptLegacy(fake, false), onActivate: func(provider.Instance) { activation <- struct{}{} }}
	manager := newRegisteredTestManager(t, fake, github)
	manager.Config.Image.HostTrustMode = config.HostTrustModeOverlay
	manager.Config.Image.HostTrustScopes = []string{"system"}
	manager.Lifecycle = activator
	manager.hostTrustResolver = func(context.Context) (hosttrust.Snapshot, error) { return snapshot, nil }
	manager.hostTrustImageEnsurer = func(context.Context) error { return nil }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- manager.RunPool(ctx, RunOptions{Instances: 1, Register: true, KeepOnExit: true, ReplaceCompleted: true, MonitorInterval: 5 * time.Millisecond, HostTrustLockHeld: true, PoolLockHeld: true})
	}()
	for i := 0; i < 2; i++ {
		select {
		case <-activation:
		case <-time.After(2 * time.Second):
			cancel()
			t.Fatalf("host trust activation %d did not occur", i+1)
		}
	}
	select {
	case <-activation:
		cancel()
		t.Fatal("host trust transport was refreshed again before the lease refresh cadence elapsed")
	case <-time.After(25 * time.Millisecond):
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestRunPoolCancellationDuringLivenessSkipsTransientWarningAndCleansUp(t *testing.T) {
	provider := &fakeProvider{ip: "127.0.0.1"}
	github := &fakeGitHub{
		runner: gh.Runner{ID: 123, Status: "online"},
		found:  true,
	}
	github.waitFunc = func(_ context.Context, name string, _ time.Duration) (gh.Runner, error) {
		return gh.Runner{Name: name, ID: 123, Status: "online"}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	github.runnerByNameFunc = func(_ context.Context, name string) (gh.Runner, bool, error) {
		if atomic.LoadInt32(&github.runnerByNameCalls) == 1 {
			cancel()
			return gh.Runner{}, false, context.Canceled
		}
		return gh.Runner{Name: name, ID: 123, Status: "online"}, true, nil
	}
	manager := newRegisteredTestManager(t, provider, github)
	var console bytes.Buffer
	runtime, err := logging.NewRuntime(logging.Options{Directory: manager.Config.Logging.Directory, ManagerSinks: logging.SinkConsole, Stdout: &console, Stderr: &console})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	manager.Logging = runtime

	if err := manager.RunPool(ctx, RunOptions{Instances: 1, Register: true, ReplaceCompleted: true, MonitorInterval: 5 * time.Millisecond, PoolLockHeld: true}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(console.String(), "runner health is temporarily unknown") {
		t.Fatalf("shutdown cancellation emitted a transient health warning: %q", console.String())
	}
	if got := atomic.LoadInt32(&provider.deleteCalls); got != 1 {
		t.Fatalf("provider cleanup calls = %d, want 1", got)
	}
	if !strings.Contains(console.String(), "Cleanup complete. EPAR can now exit safely") {
		t.Fatalf("shutdown cancellation did not complete exact cleanup: %q", console.String())
	}
}

func TestVerifyUsesIdleReadiness(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	provider := &fakeProvider{ip: "127.0.0.1"}
	github := &fakeGitHub{
		waitRunner: gh.Runner{Name: "epar-test-1", ID: 123, Status: "online"},
	}
	manager := Manager{
		Config: config.Config{
			Provider: config.ProviderConfig{SourceImage: "image", Type: "docker-container"},
			Pool:     config.PoolConfig{Instances: 1, NamePrefix: "epar-test"},
			Logging:  config.LoggingConfig{Directory: t.TempDir()},
			Runner:   config.RunnerConfig{Labels: []string{"self-hosted"}, Ephemeral: true},
		},
		Provider:    provider,
		GitHub:      github,
		ProjectRoot: t.TempDir(),
	}

	if err := manager.Verify(context.Background(), VerifyOptions{Instances: 1, RegisterOnly: true}); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&github.waitOnlineIdleCalls); got != 1 {
		t.Fatalf("WaitRunnerOnlineIdle called %d time(s), want verification to require an idle runner", got)
	}
	if got := atomic.LoadInt32(&github.waitOnlineCalls); got != 0 {
		t.Fatalf("WaitRunnerOnline called %d time(s), verification must not accept a busy runner", got)
	}
}

func TestRunPoolUsesConfiguredInstancesWhenNoOverride(t *testing.T) {
	provider := &fakeProvider{ip: "127.0.0.1"}
	manager := Manager{
		Config: config.Config{
			Provider: config.ProviderConfig{SourceImage: "image"},
			Pool:     config.PoolConfig{Instances: 2, NamePrefix: "epar-test"},
			Logging:  config.LoggingConfig{Directory: t.TempDir()},
		},
		Provider:    provider,
		ProjectRoot: t.TempDir(),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	if err := manager.RunPool(ctx, RunOptions{
		Instances:        0,
		Register:         false,
		KeepOnExit:       true,
		ReplaceCompleted: false,
		PoolLockHeld:     true,
	}); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&provider.cloneCalls); got != 2 {
		t.Fatalf("Clone called %d time(s), want configured instances 2", got)
	}
}

func TestProvisionOneRetriesTransientRuntimeValidationFailure(t *testing.T) {
	oldDelay := runtimeValidationRetryDelay
	runtimeValidationRetryDelay = 0
	t.Cleanup(func() { runtimeValidationRetryDelay = oldDelay })

	provider := &fakeProvider{
		ip:       "127.0.0.1",
		execErrs: []error{errors.New("transient validation failure"), nil},
	}
	manager := Manager{
		Config: config.Config{
			Provider: config.ProviderConfig{SourceImage: "image", Type: "docker-container"},
			Pool:     config.PoolConfig{Instances: 1, NamePrefix: "epar-test"},
			Logging:  config.LoggingConfig{Directory: t.TempDir()},
			Timeouts: config.TimeoutConfig{CommandSeconds: 5},
		},
		Provider:    provider,
		ProjectRoot: t.TempDir(),
	}

	if _, err := manager.provisionOne(context.Background(), "epar-test-1", false, false); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&provider.execCalls); got != 2 {
		t.Fatalf("runtime validation attempts = %d, want 2", got)
	}
}

func TestVerifyCleanupUsesFreshContextAfterCancellation(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	provider := &fakeProvider{
		instances: []provider.Instance{
			{Name: "epar-test-1"},
			{Name: "epar-test-unrelated"},
			{Name: "epar-testing-1"},
		},
	}
	manager := Manager{
		Config: config.Config{
			Provider: config.ProviderConfig{SourceImage: "image", Type: "docker-container"},
			Pool:     config.PoolConfig{Instances: 1, NamePrefix: "epar-test"},
			Logging:  config.LoggingConfig{Directory: t.TempDir()},
			Timeouts: config.TimeoutConfig{CommandSeconds: 5},
		},
		Provider:    provider,
		ProjectRoot: t.TempDir(),
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := manager.Verify(ctx, VerifyOptions{Instances: 1, Cleanup: true}); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&provider.canceledListCalls); got != 0 {
		t.Fatalf("cleanup List received a canceled context %d time(s)", got)
	}
	if got := atomic.LoadInt32(&provider.stopCalls); got != 3 {
		t.Fatalf("Stop called %d time(s), want 3 matching prefix-boundary instances including the verified candidate", got)
	}
	if got := atomic.LoadInt32(&provider.deleteCalls); got != 3 {
		t.Fatalf("Delete called %d time(s), want 3 matching prefix-boundary instances including the verified candidate", got)
	}
}

func TestRunPoolCleanupUsesFreshContextAfterCancellation(t *testing.T) {
	provider := &fakeProvider{
		instances: []provider.Instance{{Name: "epar-test-1"}},
	}
	manager := Manager{
		Config: config.Config{
			Provider: config.ProviderConfig{SourceImage: "image", Type: "docker-container"},
			Pool:     config.PoolConfig{Instances: 1, NamePrefix: "epar-test"},
			Logging:  config.LoggingConfig{Directory: t.TempDir()},
			Timeouts: config.TimeoutConfig{CommandSeconds: 5},
		},
		Provider:    provider,
		ProjectRoot: t.TempDir(),
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := manager.RunPool(ctx, RunOptions{Instances: 1, PoolLockHeld: true}); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&provider.canceledListCalls); got != 0 {
		t.Fatalf("cleanup List received a canceled context %d time(s)", got)
	}
	if got := atomic.LoadInt32(&provider.stopCalls); got != 1 {
		t.Fatalf("Stop called %d time(s), want 1", got)
	}
	if got := atomic.LoadInt32(&provider.deleteCalls); got != 1 {
		t.Fatalf("Delete called %d time(s), want 1", got)
	}
}

func TestProvisionOnePassesRunnerRegistrationControlsWithoutPrivateKey(t *testing.T) {
	provider := &fakeProvider{ip: "127.0.0.1"}
	github := &fakeGitHub{
		waitRunner: gh.Runner{Name: "epar-test-1", ID: 123, Status: "online"},
	}
	manager := Manager{
		Config: config.Config{
			GitHub: config.GitHubConfig{PrivateKeyPath: "/secret/app.pem"},
			Provider: config.ProviderConfig{
				SourceImage: "image",
				Type:        "docker-container",
			},
			Pool: config.PoolConfig{
				Instances:  1,
				NamePrefix: "epar-test",
			},
			Logging: config.LoggingConfig{Directory: t.TempDir()},
			Runner: config.RunnerConfig{
				Labels:          []string{"epar-core-test"},
				Ephemeral:       true,
				Group:           "epar-ci-canary",
				NoDefaultLabels: true,
			},
			Timeouts: config.TimeoutConfig{
				CommandSeconds:      5,
				GitHubOnlineSeconds: 5,
			},
		},
		Provider:    provider,
		GitHub:      github,
		ProjectRoot: t.TempDir(),
	}

	if _, err := manager.provisionOne(context.Background(), "epar-test-1", true, false); err != nil {
		t.Fatal(err)
	}
	provider.mu.Lock()
	env := provider.configureEnv
	provider.mu.Unlock()
	if env == nil {
		t.Fatal("configure-runner invocation was not captured")
	}
	for key, want := range map[string]string{
		"RUNNER_GROUP":             "epar-ci-canary",
		"RUNNER_NO_DEFAULT_LABELS": "true",
		"RUNNER_LABELS":            "epar-core-test",
		"RUNNER_EPHEMERAL":         "true",
	} {
		if got := env[key]; got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	provider.mu.Lock()
	configureOptions := provider.configureOptions
	provider.mu.Unlock()
	if configureOptions.Stdin != "token\n" {
		t.Fatalf("configure stdin = %q, want registration token line", configureOptions.Stdin)
	}
	if len(configureOptions.SensitiveValues) != 1 || configureOptions.SensitiveValues[0] != "token" {
		t.Fatalf("configure sensitive values = %#v, want registration token", configureOptions.SensitiveValues)
	}
	for key, value := range env {
		if strings.Contains(strings.ToLower(key), "private") || value == "/secret/app.pem" {
			t.Fatalf("guest registration environment exposes private key through %s", key)
		}
	}
	if got := atomic.LoadInt32(&github.waitOnlineIdleCalls); got != 1 {
		t.Fatalf("WaitRunnerOnlineIdle called %d time(s), want strict verification readiness", got)
	}
	if got := atomic.LoadInt32(&github.waitOnlineCalls); got != 0 {
		t.Fatalf("WaitRunnerOnline called %d time(s), verification must not accept a busy runner", got)
	}
}

func TestProvisionOneFailsPromptlyAfterConsecutiveRunnerProbeFailures(t *testing.T) {
	oldInterval := runnerReadinessHealthCheckInterval
	runnerReadinessHealthCheckInterval = time.Millisecond
	t.Cleanup(func() { runnerReadinessHealthCheckInterval = oldInterval })

	fake := &fakeProvider{ip: "127.0.0.1"}
	fake.execFunc = func(_ context.Context, _ string, command []string, _ provider.ExecOptions) (provider.ExecResult, error) {
		if strings.Contains(strings.Join(command, " "), "check-runner.sh") {
			return provider.ExecResult{}, errors.New("listener process is gone")
		}
		return provider.ExecResult{}, nil
	}
	github := &fakeGitHub{
		waitFunc: func(ctx context.Context, _ string, _ time.Duration) (gh.Runner, error) {
			<-ctx.Done()
			return gh.Runner{}, ctx.Err()
		},
	}
	manager := newRegisteredTestManager(t, fake, github)

	_, err := manager.provisionOne(context.Background(), "epar-test-1", true, false)
	if err == nil || !strings.Contains(err.Error(), "actions runner process failed 3 consecutive checks while waiting for GitHub online/idle") {
		t.Fatalf("provisionOne() error = %v, want prompt listener process failure", err)
	}
	if got := fake.commandCount("check-runner.sh"); got != runnerReadinessProbeFailureLimit {
		t.Fatalf("runner process checks = %d, want %d consecutive failures", got, runnerReadinessProbeFailureLimit)
	}
	if got := fake.commandCount("collect-runner-diagnostics.sh"); got != 1 {
		t.Fatalf("diagnostic collection calls = %d, want 1", got)
	}
}

func TestProvisionOneRecoversFromTransientRunnerProbeFailure(t *testing.T) {
	oldInterval := runnerReadinessHealthCheckInterval
	runnerReadinessHealthCheckInterval = time.Millisecond
	t.Cleanup(func() { runnerReadinessHealthCheckInterval = oldInterval })

	var healthChecks int32
	ready := make(chan struct{})
	fake := &fakeProvider{ip: "127.0.0.1"}
	fake.execFunc = func(_ context.Context, _ string, command []string, _ provider.ExecOptions) (provider.ExecResult, error) {
		if strings.Contains(strings.Join(command, " "), "check-runner.sh") {
			switch atomic.AddInt32(&healthChecks, 1) {
			case 1:
				return provider.ExecResult{}, errors.New("transient provider exec timeout")
			case 2:
				close(ready)
			}
			return provider.ExecResult{Stdout: runnerProcessRunningSentinel + "\n"}, nil
		}
		return provider.ExecResult{}, nil
	}
	github := &fakeGitHub{
		waitFunc: func(ctx context.Context, _ string, _ time.Duration) (gh.Runner, error) {
			select {
			case <-ctx.Done():
				return gh.Runner{}, ctx.Err()
			case <-ready:
				return gh.Runner{Name: "epar-test-1", ID: 123, Status: "online"}, nil
			}
		},
	}
	manager := newRegisteredTestManager(t, fake, github)

	vm, err := manager.provisionOne(context.Background(), "epar-test-1", true, false)
	if err != nil {
		t.Fatal(err)
	}
	if vm.RunnerID != 123 {
		t.Fatalf("RunnerID = %d, want 123", vm.RunnerID)
	}
	if got := atomic.LoadInt32(&healthChecks); got < 2 {
		t.Fatalf("runner health checks = %d, want transient failure followed by recovery", got)
	}
	if got := fake.commandCount("collect-runner-diagnostics.sh"); got != 0 {
		t.Fatalf("diagnostic collection calls = %d, want 0 after recovery", got)
	}
}

func TestProvisionOneCapturesReadinessTimeoutAndPreservesCause(t *testing.T) {
	timeoutErr := errors.New("GitHub runner online timeout")
	fake := &fakeProvider{ip: "127.0.0.1"}
	fake.execFunc = func(_ context.Context, _ string, command []string, _ provider.ExecOptions) (provider.ExecResult, error) {
		if strings.Contains(strings.Join(command, " "), "collect-runner-diagnostics.sh") {
			return provider.ExecResult{}, errors.New("diagnostic command failed")
		}
		return provider.ExecResult{}, nil
	}
	github := &fakeGitHub{waitErr: timeoutErr}
	manager := newRegisteredTestManager(t, fake, github)

	_, err := manager.provisionOne(context.Background(), "epar-test-1", true, false)
	if !errors.Is(err, timeoutErr) {
		t.Fatalf("provisionOne() error = %v, want original timeout error", err)
	}
	if got := fake.commandCount("collect-runner-diagnostics.sh"); got != 1 {
		t.Fatalf("diagnostic collection calls = %d, want 1", got)
	}
	if got := fake.logPathFor("collect-runner-diagnostics.sh"); !strings.HasSuffix(got, "epar-test-1.guest.log") {
		t.Fatalf("diagnostic LogPath = %q, want existing guest log", got)
	}
}

func TestRunPoolCancellationAfterListenerStartCleansExactRunner(t *testing.T) {
	state, err := poolstate.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var deleted atomic.Bool
	var lookupCalls atomic.Int32
	fake := &fakeProvider{ip: "127.0.0.1"}
	fake.execFunc = func(_ context.Context, _ string, command []string, _ provider.ExecOptions) (provider.ExecResult, error) {
		if strings.Contains(strings.Join(command, " "), "run-runner.sh") {
			cancel()
		}
		return provider.ExecResult{}, nil
	}
	github := &fakeGitHub{
		runnerByNameFunc: func(_ context.Context, name string) (gh.Runner, bool, error) {
			if lookupCalls.Add(1) == 1 || deleted.Load() {
				return gh.Runner{}, false, nil
			}
			return gh.Runner{Name: name, ID: 718}, true, nil
		},
		deleteFunc: func(context.Context, int64) error {
			deleted.Store(true)
			return nil
		},
		waitFunc: func(ctx context.Context, _ string, _ time.Duration) (gh.Runner, error) {
			<-ctx.Done()
			return gh.Runner{}, ctx.Err()
		},
	}
	manager := newRegisteredTestManager(t, fake, github)
	manager.Lifecycle = provider.AdaptLegacy(fake)
	manager.LifecycleState = state

	if err := manager.RunPool(ctx, RunOptions{Instances: 1, Register: true, ReplaceCompleted: true, PoolLockHeld: true, HostTrustLockHeld: true}); err != nil {
		t.Fatalf("RunPool() cancellation error = %v, want exact cleanup", err)
	}
	records, err := state.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("lifecycle records = %#v, want one tombstoned candidate", records)
	}
	record := records[0]
	if record.Phase != poolstate.PhaseTombstoned || record.GitHub.RunnerID != 718 {
		t.Fatalf("lifecycle after cancellation = phase %q runner id %d, want tombstoned with id 718", record.Phase, record.GitHub.RunnerID)
	}
	if got := atomic.LoadInt32(&github.deleteCalls); got != 1 {
		t.Fatalf("remote delete calls = %d, want 1", got)
	}
	github.mu.Lock()
	defer github.mu.Unlock()
	if len(github.deletedIDs) != 1 || github.deletedIDs[0] != 718 {
		t.Fatalf("deleted remote IDs = %v, want [718]", github.deletedIDs)
	}
	if got := atomic.LoadInt32(&fake.deleteCalls); got != 1 {
		t.Fatalf("local delete calls = %d, want 1", got)
	}
}

func TestCommonLifecycleOwnsGuestTranscriptPath(t *testing.T) {
	fake := &fakeProvider{instances: []provider.Instance{{Name: "epar-test-1", State: "running"}}}
	manager := newRegisteredTestManager(t, fake, nil)
	manager.Lifecycle = provider.AdaptLegacy(fake)

	if _, err := manager.execGuest(context.Background(), "epar-test-1", []string{"true"}, provider.ExecOptions{Env: map[string]string{"EPAR_TEST": "value"}}); err != nil {
		t.Fatal(err)
	}
	if got := fake.logPathFor("true"); got != "" {
		t.Fatalf("provider received common host transcript path %q", got)
	}
	fake.mu.Lock()
	command := fake.commands[len(fake.commands)-1]
	options := fake.execOptions[len(fake.execOptions)-1]
	fake.mu.Unlock()
	if len(options.Env) != 0 {
		t.Fatalf("provider received ambient host environment: %#v", options.Env)
	}
	if !strings.Contains(command, "env EPAR_TEST=value true") {
		t.Fatalf("provider command = %q, want explicit guest environment", command)
	}
}

func TestProvisionOneReadinessSucceedsWhileRunnerProcessStaysHealthy(t *testing.T) {
	oldInterval := runnerReadinessHealthCheckInterval
	runnerReadinessHealthCheckInterval = time.Millisecond
	t.Cleanup(func() { runnerReadinessHealthCheckInterval = oldInterval })

	provider := &fakeProvider{ip: "127.0.0.1"}
	github := &fakeGitHub{
		waitFunc: func(ctx context.Context, _ string, _ time.Duration) (gh.Runner, error) {
			select {
			case <-ctx.Done():
				return gh.Runner{}, ctx.Err()
			case <-time.After(10 * time.Millisecond):
				return gh.Runner{Name: "epar-test-1", ID: 123, Status: "online"}, nil
			}
		},
	}
	manager := newRegisteredTestManager(t, provider, github)

	vm, err := manager.provisionOne(context.Background(), "epar-test-1", true, false)
	if err != nil {
		t.Fatal(err)
	}
	if vm.RunnerID != 123 {
		t.Fatalf("RunnerID = %d, want 123", vm.RunnerID)
	}
	if got := provider.commandCount("check-runner.sh"); got == 0 {
		t.Fatal("runner process was not checked while GitHub readiness was pending")
	}
	if got := provider.commandCount("collect-runner-diagnostics.sh"); got != 0 {
		t.Fatalf("diagnostic collection calls = %d, want 0", got)
	}
}

func TestProvisioningFailureRollbackBoundary(t *testing.T) {
	tests := []struct {
		name            string
		configure       func(*fakeProvider, *fakeGitHub)
		wantPhase       LifecyclePhase
		wantLocalDelete bool
	}{
		{name: "partial clone", configure: func(p *fakeProvider, _ *fakeGitHub) { p.cloneErr = errors.New("clone failed after partial creation") }, wantLocalDelete: true},
		{name: "start", configure: func(p *fakeProvider, _ *fakeGitHub) { p.startErr = errors.New("start failed") }, wantLocalDelete: true},
		{name: "ip", configure: func(p *fakeProvider, _ *fakeGitHub) { p.ipErr = errors.New("ip failed") }, wantLocalDelete: true},
		{name: "runtime", configure: func(p *fakeProvider, _ *fakeGitHub) { p.execErr = errors.New("runtime validation failed") }, wantLocalDelete: true},
		{name: "token", configure: func(_ *fakeProvider, g *fakeGitHub) { g.registrationErr = errors.New("token failed") }, wantLocalDelete: true},
		{name: "configure runner", configure: func(p *fakeProvider, _ *fakeGitHub) {
			p.execFunc = func(_ context.Context, _ string, command []string, _ provider.ExecOptions) (provider.ExecResult, error) {
				if strings.Contains(strings.Join(command, " "), "configure-runner.sh") {
					return provider.ExecResult{}, errors.New("Http response code: ServiceUnavailable (503)")
				}
				return provider.ExecResult{}, nil
			}
		}, wantLocalDelete: true},
		{name: "listener start uncertain", configure: func(p *fakeProvider, _ *fakeGitHub) {
			p.execFunc = func(_ context.Context, _ string, command []string, _ provider.ExecOptions) (provider.ExecResult, error) {
				if strings.Contains(strings.Join(command, " "), "run-runner.sh") {
					return provider.ExecResult{}, errors.New("listener start failed ambiguously")
				}
				return provider.ExecResult{}, nil
			}
		}, wantPhase: LifecycleQuarantined},
		{name: "readiness", configure: func(_ *fakeProvider, g *fakeGitHub) { g.waitErr = errors.New("GitHub readiness failed") }, wantPhase: LifecycleQuarantined},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &fakeProvider{ip: "127.0.0.1"}
			g := &fakeGitHub{}
			tt.configure(p, g)
			manager := newRegisteredTestManager(t, p, g)
			vm, err := manager.provisionOne(context.Background(), "epar-test-stage", true, false)
			if err == nil {
				t.Fatal("provisionOne() error = nil")
			}
			if vm.Phase != tt.wantPhase {
				t.Fatalf("phase = %q, want %q", vm.Phase, tt.wantPhase)
			}
			if tt.wantLocalDelete && atomic.LoadInt32(&p.deleteCalls) == 0 {
				t.Fatal("pre-listener failure did not roll back local candidate")
			}
			if !tt.wantLocalDelete && atomic.LoadInt32(&p.deleteCalls) != 0 {
				t.Fatal("post-listener uncertainty deleted local candidate")
			}
		})
	}
}

func TestProvisioningRecordsPartialCreateIdentityBeforeExactRollback(t *testing.T) {
	const name = "epar-test-partial-create"
	partial := provider.Instance{
		Name:           name,
		ProviderID:     "fake:partial-create-id",
		Source:         "image",
		State:          "running",
		ReceiptVersion: "v1",
		Receipt:        json.RawMessage(`{"providerId":"fake:partial-create-id","source":"image"}`),
	}
	fake := &fakeProvider{}
	baseLifecycle := provider.AdaptLegacy(fake)
	lifecycle := &partialCreateLifecycle{
		Lifecycle: baseLifecycle,
		create: func() (provider.Instance, error) {
			fake.mu.Lock()
			fake.instances = append(fake.instances, partial)
			fake.mu.Unlock()
			return partial, errors.New("post-create verification failed")
		},
	}
	manager := newRegisteredTestManager(t, fake, nil)
	manager.Lifecycle = lifecycle
	store, err := poolstate.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manager.LifecycleState = store

	if _, err := manager.provisionOne(context.Background(), name, false, false); err == nil || !strings.Contains(err.Error(), "post-create verification failed") {
		t.Fatalf("provisionOne() error = %v", err)
	}
	if got := atomic.LoadInt32(&fake.deleteCalls); got != 1 {
		t.Fatalf("exact provider delete calls = %d, want 1", got)
	}
	record, err := store.Read(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	if record.ProviderID != partial.ProviderID || record.Phase != poolstate.PhaseTombstoned {
		t.Fatalf("lifecycle record = %#v, want exact provider identity tombstoned", record)
	}
}

func TestHostTrustRuntimeActivationFailsBeforeRegistrationAndRollsBack(t *testing.T) {
	fake := &fakeProvider{ip: "127.0.0.1"}
	github := &fakeGitHub{}
	activator := &activatingLifecycle{
		Lifecycle: provider.AdaptLegacy(fake, false),
		err:       errors.New("relay proof failed"),
	}
	manager := newRegisteredTestManager(t, fake, github)
	manager.Lifecycle = activator
	manager.AllowInsufficientStorage = true

	if _, err := manager.provisionOne(context.Background(), "epar-test-activation", true, false); err == nil || !strings.Contains(err.Error(), "relay proof failed") {
		t.Fatalf("provisionOne() error = %v, want relay proof failure", err)
	}
	if activator.calls != 1 {
		t.Fatalf("activation calls = %d, want 1", activator.calls)
	}
	if got := atomic.LoadInt32(&github.registrationCalls); got != 0 {
		t.Fatalf("registration token calls = %d, want 0", got)
	}
	if got := atomic.LoadInt32(&fake.deleteCalls); got != 1 {
		t.Fatalf("exact rollback delete calls = %d, want 1", got)
	}
}

func TestControllerRestartReactivatesExistingHostTrustRuntimesBeforeUse(t *testing.T) {
	snapshot := hosttrust.Snapshot{Generation: "g1", HostOS: "windows", Scopes: []string{"system"}, Certificates: []hosttrust.Certificate{{Name: "root.crt", PEM: []byte("pem")}}, CollectedAt: time.Now().UTC()}
	marker, err := hostTrustMarkerJSON(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeProvider{instances: []provider.Instance{
		{Name: "runner-b", ProviderID: "fake:runner-b", State: "running"},
		{Name: "runner-a", ProviderID: "fake:runner-a", State: "running"},
	}}
	fake.execFunc = func(_ context.Context, _ string, command []string, _ provider.ExecOptions) (provider.ExecResult, error) {
		if strings.Join(command, " ") == "cat "+hostTrustMarkerGuest {
			return provider.ExecResult{Stdout: string(marker)}, nil
		}
		return provider.ExecResult{}, nil
	}
	activator := &activatingLifecycle{Lifecycle: provider.AdaptLegacy(fake, false)}
	manager := newRegisteredTestManager(t, fake, &fakeGitHub{})
	manager.Config.Image.HostTrustMode = config.HostTrustModeOverlay
	manager.Config.Image.HostTrustScopes = []string{"system"}
	manager.Lifecycle = activator
	manager.hostTrustResolver = func(context.Context) (hosttrust.Snapshot, error) { return snapshot, nil }
	active := map[string]ProvisionedInstance{
		"runner-b": {Name: "runner-b", HostTrustGeneration: "g1", ProviderOwned: true, Phase: LifecycleReady},
		"runner-a": {Name: "runner-a", HostTrustGeneration: "g1", ProviderOwned: true, Phase: LifecycleReady},
	}

	if _, err := manager.prepareExistingHostTrustRuntimes(context.Background(), active, false); err != nil {
		t.Fatal(err)
	}
	if activator.calls != 2 {
		t.Fatalf("activation calls = %d, want one per kept instance", activator.calls)
	}
	if got := []string{activator.instances[0].ProviderID, activator.instances[1].ProviderID}; !reflect.DeepEqual(got, []string{"fake:runner-a", "fake:runner-b"}) {
		t.Fatalf("reactivated provider identities = %v, want deterministic exact instances", got)
	}
	if active["runner-a"].HostTrustGeneration != "g1" || active["runner-b"].HostTrustGeneration != "g1" {
		t.Fatalf("restored generations = %q/%q, want g1/g1", active["runner-a"].HostTrustGeneration, active["runner-b"].HostTrustGeneration)
	}
}

func TestControllerRestartFailsClosedWhenExistingHostTrustRuntimeCannotReactivate(t *testing.T) {
	snapshot := hosttrust.Snapshot{Generation: "g1", HostOS: "windows", Scopes: []string{"system"}, Certificates: []hosttrust.Certificate{{Name: "root.crt", PEM: []byte("pem")}}, CollectedAt: time.Now().UTC()}
	marker, err := hostTrustMarkerJSON(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeProvider{instances: []provider.Instance{{Name: "runner-a", ProviderID: "fake:runner-a", State: "running"}}}
	fake.execFunc = func(_ context.Context, _ string, command []string, _ provider.ExecOptions) (provider.ExecResult, error) {
		if strings.Join(command, " ") == "cat "+hostTrustMarkerGuest {
			return provider.ExecResult{Stdout: string(marker)}, nil
		}
		return provider.ExecResult{}, nil
	}
	activator := &activatingLifecycle{Lifecycle: provider.AdaptLegacy(fake, false), err: errors.New("relay unavailable")}
	manager := newRegisteredTestManager(t, fake, &fakeGitHub{})
	manager.Config.Image.HostTrustMode = config.HostTrustModeOverlay
	manager.Config.Image.HostTrustScopes = []string{"system"}
	manager.Lifecycle = activator
	manager.hostTrustResolver = func(context.Context) (hosttrust.Snapshot, error) { return snapshot, nil }

	_, err = manager.prepareExistingHostTrustRuntimes(context.Background(), map[string]ProvisionedInstance{"runner-a": {Name: "runner-a", HostTrustGeneration: "g1", ProviderOwned: true, Phase: LifecycleReady}}, false)
	if err == nil || !strings.Contains(err.Error(), "relay unavailable") {
		t.Fatalf("reactivation error = %v, want relay failure", err)
	}
	if got := atomic.LoadInt32(&fake.deleteCalls); got != 0 {
		t.Fatalf("existing instance delete calls = %d, want fail-closed preservation", got)
	}
}

func TestControllerRestartDoesNotTouchUnownedOrQuarantinedSandbox(t *testing.T) {
	fake := &fakeProvider{instances: []provider.Instance{{Name: "epar-test-unowned", ProviderID: "foreign", State: "running"}}}
	activator := &activatingLifecycle{Lifecycle: provider.AdaptLegacy(fake, false)}
	manager := newRegisteredTestManager(t, fake, &fakeGitHub{})
	manager.Config.Image.HostTrustMode = config.HostTrustModeOverlay
	manager.Config.Image.HostTrustScopes = []string{"system"}
	manager.Lifecycle = activator
	manager.hostTrustResolver = func(context.Context) (hosttrust.Snapshot, error) {
		t.Fatal("host trust was resolved for an ineligible sandbox")
		return hosttrust.Snapshot{}, nil
	}

	if generation, err := manager.prepareExistingHostTrustRuntimes(context.Background(), map[string]ProvisionedInstance{
		"epar-test-unowned": {Name: "epar-test-unowned", ProviderID: "foreign", ProviderOwned: false, Phase: LifecycleQuarantined},
	}, true); err != nil || generation != "" {
		t.Fatalf("prepareExistingHostTrustRuntimes() = %q, %v; want no-op", generation, err)
	}
	if activator.calls != 0 || atomic.LoadInt32(&fake.execCalls) != 0 {
		t.Fatalf("unowned sandbox mutations = activations %d guest execs %d, want zero", activator.calls, fake.execCalls)
	}
}

func TestRunPoolControllerRestartFencesRebindsAndLeasesExistingRunner(t *testing.T) {
	snapshot := hosttrust.Snapshot{Generation: "g1", HostOS: "windows", Scopes: []string{"system"}, Certificates: []hosttrust.Certificate{{Name: "root.crt", PEM: []byte("pem")}}, CollectedAt: time.Now().UTC()}
	marker, err := hostTrustMarkerJSON(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	name := "epar-test-existing"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var order []string
	fake := &fakeProvider{instances: []provider.Instance{{Name: name, ProviderID: "fake:" + name, State: "running"}}}
	fake.execFunc = func(_ context.Context, _ string, command []string, options provider.ExecOptions) (provider.ExecResult, error) {
		text := strings.Join(command, " ")
		switch {
		case text == "cat "+hostTrustMarkerGuest:
			order = append(order, "marker")
			return provider.ExecResult{Stdout: string(marker)}, nil
		case strings.Contains(text, "rm -f") && strings.Contains(text, hostTrustLeaseGuest) && options.Stdin == "":
			order = append(order, "fence")
		case strings.Contains(options.Stdin, `"expiresAt"`):
			order = append(order, "lease")
			cancel()
		case strings.Contains(text, "/opt/epar/validate-runtime.sh"):
			order = append(order, "verify")
		}
		return provider.ExecResult{}, nil
	}
	activator := &activatingLifecycle{Lifecycle: provider.AdaptLegacy(fake, false), onActivate: func(provider.Instance) { order = append(order, "activate") }}
	github := &fakeGitHub{
		runner:      gh.Runner{Name: name, ID: 42, Status: "online"},
		found:       true,
		listRunners: []gh.Runner{{Name: name, ID: 42, Status: "online"}},
	}
	manager := newRegisteredTestManager(t, fake, github)
	manager.Config.Image.HostTrustMode = config.HostTrustModeOverlay
	manager.Config.Image.HostTrustScopes = []string{"system"}
	manager.Lifecycle = activator
	manager.hostTrustResolver = func(context.Context) (hosttrust.Snapshot, error) { return snapshot, nil }

	if err := manager.RunPool(ctx, RunOptions{Instances: 1, Register: true, KeepOnExit: true, ReplaceCompleted: true, PoolLockHeld: true, HostTrustLockHeld: true}); err != nil {
		t.Fatal(err)
	}
	if got, want := order, []string{"fence", "marker", "activate", "verify", "marker", "lease"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("restart preparation order = %v, want %v", got, want)
	}
	if got := atomic.LoadInt32(&fake.deleteCalls); got != 0 {
		t.Fatalf("existing runner delete calls = %d, want zero", got)
	}
}

type activatingLifecycle struct {
	provider.Lifecycle
	calls       int
	verifyCalls int
	err         error
	verifyErr   error
	instances   []provider.Instance
	onActivate  func(provider.Instance)
}

func (l *activatingLifecycle) ActivateHostTrustRuntime(_ context.Context, instance provider.Instance) error {
	l.calls++
	l.instances = append(l.instances, instance)
	if l.onActivate != nil {
		l.onActivate(instance)
	}
	return l.err
}

func (l *activatingLifecycle) VerifyRuntime(ctx context.Context, instance provider.Instance) (provider.RuntimeInfo, error) {
	l.verifyCalls++
	if l.verifyErr != nil {
		return provider.RuntimeInfo{}, l.verifyErr
	}
	return l.Lifecycle.VerifyRuntime(ctx, instance)
}

type partialCreateLifecycle struct {
	provider.Lifecycle
	create func() (provider.Instance, error)
}

func (l *partialCreateLifecycle) Create(context.Context, provider.CreateRequest) (provider.Instance, error) {
	return l.create()
}

func TestConfigureFailureDeletesExactLocalAndRemoteCandidate(t *testing.T) {
	p := &fakeProvider{ip: "127.0.0.1"}
	p.execFunc = func(_ context.Context, _ string, command []string, _ provider.ExecOptions) (provider.ExecResult, error) {
		if strings.Contains(strings.Join(command, " "), "configure-runner.sh") {
			return provider.ExecResult{}, errors.New("Http response code: BadGateway from runner-registration (502)")
		}
		return provider.ExecResult{}, nil
	}
	g := &fakeGitHub{runner: gh.Runner{Name: "epar-test-candidate", ID: 456}, found: true}
	manager := newRegisteredTestManager(t, p, g)
	vm, err := manager.provisionOne(context.Background(), "epar-test-candidate", true, false)
	if err == nil {
		t.Fatal("provisionOne() error = nil")
	}
	if vm.Phase != "" {
		t.Fatalf("phase = %q, want no surviving physical candidate", vm.Phase)
	}
	if got := atomic.LoadInt32(&p.deleteCalls); got != 1 {
		t.Fatalf("local delete calls = %d, want 1", got)
	}
	if got := atomic.LoadInt32(&g.deleteCalls); got != 1 {
		t.Fatalf("remote delete calls = %d, want 1", got)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.deletedIDs) != 1 || g.deletedIDs[0] != 456 {
		t.Fatalf("deleted remote IDs = %v, want [456]", g.deletedIDs)
	}
}

func TestConfigureFailureRedactsRegistrationTokenFromErrorAndTiming(t *testing.T) {
	const sentinel = "SENTINEL-RUNNER-REGISTRATION-TOKEN"
	p := &fakeProvider{ip: "127.0.0.1"}
	p.execFunc = func(_ context.Context, _ string, command []string, _ provider.ExecOptions) (provider.ExecResult, error) {
		if strings.Contains(strings.Join(command, " "), "configure-runner.sh") {
			return provider.ExecResult{}, fmt.Errorf("configure failed with token %s", sentinel)
		}
		return provider.ExecResult{}, nil
	}
	g := &fakeGitHub{registrationToken: sentinel}
	manager := newRegisteredTestManager(t, p, g)
	timingPath, err := manager.StartStartupTiming()
	if err != nil {
		t.Fatal(err)
	}
	_, provisionErr := manager.provisionOne(context.Background(), "epar-test-secret", true, false)
	if provisionErr == nil {
		t.Fatal("provisionOne() error = nil")
	}
	manager.FinishStartupTiming(provisionErr)
	if strings.Contains(provisionErr.Error(), sentinel) {
		t.Fatalf("returned error leaked registration token: %v", provisionErr)
	}
	content, err := os.ReadFile(timingPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(content), sentinel) {
		t.Fatalf("startup timing leaked registration token: %s", content)
	}
}

func TestOneHundredRegistrationOutagesNeverExceedPhysicalCap(t *testing.T) {
	p := &fakeProvider{ip: "127.0.0.1"}
	p.execFunc = func(_ context.Context, _ string, command []string, _ provider.ExecOptions) (provider.ExecResult, error) {
		if strings.Contains(strings.Join(command, " "), "configure-runner.sh") {
			return provider.ExecResult{}, errors.New("Http response code: ServiceUnavailable (503)")
		}
		return provider.ExecResult{}, nil
	}
	manager := newRegisteredTestManager(t, p, &fakeGitHub{})
	manager.Config.Pool.Instances = 2
	for i := 0; i < 100; i++ {
		name := fmt.Sprintf("epar-test-outage-%03d", i)
		if _, err := manager.provisionOne(context.Background(), name, true, true); err == nil {
			t.Fatalf("cycle %d unexpectedly succeeded", i)
		}
	}
	if got := atomic.LoadInt32(&p.maxInventory); got > 2 {
		t.Fatalf("maximum physical inventory = %d, want <= 2", got)
	}
	instances, err := p.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(instances) != 0 {
		t.Fatalf("surviving physical inventory = %d, want 0 after local rollback", len(instances))
	}
}

func TestCleanupFailureRemainsCountedAndBlocksClone(t *testing.T) {
	p := &fakeProvider{instances: []provider.Instance{{Name: "epar-test-existing", State: "running"}}, deleteErr: errors.New("delete failed")}
	g := &fakeGitHub{}
	manager := newRegisteredTestManager(t, p, g)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := manager.RunPool(ctx, RunOptions{Instances: 1, Register: true, KeepOnExit: true, ReplaceCompleted: true, MonitorInterval: time.Millisecond, PoolLockHeld: true}); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&p.cloneCalls); got != 0 {
		t.Fatalf("clone calls = %d, want 0 while cleanup-pending resource occupies capacity", got)
	}
}

func TestInitialTerminalFailureCleansReadyInstancesButPreservesQuarantine(t *testing.T) {
	t.Run("pre-listener failure cleans earlier ready capacity", func(t *testing.T) {
		var configureCalls int32
		p := &fakeProvider{ip: "127.0.0.1"}
		p.execFunc = func(_ context.Context, _ string, command []string, _ provider.ExecOptions) (provider.ExecResult, error) {
			if strings.Contains(strings.Join(command, " "), "configure-runner.sh") && atomic.AddInt32(&configureCalls, 1) == 2 {
				return provider.ExecResult{}, errors.New("Response status code does not indicate success: 401")
			}
			return provider.ExecResult{}, nil
		}
		g := &fakeGitHub{runner: gh.Runner{ID: 123, Status: "online"}, found: true, waitRunner: gh.Runner{ID: 123, Status: "online"}}
		manager := newRegisteredTestManager(t, p, g)
		err := manager.RunPool(context.Background(), RunOptions{Instances: 2, Register: true, ReplaceCompleted: true, PoolLockHeld: true})
		if err == nil {
			t.Fatal("RunPool() error = nil")
		}
		if got := atomic.LoadInt32(&p.deleteCalls); got < 2 {
			t.Fatalf("local delete calls = %d, want failed candidate rollback plus ready-instance terminal cleanup", got)
		}
	})

	t.Run("post-listener uncertainty survives terminal return", func(t *testing.T) {
		p := &fakeProvider{ip: "127.0.0.1"}
		g := &fakeGitHub{waitErr: errors.New("Response status code does not indicate success: 401")}
		manager := newRegisteredTestManager(t, p, g)
		err := manager.RunPool(context.Background(), RunOptions{Instances: 1, Register: true, ReplaceCompleted: true, PoolLockHeld: true})
		if err == nil {
			t.Fatal("RunPool() error = nil")
		}
		if got := atomic.LoadInt32(&p.deleteCalls); got != 0 {
			t.Fatalf("local delete calls = %d, want quarantined post-listener candidate preserved", got)
		}
	})
}

func TestShutdownCancellationPreservesPostListenerCandidateWhenKeepOnExit(t *testing.T) {
	p := &fakeProvider{ip: "127.0.0.1"}
	g := &fakeGitHub{waitFunc: func(ctx context.Context, _ string, _ time.Duration) (gh.Runner, error) {
		<-ctx.Done()
		return gh.Runner{}, ctx.Err()
	}}
	manager := newRegisteredTestManager(t, p, g)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := manager.RunPool(ctx, RunOptions{Instances: 1, Register: true, KeepOnExit: true, ReplaceCompleted: true, PoolLockHeld: true}); err != nil {
		t.Fatalf("RunPool() cancellation error = %v, want clean shutdown", err)
	}
	if got := atomic.LoadInt32(&p.deleteCalls); got != 0 {
		t.Fatalf("local delete calls = %d, want post-listener candidate preserved by keep-on-exit", got)
	}
	instances, err := p.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(instances) != 1 {
		t.Fatalf("surviving local instances = %d, want 1 quarantined candidate", len(instances))
	}
}

func TestStoppedLocalIsCleanedEvenWhenGitHubIsUnavailable(t *testing.T) {
	p := &fakeProvider{instances: []provider.Instance{{Name: "epar-test-stopped", State: "stopped"}}}
	g := &fakeGitHub{listErr: &gh.HTTPError{StatusCode: 503}}
	manager := newRegisteredTestManager(t, p, g)
	active, err := manager.reconcilePhysicalPool(context.Background(), nil, true)
	if err == nil {
		t.Fatal("reconcilePhysicalPool() error = nil, want GitHub outage")
	}
	if len(active) != 0 {
		t.Fatalf("active = %#v, want stopped local removed", active)
	}
	if got := atomic.LoadInt32(&p.deleteCalls); got != 1 {
		t.Fatalf("local delete calls = %d, want 1 during GitHub outage", got)
	}
}

func TestRestartUnknownResourcesCountAndBlockAllocation(t *testing.T) {
	p := &fakeProvider{instances: []provider.Instance{{Name: "epar-test-unknown-1", State: "running"}, {Name: "epar-test-unknown-2", State: "running"}}}
	g := &fakeGitHub{listErr: &gh.HTTPError{StatusCode: 503}}
	manager := newRegisteredTestManager(t, p, g)
	active, err := manager.reconcilePhysicalPool(context.Background(), nil, true)
	if err == nil {
		t.Fatal("reconcilePhysicalPool() error = nil, want GitHub outage")
	}
	if len(active) != 2 {
		t.Fatalf("counted physical resources = %d, want 2", len(active))
	}
	for name, vm := range active {
		if vm.Phase != LifecycleQuarantined {
			t.Fatalf("%s phase = %q, want quarantined", name, vm.Phase)
		}
	}
	if got := atomic.LoadInt32(&p.cloneCalls); got != 0 {
		t.Fatalf("clone calls = %d, want 0 while dependency state is ambiguous", got)
	}
}

func TestLegacyOverCapacityInventoryBlocksAllocation(t *testing.T) {
	p := &fakeProvider{instances: []provider.Instance{{Name: "epar-test-existing-1", State: "running"}, {Name: "epar-test-existing-2", State: "running"}, {Name: "epar-test-existing-3", State: "running"}}}
	manager := newRegisteredTestManager(t, p, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := manager.RunPool(ctx, RunOptions{Instances: 2, KeepOnExit: true, PoolLockHeld: true}); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&p.cloneCalls); got != 0 {
		t.Fatalf("clone calls = %d, want 0 while legacy physical inventory exceeds target", got)
	}
}

func TestReconciliationCleanupUsesMaintenanceContext(t *testing.T) {
	p := &fakeProvider{instances: []provider.Instance{{Name: "epar-test-stopped", State: "stopped"}}}
	p.deleteFunc = func(ctx context.Context, _ string) error {
		<-ctx.Done()
		return ctx.Err()
	}
	manager := newRegisteredTestManager(t, p, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	active, err := manager.reconcilePhysicalPool(ctx, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := active["epar-test-stopped"].Phase; got != LifecycleCleanupPending {
		t.Fatalf("stopped instance phase = %q, want cleanup-pending", got)
	}
	if got := atomic.LoadInt32(&p.deleteCalls); got != 1 {
		t.Fatalf("local delete calls = %d, want 1", got)
	}
}

func TestReconciliationPropagatesControlPlaneCleanupFailure(t *testing.T) {
	const daemonFailure = "inventory control plane unavailable"
	p := &fakeProvider{
		instances: []provider.Instance{{Name: "epar-test-stopped", ProviderID: "fake:epar-test-stopped", State: "stopped"}},
		deleteErr: provider.NewControlPlaneFailure("delete Docker Sandboxes instance", errors.New(daemonFailure)),
	}
	manager := newRegisteredTestManager(t, p, nil)
	active, err := manager.reconcilePhysicalPool(context.Background(), nil, false)
	if !errors.Is(err, provider.ErrControlPlaneFailure) {
		t.Fatalf("reconcilePhysicalPool() error = %v, want control-plane failure", err)
	}
	if _, found := active["epar-test-stopped"]; found {
		t.Fatalf("active inventory = %#v, want caller to retain its prior map on typed failure", active)
	}
}

func TestLegacyIdleOverCapacityIsReducedToTarget(t *testing.T) {
	p := &fakeProvider{instances: []provider.Instance{{Name: "epar-test-existing-1", State: "running"}, {Name: "epar-test-existing-2", State: "running"}, {Name: "epar-test-existing-3", State: "running"}}}
	g := &fakeGitHub{
		listRunners: []gh.Runner{{Name: "epar-test-existing-1", ID: 1, Status: "online"}, {Name: "epar-test-existing-2", ID: 2, Status: "online"}, {Name: "epar-test-existing-3", ID: 3, Status: "online"}},
		runner:      gh.Runner{ID: 9, Status: "online"},
		found:       true,
	}
	manager := newRegisteredTestManager(t, p, g)
	active, err := manager.reconcilePhysicalPool(context.Background(), nil, true)
	if err != nil {
		t.Fatal(err)
	}
	active, err = manager.reduceOverCapacity(context.Background(), active, 1, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 {
		t.Fatalf("active count = %d, want target 1", len(active))
	}
	if got := atomic.LoadInt32(&p.deleteCalls); got != 2 {
		t.Fatalf("local delete calls = %d, want 2 idle excess runners retired", got)
	}
}

func TestLegacyOverCapacityPropagatesControlPlaneCleanupFailure(t *testing.T) {
	p := &fakeProvider{
		instances: []provider.Instance{
			{Name: "epar-test-existing-1", ProviderID: "fake:epar-test-existing-1", State: "running"},
			{Name: "epar-test-existing-2", ProviderID: "fake:epar-test-existing-2", State: "running"},
		},
		deleteErr: provider.NewControlPlaneFailure("delete Docker Sandboxes instance", errors.New("daemon inventory unavailable")),
	}
	g := &fakeGitHub{
		runner: gh.Runner{ID: 9, Status: "online"},
		found:  true,
		listRunners: []gh.Runner{
			{Name: "epar-test-existing-1", ID: 1, Status: "online"},
			{Name: "epar-test-existing-2", ID: 2, Status: "online"},
		},
	}
	manager := newRegisteredTestManager(t, p, g)
	active, err := manager.reconcilePhysicalPool(context.Background(), nil, true)
	if err != nil {
		t.Fatal(err)
	}
	_, err = manager.reduceOverCapacity(context.Background(), active, 1, true)
	if !errors.Is(err, provider.ErrControlPlaneFailure) {
		t.Fatalf("reduceOverCapacity() error = %v, want control-plane failure", err)
	}
}

func TestQuarantinedRunnersAdoptOrRetireAfterGitHubRecovery(t *testing.T) {
	p := &fakeProvider{instances: []provider.Instance{{Name: "epar-test-healthy", State: "running"}, {Name: "epar-test-offline", State: "running"}}}
	g := &fakeGitHub{listRunners: []gh.Runner{{Name: "epar-test-healthy", ID: 1, Status: "online"}, {Name: "epar-test-offline", ID: 2, Status: "offline"}}}
	manager := newRegisteredTestManager(t, p, g)
	known := map[string]ProvisionedInstance{
		"epar-test-healthy": {Name: "epar-test-healthy", Phase: LifecycleQuarantined},
		"epar-test-offline": {Name: "epar-test-offline", Phase: LifecycleQuarantined},
	}
	active, err := manager.reconcilePhysicalPool(context.Background(), known, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := active["epar-test-healthy"].Phase; got != LifecycleReady {
		t.Fatalf("healthy phase = %q, want ready", got)
	}
	if _, found := active["epar-test-offline"]; found {
		t.Fatal("offline quarantined runner survived recovery reconciliation")
	}
	if got := atomic.LoadInt32(&p.deleteCalls); got != 1 {
		t.Fatalf("local delete calls = %d, want offline runner retirement", got)
	}
}

func TestReconciliationDoesNotAdoptSameNameDifferentRunnerID(t *testing.T) {
	p := &fakeProvider{instances: []provider.Instance{{Name: "epar-test-runner", State: "running"}}}
	g := &fakeGitHub{listRunners: []gh.Runner{{Name: "epar-test-runner", ID: 43, Status: "online"}}}
	manager := newRegisteredTestManager(t, p, g)
	known := map[string]ProvisionedInstance{
		"epar-test-runner": {Name: "epar-test-runner", RunnerID: 42, Phase: LifecycleQuarantined},
	}

	active, err := manager.reconcilePhysicalPool(context.Background(), known, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := active["epar-test-runner"].Phase; got != LifecycleQuarantined {
		t.Fatalf("runner phase = %q, want quarantined", got)
	}
	if got := active["epar-test-runner"].RunnerID; got != 42 {
		t.Fatalf("persisted runner id = %d, want 42", got)
	}
	if got := atomic.LoadInt32(&g.deleteCalls); got != 0 {
		t.Fatalf("GitHub delete calls = %d, want 0 for same-name identity mismatch", got)
	}
}

func TestControllerRestartHydratesImmutableRunnerIDBeforeReconciliation(t *testing.T) {
	manager, store, name := readyLifecycleManager(t)
	p := &fakeProvider{instances: []provider.Instance{{Name: name, ProviderID: "docker:ready-id", State: "running"}}}
	g := &fakeGitHub{listRunners: []gh.Runner{{Name: name, ID: 43, Status: "online"}}}
	manager.Provider = p
	manager.Lifecycle = provider.AdaptLegacy(p)
	manager.GitHub = g
	manager.LifecycleState = store

	active, err := manager.reconcilePhysicalPool(context.Background(), nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := active[name].Phase; got != LifecycleQuarantined {
		t.Fatalf("runner phase = %q, want quarantined", got)
	}
	if got := active[name].RunnerID; got != 42 {
		t.Fatalf("hydrated runner id = %d, want 42", got)
	}
	if got := atomic.LoadInt32(&g.deleteCalls); got != 0 {
		t.Fatalf("GitHub delete calls = %d, want 0 for restart identity mismatch", got)
	}
}

func TestReplacementBackoffSequenceJitterCapRetryAfterAndReset(t *testing.T) {
	manager := Manager{Config: config.Config{Pool: config.PoolConfig{
		ReplacementRetryInitialSeconds: 15,
		ReplacementRetryMaxSeconds:     1800,
		ReplacementRetryMultiplier:     2,
		ReplacementRetryJitterPercent:  20,
	}}, randomFloat64: func() float64 { return 0.5 }}
	now := time.Unix(1_000, 0)
	state := replacementRetryState{}
	want := []time.Duration{15, 30, 60, 120, 240, 480, 960, 1800, 1800}
	for i, seconds := range want {
		state.schedule(&manager, now, errors.New("ServiceUnavailable"))
		if got := state.next.Sub(now); got != seconds*time.Second {
			t.Fatalf("attempt %d delay = %s, want %s", i+1, got, seconds*time.Second)
		}
		now = state.next
	}
	state.reset()
	if state.attempt != 0 || !state.next.IsZero() {
		t.Fatalf("reset state = %#v", state)
	}
	state = replacementRetryState{attempt: 4, next: now.Add(time.Hour)}
	before := map[string]ProvisionedInstance{"runner": {Name: "runner", Phase: LifecycleQuarantined}}
	after := map[string]ProvisionedInstance{"runner": {Name: "runner", Phase: LifecycleReady}}
	state.resetAfterAdoption(before, after)
	if state.attempt != 0 || !state.next.IsZero() {
		t.Fatalf("adoption reset state = %#v", state)
	}
	state.schedule(&manager, now, &gh.HTTPError{StatusCode: 429, RetryAfter: 45 * time.Second})
	if got := state.next.Sub(now); got != 45*time.Second {
		t.Fatalf("Retry-After delay = %s, want 45s", got)
	}
	low := Manager{Config: manager.Config, randomFloat64: func() float64 { return 0 }}
	lowState := replacementRetryState{}
	lowState.schedule(&low, now, errors.New("ServiceUnavailable"))
	if got := lowState.next.Sub(now); got != 12*time.Second {
		t.Fatalf("low jitter delay = %s, want 12s", got)
	}
	high := Manager{Config: manager.Config, randomFloat64: func() float64 { return 1 }}
	highState := replacementRetryState{attempt: 20}
	highState.schedule(&high, now, errors.New("ServiceUnavailable"))
	if got := highState.next.Sub(now); got != 1800*time.Second {
		t.Fatalf("jittered cap delay = %s, want 30m", got)
	}
	minimum := Manager{Config: config.Config{Pool: config.PoolConfig{
		ReplacementRetryInitialSeconds: 1,
		ReplacementRetryMaxSeconds:     1,
		ReplacementRetryMultiplier:     1,
		ReplacementRetryJitterPercent:  100,
	}}, randomFloat64: func() float64 { return 0 }}
	minimumState := replacementRetryState{}
	minimumState.schedule(&minimum, now, errors.New("ServiceUnavailable"))
	if got := minimumState.next.Sub(now); got != time.Second {
		t.Fatalf("minimum jittered delay = %s, want 1s", got)
	}
}

func TestTransientDependencyClassification(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "network", err: &net.DNSError{IsTimeout: true}, want: true},
		{name: "rate limit", err: &gh.HTTPError{StatusCode: http.StatusTooManyRequests}, want: true},
		{name: "server error", err: &gh.HTTPError{StatusCode: http.StatusBadGateway}, want: true},
		{name: "guest opaque 501", err: errors.New("Response status code does not indicate success: 501 (Not Implemented)"), want: true},
		{name: "guest opaque 599", err: errors.New("Http response code: 599 from runner-registration"), want: true},
		{name: "unauthorized", err: &gh.HTTPError{StatusCode: http.StatusUnauthorized}, want: false},
		{name: "forbidden", err: &gh.HTTPError{StatusCode: http.StatusForbidden}, want: false},
		{name: "guest opaque forbidden", err: errors.New("Response status code does not indicate success: 403 (Forbidden)"), want: false},
		{name: "cancellation", err: context.Canceled, want: false},
		{name: "sandbox create admission", err: provider.NewControlPlaneAdmissionFailure("create Docker Sandboxes instance", errors.New("failed to run sandbox container")), want: false},
		{name: "deterministic configuration", err: errors.New("runner labels are invalid"), want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := isTransientDependencyError(test.err); got != test.want {
				t.Fatalf("isTransientDependencyError(%v) = %t, want %t", test.err, got, test.want)
			}
		})
	}
}

func TestReplacementCooldownSkipsGitHubButContinuesLocalHousekeeping(t *testing.T) {
	p := &fakeProvider{instances: []provider.Instance{{Name: "epar-test-existing", State: "running"}}}
	g := &fakeGitHub{runner: gh.Runner{Name: "epar-test-existing", ID: 1, Status: "online"}, found: true}
	g.listFunc = func(context.Context) ([]gh.Runner, error) {
		if atomic.LoadInt32(&g.listCalls) == 1 {
			return []gh.Runner{{Name: "epar-test-existing", ID: 1, Status: "online"}}, nil
		}
		p.mu.Lock()
		if len(p.instances) > 0 {
			p.instances[0].State = "stopped"
		}
		p.mu.Unlock()
		return nil, &gh.HTTPError{StatusCode: 503}
	}
	manager := newRegisteredTestManager(t, p, g)
	manager.Config.Pool.ReplacementRetryInitialSeconds = 15
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := manager.RunPool(ctx, RunOptions{Instances: 1, Register: true, KeepOnExit: true, ReplaceCompleted: true, MonitorInterval: time.Millisecond, PoolLockHeld: true}); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&g.listCalls); got != 2 {
		t.Fatalf("GitHub ListRunners calls = %d, want initial success plus one failed retry trigger", got)
	}
	if got := atomic.LoadInt32(&g.runnerByNameCalls); got > 1 {
		t.Fatalf("GitHub RunnerByName calls = %d, want no calls during cooldown", got)
	}
	if got := atomic.LoadInt32(&p.listCalls); got <= 2 {
		t.Fatalf("local List calls = %d, want repeated housekeeping during cooldown", got)
	}
	if got := atomic.LoadInt32(&p.deleteCalls); got != 1 {
		t.Fatalf("local delete calls = %d, want stopped resource cleanup during cooldown", got)
	}
	if got := atomic.LoadInt32(&p.cloneCalls); got != 0 {
		t.Fatalf("clone calls = %d, want allocation paused during cooldown", got)
	}
}

func newRegisteredTestManager(t *testing.T, provider provider.Provider, github GitHubClient) Manager {
	t.Helper()
	return Manager{
		Config: config.Config{
			Provider: config.ProviderConfig{SourceImage: "image", Type: "docker-container"},
			Pool:     config.PoolConfig{Instances: 1, NamePrefix: "epar-test"},
			Logging:  config.LoggingConfig{Directory: t.TempDir()},
			Runner:   config.RunnerConfig{Labels: []string{"self-hosted"}, Ephemeral: true},
			Timeouts: config.TimeoutConfig{CommandSeconds: 5, GitHubOnlineSeconds: 5},
		},
		Provider:    provider,
		GitHub:      github,
		ProjectRoot: t.TempDir(),
	}
}

type fakeProvider struct {
	execErr    error
	execErrs   []error
	execFunc   func(context.Context, string, []string, provider.ExecOptions) (provider.ExecResult, error)
	ip         string
	cloneErr   error
	startErr   error
	ipErr      error
	deleteErr  error
	listErr    error
	deleteFunc func(context.Context, string) error
	mu         sync.Mutex

	configureEnv     map[string]string
	configureOptions provider.ExecOptions
	commands         []string
	execOptions      []provider.ExecOptions
	instances        []provider.Instance
	deletedNames     []string

	cloneCalls        int32
	execCalls         int32
	stopCalls         int32
	deleteCalls       int32
	canceledListCalls int32
	maxInventory      int32
	listCalls         int32
}

func (p *fakeProvider) Clone(_ context.Context, source, name string) error {
	atomic.AddInt32(&p.cloneCalls, 1)
	p.mu.Lock()
	p.instances = append(p.instances, provider.Instance{Name: name, ProviderID: "fake:" + name, Source: source, State: "running"})
	if len(p.instances) > int(atomic.LoadInt32(&p.maxInventory)) {
		atomic.StoreInt32(&p.maxInventory, int32(len(p.instances)))
	}
	p.mu.Unlock()
	return p.cloneErr
}

func (p *fakeProvider) Start(context.Context, string, provider.StartOptions) (*provider.RunningProcess, error) {
	return &provider.RunningProcess{}, p.startErr
}

func (p *fakeProvider) Exec(ctx context.Context, name string, command []string, opts provider.ExecOptions) (provider.ExecResult, error) {
	commandText := strings.Join(command, " ")
	p.mu.Lock()
	p.commands = append(p.commands, commandText)
	p.execOptions = append(p.execOptions, opts)
	p.mu.Unlock()
	if strings.Contains(strings.Join(command, " "), "configure-runner.sh") {
		p.mu.Lock()
		p.configureEnv = make(map[string]string, len(opts.Env))
		for key, value := range opts.Env {
			p.configureEnv[key] = value
		}
		p.configureOptions = opts
		p.mu.Unlock()
	}
	call := atomic.AddInt32(&p.execCalls, 1)
	if p.execFunc != nil {
		return p.execFunc(ctx, name, command, opts)
	}
	if int(call) <= len(p.execErrs) {
		return provider.ExecResult{}, p.execErrs[call-1]
	}
	if p.execErr == nil && strings.Contains(commandText, runnerProcessRunningSentinel) {
		return provider.ExecResult{Stdout: runnerProcessRunningSentinel + "\n"}, nil
	}
	return provider.ExecResult{}, p.execErr
}

func (p *fakeProvider) commandCount(fragment string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	count := 0
	for _, command := range p.commands {
		if strings.Contains(command, fragment) {
			count++
		}
	}
	return count
}

func (p *fakeProvider) logPathFor(fragment string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, command := range p.commands {
		if strings.Contains(command, fragment) {
			return p.execOptions[i].LogPath
		}
	}
	return ""
}

func (p *fakeProvider) IP(context.Context, string, int) (string, error) {
	if p.ipErr != nil {
		return "", p.ipErr
	}
	if p.ip == "" {
		return "127.0.0.1", nil
	}
	return p.ip, nil
}

func (p *fakeProvider) Stop(context.Context, string) error {
	atomic.AddInt32(&p.stopCalls, 1)
	return nil
}

func (p *fakeProvider) Delete(ctx context.Context, name string) error {
	atomic.AddInt32(&p.deleteCalls, 1)
	p.mu.Lock()
	p.deletedNames = append(p.deletedNames, name)
	p.mu.Unlock()
	if p.deleteFunc != nil {
		return p.deleteFunc(ctx, name)
	}
	if p.deleteErr != nil {
		return p.deleteErr
	}
	p.mu.Lock()
	remaining := p.instances[:0]
	for _, instance := range p.instances {
		if instance.Name != name {
			remaining = append(remaining, instance)
		}
	}
	p.instances = remaining
	p.mu.Unlock()
	return nil
}

func (p *fakeProvider) List(ctx context.Context) ([]provider.Instance, error) {
	atomic.AddInt32(&p.listCalls, 1)
	if ctx.Err() != nil {
		atomic.AddInt32(&p.canceledListCalls, 1)
		return nil, ctx.Err()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	result := append([]provider.Instance(nil), p.instances...)
	for i := range result {
		if result[i].ProviderID == "" {
			result[i].ProviderID = "fake:" + result[i].Name
		}
	}
	return result, p.listErr
}

type fakeGitHub struct {
	runner              gh.Runner
	runnerByNameFunc    func(context.Context, string) (gh.Runner, bool, error)
	waitRunner          gh.Runner
	waitErr             error
	waitFunc            func(context.Context, string, time.Duration) (gh.Runner, error)
	found               bool
	runnerErr           error
	deleteErr           error
	deleteFunc          func(context.Context, int64) error
	waitOnlineCalls     int32
	waitOnlineIdleCalls int32
	listRunners         []gh.Runner
	listErr             error
	listFunc            func(context.Context) ([]gh.Runner, error)
	registrationErr     error
	registrationFunc    func(context.Context) (gh.RegistrationToken, error)
	registrationToken   string
	policyResult        gh.RunnerGroupPolicyResult
	policyErr           error
	policyFunc          func(context.Context, string, config.RunnerGroupSecurityConfig) (gh.RunnerGroupPolicyResult, error)
	policyCalls         int32
	registrationCalls   int32
	deleteCalls         int32
	deletedIDs          []int64
	mu                  sync.Mutex
	listCalls           int32
	runnerByNameCalls   int32
}

func (g *fakeGitHub) OrganizationURL() string {
	return "https://github.test/example"
}

func (g *fakeGitHub) EvaluateRunnerGroupPolicy(ctx context.Context, group string, policy config.RunnerGroupSecurityConfig) (gh.RunnerGroupPolicyResult, error) {
	atomic.AddInt32(&g.policyCalls, 1)
	if g.policyFunc != nil {
		return g.policyFunc(ctx, group, policy)
	}
	return g.policyResult, g.policyErr
}

func (g *fakeGitHub) RegistrationToken(ctx context.Context) (gh.RegistrationToken, error) {
	atomic.AddInt32(&g.registrationCalls, 1)
	if g.registrationFunc != nil {
		return g.registrationFunc(ctx)
	}
	token := g.registrationToken
	if token == "" {
		token = "token"
	}
	return gh.RegistrationToken{Token: token}, g.registrationErr
}

func (g *fakeGitHub) ListRunners(ctx context.Context) ([]gh.Runner, error) {
	atomic.AddInt32(&g.listCalls, 1)
	if g.listFunc != nil {
		return g.listFunc(ctx)
	}
	return append([]gh.Runner(nil), g.listRunners...), g.listErr
}

func (g *fakeGitHub) RunnerByName(ctx context.Context, name string) (gh.Runner, bool, error) {
	atomic.AddInt32(&g.runnerByNameCalls, 1)
	if g.runnerByNameFunc != nil {
		return g.runnerByNameFunc(ctx, name)
	}
	return g.runner, g.found, g.runnerErr
}

func (g *fakeGitHub) WaitRunnerOnline(ctx context.Context, name string, timeout time.Duration) (gh.Runner, error) {
	atomic.AddInt32(&g.waitOnlineCalls, 1)
	return g.waitReady(ctx, name, timeout)
}

func (g *fakeGitHub) WaitRunnerOnlineIdle(ctx context.Context, name string, timeout time.Duration) (gh.Runner, error) {
	atomic.AddInt32(&g.waitOnlineIdleCalls, 1)
	return g.waitReady(ctx, name, timeout)
}

func (g *fakeGitHub) waitReady(ctx context.Context, name string, timeout time.Duration) (gh.Runner, error) {
	if g.waitFunc != nil {
		return g.waitFunc(ctx, name, timeout)
	}
	if g.waitErr != nil {
		return gh.Runner{}, g.waitErr
	}
	if g.waitRunner.ID != 0 {
		return g.waitRunner, nil
	}
	return g.runner, nil
}

func (g *fakeGitHub) DeleteRunnerIfExists(ctx context.Context, id int64) error {
	atomic.AddInt32(&g.deleteCalls, 1)
	g.mu.Lock()
	g.deletedIDs = append(g.deletedIDs, id)
	g.mu.Unlock()
	if g.deleteFunc != nil {
		return g.deleteFunc(ctx, id)
	}
	return g.deleteErr
}

func (g *fakeGitHub) DeleteRunnersByPrefix(context.Context, string) ([]gh.Runner, error) {
	return nil, nil
}
