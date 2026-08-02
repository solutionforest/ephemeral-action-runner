//go:build linux

package capacity

import (
	"errors"
	"os"
	"path/filepath"
)

// DockerSandboxesStorageRoot returns the documented XDG state root that
// contains Docker Sandboxes' persistent sandbox data.
func DockerSandboxesStorageRoot() (string, error) {
	stateHome := os.Getenv("XDG_STATE_HOME")
	if stateHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		stateHome = filepath.Join(home, ".local", "state")
	}
	if !filepath.IsAbs(stateHome) {
		return "", errors.New("XDG state home is not an absolute path")
	}
	return filepath.Join(stateHome, "sandboxes"), nil
}
