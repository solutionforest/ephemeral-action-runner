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

func WithControlPlaneLock(ctx context.Context) context.Context {
	return context.WithValue(ctx, controlPlaneLockContextKey{}, true)
}

func ControlPlaneLockHeld(ctx context.Context) bool {
	held, _ := ctx.Value(controlPlaneLockContextKey{}).(bool)
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

func tryAcquireControlPlaneLock() (func(), error) {
	path, err := controlPlaneLockPath()
	if err != nil {
		return nil, err
	}
	lock, err := filelock.Acquire(path)
	if err != nil {
		return nil, err
	}
	return func() { _ = lock.Close() }, nil
}

func TryAcquireControlPlaneRecoveryLock() (func(), error) {
	release, err := tryAcquireControlPlaneLock()
	if errors.Is(err, filelock.ErrLocked) {
		return nil, fmt.Errorf("%w: %v", ErrControlPlaneRecoveryBusy, err)
	}
	return release, err
}

func acquireControlPlaneCommandLock(ctx context.Context) (func(), error) {
	for {
		release, err := tryAcquireControlPlaneLock()
		if err == nil {
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
	return acquireControlPlaneCommandLock(ctx)
}
