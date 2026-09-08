//go:build !windows && !linux && !darwin

package filelock

import (
	"errors"
	"os"
)

var (
	errPlatformLocked      = errors.New("file lock is already held")
	errPlatformUnsupported = ErrUnsupported
)

func lockFile(_ *os.File) error {
	return errPlatformUnsupported
}

func unlockFile(_ *os.File) error {
	return nil
}
