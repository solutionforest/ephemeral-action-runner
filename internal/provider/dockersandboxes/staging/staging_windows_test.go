//go:build windows

package staging

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/windows"
)

type windowsReparseDirectoryInfo struct {
	os.FileInfo
}

func (windowsReparseDirectoryInfo) Mode() os.FileMode {
	return os.ModeDir
}

func (windowsReparseDirectoryInfo) Sys() any {
	return &syscall.Win32FileAttributeData{FileAttributes: syscall.FILE_ATTRIBUTE_DIRECTORY | syscall.FILE_ATTRIBUTE_REPARSE_POINT}
}

func TestPlatformRedirectRejectsWindowsReparseDirectory(t *testing.T) {
	if !isPlatformRedirect(windowsReparseDirectoryInfo{}) {
		t.Fatal("Windows reparse directory was not classified as a redirect")
	}
}

func TestOpenAcceptsWindowsShortPathAlias(t *testing.T) {
	longParent := filepath.Join(t.TempDir(), "Long Directory Name")
	if err := os.Mkdir(longParent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := restrictPlatformPermissions(longParent); err != nil {
		t.Fatal(err)
	}
	shortParent := stagingWindowsShortPath(t, longParent)
	if strings.EqualFold(shortParent, longParent) {
		t.Skip("filesystem did not provide a distinct short path alias")
	}
	staging, err := Open(filepath.Join(shortParent, "staging"))
	if err != nil {
		t.Fatalf("Open() rejected a Windows short path alias: %v", err)
	}
	want := filepath.Join(longParent, "staging")
	if !strings.EqualFold(staging.Root(), want) {
		t.Fatalf("Root() = %q, want canonical spelling %q", staging.Root(), want)
	}
}

func stagingWindowsShortPath(t *testing.T, path string) string {
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
