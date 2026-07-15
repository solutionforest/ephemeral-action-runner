//go:build !windows && !linux && !darwin

package filelock

import (
	"errors"
	"os"
)

var errPlatformLocked = errors.New("file locks are unsupported on this platform")

func lockFile(_ *os.File) error {
	return errPlatformLocked
}

func unlockFile(_ *os.File) error {
	return nil
}
