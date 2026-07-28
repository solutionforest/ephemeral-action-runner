//go:build windows

package capacity

import (
	"errors"
	"os"
	"path/filepath"
)

// DockerSandboxesStorageRoot returns the documented Windows root that contains
// Docker Sandboxes' persistent state, cache, configuration, and sandbox data.
func DockerSandboxesStorageRoot() (string, error) {
	localAppData := os.Getenv("LOCALAPPDATA")
	if localAppData == "" {
		return "", errors.New("LOCALAPPDATA is unavailable")
	}
	if !filepath.IsAbs(localAppData) {
		return "", errors.New("LOCALAPPDATA is not an absolute path")
	}
	return filepath.Join(localAppData, "DockerSandboxes"), nil
}
