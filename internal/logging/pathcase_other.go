//go:build !windows

package logging

func canonicalCase(path string) string { return path }
