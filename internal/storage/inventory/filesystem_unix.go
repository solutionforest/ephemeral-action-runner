//go:build !windows

package inventory

import "os"

func isRedirect(info os.FileInfo) bool { return info.Mode()&os.ModeSymlink != 0 }
