//go:build !windows

package logging

import "os"

func replaceFile(source, destination string) error { return os.Rename(source, destination) }
