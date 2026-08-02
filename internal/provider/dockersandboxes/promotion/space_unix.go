//go:build !windows

package promotion

import (
	"fmt"
	"math"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
)

func sandboxHostSpace(path string) (HostSpace, error) {
	var stats syscall.Statfs_t
	if err := syscall.Statfs(path, &stats); err != nil {
		return HostSpace{}, err
	}
	blockSize := uint64(stats.Bsize)
	available := uint64(stats.Bavail)
	total := uint64(stats.Blocks)
	if blockSize != 0 && (available > math.MaxUint64/blockSize || total > math.MaxUint64/blockSize) {
		return HostSpace{}, fmt.Errorf("filesystem space result overflow")
	}
	return HostSpace{AvailableBytes: available * blockSize, TotalBytes: total * blockSize}, nil
}

func sandboxVirtualizationAvailable() error {
	switch runtime.GOOS {
	case "linux":
		file, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
		if err != nil {
			return fmt.Errorf("open /dev/kvm read/write: %w", err)
		}
		return file.Close()
	case "darwin":
		output, err := exec.Command("/usr/sbin/sysctl", "-n", "kern.hv_support").Output()
		if err != nil {
			return fmt.Errorf("query kern.hv_support: %w", err)
		}
		if strings.TrimSpace(string(output)) != "1" {
			return fmt.Errorf("kern.hv_support did not report 1")
		}
		return nil
	default:
		return fmt.Errorf("unsupported native virtualization platform %s", runtime.GOOS)
	}
}
