// Package projectlayout defines the generated filesystem layout inside an EPAR checkout.
package projectlayout

import "path/filepath"

const (
	LocalDirectory = ".local"
	CacheDirectory = "cache"
	StateDirectory = "state"
	BinDirectory   = "bin"
	WorkDirectory  = "work"
	LogsDirectory  = "logs"
)

func LocalRoot(projectRoot string) string { return filepath.Join(projectRoot, LocalDirectory) }

func CacheRoot(projectRoot string) string {
	return filepath.Join(LocalRoot(projectRoot), CacheDirectory)
}

func StateRoot(projectRoot string) string {
	return filepath.Join(LocalRoot(projectRoot), StateDirectory)
}

func BinRoot(projectRoot string) string { return filepath.Join(LocalRoot(projectRoot), BinDirectory) }

func WorkRoot(projectRoot string) string { return filepath.Join(projectRoot, WorkDirectory) }

func LogsRoot(projectRoot string) string { return filepath.Join(WorkRoot(projectRoot), LogsDirectory) }
