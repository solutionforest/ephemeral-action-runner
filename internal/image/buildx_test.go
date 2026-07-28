package image

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBuildxBuilderNameIsStableAndProjectScoped(t *testing.T) {
	first := buildxBuilderName(filepath.Join("one", "project"))
	second := buildxBuilderName(filepath.Join("one", "project"))
	other := buildxBuilderName(filepath.Join("two", "project"))
	if first != second {
		t.Fatalf("builder names are not stable: %q != %q", first, second)
	}
	if first == other {
		t.Fatalf("different projects share builder name %q", first)
	}
	if !strings.HasPrefix(first, "epar-") || len(first) != len("epar-")+12 {
		t.Fatalf("builder name %q does not use the bounded EPAR identity", first)
	}
}

func TestLoadBuildxMetadataRequiresExactOwnershipFields(t *testing.T) {
	root := t.TempDir()
	path := BuildxMetadataPath(root)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	metadata := BuildxMetadata{
		SchemaVersion: buildxMetadataSchemaVersion,
		Builder:       buildxBuilderName(root),
		Driver:        "docker-container",
		ProjectRoot:   root,
		CacheLimit:    "64GiB",
		ConfigPath:    filepath.Join(root, ".local", "storage", "buildkitd.toml"),
		CreatedAt:     time.Now().UTC(),
	}
	content, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadBuildxMetadata(root)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Builder != metadata.Builder || loaded.CacheLimit != "64GiB" {
		t.Fatalf("loaded metadata = %+v, want %+v", loaded, metadata)
	}

	metadata.Driver = "docker"
	content, _ = json.Marshal(metadata)
	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadBuildxMetadata(root); err == nil {
		t.Fatal("LoadBuildxMetadata accepted a shared Docker driver")
	}
}
