//go:build !windows

package dockersandboxes

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
)

func TestRunRawCancellationKillsManagedProcessGroup(t *testing.T) {
	p := New("sh")
	ctx, cancel := context.WithCancel(context.Background())
	childPIDPath := filepath.Join(t.TempDir(), "child.pid")
	command := fmt.Sprintf("sleep 30 & echo $! > %q; printf ready; wait", childPIDPath)
	started := make(chan struct{})
	type rawResult struct {
		result provider.ExecResult
		err    error
	}
	finished := make(chan rawResult, 1)
	go func() {
		result, err := p.runRaw(ctx, commandRequest{
			args:        []string{"-c", command},
			operation:   "managed process group test",
			outputLimit: defaultOutputLimit,
			stdout:      &cancellationSignalWriter{started: started},
		})
		finished <- rawResult{result: result, err: err}
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("managed process group helper did not start")
	}
	var childPID int
	childDeadline := time.Now().Add(5 * time.Second)
	for childPID == 0 && time.Now().Before(childDeadline) {
		if data, err := os.ReadFile(childPIDPath); err == nil {
			childPID, err = strconv.Atoi(strings.TrimSpace(string(data)))
			if err != nil {
				t.Fatal(err)
			}
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if childPID == 0 {
		cancel()
		t.Fatal("managed process group child did not report its PID")
	}
	t.Cleanup(func() { _ = syscall.Kill(childPID, syscall.SIGKILL) })
	startedAt := time.Now()
	cancel()
	select {
	case outcome := <-finished:
		if !errors.Is(outcome.err, context.Canceled) {
			t.Fatalf("managed process group cancellation error = %v, want context.Canceled", outcome.err)
		}
		if elapsed := time.Since(startedAt); elapsed > 2*time.Second {
			t.Fatalf("managed process group cancellation took %s", elapsed)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("managed process group did not terminate after cancellation")
	}
	processGoneDeadline := time.Now().Add(2 * time.Second)
	for {
		err := syscall.Kill(childPID, 0)
		if errors.Is(err, syscall.ESRCH) {
			break
		}
		if time.Now().After(processGoneDeadline) {
			t.Fatalf("managed process group child still exists after cancellation: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
