//go:build !windows

package storage

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"syscall"
)

func platformFilesystemCapacity(path string) (uint64, uint64, error) {
	var stats syscall.Statfs_t
	if err := syscall.Statfs(path, &stats); err != nil {
		return 0, 0, err
	}
	blockSize := uint64(stats.Bsize)
	available := uint64(stats.Bavail)
	total := uint64(stats.Blocks)
	if blockSize != 0 && (available > math.MaxUint64/blockSize || total > math.MaxUint64/blockSize) {
		return 0, 0, fmt.Errorf("filesystem space result overflow")
	}
	return available * blockSize, total * blockSize, nil
}

func platformFilesystemCapacityDomain(path string) (string, string, uint64, uint64, error) {
	available, total, err := platformFilesystemCapacity(path)
	if err != nil {
		return "", "", 0, 0, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", "", 0, 0, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", "", 0, 0, fmt.Errorf("filesystem did not expose a stable capacity-domain identity")
	}
	device := uint64(stat.Dev)
	domainPath := path
	if !info.IsDir() {
		domainPath = filepath.Dir(path)
	}
	for {
		parent := filepath.Dir(domainPath)
		if parent == domainPath {
			break
		}
		parentInfo, statErr := os.Stat(parent)
		if statErr != nil {
			break
		}
		parentStat, statOK := parentInfo.Sys().(*syscall.Stat_t)
		if !statOK || uint64(parentStat.Dev) != device {
			break
		}
		domainPath = parent
	}
	return fmt.Sprintf("unix-filesystem:%x", device), domainPath, available, total, nil
}

func platformFilesystemIdentity(path string, _ bool) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", fmt.Errorf("filesystem did not expose a stable object identity")
	}
	return fmt.Sprintf("unix:%x:%x", uint64(stat.Dev), uint64(stat.Ino)), nil
}

func isFilesystemRedirect(info os.FileInfo) bool { return info.Mode()&os.ModeSymlink != 0 }

func platformCanonicalFilesystemPath(path string) (string, error) { return path, nil }
