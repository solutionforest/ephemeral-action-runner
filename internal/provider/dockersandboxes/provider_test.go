package dockersandboxes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
	"github.com/solutionforest/ephemeral-action-runner/internal/provider/dockersandboxes/staging"
)

const (
	testName            = "epar-sandbox-1"
	testID              = "9b6dbdf3-2ef4-47cb-8f55-55b26a790c8b"
	testTemplate        = "docker.io/docker/sandbox-templates:shell-docker"
	testDigest          = "sha256:39cf20eca8610000000000000000000000000000000000000000000000000000"
	emptyPortsJSON      = `[]`
	templateListJSON    = `{"images":[{"id":"39cf20eca861","repository":"docker.io/docker/sandbox-templates","tag":"shell-docker","flavor":"shell-docker","created_at":"2026-07-22T07:13:19Z","size":599103243}]}`
	healthyDiagnoseJSON = `{"version":"1.0","checks":[{"name":"daemon","status":"pass","message":"healthy","detail":"","hint":""}],"summary":{"pass":1,"warn":0,"fail":0,"skip":0}}`
)

var (
	testWorkspace  = providerTestWorkspace()
	readyListJSON  = `{"sandboxes":[{"id":"9b6dbdf3-2ef4-47cb-8f55-55b26a790c8b","name":"epar-sandbox-1","status":"running","workspaces":[` + strconv.Quote(testWorkspace) + `],"agent":"shell","additive_field":true}]}`
	inspectionJSON = `{"name":"epar-sandbox-1","agent":"shell","kits":[],"state":"running","image":"docker.io/docker/sandbox-templates:shell-docker","image_digest":"sha256:39cf20eca8610000000000000000000000000000000000000000000000000000","workspace":` + strconv.Quote(testWorkspace) + `,"network":"epar-sandbox-1","network_policy":{"scope":"global"},"proxy":"172.17.0.1:3128","sessions":0,"daemon_version":"fixture-current","daemon_uptime":"1h"}`
	testInstance   = provider.Instance{Name: testName, ProviderID: testID, Source: "shell", State: "running"}
)

func providerTestWorkspace() string {
	if runtime.GOOS == "windows" {
		return filepath.Join(filepath.VolumeName(os.TempDir())+string(filepath.Separator), "var", "lib", "epar", "staging", "job-1")
	}
	return filepath.Join(string(filepath.Separator), "var", "lib", "epar", "staging", "job-1")
}

func TestCreateDryRunFailsBeforeProviderSideEffects(t *testing.T) {
	p := NewWithDryRun("sbx", true)
	called := false
	p.runCommand = func(context.Context, commandRequest) (provider.ExecResult, error) {
		called = true
		return provider.ExecResult{}, nil
	}
	_, err := p.Create(context.Background(), provider.CreateRequest{Name: testName})
	if err == nil || !strings.Contains(err.Error(), "does not support dry-run") {
		t.Fatalf("Create() error = %v", err)
	}
	if called {
		t.Fatal("dry-run Create invoked a provider command")
	}
}

func TestCriticalTemplateAdmissionBlockFailsBeforeProviderSideEffects(t *testing.T) {
	p := New("sbx")
	called := false
	p.runCommand = func(context.Context, commandRequest) (provider.ExecResult, error) {
		called = true
		return provider.ExecResult{}, nil
	}
	p.SetTemplateAdmissionBlock("critical-revoked sha256:deadbeef")
	if reason, blocked := p.TemplateAdmissionBlock(); !blocked || !strings.Contains(reason, "critical-revoked") {
		t.Fatalf("admission block = %q, %t", reason, blocked)
	}
	_, err := p.Create(context.Background(), provider.CreateRequest{Name: testName})
	if err == nil || !strings.Contains(err.Error(), "critical-revoked") {
		t.Fatalf("Create() error = %v", err)
	}
	if called {
		t.Fatal("blocked admission invoked a provider command")
	}
	p.ClearTemplateAdmissionBlock()
	if _, blocked := p.TemplateAdmissionBlock(); blocked {
		t.Fatal("admission block was not cleared")
	}
}

func TestStartDaemonUsesExactDetachedCommand(t *testing.T) {
	p, done := scriptedProvider(t,
		commandStep{args: []string{"daemon", "start", "--detach"}},
	)
	if err := p.StartDaemon(context.Background()); err != nil {
		t.Fatal(err)
	}
	done()
}

func TestStartDaemonScrubsSSHAgentEnvironment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test helper uses a POSIX shell script")
	}
	t.Setenv("SSH_AUTH_SOCK", "/host/agent.sock")
	t.Setenv("SSH_AUTH_SOCK_GATEWAY", "gateway.example.test:3129")
	t.Setenv("SSH_AGENT_PID", "4242")
	helper := filepath.Join(t.TempDir(), "sbx-test-helper")
	script := "#!/bin/sh\n" +
		"test \"$#\" -eq 3 && test \"$1\" = daemon && test \"$2\" = start && test \"$3\" = --detach || exit 91\n" +
		"test -z \"${SSH_AUTH_SOCK:-}\" && test -z \"${SSH_AUTH_SOCK_GATEWAY:-}\" && test -z \"${SSH_AGENT_PID:-}\" || exit 92\n"
	if err := os.WriteFile(helper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := New(helper).StartDaemon(context.Background()); err != nil {
		t.Fatalf("StartDaemon() did not use exact scrubbed detached start: %v", err)
	}
}

func TestRecoverControlPlaneUsesExactColdStopAndDetachedStart(t *testing.T) {
	p, done := scriptedProvider(t,
		commandStep{args: []string{"daemon", "stop"}},
		commandStep{args: []string{"daemon", "status", "--json"}, result: provider.ExecResult{Stdout: `{"status":"running"}`}},
		commandStep{args: []string{"daemon", "status", "--json"}, result: provider.ExecResult{Stdout: `{"status":"stopped"}`}},
		commandStep{args: []string{"daemon", "status", "--json"}, result: provider.ExecResult{Stdout: `{"status":"stopped"}`}},
		commandStep{args: []string{"daemon", "start", "--detach"}},
		commandStep{args: []string{"daemon", "status", "--json"}, result: provider.ExecResult{Stdout: `{"status":"running"}`}},
	)
	var waits []time.Duration
	p.wait = func(_ context.Context, duration time.Duration) error {
		waits = append(waits, duration)
		return nil
	}
	if err := p.RecoverControlPlane(context.Background(), provider.ControlPlaneRecoveryRequest{Quiescence: time.Minute}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(waits, []time.Duration{daemonStatePollInterval, time.Minute}) {
		t.Fatalf("recovery waits = %#v, want stop-state poll followed by exact one-minute quiescence", waits)
	}
	done()
}

func TestRecoverControlPlaneRejectsUnboundedQuiescenceBeforeCommands(t *testing.T) {
	for _, duration := range []time.Duration{0, -time.Second, maximumRecoveryQuiescence + time.Nanosecond} {
		p := New("sbx-test-double")
		called := false
		p.runCommand = func(context.Context, commandRequest) (provider.ExecResult, error) {
			called = true
			return provider.ExecResult{}, nil
		}
		if err := p.RecoverControlPlane(context.Background(), provider.ControlPlaneRecoveryRequest{Quiescence: duration}); err == nil {
			t.Fatalf("RecoverControlPlane() accepted quiescence %s", duration)
		}
		if called {
			t.Fatalf("invalid quiescence %s reached provider commands", duration)
		}
	}
}

func TestRecoverControlPlaneDoesNotStartWhenStoppedStateIsUnknown(t *testing.T) {
	p, done := scriptedProvider(t,
		commandStep{args: []string{"daemon", "stop"}},
		commandStep{args: []string{"daemon", "status", "--json"}, result: provider.ExecResult{Stdout: `{"status":"wedged"}`}},
	)
	err := p.RecoverControlPlane(context.Background(), provider.ControlPlaneRecoveryRequest{Quiescence: time.Minute})
	if err == nil || errors.Unwrap(err) == nil || !strings.Contains(errors.Unwrap(err).Error(), "unsupported state") {
		t.Fatalf("RecoverControlPlane() error = %v, want unknown stopped-state refusal", err)
	}
	done()
}

func TestRecoverControlPlaneRechecksStoppedStateAfterQuiescence(t *testing.T) {
	p, done := scriptedProvider(t,
		commandStep{args: []string{"daemon", "stop"}},
		commandStep{args: []string{"daemon", "status", "--json"}, result: provider.ExecResult{Stdout: `{"status":"stopped"}`}},
		commandStep{args: []string{"daemon", "status", "--json"}, result: provider.ExecResult{Stdout: `{"status":"running"}`}},
	)
	p.wait = func(context.Context, time.Duration) error { return nil }
	err := p.RecoverControlPlane(context.Background(), provider.ControlPlaneRecoveryRequest{Quiescence: time.Minute})
	if err == nil || errors.Unwrap(err) == nil || !strings.Contains(errors.Unwrap(err).Error(), `state is "running"`) {
		t.Fatalf("RecoverControlPlane() error = %v, want post-quiescence stopped-state refusal", err)
	}
	done()
}

func TestRecoverControlPlaneSanitizesCommandOutput(t *testing.T) {
	const secret = "recovery-secret-from-daemon"
	p, done := scriptedProvider(t,
		commandStep{args: []string{"daemon", "stop"}, err: errors.New(secret)},
		commandStep{args: []string{"daemon", "status", "--json"}, result: provider.ExecResult{Stdout: `{"status":"wedged"}`}},
	)
	err := p.RecoverControlPlane(context.Background(), provider.ControlPlaneRecoveryRequest{Quiescence: time.Minute})
	if err == nil {
		t.Fatal("RecoverControlPlane() error = nil, want recovery failure")
	}
	if !errors.Is(err, provider.ErrControlPlaneRecoveryFailure) {
		t.Fatalf("RecoverControlPlane() error = %v, want recovery sentinel", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("RecoverControlPlane() leaked daemon output: %v", err)
	}
	done()
}

func TestRecoverControlPlaneQuiescenceBlocksConcurrentProviderCommands(t *testing.T) {
	p := New("sbx-test-double")
	quiescenceStarted := make(chan struct{})
	releaseQuiescence := make(chan struct{})
	recoveryDone := make(chan error, 1)
	var mu sync.Mutex
	var calls [][]string
	statusCalls := 0
	p.runCommand = func(_ context.Context, request commandRequest) (provider.ExecResult, error) {
		mu.Lock()
		calls = append(calls, append([]string(nil), request.args...))
		mu.Unlock()
		switch {
		case reflect.DeepEqual(request.args, []string{"daemon", "stop"}):
			return provider.ExecResult{}, nil
		case reflect.DeepEqual(request.args, []string{"daemon", "status", "--json"}):
			statusCalls++
			if statusCalls < 3 {
				return provider.ExecResult{Stdout: `{"status":"stopped"}`}, nil
			}
			return provider.ExecResult{Stdout: `{"status":"running"}`}, nil
		case reflect.DeepEqual(request.args, []string{"daemon", "start", "--detach"}):
			return provider.ExecResult{}, nil
		default:
			return provider.ExecResult{Stdout: readyListJSON}, nil
		}
	}
	p.wait = func(waitCtx context.Context, _ time.Duration) error {
		close(quiescenceStarted)
		select {
		case <-releaseQuiescence:
			return nil
		case <-waitCtx.Done():
			return waitCtx.Err()
		}
	}
	go func() {
		recoveryDone <- p.RecoverControlPlane(context.Background(), provider.ControlPlaneRecoveryRequest{Quiescence: time.Minute})
	}()
	select {
	case <-quiescenceStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("recovery did not reach quiescence")
	}
	operationCtx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if _, err := p.Inventory(operationCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("concurrent inventory error = %v, want context deadline while recovery owns quiescence", err)
	}
	mu.Lock()
	callCount := len(calls)
	mu.Unlock()
	if callCount != 2 {
		t.Fatalf("provider commands during quiescence = %d, want only stop and first status readback", callCount)
	}
	close(releaseQuiescence)
	if err := <-recoveryDone; err != nil {
		t.Fatalf("recovery failed after quiescence release: %v", err)
	}
}

func TestStartKeepaliveWaitsForControlPlaneRecovery(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test helper uses a POSIX shell script")
	}
	helper := filepath.Join(t.TempDir(), "sbx-test-helper")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\nsleep 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := New(helper)
	quiescenceStarted := make(chan struct{})
	releaseQuiescence := make(chan struct{})
	recoveryDone := make(chan error, 1)
	var waitOnce sync.Once
	statusCalls := 0
	p.runCommand = func(_ context.Context, request commandRequest) (provider.ExecResult, error) {
		switch {
		case reflect.DeepEqual(request.args, []string{"daemon", "stop"}):
			return provider.ExecResult{}, nil
		case reflect.DeepEqual(request.args, []string{"daemon", "status", "--json"}):
			statusCalls++
			if statusCalls < 3 {
				return provider.ExecResult{Stdout: `{"status":"stopped"}`}, nil
			}
			return provider.ExecResult{Stdout: `{"status":"running"}`}, nil
		case reflect.DeepEqual(request.args, []string{"daemon", "start", "--detach"}):
			return provider.ExecResult{}, nil
		default:
			t.Fatalf("unexpected recovery command: %#v", request.args)
			return provider.ExecResult{}, nil
		}
	}
	p.wait = func(waitCtx context.Context, _ time.Duration) error {
		waitOnce.Do(func() { close(quiescenceStarted) })
		select {
		case <-releaseQuiescence:
			return nil
		case <-waitCtx.Done():
			return waitCtx.Err()
		}
	}
	go func() {
		recoveryDone <- p.RecoverControlPlane(context.Background(), provider.ControlPlaneRecoveryRequest{Quiescence: time.Minute})
	}()
	select {
	case <-quiescenceStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("recovery did not reach quiescence")
	}
	keepaliveDone := make(chan error, 1)
	go func() {
		_, err := p.startKeepalive(context.Background(), testName, commandRequest{args: []string{"exec", "keepalive"}, operation: "test managed keepalive"})
		keepaliveDone <- err
	}()
	select {
	case err := <-keepaliveDone:
		t.Fatalf("keepalive launched during recovery quiescence: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseQuiescence)
	if err := <-recoveryDone; err != nil {
		t.Fatalf("recovery failed after quiescence release: %v", err)
	}
	if err := <-keepaliveDone; err != nil {
		t.Fatalf("keepalive failed after recovery release: %v", err)
	}
}

func TestRecoverControlPlaneQuiescenceCancellationPreventsStart(t *testing.T) {
	p, done := scriptedProvider(t,
		commandStep{args: []string{"daemon", "stop"}},
		commandStep{args: []string{"daemon", "status", "--json"}, result: provider.ExecResult{Stdout: `{"status":"stopped"}`}},
	)
	ctx, cancel := context.WithCancel(context.Background())
	p.wait = func(waitCtx context.Context, _ time.Duration) error {
		cancel()
		<-waitCtx.Done()
		return waitCtx.Err()
	}
	err := p.RecoverControlPlane(ctx, provider.ControlPlaneRecoveryRequest{Quiescence: time.Minute})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RecoverControlPlane() error = %v, want context cancellation", err)
	}
	done()
}

func TestRecoverControlPlaneStopTimeoutPreventsFurtherCommands(t *testing.T) {
	p := New("sbx-test-double")
	calls := 0
	p.runCommand = func(ctx context.Context, request commandRequest) (provider.ExecResult, error) {
		calls++
		if !reflect.DeepEqual(request.args, []string{"daemon", "stop"}) {
			t.Fatalf("command args = %#v, want exact daemon stop", request.args)
		}
		<-ctx.Done()
		return provider.ExecResult{}, ctx.Err()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	err := p.RecoverControlPlane(ctx, provider.ControlPlaneRecoveryRequest{Quiescence: time.Minute})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("RecoverControlPlane() error = %v, want context deadline", err)
	}
	if calls != 1 {
		t.Fatalf("commands after timed-out stop = %d, want only the stop command", calls)
	}
}

func TestParseDaemonControlStateFailsClosed(t *testing.T) {
	for _, test := range []struct {
		name    string
		fixture string
		want    daemonControlState
		wantErr bool
	}{
		{name: "running", fixture: `{"status":"running"}`, want: daemonControlStateRunning},
		{name: "stopped case insensitive", fixture: `{"status":"STOPPED"}`, want: daemonControlStateStopped},
		{name: "unknown", fixture: `{"status":"starting"}`, wantErr: true},
		{name: "non canonical", fixture: `{"status":" stopped "}`, wantErr: true},
		{name: "missing", fixture: `{}`, wantErr: true},
		{name: "trailing", fixture: `{"status":"running"} {}`, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseDaemonControlState([]byte(test.fixture))
			if test.wantErr {
				if err == nil {
					t.Fatalf("parseDaemonControlState(%s) = %q, want error", test.fixture, got)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("parseDaemonControlState(%s) = %q, %v, want %q", test.fixture, got, err, test.want)
			}
		})
	}
}

func TestRemoveTemplateUsesExactCacheIDAndRefusesLiveSandboxes(t *testing.T) {
	artifact := provider.TemplateArtifact{
		Reference: "docker.io/library/epar-template:one",
		CacheID:   "aaaaaaaaaaaa",
		Digest:    "sha256:aaaaaaaaaaaa0000000000000000000000000000000000000000000000000000",
	}
	template := `{"images":[{"id":"aaaaaaaaaaaa","repository":"docker.io/library/epar-template","tag":"one","flavor":"","created_at":"2026-07-29T00:00:00Z","size":1024}]}`
	p, done := scriptedProvider(t,
		commandStep{args: []string{"ls", "--json"}, result: provider.ExecResult{Stdout: `{"sandboxes":[]}`}},
		commandStep{args: []string{"template", "ls", "--json"}, result: provider.ExecResult{Stdout: template}},
		commandStep{args: []string{"template", "rm", "aaaaaaaaaaaa"}},
		commandStep{args: []string{"template", "ls", "--json"}, result: provider.ExecResult{Stdout: `{"images":[]}`}},
	)
	if err := p.RemoveTemplate(context.Background(), artifact); err != nil {
		t.Fatal(err)
	}
	done()

	p, done = scriptedProvider(t,
		commandStep{args: []string{"ls", "--json"}, result: provider.ExecResult{Stdout: readyListJSON}},
	)
	if err := p.RemoveTemplate(context.Background(), artifact); err == nil || !strings.Contains(err.Error(), "while 1 Docker Sandbox") {
		t.Fatalf("RemoveTemplate() error = %v, want live Sandbox refusal", err)
	}
	done()
}

func TestObserveTemplateRequiresExactReferenceAndCacheID(t *testing.T) {
	artifact := provider.TemplateArtifact{Reference: "docker.io/library/epar-template:one", CacheID: "aaaaaaaaaaaa"}
	p, done := scriptedProvider(t,
		commandStep{args: []string{"template", "ls", "--json"}, result: provider.ExecResult{Stdout: `{"images":[{"id":"aaaaaaaaaaaa","repository":"docker.io/library/epar-template","tag":"one","flavor":"","created_at":"2026-07-29T00:00:00Z","size":1024}]}`}},
	)
	exists, err := p.ObserveTemplate(context.Background(), artifact)
	if err != nil || !exists {
		t.Fatalf("ObserveTemplate() = %t, %v, want true", exists, err)
	}
	done()
}

func TestVerifyImportedTemplateAcceptsProviderAssignedOpaqueCacheID(t *testing.T) {
	artifact := provider.TemplateArtifact{
		Reference: "docker.io/library/epar-template:opaque",
		Digest:    "sha256:" + strings.Repeat("5", 64),
		CacheID:   "ec2006fea720",
	}
	p, done := scriptedProvider(t,
		commandStep{args: []string{"template", "ls", "--json"}, result: provider.ExecResult{Stdout: `{"images":[{"id":"ec2006fea720","repository":"docker.io/library/epar-template","tag":"opaque","created_at":"2026-08-10T00:00:00Z","size":1024}]}`}},
	)
	if err := p.VerifyImportedTemplate(context.Background(), artifact); err != nil {
		t.Fatal(err)
	}
	done()
}

func TestResolveTemplateCacheIDUsesAuthoritativeReferenceReadback(t *testing.T) {
	p, done := scriptedProvider(t,
		commandStep{args: []string{"template", "ls", "--json"}, result: provider.ExecResult{Stdout: `{"images":[{"id":"ec2006fea720","repository":"docker.io/library/epar-template","tag":"opaque","created_at":"2026-08-10T00:00:00Z","size":1024}]}`}},
	)
	cacheID, found, err := p.ResolveTemplateCacheID(context.Background(), "docker.io/library/epar-template:opaque")
	if err != nil || !found || cacheID != "ec2006fea720" {
		t.Fatalf("ResolveTemplateCacheID() = %q, %t, %v", cacheID, found, err)
	}
	done()
}

func TestResolveTemplateCacheIDRejectsAmbiguousReference(t *testing.T) {
	p, done := scriptedProvider(t,
		commandStep{args: []string{"template", "ls", "--json"}, result: provider.ExecResult{Stdout: `{"images":[{"id":"ec2006fea720","repository":"docker.io/library/epar-template","tag":"opaque","created_at":"2026-08-10T00:00:00Z","size":1024},{"id":"aaaaaaaaaaaa","repository":"docker.io/library/epar-template","tag":"opaque","created_at":"2026-08-10T00:01:00Z","size":1024}]}`}},
	)
	if _, _, err := p.ResolveTemplateCacheID(context.Background(), "docker.io/library/epar-template:opaque"); err == nil || !strings.Contains(err.Error(), "duplicate image reference") {
		t.Fatalf("ResolveTemplateCacheID() error = %v", err)
	}
	done()
}

type commandStep struct {
	args        []string
	result      provider.ExecResult
	err         error
	environment map[string]string
	stdin       string
	streamOut   string
	streamErr   string
}

type cancellationSignalWriter struct {
	once    sync.Once
	started chan struct{}
}

type fakeArchitectureEmulationEnabler struct {
	calls  int
	result architectureEmulationResult
	err    error
}

func (enabler *fakeArchitectureEmulationEnabler) Enable(context.Context, *Provider, provider.Instance) (architectureEmulationResult, error) {
	enabler.calls++
	return enabler.result, enabler.err
}

func (writer *cancellationSignalWriter) Write(data []byte) (int, error) {
	writer.once.Do(func() { close(writer.started) })
	return len(data), nil
}

func scriptedProvider(t *testing.T, steps ...commandStep) (*Provider, func()) {
	t.Helper()
	p := New("sbx-test-double")
	p.architectureEmulation = &fakeArchitectureEmulationEnabler{result: architectureEmulationResult{Mode: architectureEmulationRequired, Backend: "qemu", HandlerCount: 1}}
	index := 0
	p.runCommand = func(_ context.Context, request commandRequest) (provider.ExecResult, error) {
		t.Helper()
		if index >= len(steps) {
			t.Fatalf("unexpected command: %#v", request.args)
		}
		step := steps[index]
		index++
		if !reflect.DeepEqual(request.args, step.args) {
			t.Fatalf("command %d args = %#v, want %#v", index, request.args, step.args)
		}
		if !reflect.DeepEqual(request.environment, step.environment) {
			t.Fatalf("command %d environment = %#v, want %#v", index, request.environment, step.environment)
		}
		if request.stdin != nil {
			data, err := io.ReadAll(request.stdin)
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != step.stdin {
				t.Fatalf("command %d stdin = %q, want %q", index, data, step.stdin)
			}
		} else if step.stdin != "" {
			t.Fatalf("command %d did not receive stdin", index)
		}
		if request.stdout != nil {
			_, _ = io.WriteString(request.stdout, step.streamOut)
		}
		if request.stderr != nil {
			_, _ = io.WriteString(request.stderr, step.streamErr)
		}
		return step.result, step.err
	}
	return p, func() {
		t.Helper()
		if index != len(steps) {
			t.Fatalf("executed %d commands, want %d", index, len(steps))
		}
	}
}

func TestCreateUsesHealthyDiagnosticsAndExactArgv(t *testing.T) {
	wantCreate := []string{
		"create", "--name", testName,
		"--cpus", "4",
		"--memory", "8g",
		"--template", "docker.io/docker/sandbox-templates:shell-docker",
		"shell", testWorkspace,
	}
	p, done := scriptedProvider(t,
		commandStep{args: []string{"diagnose", "--output", "json"}, result: provider.ExecResult{Stdout: healthyDiagnoseJSON}},
		commandStep{args: []string{"template", "ls", "--json"}, result: provider.ExecResult{Stdout: templateListJSON}},
		commandStep{args: []string{"ls", "--json"}, result: provider.ExecResult{Stdout: `{"sandboxes":[]}`}},
		commandStep{
			args: wantCreate,
			environment: map[string]string{
				"DOCKER_SANDBOXES_ROOT_SIZE":   "40g",
				"DOCKER_SANDBOXES_DOCKER_SIZE": "60g",
			},
		},
		commandStep{args: []string{"ls", "--json"}, result: provider.ExecResult{Stdout: readyListJSON}},
		commandStep{args: []string{"ports", testName, "--json"}, result: provider.ExecResult{Stdout: emptyPortsJSON}},
		commandStep{args: []string{"inspect", "--json", testName}, result: provider.ExecResult{Stdout: inspectionJSON}},
		commandStep{args: []string{"exec", "-i", testName, "--", "bash", "-lc", directWorkspaceVerificationScript}},
	)
	var logOutput bytes.Buffer
	p.SetLogger(slog.New(slog.NewJSONHandler(&logOutput, nil)))
	instance, err := p.Create(context.Background(), provider.CreateRequest{
		Name:           testName,
		Template:       testTemplate,
		TemplateDigest: testDigest,
		StagingPath:    testWorkspace,
		CPUs:           4,
		Memory:         "8g",
		RootDisk:       "40g",
		DockerDisk:     "60g",
	})
	if err != nil {
		t.Fatal(err)
	}
	if instance.Name != testName || instance.ProviderID != testID {
		t.Fatalf("instance = %#v", instance)
	}
	if calls := p.architectureEmulation.(*fakeArchitectureEmulationEnabler).calls; calls != 1 {
		t.Fatalf("architecture emulation calls = %d, want 1", calls)
	}
	for _, expected := range []string{`"msg":"Docker Sandboxes architecture emulation enabled: backend=qemu registeredHandlers=1"`, `"backend":"qemu"`, `"registeredHandlers":1`} {
		if !strings.Contains(logOutput.String(), expected) {
			t.Fatalf("architecture emulation success log %q does not contain %q", logOutput.String(), expected)
		}
	}
	done()
}

func TestCreateKnownSandboxContainerFailureAddsSSHDaemonRemediation(t *testing.T) {
	const daemonFailure = "500 Internal Server Error: failed to run sandbox container"
	cause := errors.New("exit status 1")
	p, done := scriptedProvider(t,
		commandStep{args: []string{"diagnose", "--output", "json"}, result: provider.ExecResult{Stdout: healthyDiagnoseJSON}},
		commandStep{args: []string{"template", "ls", "--json"}, result: provider.ExecResult{Stdout: templateListJSON}},
		commandStep{args: []string{"ls", "--json"}, result: provider.ExecResult{Stdout: `{"sandboxes":[]}`}},
		commandStep{args: []string{"create", "--name", testName, "--cpus", "4", "--memory", "8g", "--template", testTemplate, "shell", testWorkspace}, environment: map[string]string{}, result: provider.ExecResult{Stderr: daemonFailure}, err: cause},
	)
	_, err := p.Create(context.Background(), validCreateRequest())
	if err == nil {
		t.Fatal("Create unexpectedly succeeded after the known sandbox-container failure")
	}
	if !errors.Is(err, cause) {
		t.Fatalf("Create error = %v, want original command error to remain wrapped", err)
	}
	if !errors.Is(err, provider.ErrControlPlaneAdmissionFailure) {
		t.Fatalf("Create error = %v, want typed control-plane admission failure", err)
	}
	for _, expected := range []string{
		sandboxContainerFailureSignature,
		"EPAR removes SSH-agent variables when its commands start a stopped daemon",
		"recoveryMode=exclusive-auto",
		"recoveryMode=observe never mutates the daemon",
		"Coordinate with every process using that daemon",
		"sbx daemon stop",
		"env -u SSH_AUTH_SOCK -u SSH_AUTH_SOCK_GATEWAY -u SSH_AGENT_PID sbx daemon start --detach",
	} {
		if !strings.Contains(err.Error(), expected) {
			t.Fatalf("Create error = %q, want remediation text %q", err, expected)
		}
	}
	done()
}

func TestCreateAdmissionClassificationUsesImmediateCreateStderrOnly(t *testing.T) {
	const signature = "500 Internal Server Error: failed to run sandbox container"
	if !hasSandboxCreateAdmissionSignature(signature) {
		t.Fatal("known create stderr signature was not classified")
	}
	if hasSandboxCreateAdmissionSignature("permission denied") {
		t.Fatal("unrelated create stderr was classified")
	}
}

func TestCreateDoesNotClassifyWrappedSignatureWithoutCreateStderr(t *testing.T) {
	const signature = "500 Internal Server Error: failed to run sandbox container"
	p, done := scriptedProvider(t,
		commandStep{args: []string{"diagnose", "--output", "json"}, result: provider.ExecResult{Stdout: healthyDiagnoseJSON}},
		commandStep{args: []string{"template", "ls", "--json"}, result: provider.ExecResult{Stdout: templateListJSON}},
		commandStep{args: []string{"ls", "--json"}, result: provider.ExecResult{Stdout: `{"sandboxes":[]}`}},
		commandStep{args: []string{"create", "--name", testName, "--cpus", "4", "--memory", "8g", "--template", testTemplate, "shell", testWorkspace}, environment: map[string]string{}, result: provider.ExecResult{Stderr: "permission denied"}, err: errors.New(signature)},
	)
	_, err := p.Create(context.Background(), validCreateRequest())
	if err == nil || errors.Is(err, provider.ErrControlPlaneAdmissionFailure) {
		t.Fatalf("Create error = %v, want ordinary failure without admission classification", err)
	}
	done()
}

func TestCreateRemovesEmptyStagingAfterImmediateCreateFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test helper uses a POSIX shell script")
	}
	root := filepath.Join(t.TempDir(), "staging")
	if _, err := staging.Open(root); err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(t.TempDir(), "sbx-test-helper")
	script := fmt.Sprintf(`#!/bin/sh
case "$1:$2" in
diagnose:--output) printf '%%s' '%s' ;;
template:ls) printf '%%s' '%s' ;;
ls:--json) printf '%%s' '{"sandboxes":[]}' ;;
create:*) printf '%%s\n' '500 Internal Server Error: failed to run sandbox container' >&2; exit 1 ;;
*) exit 91 ;;
esac
`, healthyDiagnoseJSON, templateListJSON)
	if err := os.WriteFile(helper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	p := New(helper)
	request := validCreateRequest()
	request.StagingPath = filepath.Join(root, testName)
	if _, err := p.Create(context.Background(), request); err == nil || !errors.Is(err, provider.ErrControlPlaneAdmissionFailure) {
		t.Fatalf("Create() error = %v, want typed admission failure", err)
	}
	if _, err := os.Stat(request.StagingPath); !os.IsNotExist(err) {
		t.Fatalf("failed create staging path stat error = %v, want exact empty staging removal", err)
	}
}

func TestCreateUnrelatedFailureDoesNotAddSSHDaemonRemediation(t *testing.T) {
	cause := errors.New("permission denied")
	p, done := scriptedProvider(t,
		commandStep{args: []string{"diagnose", "--output", "json"}, result: provider.ExecResult{Stdout: healthyDiagnoseJSON}},
		commandStep{args: []string{"template", "ls", "--json"}, result: provider.ExecResult{Stdout: templateListJSON}},
		commandStep{args: []string{"ls", "--json"}, result: provider.ExecResult{Stdout: `{"sandboxes":[]}`}},
		commandStep{args: []string{"create", "--name", testName, "--cpus", "4", "--memory", "8g", "--template", testTemplate, "shell", testWorkspace}, environment: map[string]string{}, err: cause},
	)
	_, err := p.Create(context.Background(), validCreateRequest())
	if err == nil || !errors.Is(err, cause) {
		t.Fatalf("Create error = %v, want original unrelated command error", err)
	}
	for _, misleading := range []string{sandboxContainerFailureSignature, "SSH-agent", "sbx daemon stop", "SSH_AUTH_SOCK"} {
		if strings.Contains(err.Error(), misleading) {
			t.Fatalf("unrelated Create error = %q, unexpectedly contains %q", err, misleading)
		}
	}
	done()
}

func TestCreateBestEffortContinuesAfterQEMUFailureWithExactReceipt(t *testing.T) {
	p, done := scriptedProvider(t,
		commandStep{args: []string{"diagnose", "--output", "json"}, result: provider.ExecResult{Stdout: healthyDiagnoseJSON}},
		commandStep{args: []string{"template", "ls", "--json"}, result: provider.ExecResult{Stdout: templateListJSON}},
		commandStep{args: []string{"ls", "--json"}, result: provider.ExecResult{Stdout: `{"sandboxes":[]}`}},
		commandStep{args: []string{"create", "--name", testName, "--cpus", "4", "--memory", "8g", "--template", testTemplate, "shell", testWorkspace}, environment: map[string]string{}},
		commandStep{args: []string{"ls", "--json"}, result: provider.ExecResult{Stdout: readyListJSON}},
		commandStep{args: []string{"ports", testName, "--json"}, result: provider.ExecResult{Stdout: emptyPortsJSON}},
		commandStep{args: []string{"inspect", "--json", testName}, result: provider.ExecResult{Stdout: inspectionJSON}},
		commandStep{args: []string{"exec", "-i", testName, "--", "bash", "-lc", directWorkspaceVerificationScript}},
		commandStep{args: []string{"exec", testName, "--", "sudo", "-n", architectureEmulationHelper}, err: errors.New("binfmt_misc is unavailable")},
		commandStep{args: []string{"exec", testName, "--", "sudo", "-n", nativeArchitectureHelper, "linux/amd64"}, result: provider.ExecResult{Stdout: `{"backend":"native","handlerCount":0,"platform":"linux/amd64"}`}},
	)
	p.architectureEmulation = bestEffortArchitectureEnabler{platform: "linux/amd64"}
	var logOutput bytes.Buffer
	p.SetLogger(slog.New(slog.NewJSONHandler(&logOutput, nil)))
	instance, err := p.Create(context.Background(), validCreateRequest())
	if err != nil {
		t.Fatal(err)
	}
	if instance.Name != testName || instance.ProviderID != testID || instance.ReceiptVersion != "v1" || len(instance.Receipt) == 0 {
		t.Fatalf("instance = %#v, want exact receipted identity", instance)
	}
	if !strings.Contains(logOutput.String(), "QEMU/binfmt unavailable; continuing with verified native") {
		t.Fatalf("best-effort create log = %q, want native fallback warning", logOutput.String())
	}
	done()
}

func TestNewWithArchitectureModeMapsEveryValidatedMode(t *testing.T) {
	for _, test := range []struct {
		mode string
		want any
	}{
		{mode: architectureEmulationBestEffort, want: bestEffortArchitectureEnabler{}},
		{mode: architectureEmulationRequired, want: qemuBinfmtEnabler{}},
		{mode: architectureEmulationNativeOnly, want: nativeArchitectureEnabler{}},
	} {
		t.Run(test.mode, func(t *testing.T) {
			p := NewWithArchitectureMode("sbx", false, test.mode, "linux/amd64")
			if reflect.TypeOf(p.architectureEmulation) != reflect.TypeOf(test.want) {
				t.Fatalf("architecture enabler type = %T, want %T", p.architectureEmulation, test.want)
			}
		})
	}
	if p := NewWithArchitectureMode("sbx", false, "invalid", "linux/amd64"); p.architectureEmulation != nil {
		t.Fatalf("invalid mode enabler = %T, want nil fail-closed enabler", p.architectureEmulation)
	}
}

func TestNewDefaultsToNativeOnly(t *testing.T) {
	p := New("sbx")
	enabler, ok := p.architectureEmulation.(nativeArchitectureEnabler)
	if !ok {
		t.Fatalf("default architecture enabler = %T, want nativeArchitectureEnabler", p.architectureEmulation)
	}
	if enabler.platform != defaultNativePlatform() {
		t.Fatalf("default native platform = %q, want %q", enabler.platform, defaultNativePlatform())
	}
}

func TestExperimentalV2ReceiptRemainsReadableForCleanup(t *testing.T) {
	payload, err := json.Marshal(map[string]any{
		"schemaVersion":   2,
		"stagingPath":     testWorkspace,
		"stagingIdentity": "test-staging-identity",
		"template":        testTemplate,
		"templateDigest":  testDigest,
		"architectureEmulation": map[string]any{
			"linux/amd64": map[string]any{"backend": "qemu", "handler": "qemu-x86_64"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := parseExperimentalInstanceReceipt(payload)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.SchemaVersion != 2 || receipt.StagingPath != testWorkspace || receipt.TemplateDigest != testDigest {
		t.Fatalf("experimental cleanup receipt = %#v", receipt)
	}
}

func TestQEMUBinfmtEnablerUsesExactGuestHelper(t *testing.T) {
	p, done := scriptedProvider(t,
		commandStep{
			args:   []string{"exec", testName, "--", "sudo", "-n", architectureEmulationHelper},
			result: provider.ExecResult{Stdout: `{"backend":"qemu","handlerCount":9}`},
		},
	)
	result, err := (qemuBinfmtEnabler{}).Enable(context.Background(), p, testInstance)
	if err != nil {
		t.Fatal(err)
	}
	if result.Mode != architectureEmulationRequired || result.Backend != "qemu" || result.HandlerCount != 9 {
		t.Fatalf("architecture emulation result = %#v", result)
	}
	done()
}

func TestNativeArchitectureEnablerUsesExactGuestHelper(t *testing.T) {
	p, done := scriptedProvider(t,
		commandStep{
			args:   []string{"exec", testName, "--", "sudo", "-n", nativeArchitectureHelper, "linux/amd64"},
			result: provider.ExecResult{Stdout: `{"backend":"native","handlerCount":0,"platform":"linux/amd64"}`},
		},
	)
	result, err := (nativeArchitectureEnabler{platform: "linux/amd64"}).Enable(context.Background(), p, testInstance)
	if err != nil {
		t.Fatal(err)
	}
	if result.Mode != architectureEmulationNativeOnly || result.Backend != "native" || result.HandlerCount != 0 || result.Platform != "linux/amd64" {
		t.Fatalf("native-only architecture result = %#v", result)
	}
	done()
}

func TestNativeArchitectureEnablerFailsClosedOnUnsupportedEvidence(t *testing.T) {
	for _, output := range []string{
		`{"backend":"native","handlerCount":1,"platform":"linux/amd64"}`,
		`{"backend":"native","handlerCount":0,"platform":"linux/arm64"}`,
		`{"backend":"qemu","handlerCount":1,"platform":"linux/amd64"}`,
		`not-json`,
	} {
		t.Run(output, func(t *testing.T) {
			p, done := scriptedProvider(t,
				commandStep{args: []string{"exec", testName, "--", "sudo", "-n", nativeArchitectureHelper, "linux/amd64"}, result: provider.ExecResult{Stdout: output}},
			)
			if _, err := (nativeArchitectureEnabler{platform: "linux/amd64"}).Enable(context.Background(), p, testInstance); err == nil || !strings.Contains(err.Error(), "unsupported evidence") {
				t.Fatalf("Enable() error = %v", err)
			}
			done()
		})
	}
}

func TestBestEffortArchitectureUsesQEMUWhenAvailable(t *testing.T) {
	p, done := scriptedProvider(t,
		commandStep{
			args:   []string{"exec", testName, "--", "sudo", "-n", architectureEmulationHelper},
			result: provider.ExecResult{Stdout: `{"backend":"qemu","handlerCount":9}`},
		},
	)
	result, err := (bestEffortArchitectureEnabler{platform: "linux/amd64"}).Enable(context.Background(), p, testInstance)
	if err != nil {
		t.Fatal(err)
	}
	if result.Mode != architectureEmulationBestEffort || result.Backend != "qemu" || result.HandlerCount != 9 || result.Warning != "" {
		t.Fatalf("best-effort QEMU result = %#v", result)
	}
	done()
}

func TestBestEffortArchitectureFallsBackToVerifiedNative(t *testing.T) {
	for _, handlerCount := range []int{0, 2} {
		t.Run(strconv.Itoa(handlerCount), func(t *testing.T) {
			p, done := scriptedProvider(t,
				commandStep{args: []string{"exec", testName, "--", "sudo", "-n", architectureEmulationHelper}, err: errors.New("binfmt_misc is unavailable")},
				commandStep{
					args:   []string{"exec", testName, "--", "sudo", "-n", nativeArchitectureHelper, "linux/amd64"},
					result: provider.ExecResult{Stdout: fmt.Sprintf(`{"backend":"native","handlerCount":%d,"platform":"linux/amd64"}`, handlerCount)},
				},
			)
			result, err := (bestEffortArchitectureEnabler{platform: "linux/amd64"}).Enable(context.Background(), p, testInstance)
			if err != nil {
				t.Fatal(err)
			}
			if result.Mode != architectureEmulationBestEffort || result.Backend != "native" || result.HandlerCount != handlerCount || result.Platform != "linux/amd64" || !strings.Contains(result.Warning, "binfmt_misc is unavailable") {
				t.Fatalf("best-effort native result = %#v", result)
			}
			done()
		})
	}
}

func TestBestEffortArchitectureBoundsFallbackWarning(t *testing.T) {
	p, done := scriptedProvider(t,
		commandStep{args: []string{"exec", testName, "--", "sudo", "-n", architectureEmulationHelper}, err: errors.New(strings.Repeat("x", architectureWarningLimit+100))},
		commandStep{args: []string{"exec", testName, "--", "sudo", "-n", nativeArchitectureHelper, "linux/amd64"}, result: provider.ExecResult{Stdout: `{"backend":"native","handlerCount":0,"platform":"linux/amd64"}`}},
	)
	result, err := (bestEffortArchitectureEnabler{platform: "linux/amd64"}).Enable(context.Background(), p, testInstance)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Warning) != architectureWarningLimit {
		t.Fatalf("warning length = %d, want %d", len(result.Warning), architectureWarningLimit)
	}
	done()
}

func TestBestEffortArchitectureFailsWhenNativeVerificationFails(t *testing.T) {
	p, done := scriptedProvider(t,
		commandStep{args: []string{"exec", testName, "--", "sudo", "-n", architectureEmulationHelper}, err: errors.New("binfmt_misc is unavailable")},
		commandStep{args: []string{"exec", testName, "--", "sudo", "-n", nativeArchitectureHelper, "linux/amd64"}, result: provider.ExecResult{Stdout: `{"backend":"native","handlerCount":0,"platform":"linux/arm64"}`}},
	)
	if _, err := (bestEffortArchitectureEnabler{platform: "linux/amd64"}).Enable(context.Background(), p, testInstance); err == nil || !strings.Contains(err.Error(), "QEMU/binfmt activation failed") || !strings.Contains(err.Error(), "native architecture verification also failed") {
		t.Fatalf("Enable() error = %v", err)
	}
	done()
}

func TestNativeOnlyArchitectureCapabilityLogsVisibleWarning(t *testing.T) {
	var logOutput bytes.Buffer
	p := New("sbx-test-double")
	p.SetLogger(slog.New(slog.NewJSONHandler(&logOutput, nil)))
	p.logArchitectureCapability(testName, architectureEmulationResult{
		Mode: architectureEmulationNativeOnly, Backend: "native", HandlerCount: 0, Platform: "linux/amd64",
	})
	for _, expected := range []string{
		`"level":"WARN"`,
		`"msg":"Docker Sandboxes native-only architecture verified: platform=linux/amd64; foreign-architecture containers are unsupported"`,
		`"architectureEmulation":"native-only"`,
		`"backend":"native"`,
		`"platform":"linux/amd64"`,
		`"registeredHandlers":0`,
	} {
		if !strings.Contains(logOutput.String(), expected) {
			t.Fatalf("native-only architecture warning %q does not contain %q", logOutput.String(), expected)
		}
	}
}

func TestBestEffortNativeFallbackLogsVisibleWarning(t *testing.T) {
	var logOutput bytes.Buffer
	p := New("sbx-test-double")
	p.SetLogger(slog.New(slog.NewJSONHandler(&logOutput, nil)))
	p.logArchitectureCapability(testName, architectureEmulationResult{
		Mode: architectureEmulationBestEffort, Backend: "native", HandlerCount: 0, Platform: "linux/amd64", Warning: "binfmt_misc is unavailable",
	})
	for _, expected := range []string{
		`"level":"WARN"`,
		`"msg":"Docker Sandboxes QEMU/binfmt unavailable; continuing with verified native platform=linux/amd64; foreign-architecture containers may fail"`,
		`"architectureEmulation":"best-effort"`,
		`"backend":"native"`,
		`"qemuError":"binfmt_misc is unavailable"`,
	} {
		if !strings.Contains(logOutput.String(), expected) {
			t.Fatalf("best-effort architecture warning %q does not contain %q", logOutput.String(), expected)
		}
	}
}

func TestArchitectureCapabilityLogIsEmittedOncePerLiveInstance(t *testing.T) {
	var logOutput bytes.Buffer
	p := New("sbx-test-double")
	p.SetLogger(slog.New(slog.NewJSONHandler(&logOutput, nil)))
	evidence := architectureEmulationResult{Mode: architectureEmulationBestEffort, Backend: "native", Platform: "linux/amd64", Warning: "binfmt_misc is unavailable"}
	p.logArchitectureCapability(testName, evidence)
	p.logArchitectureCapability(testName, evidence)
	if got := strings.Count(logOutput.String(), "QEMU/binfmt unavailable; continuing with verified native"); got != 1 {
		t.Fatalf("fallback warning count = %d, want 1: %s", got, logOutput.String())
	}
}

func TestQEMUBinfmtEnablerFailsClosedOnUnsupportedEvidence(t *testing.T) {
	for _, output := range []string{
		`{"backend":"qemu","handlerCount":0}`,
		`{"backend":"rosetta","handlerCount":1}`,
		`not-json`,
	} {
		t.Run(output, func(t *testing.T) {
			p, done := scriptedProvider(t,
				commandStep{args: []string{"exec", testName, "--", "sudo", "-n", architectureEmulationHelper}, result: provider.ExecResult{Stdout: output}},
			)
			if _, err := (qemuBinfmtEnabler{}).Enable(context.Background(), p, testInstance); err == nil || !strings.Contains(err.Error(), "unsupported evidence") {
				t.Fatalf("Enable() error = %v", err)
			}
			done()
		})
	}
}

func TestCreateReturnsExactReceiptWhenPostCreateVerificationFails(t *testing.T) {
	p, done := scriptedProvider(t,
		commandStep{args: []string{"diagnose", "--output", "json"}, result: provider.ExecResult{Stdout: healthyDiagnoseJSON}},
		commandStep{args: []string{"template", "ls", "--json"}, result: provider.ExecResult{Stdout: templateListJSON}},
		commandStep{args: []string{"ls", "--json"}, result: provider.ExecResult{Stdout: `{"sandboxes":[]}`}},
		commandStep{args: []string{"create", "--name", testName, "--cpus", "4", "--memory", "8g", "--template", testTemplate, "shell", testWorkspace}, environment: map[string]string{}},
		commandStep{args: []string{"ls", "--json"}, result: provider.ExecResult{Stdout: readyListJSON}},
		commandStep{args: []string{"ports", testName, "--json"}, result: provider.ExecResult{Stdout: emptyPortsJSON}},
		commandStep{args: []string{"inspect", "--json", testName}, result: provider.ExecResult{Stdout: inspectionJSON}},
		commandStep{args: []string{"exec", "-i", testName, "--", "bash", "-lc", directWorkspaceVerificationScript}, err: errors.New("workspace verification failed")},
	)
	instance, err := p.Create(context.Background(), validCreateRequest())
	if err == nil || !strings.Contains(err.Error(), "workspace verification failed") {
		t.Fatalf("Create() error = %v", err)
	}
	if instance.Name != testName || instance.ProviderID != testID || instance.ReceiptVersion != "v1" || len(instance.Receipt) == 0 {
		t.Fatalf("Create() partial instance = %#v, want exact receipted identity", instance)
	}
	var receipt instanceReceipt
	if err := json.Unmarshal(instance.Receipt, &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.StagingPath != testWorkspace || receipt.StagingIdentity == "" || receipt.Template != testTemplate || receipt.TemplateDigest != testDigest {
		t.Fatalf("Create() receipt = %#v", receipt)
	}
	done()
}

func TestCreateReturnsExactReceiptWhenArchitectureEmulationSetupFails(t *testing.T) {
	p, done := scriptedProvider(t,
		commandStep{args: []string{"diagnose", "--output", "json"}, result: provider.ExecResult{Stdout: healthyDiagnoseJSON}},
		commandStep{args: []string{"template", "ls", "--json"}, result: provider.ExecResult{Stdout: templateListJSON}},
		commandStep{args: []string{"ls", "--json"}, result: provider.ExecResult{Stdout: `{"sandboxes":[]}`}},
		commandStep{args: []string{"create", "--name", testName, "--cpus", "4", "--memory", "8g", "--template", testTemplate, "shell", testWorkspace}, environment: map[string]string{}},
		commandStep{args: []string{"ls", "--json"}, result: provider.ExecResult{Stdout: readyListJSON}},
		commandStep{args: []string{"ports", testName, "--json"}, result: provider.ExecResult{Stdout: emptyPortsJSON}},
		commandStep{args: []string{"inspect", "--json", testName}, result: provider.ExecResult{Stdout: inspectionJSON}},
		commandStep{args: []string{"exec", "-i", testName, "--", "bash", "-lc", directWorkspaceVerificationScript}},
	)
	p.architectureEmulation = &fakeArchitectureEmulationEnabler{err: errors.New("binfmt_misc is unavailable")}
	instance, err := p.Create(context.Background(), validCreateRequest())
	if err == nil || !strings.Contains(err.Error(), "binfmt_misc is unavailable") {
		t.Fatalf("Create() error = %v", err)
	}
	if instance.Name != testName || instance.ProviderID != testID || instance.ReceiptVersion != "v1" || len(instance.Receipt) == 0 {
		t.Fatalf("Create() partial instance = %#v, want exact receipted identity", instance)
	}
	if calls := p.architectureEmulation.(*fakeArchitectureEmulationEnabler).calls; calls != 1 {
		t.Fatalf("architecture emulation calls = %d, want 1", calls)
	}
	done()
}

func TestSplitTemplateReferenceCanonicalizesDockerHubNames(t *testing.T) {
	tests := map[string]string{
		"epar-template:version":                       "docker.io/library/epar-template",
		"docker/sandbox-templates:shell-docker":       "docker.io/docker/sandbox-templates",
		"docker.io/library/epar-template:version":     "docker.io/library/epar-template",
		"registry.example.test/team/template:version": "registry.example.test/team/template",
		"localhost:5000/team/template:version":        "localhost:5000/team/template",
	}
	for reference, expectedRepository := range tests {
		repository, _, err := splitTemplateReference(reference)
		if err != nil {
			t.Fatalf("splitTemplateReference(%q): %v", reference, err)
		}
		if repository != expectedRepository {
			t.Fatalf("splitTemplateReference(%q) repository = %q, want %q", reference, repository, expectedRepository)
		}
	}
}

func TestCreateFailsClosedOnCachedTemplateIdentityMismatch(t *testing.T) {
	mismatch := strings.Replace(templateListJSON, "39cf20eca861", "aaaaaaaaaaaa", 1)
	p, done := scriptedProvider(t,
		commandStep{args: []string{"diagnose", "--output", "json"}, result: provider.ExecResult{Stdout: healthyDiagnoseJSON}},
		commandStep{args: []string{"template", "ls", "--json"}, result: provider.ExecResult{Stdout: mismatch}},
	)
	if _, err := p.Create(context.Background(), validCreateRequest()); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("err = %v", err)
	}
	done()
}

func TestCreateSucceedsWithImportedTemplateAndNoDockerStagingImage(t *testing.T) {
	p, done := scriptedProvider(t,
		commandStep{args: []string{"diagnose", "--output", "json"}, result: provider.ExecResult{Stdout: healthyDiagnoseJSON}},
		commandStep{args: []string{"template", "ls", "--json"}, result: provider.ExecResult{Stdout: templateListJSON}},
		commandStep{args: []string{"ls", "--json"}, result: provider.ExecResult{Stdout: `{"sandboxes":[]}`}},
		commandStep{args: []string{"create", "--name", testName, "--cpus", "4", "--memory", "8g", "--template", testTemplate, "shell", testWorkspace}, environment: map[string]string{}},
		commandStep{args: []string{"ls", "--json"}, result: provider.ExecResult{Stdout: readyListJSON}},
		commandStep{args: []string{"ports", testName, "--json"}, result: provider.ExecResult{Stdout: emptyPortsJSON}},
		commandStep{args: []string{"inspect", "--json", testName}, result: provider.ExecResult{Stdout: inspectionJSON}},
		commandStep{args: []string{"exec", "-i", testName, "--", "bash", "-lc", directWorkspaceVerificationScript}},
	)
	if _, err := p.Create(context.Background(), validCreateRequest()); err != nil {
		t.Fatal(err)
	}
	done()
}

func TestCreateUsesActiveProviderAssignedOpaqueCacheID(t *testing.T) {
	opaqueTemplateList := strings.Replace(templateListJSON, "39cf20eca861", "ec2006fea720", 1)
	opaqueInspection := strings.Replace(inspectionJSON, testDigest, "sha256:ec2006fea720"+strings.Repeat("9", 52), 1)
	p, done := scriptedProvider(t,
		commandStep{args: []string{"diagnose", "--output", "json"}, result: provider.ExecResult{Stdout: healthyDiagnoseJSON}},
		commandStep{args: []string{"template", "ls", "--json"}, result: provider.ExecResult{Stdout: opaqueTemplateList}},
		commandStep{args: []string{"ls", "--json"}, result: provider.ExecResult{Stdout: `{"sandboxes":[]}`}},
		commandStep{args: []string{"create", "--name", testName, "--cpus", "4", "--memory", "8g", "--template", testTemplate, "shell", testWorkspace}, environment: map[string]string{}},
		commandStep{args: []string{"ls", "--json"}, result: provider.ExecResult{Stdout: readyListJSON}},
		commandStep{args: []string{"ports", testName, "--json"}, result: provider.ExecResult{Stdout: emptyPortsJSON}},
		commandStep{args: []string{"inspect", "--json", testName}, result: provider.ExecResult{Stdout: opaqueInspection}},
		commandStep{args: []string{"exec", "-i", testName, "--", "bash", "-lc", directWorkspaceVerificationScript}},
	)
	active := provider.TemplateArtifact{Reference: testTemplate, Digest: testDigest, CacheID: "ec2006fea720", Platform: "linux/amd64", RootDisk: "20GiB"}
	if err := p.ActivateTemplate(active); err != nil {
		t.Fatal(err)
	}
	request := validCreateRequest()
	request.Template = ""
	request.TemplateDigest = ""
	if _, err := p.Create(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	done()
}

func TestInstanceAdmissionUsesExactInspectionAndRejectsAttachedCapabilities(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		p, done := identityAdmissionScript(t, commandStep{args: []string{"inspect", "--json", testName}, result: provider.ExecResult{Stdout: inspectionJSON}})
		if err := p.VerifyInstanceAdmission(context.Background(), testInstance); err != nil {
			t.Fatal(err)
		}
		done()
	})
	t.Run("different daemon version", func(t *testing.T) {
		fixture := strings.Replace(inspectionJSON, `"daemon_version":"fixture-current"`, `"daemon_version":"fixture-next"`, 1)
		p, done := identityAdmissionScript(t, commandStep{args: []string{"inspect", "--json", testName}, result: provider.ExecResult{Stdout: fixture}})
		if err := p.VerifyInstanceAdmission(context.Background(), testInstance); err != nil {
			t.Fatal(err)
		}
		done()
	})
	for _, mutation := range []struct {
		name     string
		old      string
		new      string
		metadata string
	}{
		{name: "kit", old: `"kits":[]`, new: `"kits":[{"name":"docker-auth","token":"kit-token-metadata"}]`, metadata: "kit-token-metadata"},
		{name: "published_ports", old: `"state":"running"`, new: `"state":"running","published_ports":["8080:80"]`, metadata: "8080:80"},
		{name: "ports", old: `"state":"running"`, new: `"state":"running","ports":[{"host_port":8080}]`, metadata: "8080"},
		{name: "auth", old: `"state":"running"`, new: `"state":"running","auth":{"provider":"registry-auth-metadata"}`, metadata: "registry-auth-metadata"},
		{name: "auth_mode", old: `"state":"running"`, new: `"state":"running","auth_mode":"docker-login-metadata"`, metadata: "docker-login-metadata"},
		{name: "docker_auth", old: `"state":"running"`, new: `"state":"running","docker_auth":{"registry":"registry-token-metadata"}`, metadata: "registry-token-metadata"},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			fixture := strings.Replace(inspectionJSON, mutation.old, mutation.new, 1)
			p, done := identityAdmissionScript(t, commandStep{args: []string{"inspect", "--json", testName}, result: provider.ExecResult{Stdout: fixture}})
			err := p.VerifyInstanceAdmission(context.Background(), testInstance)
			if err == nil {
				t.Fatal("attached capability was accepted")
			}
			if mutation.metadata != "" && strings.Contains(err.Error(), mutation.metadata) {
				t.Fatalf("attached capability metadata leaked in error: %v", err)
			}
			done()
		})
	}
}

func TestInstanceAdmissionIgnoresProviderSecretsMetadata(t *testing.T) {
	for _, test := range []struct {
		name  string
		value string
	}{
		{name: "missing", value: ""},
		{name: "string", value: `"registry-secret-metadata"`},
		{name: "number", value: `42`},
		{name: "boolean", value: `true`},
		{name: "object", value: `{"token":"registry-secret-metadata"}`},
		{name: "array", value: `["registry-secret-metadata",{"kind":"service"}]`},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := inspectionJSON
			if test.value != "" {
				fixture = strings.Replace(inspectionJSON, `"state":"running"`, `"state":"running","secrets":`+test.value, 1)
			}
			p, done := identityAdmissionScript(t, commandStep{args: []string{"inspect", "--json", testName}, result: provider.ExecResult{Stdout: fixture}})
			if err := p.VerifyInstanceAdmission(context.Background(), testInstance); err != nil {
				t.Fatalf("provider secrets metadata affected admission: %v", err)
			}
			done()
		})
	}
}

func TestInstanceAdmissionRejectsCredentialMetadataWithoutEchoingIt(t *testing.T) {
	const metadata = "registry-token-metadata"
	fixture := strings.Replace(inspectionJSON, `"state":"running"`, `"state":"running","docker_auth":{"registry":"`+metadata+`"}`, 1)
	p, done := identityAdmissionScript(t, commandStep{args: []string{"inspect", "--json", testName}, result: provider.ExecResult{Stdout: fixture}})
	err := p.VerifyInstanceAdmission(context.Background(), testInstance)
	if err == nil || !strings.Contains(err.Error(), "forbidden attached capability") || strings.Contains(err.Error(), metadata) {
		t.Fatalf("credential capability error = %v", err)
	}
	done()
}

func TestInstanceAdmissionRechecksConfiguredArchitectureCapability(t *testing.T) {
	p, done := identityAdmissionScript(t, commandStep{args: []string{"inspect", "--json", testName}, result: provider.ExecResult{Stdout: inspectionJSON}})
	p.architectureEmulation = &fakeArchitectureEmulationEnabler{err: errors.New("required QEMU handlers are unavailable")}
	if err := p.VerifyInstanceAdmission(context.Background(), testInstance); err == nil || !strings.Contains(err.Error(), "required QEMU handlers are unavailable") {
		t.Fatalf("VerifyInstanceAdmission() error = %v, want configured architecture rejection", err)
	}
	if calls := p.architectureEmulation.(*fakeArchitectureEmulationEnabler).calls; calls != 1 {
		t.Fatalf("architecture admission calls = %d, want 1", calls)
	}
	done()
}

func TestInstanceAdmissionRejectsPublishedPortInventory(t *testing.T) {
	for _, test := range []struct {
		name    string
		fixture string
		message string
	}{
		{name: "published", fixture: `[{"host_ip":"127.0.0.1","host_port":60002,"sandbox_port":9418,"protocol":"tcp"}]`, message: "forbidden published port"},
		{name: "null", fixture: `null`, message: "unsupported JSON schema"},
		{name: "wrapper", fixture: `{"ports":[]}`, message: "unsupported JSON schema"},
		{name: "trailing-json", fixture: `[] {}`, message: "unsupported JSON schema"},
	} {
		t.Run(test.name, func(t *testing.T) {
			p, done := scriptedProvider(t,
				commandStep{args: []string{"ls", "--json"}, result: provider.ExecResult{Stdout: readyListJSON}},
				commandStep{args: []string{"ports", testName, "--json"}, result: provider.ExecResult{Stdout: test.fixture}},
			)
			if err := p.VerifyInstanceAdmission(context.Background(), testInstance); err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("published port admission error = %v", err)
			}
			done()
		})
	}
}

func TestChildEnvironmentStripsHostSSHAgent(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "/host/agent.sock")
	t.Setenv("SSH_AUTH_SOCK_GATEWAY", "gateway.example.test:3129")
	t.Setenv("SSH_AGENT_PID", "4242")
	environment := childEnvironment(nil)
	for _, item := range environment {
		key, _, _ := strings.Cut(item, "=")
		if strings.EqualFold(key, "SSH_AUTH_SOCK") || strings.EqualFold(key, "SSH_AUTH_SOCK_GATEWAY") || strings.EqualFold(key, "SSH_AGENT_PID") {
			t.Fatalf("host SSH agent variable survived child environment filtering: %q", key)
		}
	}
}

func TestDirectWorkspaceVerificationRejectsSSHAgentForwardingWithRemediation(t *testing.T) {
	for _, required := range []string{"SSH_AUTH_SOCK", "SSH_AUTH_SOCK_GATEWAY", "SSH_AGENT_PID", "restart it with SSH_AUTH_SOCK"} {
		if !strings.Contains(directWorkspaceVerificationScript, required) {
			t.Fatalf("direct workspace verification omitted SSH-agent guardrail %q", required)
		}
	}
	for name, environment := range map[string]string{
		"socket":  "SSH_AUTH_SOCK=/tmp/host-agent.sock",
		"gateway": "SSH_AUTH_SOCK_GATEWAY=gateway.example.test:3129",
		"pid":     "SSH_AGENT_PID=4242",
	} {
		t.Run(name, func(t *testing.T) {
			command := exec.Command("bash", "-c", directWorkspaceVerificationScript)
			command.Env = append(childEnvironment(nil), environment)
			output, err := command.CombinedOutput()
			if err == nil {
				t.Fatal("workspace verification accepted host SSH-agent forwarding")
			}
			if !strings.Contains(string(output), "Docker Sandboxes exposed host SSH-agent forwarding") {
				t.Fatalf("workspace verification output = %q, want actionable SSH-agent diagnostic", output)
			}
		})
	}
}

func TestTemplateInventorySchemaFailsClosed(t *testing.T) {
	for _, fixture := range []string{
		`[]`,
		`{"templates":[]}`,
		`{"images":null}`,
		`{"images":[{"id":"39cf20eca861","repository":"docker.io/docker/sandbox-templates","tag":"shell-docker","flavor":"shell-docker","created_at":"not-a-time","size":599103243}]}`,
		`{"images":[{"id":"39cf20eca861","repository":"docker.io/docker/sandbox-templates","tag":"shell-docker","flavor":"shell-docker","created_at":"2026-07-22T07:13:19Z","size":"599103243"}]}`,
	} {
		if _, err := parseTemplateInventory([]byte(fixture)); err == nil {
			t.Fatalf("template schema drift was accepted: %s", fixture)
		}
	}
}

func TestParseTemplateInventoryAcceptsCustomTemplateWithoutFlavor(t *testing.T) {
	images, err := parseTemplateInventory([]byte(`{"images":[{"id":"f40a31d1d676","repository":"docker.io/library/epar-docker-sandboxes-catthehacker-act-22.04","tag":"20260715-amd64","created_at":"2026-07-23T04:37:43Z","size":1019288120}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(images) != 1 || images[0].Flavor != "" || images[0].ID != "f40a31d1d676" {
		t.Fatalf("custom template inventory = %#v", images)
	}
}

func TestCachedTemplatesUsesMachineReadableInventoryWithoutVersionGate(t *testing.T) {
	p, done := scriptedProvider(t,
		commandStep{args: []string{"template", "ls", "--json"}, result: provider.ExecResult{Stdout: templateListJSON}},
	)
	templates, err := p.CachedTemplates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(templates) != 1 {
		t.Fatalf("templates = %#v", templates)
	}
	template := templates[0]
	if template.Reference != testTemplate || template.CacheID != "39cf20eca861" || !template.CreatedAt.Equal(time.Date(2026, time.July, 22, 7, 13, 19, 0, time.UTC)) || template.SizeBytes != 599103243 {
		t.Fatalf("cached template = %#v", template)
	}
	done()
}

func TestCachedTemplatesFailsClosedOnMalformedInventory(t *testing.T) {
	p, done := scriptedProvider(t,
		commandStep{args: []string{"template", "ls", "--json"}, result: provider.ExecResult{Stdout: `{"images":[{"id":"not-a-cache-id"}]}`}},
	)
	if _, err := p.CachedTemplates(context.Background()); err == nil {
		t.Fatal("malformed template inventory was accepted")
	}
	done()
}

func TestReadGlobalNetworkPolicyUsesGlobalOnlyReadback(t *testing.T) {
	fixture := policyFixture(`[
		{"id":"global-1","name":"global","policy_id":"local","scope":"global","applies_to":"all","resource_type":"network","decision":"allow","resources":["api.example.com"],"origin":"local","status":"active","editable":true},
		{"id":"sandbox-1","name":"sandbox","policy_id":"local","scope":"sandbox:epar-sandbox-1","applies_to":"sandbox:epar-sandbox-1","resource_type":"network","decision":"deny","resources":["**"],"origin":"scoped","status":"inactive","editable":true,"sandbox_id":"epar-sandbox-1"}
	]`)
	p, done := scriptedProvider(t,
		commandStep{args: []string{"policy", "ls", "--include-inactive", "--json"}, result: provider.ExecResult{Stdout: fixture}},
	)
	rules, err := p.ReadGlobalNetworkPolicy(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 1 || rules[0].ID != "global-1" || rules[0].Scope != "global" || rules[0].AppliesTo != "all" || !rules[0].Active {
		t.Fatalf("global policy rules = %#v", rules)
	}
	done()
}

func TestReadGlobalNetworkPolicyFailsClosedOnMalformedJSON(t *testing.T) {
	p, done := scriptedProvider(t,
		commandStep{args: []string{"policy", "ls", "--include-inactive", "--json"}, result: provider.ExecResult{Stdout: `{"rules":null}`}},
	)
	if _, err := p.ReadGlobalNetworkPolicy(context.Background()); err == nil {
		t.Fatal("malformed global policy was accepted")
	}
	done()
}

func TestDiagnosticsGateFailsClosedBeforeMutation(t *testing.T) {
	p, done := scriptedProvider(t,
		commandStep{args: []string{"diagnose", "--output", "json"}, result: provider.ExecResult{Stdout: `{"version":"1.0","checks":[{"name":"daemon","status":"fail","message":"unhealthy","detail":"","hint":"restart the daemon"}],"summary":{"pass":0,"warn":0,"fail":1,"skip":0}}`}},
	)
	_, err := p.Create(context.Background(), validCreateRequest())
	if err == nil || !strings.Contains(err.Error(), "1 failed check") || !strings.Contains(err.Error(), "sbx diagnose --output json") || !strings.Contains(err.Error(), "hints for each failed check") {
		t.Fatalf("err = %v", err)
	}
	done()
}

func TestAdmissionUsesDiagnosticsWithoutReadingGlobalSecrets(t *testing.T) {
	p, done := scriptedProvider(t,
		commandStep{args: []string{"diagnose", "--output", "json"}, result: provider.ExecResult{Stdout: healthyDiagnoseJSON}},
	)
	if err := p.VerifyAdmission(context.Background()); err != nil {
		t.Fatal(err)
	}
	done()
}

func TestAdmissionRechecksDiagnostics(t *testing.T) {
	p, done := scriptedProvider(t,
		commandStep{args: []string{"diagnose", "--output", "json"}, result: provider.ExecResult{Stdout: healthyDiagnoseJSON}},
		commandStep{args: []string{"diagnose", "--output", "json"}, result: provider.ExecResult{Stdout: `{"version":"1.0","checks":[{"name":"daemon","status":"fail","message":"unhealthy","detail":"","hint":"restart the daemon"}],"summary":{"pass":0,"warn":0,"fail":1,"skip":0}}`}},
	)
	if err := p.VerifyAdmission(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := p.VerifyAdmission(context.Background()); err == nil || !strings.Contains(err.Error(), "1 failed check") || !strings.Contains(err.Error(), "sbx diagnose --output json") || !strings.Contains(err.Error(), "hints for each failed check") {
		t.Fatalf("failed diagnostics were accepted: %v", err)
	}
	done()
}

func TestInventoryParsesWrapperAndFailsClosedOnSchemaDrift(t *testing.T) {
	items, err := parseInventory([]byte(readyListJSON))
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Instance.Name != testInstance.Name || items[0].Instance.ProviderID != testInstance.ProviderID || !reflect.DeepEqual(items[0].Workspaces, []string{testWorkspace}) {
		t.Fatalf("items = %#v", items)
	}
	for _, fixture := range []string{
		`[]`,
		`{"items":[]}`,
		`{"sandboxes":null}`,
		`{"sandboxes":[{"name":"epar-sandbox-1","status":"running","workspaces":["/staging"]}]}`,
		`{"sandboxes":[{"id":"id-1","name":"epar-sandbox-1","state":"running","workspaces":["/staging"]}]}`,
		`{"sandboxes":[{"id":"id-1","name":"epar-sandbox-1","status":"running"}]}`,
		`{"sandboxes":[{"id":"id-1","name":"epar-sandbox-1","status":"running","workspaces":[]}]}`,
		`{"sandboxes":[{"id":"id-1","name":"epar-sandbox-1","status":"running","workspaces":["/staging","/staging"]}]}`,
	} {
		if _, err := parseInventory([]byte(fixture)); err == nil {
			t.Fatalf("schema drift was accepted: %s", fixture)
		}
	}
}

func TestInventoryRetriesOneInvalidMachineReadableResponse(t *testing.T) {
	p, done := scriptedProvider(t,
		commandStep{args: []string{"ls", "--json"}, result: provider.ExecResult{Stdout: "Starting sandboxd daemon..."}},
		commandStep{args: []string{"ls", "--json"}, result: provider.ExecResult{Stdout: readyListJSON}},
	)
	items, err := p.Inventory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Instance.ProviderID != testID {
		t.Fatalf("retried inventory = %#v, want exact fixture identity", items)
	}
	done()
}

func TestInventoryFailsClosedAfterSecondInvalidMachineReadableResponse(t *testing.T) {
	p, done := scriptedProvider(t,
		commandStep{args: []string{"ls", "--json"}, result: provider.ExecResult{Stdout: "Starting sandboxd daemon..."}},
		commandStep{args: []string{"ls", "--json"}, result: provider.ExecResult{Stdout: `{"items":[]}`}},
	)
	_, err := p.Inventory(context.Background())
	if err == nil || !errors.Is(err, provider.ErrControlPlaneFailure) {
		t.Fatalf("persistent invalid inventory output was accepted: %v", err)
	}
	if cause := errors.Unwrap(err); cause == nil || !strings.Contains(cause.Error(), "unsupported json schema") {
		t.Fatalf("persistent invalid inventory cause = %v, want schema error", cause)
	}
	done()
}

func TestInventoryDoesNotRetryCommandFailure(t *testing.T) {
	expected := errors.New("sandboxd unavailable with secret-token")
	p, done := scriptedProvider(t,
		commandStep{args: []string{"ls", "--json"}, err: expected},
	)
	_, err := p.Inventory(context.Background())
	if !errors.Is(err, expected) {
		t.Fatalf("Inventory() error = %v, want command failure", err)
	}
	if !errors.Is(err, provider.ErrControlPlaneFailure) {
		t.Fatalf("Inventory() error = %v, want control-plane sentinel", err)
	}
	var failure *provider.ControlPlaneFailure
	if !errors.As(err, &failure) {
		t.Fatalf("Inventory() error type = %T, want *provider.ControlPlaneFailure", err)
	}
	if strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("Inventory() user-facing error leaked command detail: %v", err)
	}
	done()
}

func TestInventoryWrapsPersistentMalformedOutputAsControlPlaneFailure(t *testing.T) {
	p, done := scriptedProvider(t,
		commandStep{args: []string{"ls", "--json"}, result: provider.ExecResult{Stdout: `{"sandboxes":`}},
		commandStep{args: []string{"ls", "--json"}, result: provider.ExecResult{Stdout: `{"sandboxes":`}},
	)
	_, err := p.Inventory(context.Background())
	if !errors.Is(err, provider.ErrControlPlaneFailure) {
		t.Fatalf("Inventory() error = %v, want control-plane sentinel", err)
	}
	var failure *provider.ControlPlaneFailure
	if !errors.As(err, &failure) {
		t.Fatalf("Inventory() error type = %T, want *provider.ControlPlaneFailure", err)
	}
	done()
}

func TestInventoryUsesProviderOwnedReadbackDeadline(t *testing.T) {
	p := New("sbx-test-double")
	p.runCommand = func(ctx context.Context, request commandRequest) (provider.ExecResult, error) {
		if !reflect.DeepEqual(request.args, []string{"ls", "--json"}) {
			t.Fatalf("inventory args = %#v", request.args)
		}
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatal("inventory command had no provider-owned deadline")
		}
		remaining := time.Until(deadline)
		if remaining <= 0 || remaining > providerReadbackTimeout || remaining < providerReadbackTimeout-5*time.Second {
			t.Fatalf("inventory deadline remaining = %s, want approximately %s", remaining, providerReadbackTimeout)
		}
		return provider.ExecResult{Stdout: readyListJSON}, nil
	}
	items, err := p.Inventory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Instance.ProviderID != testID {
		t.Fatalf("inventory = %#v, want exact fixture identity", items)
	}
}

func TestRunHonorsOperationTimeout(t *testing.T) {
	p := New("sbx-test-double")
	p.runCommand = func(ctx context.Context, _ commandRequest) (provider.ExecResult, error) {
		<-ctx.Done()
		return provider.ExecResult{}, ctx.Err()
	}
	_, err := p.run(context.Background(), commandRequest{
		args:      []string{"ls", "--json"},
		operation: "bounded test command",
		timeout:   10 * time.Millisecond,
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("bounded command error = %v, want context deadline exceeded", err)
	}
}

func TestLifecycleCommandsUseExactIdentityAndArgv(t *testing.T) {
	t.Run("start", func(t *testing.T) {
		p, done := identityScript(t, commandStep{args: []string{"exec", "-i", testName, "--", "/bin/sleep", "infinity"}})
		if _, err := p.Start(context.Background(), testInstance, provider.StartOptions{}); err != nil {
			t.Fatal(err)
		}
		done()
	})
	t.Run("verify", func(t *testing.T) {
		p, done := identityScript(t, commandStep{
			args:   []string{"exec", "-i", testName, "--", "bash", "-lc", runtimeVerificationScript},
			result: provider.ExecResult{Stdout: `"29.5.3"`},
		})
		info, err := p.VerifyRuntime(context.Background(), testInstance)
		if err != nil || !info.Ready || info.Runtime != "docker" || info.Version != "29.5.3" {
			t.Fatalf("info = %#v, err = %v", info, err)
		}
		done()
	})
	t.Run("stop_preserves_state", func(t *testing.T) {
		p, done := identityScript(t, commandStep{args: []string{"stop", testName}})
		if err := p.Stop(context.Background(), testInstance); err != nil {
			t.Fatal(err)
		}
		done()
	})
	t.Run("delete_is_exact_force_remove", func(t *testing.T) {
		p, done := identityScript(t, commandStep{args: []string{"rm", "--force", testName}})
		if err := p.Delete(context.Background(), testInstance); err != nil {
			t.Fatal(err)
		}
		done()
	})
	t.Run("address_is_unavailable_without_command", func(t *testing.T) {
		p, done := scriptedProvider(t)
		address, available, err := p.Address(context.Background(), testInstance, 30)
		if err != nil || available || address != "" {
			t.Fatalf("address = %q, available = %v, err = %v", address, available, err)
		}
		done()
	})
}

func TestExecPreservesGuestArgvStdinAndRedactsAllSurfaces(t *testing.T) {
	const secret = "sentinel-sandbox-secret"
	var stdout, stderr bytes.Buffer
	p, done := identityScript(t, commandStep{
		args:      []string{"exec", "-i", testName, "--", "sh", "-c", "printf '%s' \"$1\"", "sh", "semi;colon", "--privileged"},
		stdin:     "payload\n",
		streamOut: "stream " + secret,
		streamErr: "TOKEN=" + secret,
		result: provider.ExecResult{
			Stdout: "result " + secret,
			Stderr: "SECRET=" + secret,
		},
		err: errors.New("exit status 17: " + secret),
	})
	result, err := p.Exec(context.Background(), testInstance,
		[]string{"sh", "-c", "printf '%s' \"$1\"", "sh", "semi;colon", "--privileged"},
		provider.ExecOptions{Stdin: "payload\n", SensitiveValues: []string{secret}, Stdout: &stdout, Stderr: &stderr},
	)
	if err == nil {
		t.Fatal("expected fake execution error")
	}
	combined := stdout.String() + stderr.String() + result.Stdout + result.Stderr + err.Error()
	if strings.Contains(combined, secret) {
		t.Fatalf("secret leaked: %q", combined)
	}
	if !strings.Contains(combined, "[REDACTED]") {
		t.Fatalf("redaction marker missing: %q", combined)
	}
	done()
}

func TestExecCancellationPropagatesWithoutRealSbx(t *testing.T) {
	p := New("sbx-test-double")
	call := 0
	p.runCommand = func(ctx context.Context, request commandRequest) (provider.ExecResult, error) {
		call++
		switch call {
		case 1:
			if !reflect.DeepEqual(request.args, []string{"ls", "--json"}) {
				t.Fatalf("identity args = %#v", request.args)
			}
			return provider.ExecResult{Stdout: readyListJSON}, nil
		case 2:
			if !reflect.DeepEqual(request.args, []string{"exec", "-i", testName, "--", "sleep", "30"}) {
				t.Fatalf("exec args = %#v", request.args)
			}
			<-ctx.Done()
			return provider.ExecResult{}, ctx.Err()
		default:
			t.Fatalf("unexpected call %d", call)
			return provider.ExecResult{}, nil
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(10*time.Millisecond, cancel)
	_, err := p.Exec(ctx, testInstance, []string{"sleep", "30"}, provider.ExecOptions{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

func TestKeepaliveSurvivesSuccessfulStartContextCancellation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test helper uses a POSIX shell script")
	}
	marker := filepath.Join(t.TempDir(), "survived")
	helper := filepath.Join(t.TempDir(), "sbx-test-helper")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\nsleep 1\nprintf survived >\"$2\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := New(helper)
	ctx, cancel := context.WithCancel(context.Background())
	process, err := p.startKeepalive(ctx, testName, commandRequest{
		args:      []string{"exec", marker},
		operation: "test managed keepalive",
	})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if content, readErr := os.ReadFile(marker); readErr == nil {
			if string(content) != "survived" {
				t.Fatalf("marker = %q", content)
			}
			break
		} else if !errors.Is(readErr, os.ErrNotExist) {
			t.Fatal(readErr)
		}
		if time.Now().After(deadline) {
			t.Fatal("keepalive was terminated when the successful Start context was canceled")
		}
		time.Sleep(25 * time.Millisecond)
	}
	if process.PID <= 0 {
		t.Fatalf("keepalive PID = %d", process.PID)
	}
}

func TestKeepaliveCancellationDuringStartupStopsProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test helper uses a POSIX shell script")
	}
	marker := filepath.Join(t.TempDir(), "unexpected")
	helper := filepath.Join(t.TempDir(), "sbx-test-helper")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\nsleep 1\nprintf unexpected >\"$2\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := New(helper)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	_, err := p.startKeepalive(ctx, testName, commandRequest{
		args:      []string{"exec", marker},
		operation: "test managed keepalive",
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context cancellation", err)
	}
	time.Sleep(1100 * time.Millisecond)
	if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("canceled startup left the keepalive running: %v", statErr)
	}
}

func TestKeepaliveCancellationDuringStartupStopsProcessTree(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test helper uses a POSIX shell script")
	}
	marker := filepath.Join(t.TempDir(), "unexpected")
	helper := filepath.Join(t.TempDir(), "sbx-test-helper")
	script := "#!/bin/sh\n(sleep 1; printf child-survived >\"$2.child\") &\nprintf '%s' \"$!\" >\"$2.pid\"\nwait\n"
	if err := os.WriteFile(helper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	p := New(helper)
	_, err := p.startKeepalive(ctx, testName, commandRequest{args: []string{"exec", marker}, operation: "test managed keepalive"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context cancellation", err)
	}
	time.Sleep(1500 * time.Millisecond)
	if _, statErr := os.Stat(marker + ".child"); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("canceled startup left a descendant running: %v", statErr)
	}
}

func TestRunRawNormalizesCommandContextKillToCancellation(t *testing.T) {
	if os.Getenv("EPAR_DOCKER_SANDBOXES_RUN_RAW_HELPER") == "1" {
		_, _ = os.Stdout.WriteString("ready\n")
		time.Sleep(30 * time.Second)
		return
	}
	t.Setenv("EPAR_DOCKER_SANDBOXES_RUN_RAW_HELPER", "1")

	p := New(os.Args[0])
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	type rawResult struct {
		result provider.ExecResult
		err    error
	}
	finished := make(chan rawResult, 1)
	go func() {
		result, err := p.runRaw(ctx, commandRequest{
			args:        []string{"-test.run=^TestRunRawNormalizesCommandContextKillToCancellation$"},
			operation:   "test helper",
			outputLimit: defaultOutputLimit,
			stdout:      &cancellationSignalWriter{started: started},
		})
		finished <- rawResult{result: result, err: err}
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("runRaw helper process did not start")
	}
	cancel()
	select {
	case outcome := <-finished:
		if !errors.Is(outcome.err, context.Canceled) {
			t.Fatalf("runRaw cancellation error = %v, want context.Canceled", outcome.err)
		}
		var exitErr *exec.ExitError
		if errors.As(outcome.err, &exitErr) {
			t.Fatalf("runRaw retained cancellation-induced process exit as a concrete error: %v", outcome.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runRaw did not return after cancellation")
	}
}

func TestStopAndDeleteAreIdempotentWhenInventorySaysMissing(t *testing.T) {
	for _, test := range []struct {
		name string
		call func(*Provider) error
	}{
		{name: "stop", call: func(p *Provider) error { return p.Stop(context.Background(), testInstance) }},
		{name: "delete", call: func(p *Provider) error { return p.Delete(context.Background(), testInstance) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			p, done := scriptedProvider(t, commandStep{args: []string{"ls", "--json"}, result: provider.ExecResult{Stdout: `{"sandboxes":[]}`}})
			if err := test.call(p); err != nil {
				t.Fatal(err)
			}
			done()
		})
	}
}

func TestIdentityMismatchFailsBeforeStateMutation(t *testing.T) {
	mismatch := strings.Replace(readyListJSON, testID, "different-id", 1)
	p, done := scriptedProvider(t, commandStep{args: []string{"ls", "--json"}, result: provider.ExecResult{Stdout: mismatch}})
	if err := p.Delete(context.Background(), testInstance); err == nil || !strings.Contains(err.Error(), "identity changed") {
		t.Fatalf("err = %v", err)
	}
	done()
}

func TestDiagnosticsUsesBoundedMachineReadableCommands(t *testing.T) {
	p, done := scriptedProvider(t,
		commandStep{args: []string{"ls", "--json"}, result: provider.ExecResult{Stdout: readyListJSON}},
		commandStep{args: []string{"daemon", "status", "--json"}, result: provider.ExecResult{Stdout: `{"status":"running","socket":"\\\\.\\pipe\\docker_kaname_sandboxd","logs":"C:\\logs\\daemon.log"}`}},
		commandStep{args: []string{"diagnose", "--output", "json"}, result: provider.ExecResult{Stdout: `{"version":"1.0","checks":[{"name":"daemon","status":"pass","message":"ok","detail":"","hint":""},{"name":"optional update","status":"warn","message":"available","detail":"","hint":""},{"name":"optional integration","status":"skip","message":"not configured","detail":"","hint":""}],"summary":{"pass":1,"warn":1,"fail":0,"skip":1}}`}},
	)
	diagnostics, err := p.Diagnostics(context.Background(), testInstance)
	if err != nil || !diagnostics.Healthy || diagnostics.ChecksPassed != 1 || diagnostics.ChecksWarned != 1 || diagnostics.ChecksSkipped != 1 {
		t.Fatalf("diagnostics = %#v, err = %v", diagnostics, err)
	}
	done()
}

func TestVerifyHostReadinessAcceptsWarningsWithPassingChecksAndNoFailures(t *testing.T) {
	p, done := scriptedProvider(t,
		commandStep{args: []string{"diagnose", "--output", "json"}, result: provider.ExecResult{Stdout: `{"version":"1.0","checks":[{"name":"daemon","status":"pass","message":"healthy","detail":"","hint":""},{"name":"optional update","status":"warn","message":"available","detail":"","hint":""}],"summary":{"pass":1,"warn":1,"fail":0,"skip":0}}`}},
	)
	readiness, err := p.VerifyHostReadiness(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if readiness.ChecksPassed != 1 || readiness.ChecksWarned != 1 || readiness.ChecksFailed != 0 || readiness.ChecksSkipped != 0 {
		t.Fatalf("readiness = %#v", readiness)
	}
	done()
}

func TestVerifyHostReadinessRejectsFailedOrEmptyDiagnostics(t *testing.T) {
	tests := []struct {
		name    string
		fixture string
		want    string
	}{
		{
			name:    "failed check",
			fixture: `{"version":"1.0","checks":[{"name":"daemon","status":"fail","message":"unhealthy","detail":"","hint":""}],"summary":{"pass":0,"warn":0,"fail":1,"skip":0}}`,
			want:    "1 failed check",
		},
		{
			name:    "no passing check",
			fixture: `{"version":"1.0","checks":[{"name":"optional","status":"skip","message":"not applicable","detail":"","hint":""}],"summary":{"pass":0,"warn":0,"fail":0,"skip":1}}`,
			want:    "no passing checks",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			p, done := scriptedProvider(t,
				commandStep{args: []string{"diagnose", "--output", "json"}, result: provider.ExecResult{Stdout: test.fixture}},
			)
			if _, err := p.VerifyHostReadiness(context.Background()); err == nil || !strings.Contains(err.Error(), test.want) || !strings.Contains(err.Error(), "sbx diagnose --output json") {
				t.Fatalf("VerifyHostReadiness() error = %v, want %q and diagnostic command", err, test.want)
			}
			done()
		})
	}
}

func TestVerifyHostReadinessRejectsCommandAndSchemaFailures(t *testing.T) {
	tests := []struct {
		name string
		step commandStep
		want string
	}{
		{
			name: "command failure",
			step: commandStep{args: []string{"diagnose", "--output", "json"}, err: errors.New("diagnose unavailable")},
			want: "diagnose unavailable",
		},
		{
			name: "malformed json",
			step: commandStep{args: []string{"diagnose", "--output", "json"}, result: provider.ExecResult{Stdout: `{"summary":`}},
			want: "unsupported json schema",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			p, done := scriptedProvider(t, test.step)
			if _, err := p.VerifyHostReadiness(context.Background()); err == nil || !strings.Contains(err.Error(), test.want) || !strings.Contains(err.Error(), "sbx diagnose --output json") {
				t.Fatalf("VerifyHostReadiness() error = %v, want %q and diagnostic command", err, test.want)
			}
			done()
		})
	}
}

func TestDiagnosticsRequiresEverySummaryCount(t *testing.T) {
	fixture := `{"version":"1.0","checks":[{"name":"daemon","status":"pass","message":"ok","detail":"","hint":""}],"summary":{"pass":1,"warn":0,"fail":0}}`
	if _, _, _, _, err := parseDiagnose([]byte(fixture)); err == nil {
		t.Fatal("diagnostics accepted a missing summary count")
	}
}

func TestDiagnosticsRejectsOversizedOutput(t *testing.T) {
	p, done := scriptedProvider(t,
		commandStep{args: []string{"ls", "--json"}, result: provider.ExecResult{Stdout: readyListJSON}},
		commandStep{args: []string{"daemon", "status", "--json"}, result: provider.ExecResult{Stdout: strings.Repeat("x", diagnosticOutputLimit+1)}},
	)
	if _, err := p.Diagnostics(context.Background(), testInstance); err == nil || !strings.Contains(err.Error(), "output limit") {
		t.Fatalf("err = %v", err)
	}
	done()
}

func TestPolicyCommandsAreSandboxScopedAndReadBackExactRules(t *testing.T) {
	globalRule := `{"id":"global-1","name":"automatic baseline","policy_id":"local-policy","scope":"global","applies_to":"all","resource_type":"network","decision":"allow","resources":["openrouter.ai"],"origin":"local","status":"active","editable":true}`
	sandboxRule := `{"id":"rule-1","name":"job allowlist","policy_id":"local","scope":"sandbox:epar-sandbox-1","applies_to":"sandbox:epar-sandbox-1","resource_type":"network","decision":"allow","resources":["api.example.com","*.packages.example.com:443"],"origin":"scoped","status":"active","editable":true,"sandbox_id":"epar-sandbox-1","additive":42}`
	policyJSON := policyFixture(`[` + globalRule + `,` + sandboxRule + `]`)
	rule := provider.NetworkPolicyRule{Decision: provider.NetworkPolicyAllow, Resources: []string{"api.example.com", "*.packages.example.com:443"}}
	t.Run("apply", func(t *testing.T) {
		p, done := scriptedProvider(t,
			commandStep{args: []string{"ls", "--json"}, result: provider.ExecResult{Stdout: readyListJSON}},
			commandStep{args: []string{"policy", "allow", "network", "--sandbox", testName, "api.example.com,*.packages.example.com:443"}},
			commandStep{args: []string{"policy", "ls", testName, "--include-inactive", "--json"}, result: provider.ExecResult{Stdout: policyJSON}},
		)
		if err := p.ApplyNetworkPolicy(context.Background(), testInstance, []provider.NetworkPolicyRule{rule}); err != nil {
			t.Fatal(err)
		}
		done()
	})
	t.Run("apply_open_public_egress", func(t *testing.T) {
		openRule := `{"id":"rule-open","name":"open public egress","policy_id":"local","scope":"sandbox:epar-sandbox-1","applies_to":"sandbox:epar-sandbox-1","resource_type":"network","decision":"allow","resources":["**"],"origin":"scoped","status":"active","editable":true,"sandbox_id":"epar-sandbox-1"}`
		p, done := scriptedProvider(t,
			commandStep{args: []string{"ls", "--json"}, result: provider.ExecResult{Stdout: readyListJSON}},
			commandStep{args: []string{"policy", "allow", "network", "--sandbox", testName, "**"}},
			commandStep{args: []string{"policy", "ls", testName, "--include-inactive", "--json"}, result: provider.ExecResult{Stdout: policyFixture(`[` + globalRule + `,` + openRule + `]`)}},
		)
		if err := p.ApplyNetworkPolicy(context.Background(), testInstance, []provider.NetworkPolicyRule{{
			Decision:  provider.NetworkPolicyAllow,
			Resources: []string{"**"},
		}}); err != nil {
			t.Fatal(err)
		}
		done()
	})
	t.Run("remove_by_stable_rule_id", func(t *testing.T) {
		p, done := scriptedProvider(t,
			commandStep{args: []string{"ls", "--json"}, result: provider.ExecResult{Stdout: readyListJSON}},
			commandStep{args: []string{"policy", "ls", testName, "--include-inactive", "--json"}, result: provider.ExecResult{Stdout: policyJSON}},
			commandStep{args: []string{"policy", "rm", "network", "--sandbox", testName, "--id", "rule-1"}},
			commandStep{args: []string{"policy", "ls", testName, "--include-inactive", "--json"}, result: provider.ExecResult{Stdout: policyFixture(`[` + globalRule + `]`)}},
		)
		remove := provider.NetworkPolicyRule{
			ID:           "rule-1",
			PolicyID:     "local",
			Scope:        "sandbox:" + testName,
			AppliesTo:    "sandbox:" + testName,
			ResourceType: "network",
			Resources:    append([]string(nil), rule.Resources...),
			Decision:     provider.NetworkPolicyAllow,
			Origin:       "scoped",
			Editable:     true,
		}
		if err := p.RemoveNetworkPolicy(context.Background(), testInstance, []provider.NetworkPolicyRule{remove}); err != nil {
			t.Fatal(err)
		}
		done()
	})
}

func TestPolicyRemovalRefusesChangedStableIdentity(t *testing.T) {
	fixture := policyFixture(`[{"id":"rule-1","name":"job allowlist","policy_id":"local","scope":"sandbox:epar-sandbox-1","applies_to":"sandbox:epar-sandbox-1","resource_type":"network","decision":"allow","resources":["api.example.com"],"origin":"scoped","status":"inactive","editable":true,"sandbox_id":"epar-sandbox-1"}]`)
	p, done := scriptedProvider(t,
		commandStep{args: []string{"ls", "--json"}, result: provider.ExecResult{Stdout: readyListJSON}},
		commandStep{args: []string{"policy", "ls", testName, "--include-inactive", "--json"}, result: provider.ExecResult{Stdout: fixture}},
	)
	changed := provider.NetworkPolicyRule{
		ID:           "rule-1",
		PolicyID:     "local",
		Scope:        "sandbox:" + testName,
		AppliesTo:    "sandbox:" + testName,
		ResourceType: "network",
		Resources:    []string{"different.example.com"},
		Decision:     provider.NetworkPolicyAllow,
		Origin:       "scoped",
		Editable:     true,
	}
	if err := p.RemoveNetworkPolicy(context.Background(), testInstance, []provider.NetworkPolicyRule{changed}); err == nil || !strings.Contains(err.Error(), "stable identity changed") {
		t.Fatalf("err = %v", err)
	}
	done()
}

func TestPolicyReadPreservesGlobalAndSandboxAttribution(t *testing.T) {
	fixture := policyFixture(`[
		{"id":"global-1","name":"automatic baseline","policy_id":"local-policy","scope":"global","applies_to":"all","resource_type":"network","decision":"allow","resources":["openrouter.ai"],"origin":"local","status":"active","editable":true},
		{"id":"rule-1","name":"job allowlist","policy_id":"local","scope":"sandbox:epar-sandbox-1","applies_to":"sandbox:epar-sandbox-1","resource_type":"network","decision":"deny","resources":["**"],"origin":"scoped","status":"inactive","editable":true,"sandbox_id":"epar-sandbox-1"},
		{"id":"other-1","name":"another sandbox","policy_id":"local","scope":"sandbox:other-sandbox","applies_to":"sandbox:other-sandbox","resource_type":"network","decision":"allow","resources":["other.example.com"],"origin":"scoped","status":"active","editable":true,"sandbox_id":"other-sandbox"},
		{"id":"fs-1","name":"filesystem baseline","policy_id":"local-policy","scope":"global","applies_to":"all","resource_type":"filesystem","decision":"allow","resources":["/workspace"],"origin":"local","status":"active","editable":false}
	]`)
	rules, err := parseNetworkPolicy([]byte(fixture), testName)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 3 {
		t.Fatalf("rules = %#v", rules)
	}
	global := rules[0]
	if global.ID != "global-1" || global.Name != "automatic baseline" || global.PolicyID != "local-policy" || global.Scope != "global" || global.AppliesTo != "all" || global.ResourceType != "network" || global.Origin != "local" || global.Status != "active" || !global.Editable || !global.Active {
		t.Fatalf("global attribution was not preserved: %#v", global)
	}
	local := rules[1]
	if local.Scope != "sandbox:"+testName || local.AppliesTo != "sandbox:"+testName || local.Origin != "scoped" || local.Status != "inactive" || local.Active {
		t.Fatalf("sandbox attribution was not preserved: %#v", local)
	}
	filesystem := rules[2]
	if filesystem.ID != "fs-1" || filesystem.ResourceType != "filesystem" || filesystem.Scope != "global" || filesystem.AppliesTo != "all" || filesystem.Editable {
		t.Fatalf("filesystem attribution was not preserved: %#v", filesystem)
	}
}

func TestPolicyReadPreservesExactKitAttribution(t *testing.T) {
	fixture := policyFixture(`[{"id":"kit-rule-1","name":"kit:epar-sandbox-1","policy_id":"kit-policy-1","scope":"sandbox:epar-sandbox-1","applies_to":"sandbox:epar-sandbox-1","resource_type":"network","decision":"allow","resources":["openrouter.ai"],"origin":"scoped","status":"active","editable":false,"sandbox_id":"epar-sandbox-1"}]`)
	rules, err := parseNetworkPolicy([]byte(fixture), testName)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 1 || rules[0].Scope != "sandbox:"+testName || rules[0].AppliesTo != "sandbox:"+testName || rules[0].Origin != "scoped" || rules[0].Editable || !rules[0].Active {
		t.Fatalf("kit attribution was not preserved: %#v", rules)
	}
}

func TestPolicyReadRejectsMismatchedSandboxAttribution(t *testing.T) {
	for _, fixture := range []string{
		policyFixture(`[{"id":"rule-1","name":"rule","policy_id":"local","scope":"sandbox:epar-sandbox-1","applies_to":"sandbox:other-sandbox","resource_type":"network","decision":"allow","resources":["api.example.com"],"origin":"scoped","status":"active","editable":true,"sandbox_id":"epar-sandbox-1"}]`),
		policyFixture(`[{"id":"rule-1","name":"rule","policy_id":"local","scope":"sandbox:epar-sandbox-1","applies_to":"sandbox:epar-sandbox-1","resource_type":"network","decision":"allow","resources":["api.example.com"],"origin":"scoped","status":"active","editable":true,"sandbox_id":"other-sandbox"}]`),
	} {
		if _, err := parseNetworkPolicy([]byte(fixture), testName); err == nil {
			t.Fatalf("mismatched sandbox attribution was accepted: %s", fixture)
		}
	}
}

func TestPolicyRemovalRefusesGlobalRule(t *testing.T) {
	global := provider.NetworkPolicyRule{ID: "global-1", PolicyID: "local-policy", Scope: "global", AppliesTo: "all", ResourceType: "network", Resources: []string{"openrouter.ai"}, Decision: provider.NetworkPolicyAllow, Origin: "local", Editable: true}
	fixture := policyFixture(`[{"id":"global-1","name":"automatic baseline","policy_id":"local-policy","scope":"global","applies_to":"all","resource_type":"network","decision":"allow","resources":["openrouter.ai"],"origin":"local","status":"active","editable":true}]`)
	p, done := scriptedProvider(t,
		commandStep{args: []string{"ls", "--json"}, result: provider.ExecResult{Stdout: readyListJSON}},
		commandStep{args: []string{"policy", "ls", testName, "--include-inactive", "--json"}, result: provider.ExecResult{Stdout: fixture}},
	)
	if err := p.RemoveNetworkPolicy(context.Background(), testInstance, []provider.NetworkPolicyRule{global}); err == nil || !strings.Contains(err.Error(), "refusing to remove") {
		t.Fatalf("err = %v", err)
	}
	done()
}

func TestPolicyRemovalRefusesFilesystemRule(t *testing.T) {
	filesystem := provider.NetworkPolicyRule{ID: "fs-1", PolicyID: "local-policy", Scope: "global", AppliesTo: "all", ResourceType: "filesystem", Resources: []string{"/workspace"}, Decision: provider.NetworkPolicyAllow, Origin: "local", Editable: false}
	fixture := policyFixture(`[{"id":"fs-1","name":"filesystem baseline","policy_id":"local-policy","scope":"global","applies_to":"all","resource_type":"filesystem","decision":"allow","resources":["/workspace"],"origin":"local","status":"active","editable":false}]`)
	p, done := scriptedProvider(t,
		commandStep{args: []string{"ls", "--json"}, result: provider.ExecResult{Stdout: readyListJSON}},
		commandStep{args: []string{"policy", "ls", testName, "--include-inactive", "--json"}, result: provider.ExecResult{Stdout: fixture}},
	)
	if err := p.RemoveNetworkPolicy(context.Background(), testInstance, []provider.NetworkPolicyRule{filesystem}); err == nil || !strings.Contains(err.Error(), "refusing to remove") {
		t.Fatalf("err = %v", err)
	}
	done()
}

func TestPolicySchemaFailsClosed(t *testing.T) {
	for _, fixture := range []string{
		`[]`,
		`{"policies":[]}`,
		`{"rules":null}`,
		policyFixture(`[{"id":"rule-1","name":"rule","policy_id":"local","scope":"sandbox:epar-sandbox-1","applies_to":"sandbox:epar-sandbox-1","resource_type":"network","decision":"permit","resources":["api.example.com"],"origin":"scoped","status":"active","editable":true,"sandbox_id":"epar-sandbox-1"}]`),
		policyFixture(`[{"id":"rule-1","name":"rule","policy_id":"local","scope":"sandbox:epar-sandbox-1","applies_to":"sandbox:epar-sandbox-1","resource_type":"network","decision":"allow","resources":["api.example.com"],"origin":"scoped","status":"unknown","editable":true,"sandbox_id":"epar-sandbox-1"}]`),
	} {
		if _, err := parseNetworkPolicy([]byte(fixture), testName); err == nil {
			t.Fatalf("policy schema drift was accepted: %s", fixture)
		}
	}
}

func TestPolicyReadbackAcceptsAttributedProviderBaselinePatterns(t *testing.T) {
	fixture := policyFixture(`[{"id":"default-cert-validation","name":"default-cert-validation","policy_id":"local-policy","scope":"global","applies_to":"all","resource_type":"network","decision":"allow","resources":["**.example.com:443","crl*.digicert.com:80","**"],"origin":"local","status":"active","editable":true}]`)
	rules, err := parseNetworkPolicy([]byte(fixture), testName)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 1 || !reflect.DeepEqual(rules[0].Resources, []string{"**.example.com:443", "crl*.digicert.com:80", "**"}) {
		t.Fatalf("provider baseline resources = %#v", rules)
	}
}

func TestInjectionCorpusIsRejectedBeforeCommandExecution(t *testing.T) {
	for _, name := range []string{"-other", "../other", "other/name", "name;rm", "name\nnext", "name+other", ""} {
		request := validCreateRequest()
		request.Name = name
		if err := validateCreateRequest(request); err == nil {
			t.Fatalf("name injection accepted: %q", name)
		}
	}
	for _, resource := range []string{"-api.example.com", "api.example.com,evil.example.com", "api.example.com --sandbox other", "api.example.com;reset", "api.example.com\n**", "https://api.example.com", "127.0.0.1", "10.0.0.0/8", "[::1]:443", "*.localhost"} {
		rule := provider.NetworkPolicyRule{Decision: provider.NetworkPolicyAllow, Resources: []string{resource}}
		if err := validateNetworkRule(rule, false); err == nil {
			t.Fatalf("resource injection accepted: %q", resource)
		}
	}
	openRule := provider.NetworkPolicyRule{Decision: provider.NetworkPolicyAllow, Resources: []string{"**"}}
	if err := validateNetworkRule(openRule, false); err != nil {
		t.Fatalf("owned sandbox-scoped open rule was rejected: %v", err)
	}
	denyAllRule := provider.NetworkPolicyRule{Decision: provider.NetworkPolicyDeny, Resources: []string{"**"}}
	if err := validateNetworkRule(denyAllRule, false); err == nil {
		t.Fatal("unbounded deny wildcard was accepted")
	}
}

func TestCommandBoundaryRejectsInteractiveAndDestructiveGlobalCommands(t *testing.T) {
	for _, arguments := range [][]string{{"daemon", "status", "--json"}, {"daemon", "stop"}, {"daemon", "start", "--detach"}} {
		if err := validateCommandRequest(commandRequest{args: arguments, operation: "test exact daemon command"}); err != nil {
			t.Fatalf("exact Docker Sandboxes daemon command was rejected: %q: %v", arguments, err)
		}
	}
	for _, command := range []string{"tui", "reset", "run", "kit", "secret", "login", "logout", "setup", "ssh", "cp", "completion", "help"} {
		err := validateCommandRequest(commandRequest{args: []string{command}, operation: "test forbidden command"})
		if err == nil {
			t.Fatalf("forbidden Docker Sandboxes command %q was accepted", command)
		}
	}
	for _, arguments := range [][]string{{"inspect"}, {"inspect", testName}, {"inspect", "--json", "../other"}, {"inspect", "--debug", testName}} {
		if err := validateCommandRequest(commandRequest{args: arguments, operation: "test forbidden inspection"}); err == nil {
			t.Fatalf("non-exact Docker Sandboxes inspection was accepted: %q", arguments)
		}
	}
	for _, arguments := range [][]string{{"ports"}, {"ports", testName}, {"ports", "../other", "--json"}, {"ports", testName, "--json", "extra"}, {"ports", testName, "--unpublish"}} {
		if err := validateCommandRequest(commandRequest{args: arguments, operation: "test forbidden port command"}); err == nil {
			t.Fatalf("non-exact Docker Sandboxes published-port inspection was accepted: %q", arguments)
		}
	}
	for _, arguments := range [][]string{{"daemon"}, {"daemon", "start"}, {"daemon", "start", "--foreground"}, {"daemon", "stop", "--detach"}, {"daemon", "stop", "extra"}, {"daemon", "restart"}, {"daemon", "restart", "--detach"}, {"daemon", "status"}, {"daemon", "status", "--debug"}} {
		if err := validateCommandRequest(commandRequest{args: arguments, operation: "test forbidden daemon command"}); err == nil {
			t.Fatalf("non-exact Docker Sandboxes daemon command was accepted: %q", arguments)
		}
	}
}

func TestExecRejectsHostPathsAndEnvironmentPassthrough(t *testing.T) {
	p, done := scriptedProvider(t)
	if _, err := p.Exec(context.Background(), testInstance, []string{"true"}, provider.ExecOptions{LogPath: "/host/log"}); err == nil {
		t.Fatal("host log path was accepted")
	}
	if _, err := p.Exec(context.Background(), testInstance, []string{"true"}, provider.ExecOptions{Env: map[string]string{"TOKEN": "secret"}}); err == nil {
		t.Fatal("guest environment passthrough was accepted")
	}
	done()
}

func identityScript(t *testing.T, operation commandStep) (*Provider, func()) {
	t.Helper()
	p, done := scriptedProvider(t,
		commandStep{args: []string{"ls", "--json"}, result: provider.ExecResult{Stdout: readyListJSON}},
		operation,
	)
	return p, done
}

func identityAdmissionScript(t *testing.T, inspection commandStep) (*Provider, func()) {
	t.Helper()
	p, done := scriptedProvider(t,
		commandStep{args: []string{"ls", "--json"}, result: provider.ExecResult{Stdout: readyListJSON}},
		commandStep{args: []string{"ports", testName, "--json"}, result: provider.ExecResult{Stdout: emptyPortsJSON}},
		inspection,
	)
	return p, done
}

func validCreateRequest() provider.CreateRequest {
	return provider.CreateRequest{
		Name:           testName,
		Template:       testTemplate,
		TemplateDigest: testDigest,
		StagingPath:    testWorkspace,
		CPUs:           4,
		Memory:         "8g",
	}
}

func policyFixture(rules string) string {
	return `{"rules":` + rules + `}`
}
