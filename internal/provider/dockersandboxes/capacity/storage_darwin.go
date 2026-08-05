//go:build darwin

package capacity

import (
	"fmt"

	"github.com/solutionforest/ephemeral-action-runner/internal/provider/storagepath"
)

// DockerSandboxesStorageRoot returns the documented macOS state root that
// contains Docker Sandboxes' persistent sandbox data.
func DockerSandboxesStorageRoot() (string, error) {
	environment, err := storagepath.CurrentEnvironment()
	if err != nil {
		return "", err
	}
	roots, err := storagepath.DockerSandboxesRoots(environment)
	if err != nil {
		return "", err
	}
	for _, root := range roots {
		if root.ID == "state" {
			return root.Path, nil
		}
	}
	return "", fmt.Errorf("Docker Sandboxes state root was not discovered")
}
