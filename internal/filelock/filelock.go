// Package filelock provides non-blocking, advisory, cross-process file locks.
package filelock

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
)

// ErrLocked means another process or file descriptor currently owns the lock.
var ErrLocked = errors.New("file lock is already held")

// Lock is an exclusive advisory lock. Close releases it and is idempotent.
type Lock struct {
	file *os.File
	once sync.Once
	err  error
}

// Acquire opens path, creating it when necessary, and attempts to acquire an
// exclusive lock without waiting.
func Acquire(path string) (*Lock, error) {
	if path == "" {
		return nil, errors.New("lock path is empty")
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file %s: %w", path, err)
	}
	if err := lockFile(file); err != nil {
		_ = file.Close()
		if errors.Is(err, errPlatformLocked) {
			return nil, fmt.Errorf("%w: %s", ErrLocked, path)
		}
		return nil, fmt.Errorf("lock file %s: %w", path, err)
	}
	return &Lock{file: file}, nil
}

// ReplaceContent atomically with respect to this lock replaces the lock-file
// payload while retaining ownership of the platform lock. This matters on
// Windows, where writing the locked byte range through a second file handle
// can fail even when both handles belong to the same process.
func (lock *Lock) ReplaceContent(content []byte) error {
	if lock == nil || lock.file == nil {
		return errors.New("file lock is not open")
	}
	if err := lock.file.Truncate(0); err != nil {
		return fmt.Errorf("truncate lock file: %w", err)
	}
	if _, err := lock.file.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("seek lock file: %w", err)
	}
	if _, err := lock.file.Write(content); err != nil {
		return fmt.Errorf("write lock file: %w", err)
	}
	if err := lock.file.Sync(); err != nil {
		return fmt.Errorf("sync lock file: %w", err)
	}
	return nil
}

// Close releases the lock.
func (lock *Lock) Close() error {
	if lock == nil {
		return nil
	}
	lock.once.Do(func() {
		lock.err = unlockFile(lock.file)
		if err := lock.file.Close(); lock.err == nil {
			lock.err = err
		}
	})
	return lock.err
}
