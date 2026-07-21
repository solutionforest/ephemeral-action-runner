//go:build windows

package logging

import "strings"

func canonicalCase(path string) string { return strings.ToLower(path) }
