package image

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDockerSandboxesArtifactWorkspaceIsConfigurationScoped(t *testing.T) {
	project := t.TempDir()
	firstConfig := filepath.Join(project, ".local", "config.yml")
	secondConfig := filepath.Join(project, ".local", "config.docker-sandboxes.yml")
	if err := os.MkdirAll(filepath.Dir(firstConfig), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{firstConfig, secondConfig} {
		if err := os.WriteFile(path, []byte("provider:\n  type: docker-sandboxes\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	first := &Coordinator{ProjectRoot: project, ConfigPath: firstConfig}
	second := &Coordinator{ProjectRoot: project, ConfigPath: secondConfig}
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

func TestDockerSandboxesBuildGenerationResumesAndRotatesWithoutConfigCollisions(t *testing.T) {
	project := t.TempDir()
	firstConfig := filepath.Join(project, ".local", "config.first.yml")
	secondConfig := filepath.Join(project, ".local", "config.second.yml")
	if err := os.MkdirAll(filepath.Dir(firstConfig), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{firstConfig, secondConfig} {
		if err := os.WriteFile(path, []byte("provider:\n  type: docker-sandboxes\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	const manifestHash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	first := &Coordinator{ProjectRoot: project, ConfigPath: firstConfig}
	second := &Coordinator{ProjectRoot: project, ConfigPath: secondConfig}

	firstGeneration, firstWorkspace, firstLock, err := first.beginDockerSandboxesBuild(manifestHash)
	if err != nil {
		t.Fatal(err)
	}
	legacyWorkspace, err := first.dockerSandboxesArtifactRoot(manifestHash)
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(filepath.Clean(firstWorkspace), filepath.Clean(legacyWorkspace)+string(filepath.Separator)) {
		t.Fatalf("generation workspace %q is nested beneath legacy cleanup target %q", firstWorkspace, legacyWorkspace)
	}
	if err := firstLock.Close(); err != nil {
		t.Fatal(err)
	}
	resumedGeneration, resumedWorkspace, resumedLock, err := first.beginDockerSandboxesBuild(manifestHash)
	if err != nil {
		t.Fatal(err)
	}
	if resumedGeneration != firstGeneration || resumedWorkspace != firstWorkspace {
		t.Fatalf("interrupted generation did not resume exactly: first=%+v %q resumed=%+v %q", firstGeneration, firstWorkspace, resumedGeneration, resumedWorkspace)
	}
	if err := resumedLock.Close(); err != nil {
		t.Fatal(err)
	}

	secondGeneration, secondWorkspace, secondLock, err := second.beginDockerSandboxesBuild(manifestHash)
	if err != nil {
		t.Fatal(err)
	}
	if err := secondLock.Close(); err != nil {
		t.Fatal(err)
	}
	firstTag := dockerSandboxesGenerationTag("full-latest", manifestHash, "amd64", firstGeneration.ConfigID, firstGeneration.Generation)
	secondTag := dockerSandboxesGenerationTag("full-latest", manifestHash, "amd64", secondGeneration.ConfigID, secondGeneration.Generation)
	if firstGeneration.ConfigID == secondGeneration.ConfigID || firstGeneration.Generation == secondGeneration.Generation || firstWorkspace == secondWorkspace || firstTag == secondTag {
		t.Fatalf("configuration-scoped candidates collided: first=%+v %q %q second=%+v %q %q", firstGeneration, firstWorkspace, firstTag, secondGeneration, secondWorkspace, secondTag)
	}
	if !strings.Contains(firstTag, manifestHash[:16]+"-amd64-"+firstGeneration.ConfigID+"-"+firstGeneration.Generation) {
		t.Fatalf("generation tag %q does not bind manifest, platform, config, and generation", firstTag)
	}

	rotatingGeneration, rotatingWorkspace, rotatingLock, err := first.beginDockerSandboxesBuild(manifestHash)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.clearDockerSandboxesBuildGeneration(manifestHash, rotatingWorkspace); err != nil {
		t.Fatal(err)
	}
	if err := rotatingLock.Close(); err != nil {
		t.Fatal(err)
	}
	nextGeneration, nextWorkspace, nextLock, err := first.beginDockerSandboxesBuild(manifestHash)
	if err != nil {
		t.Fatal(err)
	}
	defer nextLock.Close()
	nextTag := dockerSandboxesGenerationTag("full-latest", manifestHash, "amd64", nextGeneration.ConfigID, nextGeneration.Generation)
	if rotatingGeneration.Generation == nextGeneration.Generation || rotatingWorkspace == nextWorkspace || firstTag == nextTag {
		t.Fatalf("completed force-build generation was reused: previous=%+v %q next=%+v %q", rotatingGeneration, rotatingWorkspace, nextGeneration, nextWorkspace)
	}
}
