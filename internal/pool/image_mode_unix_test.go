//go:build unix

package pool

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestCopyFileEnforcesRequestedModeUnderRestrictiveUmask(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "source.sh")
	dst := filepath.Join(dir, "build-context", "copied.sh")
	if err := os.WriteFile(src, []byte("#!/usr/bin/env bash\n"), 0644); err != nil {
		t.Fatal(err)
	}

	oldUmask := syscall.Umask(0077)
	t.Cleanup(func() { syscall.Umask(oldUmask) })
	if err := copyFile(src, dst, 0755); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0755 {
		t.Fatalf("copied mode = %#o, want 0755", got)
	}
}
