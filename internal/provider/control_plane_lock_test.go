package provider

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestControlPlaneLockExcludesConcurrentCommands(t *testing.T) {
	t.Setenv("EPAR_STATE_HOME", t.TempDir())
	release, err := TryAcquireControlPlaneRecoveryLock()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if _, err := AcquireControlPlaneCommandLock(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("AcquireControlPlaneCommandLock() = %v, want context deadline", err)
	}
	release()
	commandRelease, err := AcquireControlPlaneCommandLock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	commandRelease()
}
