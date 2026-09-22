package provider

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/filelock"
)

func TestControlPlaneRecoveryLockExcludesCommands(t *testing.T) {
	t.Setenv("EPAR_STATE_HOME", t.TempDir())
	release, err := TryAcquireControlPlaneRecoveryLock()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if _, err := AcquireControlPlaneCommandLock(ctx); !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "host command lock before command execution") {
		t.Fatalf("AcquireControlPlaneCommandLock() = %v, want pre-exec lock-wait context deadline", err)
	}
	recoveryCtx, recoveryCancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer recoveryCancel()
	if nextRelease, err := AcquireControlPlaneRecoveryLock(recoveryCtx); !errors.Is(err, context.DeadlineExceeded) {
		if nextRelease != nil {
			nextRelease()
		}
		t.Fatalf("waiting recovery = %v, want context deadline", err)
	}
	release()
	commandRelease, err := AcquireControlPlaneCommandLock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	commandRelease()
}

func TestControlPlaneCommandsShareAcrossProcesses(t *testing.T) {
	if mode := os.Getenv("EPAR_COMMAND_LOCK_HELPER"); mode != "" {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		release, err := AcquireControlPlaneCommandLock(ctx)
		if mode == "blocked" {
			if release != nil {
				release()
			}
			if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "host command lock before command execution") {
				t.Fatalf("child command bypassed recovery or lost wait diagnostic: %v", err)
			}
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		if release, err := TryAcquireControlPlaneRecoveryLock(); !errors.Is(err, ErrControlPlaneRecoveryBusy) {
			if release != nil {
				release()
			}
			t.Fatalf("recovery during shared commands: %v", err)
		}
		return
	}
	t.Setenv("EPAR_STATE_HOME", t.TempDir())
	release, err := AcquireControlPlaneCommandLock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	runControlPlaneCommandChild(t, "shared")
}

func runControlPlaneCommandChild(t *testing.T, mode string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestControlPlaneCommandsShareAcrossProcesses$")
	child.Env = append(os.Environ(), "EPAR_COMMAND_LOCK_HELPER="+mode)
	if output, err := child.CombinedOutput(); err != nil {
		t.Fatalf("child: %v\n%s", err, output)
	}
}

func TestControlPlanePendingRecoveryAdmission(t *testing.T) {
	for _, cancelRecovery := range []bool{false, true} {
		t.Run(map[bool]string{false: "drain", true: "cancel"}[cancelRecovery], func(t *testing.T) {
			t.Setenv("EPAR_STATE_HOME", t.TempDir())
			releaseCommand, err := AcquireControlPlaneCommandLock(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer releaseCommand()
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			type result struct {
				release func()
				err     error
			}
			done := make(chan result, 1)
			go func() { release, err := AcquireControlPlaneRecoveryLock(ctx); done <- result{release, err} }()
			// Observe the admission reservation while the original reader keeps
			// recovery from acquiring the exclusive resource lock.
			path, err := controlPlaneLockPath()
			if err != nil {
				t.Fatal(err)
			}
			for {
				gate, err := filelock.Acquire(path + ".admission")
				if errors.Is(err, filelock.ErrLocked) {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				_ = gate.Close()
				if ctx.Err() != nil {
					t.Fatal("recovery never reserved admission")
				}
				time.Sleep(time.Millisecond)
			}
			runControlPlaneCommandChild(t, "blocked")
			blockedCtx, blockedCancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
			defer blockedCancel()
			if release, err := AcquireControlPlaneCommandLock(blockedCtx); !errors.Is(err, context.DeadlineExceeded) {
				if release != nil {
					release()
				}
				t.Fatalf("new command bypassed pending recovery: %v", err)
			}
			if cancelRecovery {
				cancel()
			} else {
				releaseCommand()
			}
			got := <-done
			if cancelRecovery {
				if !errors.Is(got.err, context.Canceled) {
					t.Fatalf("canceled recovery: %v", got.err)
				}
			} else {
				if got.err != nil {
					t.Fatal(got.err)
				}
				defer got.release()
				runControlPlaneCommandChild(t, "blocked")
				if release, err := TryAcquireControlPlaneRecoveryLock(); !errors.Is(err, ErrControlPlaneRecoveryBusy) {
					if release != nil {
						release()
					}
					t.Fatalf("second recovery: %v", err)
				}
				got.release()
			}
			releaseCommand()
			nextCtx, nextCancel := context.WithTimeout(context.Background(), time.Second)
			defer nextCancel()
			release, err := AcquireControlPlaneCommandLock(nextCtx)
			if err != nil {
				t.Fatal(err)
			}
			release()
			runControlPlaneCommandChild(t, "shared")
		})
	}
}

func TestControlPlaneCanceledContextDoesNotAcquire(t *testing.T) {
	t.Setenv("EPAR_STATE_HOME", t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, acquire := range []func(context.Context) (func(), error){AcquireControlPlaneCommandLock, AcquireControlPlaneRecoveryLock} {
		if release, err := acquire(ctx); !errors.Is(err, context.Canceled) {
			if release != nil {
				release()
			}
			t.Fatalf("canceled acquire: %v", err)
		}
	}
}
