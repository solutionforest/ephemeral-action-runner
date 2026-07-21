//go:build windows

package logging

import (
	"os"
	"syscall"
	"testing"
)

func TestWindowsReparsePointDetection(t *testing.T) {
	info := fakeFileInfo{name: "junction", mode: os.ModeDir, sys: &syscall.Win32FileAttributeData{FileAttributes: syscall.FILE_ATTRIBUTE_REPARSE_POINT}}
	if !isLinkOrReparse(info) {
		t.Fatal("Windows reparse point was not detected")
	}
}
