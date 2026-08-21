//go:build !windows

package promotion

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRunSBXCommandBoundsProcessTreeOnContextCancellation(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("EPAR_STATE_HOME", t.TempDir())
	helper := filepath.Join(dir, "sbx")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\n(sleep 30) &\nwait\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := runSBXCommand(ctx, []string{"version"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("runSBXCommand() error = %v, want context deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("runSBXCommand() took %s after cancellation; process tree was not bounded", elapsed)
	}
}
