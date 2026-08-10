//go:build windows

package promotion

import (
	"errors"
	"fmt"

	"golang.org/x/sys/windows"
)

const processorFeatureVirtualizationFirmwareEnabled = 21

func sandboxHostSpace(path string) (HostSpace, error) {
	pathPointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return HostSpace{}, err
	}
	var available uint64
	var total uint64
	var free uint64
	if err := windows.GetDiskFreeSpaceEx(pathPointer, &available, &total, &free); err != nil {
		return HostSpace{}, fmt.Errorf("GetDiskFreeSpaceEx: %w", err)
	}
	return HostSpace{AvailableBytes: available, TotalBytes: total}, nil
}

func sandboxVirtualizationAvailable() error {
	if !windows.IsProcessorFeaturePresent(processorFeatureVirtualizationFirmwareEnabled) {
		return errors.New("Windows did not report firmware virtualization as enabled and available to the operating system")
	}
	return nil
}
