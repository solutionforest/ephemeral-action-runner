//go:build !darwin && !linux && !windows

package provider

import "errors"

func prepareControlPlaneRecoveryDirectory(string) error {
	return errors.New("directory durability is unsupported on this platform")
}

func replaceControlPlaneRecoveryFile(string, string) error {
	return errors.New("atomic recovery ledger replacement is unsupported on this platform")
}

func finalizeControlPlaneRecoveryDirectory(string) error {
	return errors.New("directory durability is unsupported on this platform")
}
