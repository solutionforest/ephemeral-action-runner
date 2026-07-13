package pool

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
)

func TestImageManifestHashChangesWithImageInputs(t *testing.T) {
	root := t.TempDir()
	writeManifestTestScripts(t, root)
	if err := os.MkdirAll(filepath.Join(root, "work", "images"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "work", "images", "source.tar"), []byte("source-v1"), 0644); err != nil {
		t.Fatal(err)
	}
	customPath := filepath.Join(root, "custom.sh")
	if err := os.WriteFile(customPath, []byte("echo custom-v1\n"), 0644); err != nil {
		t.Fatal(err)
	}

	manager := Manager{
		Config: config.Config{
			Image: config.ImageConfig{
				SourceType:           config.ImageSourceRootFSTar,
				SourceImage:          "work/images/source.tar",
				OutputImage:          "work/images/out.tar",
				RunnerVersion:        "latest",
				CustomInstallScripts: []string{"custom.sh"},
			},
			Provider: config.ProviderConfig{Type: "wsl"},
		},
		ProjectRoot: root,
	}
	hash := func() string {
		t.Helper()
		manifest, err := manager.desiredImageManifest(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		got, err := imageManifestHash(manifest)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	base := hash()

	manager.Config.Image.RunnerVersion = "2.999.0"
	if got := hash(); got == base {
		t.Fatal("manifest hash did not change after runner version changed")
	}
	manager.Config.Image.RunnerVersion = "latest"

	if err := os.WriteFile(filepath.Join(root, "scripts", "guest", "ubuntu", "install-base.sh"), []byte("echo script-v2\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if got := hash(); got == base {
		t.Fatal("manifest hash did not change after EPAR script changed")
	}
	if err := os.WriteFile(filepath.Join(root, "scripts", "guest", "ubuntu", "install-base.sh"), []byte("echo script-v1\n"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(customPath, []byte("echo custom-v2\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if got := hash(); got == base {
		t.Fatal("manifest hash did not change after custom script changed")
	}
	if err := os.WriteFile(customPath, []byte("echo custom-v1\n"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(root, "work", "images", "source.tar"), []byte("source-v2"), 0644); err != nil {
		t.Fatal(err)
	}
	if got := hash(); got == base {
		t.Fatal("manifest hash did not change after source tar changed")
	}
}

func TestDockerDindImageStateUsesManifestLabel(t *testing.T) {
	oldOutput := runHostOutputCommand
	t.Cleanup(func() {
		runHostOutputCommand = oldOutput
	})
	manager := Manager{Config: config.Config{
		Image:    config.ImageConfig{OutputImage: "epar-test"},
		Provider: config.ProviderConfig{Type: "docker-dind"},
	}}

	runHostOutputCommand = func(context.Context, string, ...string) (string, error) {
		return "", errors.New("No such image: epar-test")
	}
	state, err := manager.currentImageState(context.Background(), "want")
	if err != nil {
		t.Fatal(err)
	}
	if state != imageStateMissing {
		t.Fatalf("state = %v, want missing", state)
	}

	runHostOutputCommand = func(context.Context, string, ...string) (string, error) {
		return `{"` + imageManifestLabel + `":"want"}`, nil
	}
	state, err = manager.currentImageState(context.Background(), "want")
	if err != nil {
		t.Fatal(err)
	}
	if state != imageStateCurrent {
		t.Fatalf("state = %v, want current", state)
	}

	runHostOutputCommand = func(context.Context, string, ...string) (string, error) {
		return `{"` + imageManifestLabel + `":"old"}`, nil
	}
	state, err = manager.currentImageState(context.Background(), "want")
	if err != nil {
		t.Fatal(err)
	}
	if state != imageStateOutdated {
		t.Fatalf("state = %v, want outdated", state)
	}
}

func TestWSLImageStateUsesSidecarManifest(t *testing.T) {
	root := t.TempDir()
	output := filepath.Join(root, "work", "images", "out.tar")
	manager := Manager{ProjectRoot: root, Config: config.Config{
		Image:    config.ImageConfig{OutputImage: "work/images/out.tar"},
		Provider: config.ProviderConfig{Type: "wsl"},
	}}

	state, err := manager.currentImageState(context.Background(), "want")
	if err != nil {
		t.Fatal(err)
	}
	if state != imageStateMissing {
		t.Fatalf("state = %v, want missing", state)
	}

	if err := os.MkdirAll(filepath.Dir(output), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(output, []byte("tar"), 0644); err != nil {
		t.Fatal(err)
	}
	state, err = manager.currentImageState(context.Background(), "want")
	if err != nil {
		t.Fatal(err)
	}
	if state != imageStateOutdated {
		t.Fatalf("state = %v, want outdated without sidecar", state)
	}

	if err := writeStoredImageManifest(wslImageManifestSidecarPath(output), ImageManifest{ProviderType: "wsl", OutputImage: "work/images/out.tar"}); err != nil {
		t.Fatal(err)
	}
	stored, err := readStoredImageManifest(wslImageManifestSidecarPath(output))
	if err != nil {
		t.Fatal(err)
	}
	state, err = manager.currentImageState(context.Background(), stored.Hash)
	if err != nil {
		t.Fatal(err)
	}
	if state != imageStateCurrent {
		t.Fatalf("state = %v, want current", state)
	}
	state, err = manager.currentImageState(context.Background(), "different")
	if err != nil {
		t.Fatal(err)
	}
	if state != imageStateOutdated {
		t.Fatalf("state = %v, want outdated for mismatch", state)
	}
}

func TestPrepareWSLDockerSourceRootfsInvalidatesMismatchedCache(t *testing.T) {
	oldLogged := runHostLoggedCommand
	oldOutput := runHostOutputCommand
	oldQuiet := runHostQuietCommand
	t.Cleanup(func() {
		runHostLoggedCommand = oldLogged
		runHostOutputCommand = oldOutput
		runHostQuietCommand = oldQuiet
	})
	root := t.TempDir()
	outputPath := filepath.Join(root, "work", "images", "epar-wsl-catthehacker-ubuntu.tar")
	rootfsPath := wslDockerSourceRootfsPath(outputPath)
	if err := os.MkdirAll(filepath.Dir(rootfsPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rootfsPath, []byte("old-rootfs"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rootfsPath+".env", []byte("export OLD='true'\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := writeSourceCacheManifest(sourceCacheManifestPath(rootfsPath), sourceCacheManifest{
		SourceImage:  "ghcr.io/catthehacker/ubuntu:full-latest",
		SourceDigest: "old",
	}); err != nil {
		t.Fatal(err)
	}

	var calls []string
	runHostLoggedCommand = func(_ context.Context, _ string, name string, args ...string) error {
		calls = append(calls, name+" "+strings.Join(args, " "))
		if len(args) > 0 && args[0] == "export" {
			for i := 0; i+1 < len(args); i++ {
				if args[i] == "-o" {
					return os.WriteFile(args[i+1], []byte("new-rootfs"), 0644)
				}
			}
		}
		return nil
	}
	runHostOutputCommand = func(context.Context, string, ...string) (string, error) {
		return `["ImageVersion=20260707.1"]`, nil
	}
	runHostQuietCommand = func(context.Context, string, ...string) error {
		return nil
	}

	manager := Manager{
		Config: config.Config{
			Image: config.ImageConfig{SourceImage: "ghcr.io/catthehacker/ubuntu:full-latest"},
		},
		ProjectRoot: root,
	}
	_, _, err := manager.prepareWSLDockerSourceRootfs(context.Background(), outputPath, filepath.Join(root, "build.log"), ImageManifest{SourceDigest: "new"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(calls, "\n"), "docker export -o "+rootfsPath+".tmp") {
		t.Fatalf("expected cache reconversion, calls:\n%s", strings.Join(calls, "\n"))
	}
	content, err := os.ReadFile(rootfsPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "new-rootfs" {
		t.Fatalf("rootfs content = %q, want new-rootfs", content)
	}
}

func writeManifestTestScripts(t *testing.T, root string) {
	t.Helper()
	files := map[string]string{
		filepath.Join(root, "scripts", "guest", "ubuntu", "install-base.sh"):   "echo script-v1\n",
		filepath.Join(root, "scripts", "container", "ubuntu", "entrypoint.sh"): "echo entrypoint\n",
	}
	for path, content := range files {
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
}
