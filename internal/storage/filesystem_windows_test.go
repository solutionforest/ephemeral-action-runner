//go:build windows

package storage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestSnapshotFilesystemTargetAcceptsWindowsShortPathAlias(t *testing.T) {
	longRoot := filepath.Join(t.TempDir(), "Long Directory Name")
	if err := os.Mkdir(longRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	shortRoot := windowsShortPath(t, longRoot)
	if strings.EqualFold(shortRoot, longRoot) {
		t.Skip("filesystem did not provide a distinct short path alias")
	}
	longPath := filepath.Join(longRoot, "artifact.bin")
	if err := os.WriteFile(longPath, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	target, err := SnapshotFilesystemTarget(filepath.Join(shortRoot, "artifact.bin"))
	if err != nil {
		t.Fatalf("SnapshotFilesystemTarget() rejected a Windows short path alias: %v", err)
	}
	if !strings.EqualFold(target.Locator, longPath) {
		t.Fatalf("SnapshotFilesystemTarget() locator = %q, want canonical spelling %q", target.Locator, longPath)
	}
}

func windowsShortPath(t *testing.T, path string) string {
	t.Helper()
	pointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	size := uint32(len(path) + 1)
	for {
		buffer := make([]uint16, size)
		length, err := windows.GetShortPathName(pointer, &buffer[0], size)
		if err != nil {
			t.Skipf("Windows short paths unavailable: %v", err)
		}
		if length < size {
			return windows.UTF16ToString(buffer[:length])
		}
		size = length + 1
	}
}
