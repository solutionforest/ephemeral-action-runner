//go:build linux

package capacity

import (
	"path/filepath"
	"testing"
)

func TestDockerSandboxesStorageRootUsesXDGStateHome(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	got, err := DockerSandboxesStorageRoot()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(stateHome, "sandboxes"); got != want {
		t.Fatalf("storage root = %q, want %q", got, want)
	}
}

func TestDockerSandboxesStorageRootRejectsRelativeXDGStateHome(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "relative-state")
	if _, err := DockerSandboxesStorageRoot(); err == nil {
		t.Fatal("relative XDG_STATE_HOME was accepted")
	}
}
