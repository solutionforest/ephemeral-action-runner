//go:build darwin || linux

package provider

import (
	"fmt"
	"os"
)

func prepareControlPlaneRecoveryDirectory(directory string) error {
	return syncControlPlaneRecoveryDirectory(directory)
}

func replaceControlPlaneRecoveryFile(source, destination string) error {
	return os.Rename(source, destination)
}

func finalizeControlPlaneRecoveryDirectory(directory string) error {
	return syncControlPlaneRecoveryDirectory(directory)
}

func syncControlPlaneRecoveryDirectory(directory string) error {
	file, err := os.Open(directory)
	if err != nil {
		return fmt.Errorf("open directory: %w", err)
	}
	syncErr := file.Sync()
	closeErr := file.Close()
	if syncErr != nil {
		return fmt.Errorf("flush directory: %w", syncErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close directory: %w", closeErr)
	}
	return nil
}
