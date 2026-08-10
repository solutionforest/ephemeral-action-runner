package image

import (
	"os"
	"path/filepath"
	"testing"
)

func TestActivateWSLArtifactReplacesVerifiedPair(t *testing.T) {
	dir := t.TempDir()
	output := filepath.Join(dir, "runner.tar")
	candidate := output + ".epar-candidate-test"
	writeWSLArtifactTestPair(t, output, "old", ImageManifest{SchemaVersion: 1, OutputImage: "old"})
	writeWSLArtifactTestPair(t, candidate, "new", ImageManifest{SchemaVersion: 1, OutputImage: "new"})

	if err := activateWSLArtifact(candidate, output); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "new" {
		t.Fatalf("active archive = %q, want new", content)
	}
	stored, err := readStoredImageManifest(wslImageManifestSidecarPath(output))
	if err != nil {
		t.Fatal(err)
	}
	if stored.Manifest.OutputImage != "new" {
		t.Fatalf("active manifest output = %q, want new", stored.Manifest.OutputImage)
	}
	if _, err := os.Stat(output + wslPreviousArtifactSuffix); !os.IsNotExist(err) {
		t.Fatalf("previous archive still exists: %v", err)
	}
}

func TestRecoverWSLArtifactSwapRestoresPreviousPair(t *testing.T) {
	dir := t.TempDir()
	output := filepath.Join(dir, "runner.tar")
	previous := output + wslPreviousArtifactSuffix
	previousSidecar := wslImageManifestSidecarPath(output) + wslPreviousArtifactSuffix
	writeWSLArtifactTestPair(t, previous, "old", ImageManifest{SchemaVersion: 1, OutputImage: "old"})
	if err := os.Rename(wslImageManifestSidecarPath(previous), previousSidecar); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(output, []byte("partial-new"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := recoverWSLArtifactSwap(output); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "old" {
		t.Fatalf("recovered archive = %q, want old", content)
	}
	stored, err := readStoredImageManifest(wslImageManifestSidecarPath(output))
	if err != nil {
		t.Fatal(err)
	}
	if stored.Manifest.OutputImage != "old" {
		t.Fatalf("recovered manifest output = %q, want old", stored.Manifest.OutputImage)
	}
}

func TestActivateWSLArtifactRejectsSymlinkCandidate(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.tar")
	if err := os.WriteFile(target, []byte("candidate"), 0o600); err != nil {
		t.Fatal(err)
	}
	candidate := filepath.Join(dir, "candidate.tar")
	if err := os.Symlink(target, candidate); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := writeStoredImageManifest(wslImageManifestSidecarPath(candidate), ImageManifest{SchemaVersion: 1}); err != nil {
		t.Fatal(err)
	}

	if err := activateWSLArtifact(candidate, filepath.Join(dir, "output.tar")); err == nil {
		t.Fatal("activateWSLArtifact accepted a symlink candidate")
	}
}

func writeWSLArtifactTestPair(t *testing.T, output, content string, manifest ImageManifest) {
	t.Helper()
	if err := os.WriteFile(output, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeStoredImageManifest(wslImageManifestSidecarPath(output), manifest); err != nil {
		t.Fatal(err)
	}
}
