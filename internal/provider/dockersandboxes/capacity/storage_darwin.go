//go:build darwin

package capacity

import (
	"errors"
	"os"
	"path/filepath"
)

// DockerSandboxesStorageRoot returns the documented macOS state root that
// contains Docker Sandboxes' persistent sandbox data.
func DockerSandboxesStorageRoot() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(home) {
		return "", errors.New("home directory is not an absolute path")
	}
	return filepath.Join(home, "Library", "Application Support", "com.docker.sandboxes"), nil
}
