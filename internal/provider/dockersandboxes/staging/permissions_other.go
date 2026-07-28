//go:build !windows

package staging

import (
	"fmt"
	"os"
	"syscall"
)

func restrictPlatformPermissions(path string) error {
	return os.Chmod(path, 0o700)
}

func validatePlatformPermissions(_ string, info os.FileInfo) error {
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("mode %04o permits group or other access; require owner-only access", info.Mode().Perm())
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("filesystem did not expose staging ownership")
	}
	if stat.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("owner uid %d does not match controller uid %d", stat.Uid, os.Geteuid())
	}
	return nil
}
