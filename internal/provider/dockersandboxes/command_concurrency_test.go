package dockersandboxes

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
)

type commandStartedWriter struct {
	once    sync.Once
	started chan struct{}
}

func (w *commandStartedWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.started) })
	return len(p), nil
}

func TestRunRawLongCommandDoesNotBlockAnotherSandboxCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell helper; shared Windows lock compilation covered separately")
	}
	t.Setenv("EPAR_STATE_HOME", t.TempDir())
	helper := filepath.Join(t.TempDir(), "sbx-helper")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\nif [ \"$1\" = slow ]; then echo started; exec sleep 30; fi\necho renewed\n"), 0755); err != nil {
		t.Fatal(err)
	}
	p := New(helper)
	ctx, cancel := context.WithCancel(context.Background())
	started := &commandStartedWriter{started: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		_, err := p.runRaw(ctx, commandRequest{args: []string{"slow"}, operation: "simulate long registration", outputLimit: 1024, stdout: started})
		done <- err
	}()
	t.Cleanup(func() { cancel(); <-done })
	select {
	case <-started.started:
	case <-time.After(3 * time.Second):
		t.Fatal("slow helper did not start")
	}
	leaseCtx, leaseCancel := context.WithTimeout(context.Background(), time.Second)
	defer leaseCancel()
	result, err := p.runRaw(leaseCtx, commandRequest{args: []string{"fast"}, operation: "simulate another runner lease", outputLimit: 1024})
	if err != nil || strings.TrimSpace(result.Stdout) != "renewed" {
		t.Fatalf("concurrent lease = %+v, %v", result, err)
	}
	select {
	case err := <-done:
		done <- err
		t.Fatalf("long command unexpectedly finished: %v", err)
	default:
	}
}

func TestRunRawReportsAdmissionTimeoutBeforeCommandStart(t *testing.T) {
	t.Setenv("EPAR_STATE_HOME", t.TempDir())
	release, err := provider.TryAcquireControlPlaneRecoveryLock()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = New("must-not-be-started").runRaw(ctx, commandRequest{args: []string{"ls"}, operation: "inventory", outputLimit: 1024})
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "command not started") {
		t.Fatalf("error=%v; want distinct pre-execution admission deadline", err)
	}
}
