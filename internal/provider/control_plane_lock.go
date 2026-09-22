package provider

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/filelock"
	storagecatalog "github.com/solutionforest/ephemeral-action-runner/internal/storage/catalog"
)

const (
	controlPlaneLockDirectory = "provider-control-plane-recovery"
	controlPlaneLockName      = "docker-sandboxes.lock"
	controlPlaneLockRetry     = 50 * time.Millisecond
)

var ErrControlPlaneRecoveryBusy = errors.New("provider control-plane recovery is already active")

type controlPlaneLockContextKey struct{}

type controlPlaneRecoveryCoordinatorContextKey struct{}

func WithControlPlaneLock(ctx context.Context) context.Context {
	return context.WithValue(ctx, controlPlaneLockContextKey{}, true)
}

func ControlPlaneLockHeld(ctx context.Context) bool {
	held, _ := ctx.Value(controlPlaneLockContextKey{}).(bool)
	return held
}

// WithControlPlaneRecoveryCoordinator marks the context passed through a
// provider-owned host-wide recovery coordinator. Providers use the marker to
// avoid reacquiring a lease they already hold while running the synchronous
// recovery callback.
func WithControlPlaneRecoveryCoordinator(ctx context.Context) context.Context {
	return context.WithValue(ctx, controlPlaneRecoveryCoordinatorContextKey{}, true)
}

// ControlPlaneRecoveryCoordinatorHeld reports whether provider recovery is
// already executing inside its host-wide coordinator callback.
func ControlPlaneRecoveryCoordinatorHeld(ctx context.Context) bool {
	held, _ := ctx.Value(controlPlaneRecoveryCoordinatorContextKey{}).(bool)
	return held
}

func controlPlaneLockPath() (string, error) {
	root, err := storagecatalog.DefaultRoot()
	if err != nil {
		return "", err
	}
	lockRoot := filepath.Join(root, controlPlaneLockDirectory)
	if err := os.MkdirAll(lockRoot, 0o700); err != nil {
		return "", err
	}
	return filepath.Join(lockRoot, controlPlaneLockName), nil
}

// tryAcquireControlPlaneLock takes the admission gate only while admitting a
// command, or for the entire recovery lease. Never unlink either lock file:
// other processes may already have the same inode open.
func tryAcquireControlPlaneLock(shared bool) (func(), error) {
	path, err := controlPlaneLockPath()
	if err != nil {
		return nil, err
	}
	gate, err := filelock.Acquire(path + ".admission")
	if err != nil {
		return nil, err
	}
	acquire := filelock.Acquire
	if shared {
		acquire = filelock.AcquireShared
	}
	lock, err := acquire(path)
	if err != nil {
		_ = gate.Close()
		return nil, err
	}
	if shared {
		_ = gate.Close()
	}
	return func() { _ = lock.Close(); _ = gate.Close() }, nil
}

func TryAcquireControlPlaneRecoveryLock() (func(), error) {
	release, err := tryAcquireControlPlaneLock(false)
	if errors.Is(err, filelock.ErrLocked) {
		return nil, fmt.Errorf("%w: %v", ErrControlPlaneRecoveryBusy, err)
	}
	return release, err
}

// AcquireControlPlaneRecoveryLock blocks new ordinary commands once it owns the
// admission gate, then waits for existing commands to drain. The caller must
// bound admission with a context deadline. Cancellation releases the gate.
// OS scheduling before gate acquisition is not FIFO; after acquisition, new
// commands cannot starve recovery. The legacy Try API does not reserve admission.
func AcquireControlPlaneRecoveryLock(ctx context.Context) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path, err := controlPlaneLockPath()
	if err != nil {
		return nil, err
	}
	gate, err := waitControlPlaneFileLock(ctx, func() (*filelock.Lock, error) {
		return filelock.Acquire(path + ".admission")
	})
	if err != nil {
		return nil, fmt.Errorf("wait for Docker Sandboxes host recovery admission: %w", err)
	}
	lock, err := waitControlPlaneFileLock(ctx, func() (*filelock.Lock, error) {
		return filelock.Acquire(path)
	})
	if err != nil {
		_ = gate.Close()
		return nil, fmt.Errorf("wait for Docker Sandboxes host commands to drain before recovery: %w", err)
	}
	return func() { _ = lock.Close(); _ = gate.Close() }, nil
}

func waitControlPlaneFileLock(ctx context.Context, acquire func() (*filelock.Lock, error)) (*filelock.Lock, error) {
	var lock *filelock.Lock
	_, err := waitControlPlaneLock(ctx, func() (func(), error) {
		var err error
		lock, err = acquire()
		return func() { _ = lock.Close() }, err
	})
	return lock, err
}

func waitControlPlaneLock(ctx context.Context, acquire func() (func(), error)) (func(), error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		release, err := acquire()
		if err == nil {
			if err := ctx.Err(); err != nil {
				release()
				return nil, err
			}
			return release, nil
		}
		if !errors.Is(err, filelock.ErrLocked) {
			return nil, fmt.Errorf("acquire Docker Sandboxes host control-plane lock: %w", err)
		}
		timer := time.NewTimer(controlPlaneLockRetry)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func AcquireControlPlaneCommandLock(ctx context.Context) (func(), error) {
	release, err := waitControlPlaneLock(ctx, func() (func(), error) {
		return tryAcquireControlPlaneLock(true)
	})
	if err != nil {
		return nil, fmt.Errorf("wait for Docker Sandboxes host command lock before command execution: %w", err)
	}
	return release, nil
}
