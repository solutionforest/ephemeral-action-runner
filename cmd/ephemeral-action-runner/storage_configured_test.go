package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
	"github.com/solutionforest/ephemeral-action-runner/internal/storage"
)

func TestConfiguredStorageFilesIncludesCurrentWSLArtifacts(t *testing.T) {
	project := t.TempDir()
	configuredAt := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	cfg := config.Default()
	cfg.Provider.Type = "wsl"
	cfg.Image.SourceType = "docker-image"
	cfg.Image.OutputImage = filepath.Join("work", "images", "runner.tar")

	files := configuredStorageFiles(cfg, project, configuredAt)
	if len(files) != 4 {
		t.Fatalf("configuredStorageFiles() returned %d files, want 4: %+v", len(files), files)
	}
	byRole := make(map[string]storage.ArtifactKind, len(files))
	for _, file := range files {
		if file.Provider != "wsl" || !file.Current || file.ConfiguredAt != configuredAt {
			t.Fatalf("configured file = %+v", file)
		}
		byRole[file.Role] = file.Kind
	}
	if byRole["reusable-image"] != storage.ArtifactProviderImage || byRole["source-rootfs-cache"] != storage.ArtifactProviderCache || byRole["image-manifest"] != storage.ArtifactOther || byRole["source-cache-manifest"] != storage.ArtifactOther {
		t.Fatalf("configured roles = %+v", byRole)
	}
}

func TestConfiguredStorageFilesSkipsNonWSLProvider(t *testing.T) {
	cfg := config.Default()
	cfg.Provider.Type = "docker-container"
	if files := configuredStorageFiles(cfg, t.TempDir(), time.Now()); len(files) != 0 {
		t.Fatalf("configuredStorageFiles() = %+v, want none", files)
	}
}
