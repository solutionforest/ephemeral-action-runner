//go:build windows

package storage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	winio "github.com/Microsoft/go-winio"
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
	want, err := platformCanonicalFilesystemPath(longPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(target.Locator, want) {
		t.Fatalf("SnapshotFilesystemTarget() locator = %q, want canonical spelling %q", target.Locator, want)
	}
}

func TestProbeFilesystemCapacityDomainFollowsWindowsJunction(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	junction := filepath.Join(root, "junction")
	if err := os.Mkdir(junction, 0o700); err != nil {
		t.Fatal(err)
	}
	setWindowsJunction(t, junction, target)
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	targetDomain, err := ProbeFilesystemCapacityDomain(target, now)
	if err != nil {
		t.Fatalf("ProbeFilesystemCapacityDomain(target) error = %v", err)
	}
	junctionDomain, err := ProbeFilesystemCapacityDomain(junction, now)
	if err != nil {
		t.Fatalf("ProbeFilesystemCapacityDomain(junction) error = %v", err)
	}
	if targetDomain.ID == "" || targetDomain.ID != junctionDomain.ID {
		t.Fatalf("target domain=%+v junction domain=%+v, want one Windows volume", targetDomain, junctionDomain)
	}
	if _, err := SnapshotFilesystemTarget(junction); err == nil {
		t.Fatal("SnapshotFilesystemTarget() accepted a junction allowed only for read-only capacity discovery")
	}
}

func setWindowsJunction(t *testing.T, junction, target string) {
	t.Helper()
	pointer, err := windows.UTF16PtrFromString(junction)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFile(pointer, windows.GENERIC_WRITE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		t.Skipf("Windows junction handle unavailable: %v", err)
	}
	defer windows.CloseHandle(handle)
	data := winio.EncodeReparsePoint(&winio.ReparsePoint{Target: target, IsMountPoint: true})
	if err := windows.DeviceIoControl(handle, windows.FSCTL_SET_REPARSE_POINT, &data[0], uint32(len(data)), nil, 0, nil, nil); err != nil {
		t.Skipf("Windows junction creation unavailable: %v", err)
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
