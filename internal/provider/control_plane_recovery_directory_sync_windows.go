//go:build windows

package provider

import (
	"syscall"
	"unsafe"
)

const (
	controlPlaneMoveFileReplaceExisting = 0x00000001
	controlPlaneMoveFileWriteThrough    = 0x00000008
)

var controlPlaneMoveFileEx = syscall.NewLazyDLL("kernel32.dll").NewProc("MoveFileExW")

func prepareControlPlaneRecoveryDirectory(string) error {
	return nil
}

func replaceControlPlaneRecoveryFile(source, destination string) error {
	sourcePath, err := syscall.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	destinationPath, err := syscall.UTF16PtrFromString(destination)
	if err != nil {
		return err
	}
	result, _, callErr := controlPlaneMoveFileEx.Call(
		uintptr(unsafe.Pointer(sourcePath)),
		uintptr(unsafe.Pointer(destinationPath)),
		controlPlaneMoveFileReplaceExisting|controlPlaneMoveFileWriteThrough,
	)
	if result == 0 {
		if errno, ok := callErr.(syscall.Errno); ok && errno == 0 {
			return syscall.EINVAL
		}
		return callErr
	}
	return nil
}

func finalizeControlPlaneRecoveryDirectory(string) error {
	return nil
}
