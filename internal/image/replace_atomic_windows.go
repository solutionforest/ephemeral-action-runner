//go:build windows

package image

import (
	"syscall"
	"unsafe"
)

const (
	imageMoveFileReplaceExisting = 0x00000001
	imageMoveFileWriteThrough    = 0x00000008
)

var imageMoveFileEx = syscall.NewLazyDLL("kernel32.dll").NewProc("MoveFileExW")

func replaceAtomicFile(source, destination string) error {
	sourcePath, err := syscall.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	destinationPath, err := syscall.UTF16PtrFromString(destination)
	if err != nil {
		return err
	}
	result, _, callErr := imageMoveFileEx.Call(
		uintptr(unsafe.Pointer(sourcePath)),
		uintptr(unsafe.Pointer(destinationPath)),
		imageMoveFileReplaceExisting|imageMoveFileWriteThrough,
	)
	if result == 0 {
		if errno, ok := callErr.(syscall.Errno); ok && errno == 0 {
			return syscall.EINVAL
		}
		return callErr
	}
	return nil
}
