//go:build windows

package filelock

import (
	"errors"
	"os"
	"syscall"
	"unsafe"
)

const (
	lockfileFailImmediately = 0x00000001
	lockfileExclusiveLock   = 0x00000002
	lockfileOffsetHigh      = 1
)

var (
	errPlatformLocked = errors.New("platform file lock is already held")
	kernel32          = syscall.NewLazyDLL("kernel32.dll")
	procLockFileEx    = kernel32.NewProc("LockFileEx")
	procUnlockFileEx  = kernel32.NewProc("UnlockFileEx")
)

func lockFile(file *os.File) error {
	// Keep the lock range outside the metadata payload. Windows enforces byte
	// range locks for ordinary reads, so locking byte zero would prevent another
	// process from reading owner diagnostics while the lock is held.
	overlapped := syscall.Overlapped{OffsetHigh: lockfileOffsetHigh}
	result, _, callErr := procLockFileEx.Call(file.Fd(), lockfileExclusiveLock|lockfileFailImmediately, 0, 1, 0, uintptr(unsafe.Pointer(&overlapped)))
	if result != 0 {
		return nil
	}
	if callErr == syscall.Errno(33) || callErr == syscall.Errno(158) {
		return errPlatformLocked
	}
	return callErr
}

func unlockFile(file *os.File) error {
	overlapped := syscall.Overlapped{OffsetHigh: lockfileOffsetHigh}
	result, _, callErr := procUnlockFileEx.Call(file.Fd(), 0, 1, 0, uintptr(unsafe.Pointer(&overlapped)))
	if result != 0 {
		return nil
	}
	return callErr
}
