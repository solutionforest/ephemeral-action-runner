package image

import (
	"path/filepath"
	"testing"
)

func TestDockerSandboxesArtifactWorkspaceIsConfigurationScoped(t *testing.T) {
	project := t.TempDir()
	first := &Coordinator{ProjectRoot: project, ConfigPath: filepath.Join(project, ".local", "config.yml")}
	second := &Coordinator{ProjectRoot: project, ConfigPath: filepath.Join(project, ".local", "config.docker-sandboxes.yml")}
	const manifestHash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	firstPath, err := first.dockerSandboxesArtifactRoot(manifestHash)
	if err != nil {
		t.Fatal(err)
	}
	firstAgain, err := first.dockerSandboxesArtifactRoot(manifestHash)
	if err != nil {
		t.Fatal(err)
	}
	secondPath, err := second.dockerSandboxesArtifactRoot(manifestHash)
	if err != nil {
		t.Fatal(err)
	}
	if firstPath != firstAgain {
		t.Fatalf("same configuration workspace is unstable: %q != %q", firstPath, firstAgain)
	}
	if firstPath == secondPath {
		t.Fatalf("different configs share Docker Sandboxes workspace %q", firstPath)
	}
}
