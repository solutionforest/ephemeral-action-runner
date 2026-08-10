//go:build windows

package hosttrust

import (
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	lockfileFailImmediately = 0x00000001
	lockfileExclusiveLock   = 0x00000002
	errorLockViolation      = syscall.Errno(33)
)

var (
	kernel32         = syscall.NewLazyDLL("kernel32.dll")
	procLockFileEx   = kernel32.NewProc("LockFileEx")
	procUnlockFileEx = kernel32.NewProc("UnlockFileEx")
	procOpenProcess  = kernel32.NewProc("OpenProcess")
	procCloseHandle  = kernel32.NewProc("CloseHandle")
)

func platformProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err == nil {
		defer windows.CloseHandle(snapshot)
		var entry windows.ProcessEntry32
		entry.Size = uint32(unsafe.Sizeof(entry))
		if err := windows.Process32First(snapshot, &entry); err == nil {
			for {
				if entry.ProcessID == uint32(pid) {
					return true
				}
				if err := windows.Process32Next(snapshot, &entry); err != nil {
					return false
				}
			}
		}
	}

	// Enumeration is normally authoritative, including for protected
	// processes. Retain the conservative OpenProcess fallback only when a
	// process snapshot cannot be read at all.
	const synchronize = 0x00100000
	handle, _, callErr := procOpenProcess.Call(synchronize, 0, uintptr(pid))
	if handle != 0 {
		procCloseHandle.Call(handle)
		return true
	}
	return callErr == syscall.Errno(5) // Access denied also proves it exists.
}

func acquirePlatformConfigLock(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	var overlapped syscall.Overlapped
	r1, _, callErr := procLockFileEx.Call(
		file.Fd(), lockfileExclusiveLock|lockfileFailImmediately, 0, 1, 0, uintptr(unsafe.Pointer(&overlapped)),
	)
	if r1 != 0 {
		return file, nil
	}
	_ = file.Close()
	if callErr == errorLockViolation {
		return nil, errPlatformLockHeld
	}
	return nil, callErr
}

func releasePlatformConfigLock(file *os.File) error {
	var overlapped syscall.Overlapped
	r1, _, callErr := procUnlockFileEx.Call(file.Fd(), 0, 1, 0, uintptr(unsafe.Pointer(&overlapped)))
	if r1 != 0 {
		return nil
	}
	return callErr
}
