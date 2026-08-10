//go:build !windows

package image

import "os"

func replaceAtomicFile(source, destination string) error {
	return os.Rename(source, destination)
}
