package pool

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/solutionforest/ephemeral-action-runner/internal/logging"
)

func TestPoolLifecycleConsoleGuidance(t *testing.T) {
	var console bytes.Buffer
	runtime, err := logging.NewRuntime(logging.Options{
		Directory:    t.TempDir(),
		ManagerSinks: logging.SinkConsole,
		Stdout:       &console,
		Stderr:       &console,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	manager := Manager{Logging: runtime}

	manager.logPoolRunning("Docker Sandboxes pool")
	manager.logReplacementReady("Docker Sandboxes pool", "epar-test-002")
	cleanupCalled := false
	if err := manager.cleanupPoolWithStatus("owned runner resources", func() error {
		cleanupCalled = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !cleanupCalled {
		t.Fatal("cleanup callback was not called")
	}

	output := console.String()
	for _, expected := range []string{
		"Docker Sandboxes pool is running.",
		"Press Ctrl-C once to stop; wait for cleanup confirmation before closing this window.",
		"Replacement runner epar-test-002 is online; Docker Sandboxes pool is ready for the next job.",
		"Stopping EPAR pool. Cleaning up owned runner resources.",
		"Please wait; do not press Ctrl-C again or close this window.",
		"Cleanup complete. EPAR can now exit safely.",
	} {
		if !strings.Contains(output, expected) {
			t.Fatalf("lifecycle console output does not contain %q: %q", expected, output)
		}
	}
	if strings.Index(output, "Stopping EPAR pool.") > strings.Index(output, "Cleanup complete.") {
		t.Fatalf("cleanup completion preceded cleanup start: %q", output)
	}
}

func TestPoolLifecycleConsoleReportsIncompleteCleanup(t *testing.T) {
	var console bytes.Buffer
	runtime, err := logging.NewRuntime(logging.Options{
		Directory:    t.TempDir(),
		ManagerSinks: logging.SinkConsole,
		Stdout:       &console,
		Stderr:       &console,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	manager := Manager{Logging: runtime}
	cleanupErr := errors.New("remote state unavailable")

	err = manager.cleanupPoolWithStatus("owned runner resources", func() error { return cleanupErr })
	if !errors.Is(err, cleanupErr) {
		t.Fatalf("cleanup error = %v, want %v", err, cleanupErr)
	}
	output := console.String()
	if !strings.Contains(output, "Cleanup did not fully complete.") || !strings.Contains(output, "will reconcile it on the next run") || strings.Contains(output, "EPAR can now exit safely") {
		t.Fatalf("incomplete cleanup console output = %q", output)
	}
}
