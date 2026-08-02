//go:build !windows

package storage

import (
	"fmt"
	"math"
	"os"
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
