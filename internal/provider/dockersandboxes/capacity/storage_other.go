//go:build !windows && !linux && !darwin

package capacity

import (
	"fmt"
	"runtime"
)

func DockerSandboxesStorageRoot() (string, error) {
	return "", fmt.Errorf("Docker Sandboxes storage discovery is unsupported on %s", runtime.GOOS)
}
