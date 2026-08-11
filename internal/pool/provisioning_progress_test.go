package pool

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
	"github.com/solutionforest/ephemeral-action-runner/internal/logging"
)

func TestDockerSandboxesCreateHeartbeatDefaultIsFiveSeconds(t *testing.T) {
	if got, want := dockerSandboxesCreateHeartbeatInterval, 5*time.Second; got != want {
		t.Fatalf("Docker Sandboxes create heartbeat = %s, want %s", got, want)
	}
}

func TestDockerSandboxesCreateProgressReportsHeartbeatAndCompletion(t *testing.T) {
	manager, console, closeRuntime := newProvisioningProgressTestManager(t)
	defer closeRuntime()
	setProvisioningProgressTestGlobals(t, false, console, 5*time.Millisecond)

	if err := manager.runDockerSandboxesCreateProgress("sandbox-one", func() error {
		time.Sleep(20 * time.Millisecond)
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	output := console.String()
	for _, wanted := range []string{
		"Docker Sandboxes instance preparation started",
		"first use may materialize cached template layers",
		"Docker Sandboxes instance preparation: still working; elapsed",
		"Docker Sandboxes instance preparation complete",
	} {
		if !strings.Contains(output, wanted) {
			t.Fatalf("progress output omitted %q: %q", wanted, output)
		}
	}
	lengthAfterReturn := console.Len()
	time.Sleep(10 * time.Millisecond)
	if console.Len() != lengthAfterReturn {
		t.Fatalf("heartbeat continued after create returned: before=%d after=%d", lengthAfterReturn, console.Len())
	}
}

func TestDockerSandboxesCreateProgressUsesOneInteractiveLine(t *testing.T) {
	manager, console, closeRuntime := newProvisioningProgressTestManager(t)
	defer closeRuntime()
	setProvisioningProgressTestGlobals(t, true, console, 5*time.Millisecond)

	if err := manager.runDockerSandboxesCreateProgress("sandbox-one", func() error {
		time.Sleep(20 * time.Millisecond)
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	output := console.String()
	if !strings.Contains(output, "\r\033[2KDocker Sandboxes instance preparation: still working; elapsed") {
		t.Fatalf("interactive progress did not redraw one terminal line: %q", output)
	}
	if strings.Contains(output, "[INFO] Docker Sandboxes instance preparation: still working") {
		t.Fatalf("interactive progress unexpectedly emitted durable heartbeat records: %q", output)
	}
	if !strings.Contains(output, "Docker Sandboxes instance preparation complete") {
		t.Fatalf("interactive progress omitted completion: %q", output)
	}
	if !strings.Contains(output, "\r\033[2K\n") {
		t.Fatalf("interactive progress did not terminate the cleared heartbeat line before completion: %q", output)
	}
}

func TestDockerSandboxesCreateProgressUsesOneInteractiveLineWhenTranscriptsUseConsole(t *testing.T) {
	manager, console, closeRuntime := newProvisioningProgressTestManager(t)
	defer closeRuntime()
	manager.Config.Logging.TranscriptSinks = []string{"console"}
	setProvisioningProgressTestGlobals(t, true, console, 5*time.Millisecond)

	if err := manager.runDockerSandboxesCreateProgress("sandbox-one", func() error {
		time.Sleep(20 * time.Millisecond)
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	output := console.String()
	if !strings.Contains(output, "\r\033[2KDocker Sandboxes instance preparation: still working; elapsed") {
		t.Fatalf("transcript-console configuration disabled interactive redraw: %q", output)
	}
}

func TestDockerSandboxesCreateProgressDoesNotReportFailureAsComplete(t *testing.T) {
	manager, console, closeRuntime := newProvisioningProgressTestManager(t)
	defer closeRuntime()
	setProvisioningProgressTestGlobals(t, false, console, 0)
	expected := errors.New("injected create failure")

	err := manager.runDockerSandboxesCreateProgress("sandbox-one", func() error { return expected })
	if !errors.Is(err, expected) {
		t.Fatalf("error = %v, want %v", err, expected)
	}
	output := console.String()
	if !strings.Contains(output, "Docker Sandboxes instance preparation failed") {
		t.Fatalf("failed create omitted terminal progress state: %q", output)
	}
	if strings.Contains(output, "Docker Sandboxes instance preparation complete") {
		t.Fatalf("failed create was reported complete: %q", output)
	}
}

func TestNonSandboxProviderDoesNotUseSandboxCreateProgress(t *testing.T) {
	manager, console, closeRuntime := newProvisioningProgressTestManager(t)
	defer closeRuntime()
	manager.Config.Provider.Type = "docker-container"
	setProvisioningProgressTestGlobals(t, false, console, time.Millisecond)
	called := false

	if err := manager.runDockerSandboxesCreateProgress("container-one", func() error {
		called = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("non-Sandbox provider operation was not called")
	}
	if strings.Contains(console.String(), "Docker Sandboxes instance preparation") {
		t.Fatalf("non-Sandbox provider emitted Sandbox progress: %q", console.String())
	}
}

func newProvisioningProgressTestManager(t *testing.T) (*Manager, *bytes.Buffer, func()) {
	t.Helper()
	root := t.TempDir()
	console := &bytes.Buffer{}
	runtime, err := logging.NewRuntime(logging.Options{
		Directory:       root,
		ManagerSinks:    logging.SinkConsole,
		TranscriptSinks: logging.SinkFile,
		Stdout:          console,
		Stderr:          console,
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Provider.Type = "docker-sandboxes"
	cfg.Logging.Directory = root
	cfg.Logging.ManagerSinks = []string{"console"}
	cfg.Logging.ManagerConsoleFormat = "text"
	cfg.Logging.TranscriptSinks = []string{"file"}
	return &Manager{Config: cfg, Logging: runtime}, console, func() {
		if err := runtime.Close(); err != nil {
			t.Errorf("close logging runtime: %v", err)
		}
	}
}

func setProvisioningProgressTestGlobals(t *testing.T, terminal bool, console *bytes.Buffer, interval time.Duration) {
	t.Helper()
	previousTerminal := dockerPullProgressTerminal
	previousWidth := progressTerminalWidth
	previousConsole := dockerPullProgressConsole
	previousInterval := dockerSandboxesCreateHeartbeatInterval
	dockerPullProgressTerminal = func() bool { return terminal }
	progressTerminalWidth = func() int { return 160 }
	dockerPullProgressConsole = console
	dockerSandboxesCreateHeartbeatInterval = interval
	t.Cleanup(func() {
		dockerPullProgressTerminal = previousTerminal
		progressTerminalWidth = previousWidth
		dockerPullProgressConsole = previousConsole
		dockerSandboxesCreateHeartbeatInterval = previousInterval
	})
}
