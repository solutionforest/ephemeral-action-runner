package image

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVerifyDockerSandboxesArchiveAcceptsExactDockerExport(t *testing.T) {
	path, digest, labels := writeDockerArchiveFixture(t, false, false)
	verified, err := verifyDockerSandboxesArchive(path, "docker.io/library/epar-template:test-amd64", "linux/amd64", digest, labels)
	if err != nil {
		t.Fatal(err)
	}
	if verified.ImageDigest != digest || verified.ArchiveBytes == 0 || !validSHA256(verified.ArchiveSHA256) {
		t.Fatalf("verification = %+v", verified)
	}
}

func TestVerifyDockerSandboxesArchiveAgainstOptionalBuildxFixture(t *testing.T) {
	archivePath := os.Getenv("EPAR_TEST_DOCKER_ARCHIVE")
	metadataPath := os.Getenv("EPAR_TEST_DOCKER_ARCHIVE_METADATA")
	if archivePath == "" || metadataPath == "" {
		t.Skip("set EPAR_TEST_DOCKER_ARCHIVE and EPAR_TEST_DOCKER_ARCHIVE_METADATA to validate a real Buildx export")
	}
	var metadata dockerSandboxesBuildMetadata
	if err := readJSONFile(metadataPath, &metadata); err != nil {
		t.Fatal(err)
	}
	if _, err := verifyDockerSandboxesArchive(archivePath, "docker.io/library/epar-template:probe", "linux/amd64", metadata.ImageDigest, map[string]string{
		"io.solutionforest.epar.schema":       "1",
		"io.solutionforest.epar.installation": "probe",
		"io.solutionforest.epar.provider":     "docker-sandboxes",
		"io.solutionforest.epar.role":         "template-staging",
		"io.solutionforest.epar.manifest":     strings.Repeat("a", 64),
	}); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyDockerSandboxesArchiveRejectsTamperingAndUnsafeEntries(t *testing.T) {
	t.Run("digest tampering", func(t *testing.T) {
		path, digest, labels := writeDockerArchiveFixture(t, true, false)
		if _, err := verifyDockerSandboxesArchive(path, "epar-template:test-amd64", "linux/amd64", digest, labels); err == nil {
			t.Fatal("tampered layer was accepted")
		}
	})
	t.Run("duplicate control entry", func(t *testing.T) {
		path, digest, labels := writeDockerArchiveFixture(t, false, true)
		if _, err := verifyDockerSandboxesArchive(path, "epar-template:test-amd64", "linux/amd64", digest, labels); err == nil || !strings.Contains(err.Error(), "duplicate") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("unsafe link", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "unsafe.tar")
		writeTarFixture(t, path, []tarFixtureEntry{{name: "manifest.json", content: []byte("[]")}, {name: "escape", typeflag: tar.TypeSymlink, linkname: "../outside"}})
		if _, err := verifyDockerSandboxesArchive(path, "epar-template:test-amd64", "linux/amd64", "sha256:"+strings.Repeat("a", 64), nil); err == nil || !strings.Contains(err.Error(), "unsafe type") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("truncated", func(t *testing.T) {
		path, digest, labels := writeDockerArchiveFixture(t, false, false)
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, content[:len(content)/2], 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := verifyDockerSandboxesArchive(path, "epar-template:test-amd64", "linux/amd64", digest, labels); err == nil {
			t.Fatal("truncated archive was accepted")
		}
	})
}

func TestVerifyDockerSandboxesArchiveAcceptsExactOCIIndex(t *testing.T) {
	labels := archiveFixtureLabels()
	config := dockerImageConfig{Architecture: "arm64", OS: "linux"}
	config.Config.Labels = labels
	configBytes, _ := json.Marshal(config)
	configDigest := digestBytes(configBytes)
	layer := []byte("layer")
	layerDigest := digestBytes(layer)
	manifest := ociManifest{
		SchemaVersion: 2,
		Config:        ociDescriptor{MediaType: "application/vnd.oci.image.config.v1+json", Digest: configDigest, Size: uint64(len(configBytes))},
		Layers:        []ociDescriptor{{MediaType: "application/vnd.oci.image.layer.v1.tar", Digest: layerDigest, Size: uint64(len(layer))}},
	}
	manifestBytes, _ := json.Marshal(manifest)
	manifestDigest := digestBytes(manifestBytes)
	platform := struct {
		OS           string `json:"os"`
		Architecture string `json:"architecture"`
	}{OS: "linux", Architecture: "arm64"}
	index := ociIndex{SchemaVersion: 2, Manifests: []ociDescriptor{{
		MediaType: "application/vnd.oci.image.manifest.v1+json",
		Digest:    manifestDigest,
		Size:      uint64(len(manifestBytes)),
		Annotations: map[string]string{
			"io.containerd.image.name":          "docker.io/library/epar-template:test-arm64",
			"org.opencontainers.image.ref.name": "test-arm64",
		},
		Platform: &platform,
	}}}
	indexBytes, _ := json.Marshal(index)
	path := filepath.Join(t.TempDir(), "oci.tar")
	writeTarFixture(t, path, []tarFixtureEntry{
		{name: "oci-layout", content: []byte(`{"imageLayoutVersion":"1.0.0"}`)},
		{name: "index.json", content: indexBytes},
		{name: "blobs/sha256/" + strings.TrimPrefix(manifestDigest, "sha256:"), content: manifestBytes},
		{name: "blobs/sha256/" + strings.TrimPrefix(configDigest, "sha256:"), content: configBytes},
		{name: "blobs/sha256/" + strings.TrimPrefix(layerDigest, "sha256:"), content: layer},
	})
	if _, err := verifyDockerSandboxesArchive(path, "docker.io/library/epar-template:test-arm64", "linux/arm64", manifestDigest, labels); err != nil {
		t.Fatal(err)
	}
}

type tarFixtureEntry struct {
	name     string
	content  []byte
	typeflag byte
	linkname string
}

func writeDockerArchiveFixture(t *testing.T, tamperLayer, duplicateManifest bool) (string, string, map[string]string) {
	t.Helper()
	labels := archiveFixtureLabels()
	layer := []byte("verified layer content")
	layerDigest := digestBytes(layer)
	config := dockerImageConfig{Architecture: "amd64", OS: "linux"}
	config.Config.Labels = labels
	config.RootFS.DiffIDs = []string{layerDigest}
	configBytes, _ := json.Marshal(config)
	configDigest := digestBytes(configBytes)
	configName := strings.TrimPrefix(configDigest, "sha256:") + ".json"
	manifestBytes, _ := json.Marshal([]dockerSaveManifestEntry{{
		Config:   configName,
		RepoTags: []string{"epar-template:test-amd64"},
		Layers:   []string{"layer/layer.tar"},
	}})
	if tamperLayer {
		layer = []byte("tampered")
	}
	entries := []tarFixtureEntry{
		{name: "manifest.json", content: manifestBytes},
		{name: configName, content: configBytes},
		{name: "layer/layer.tar", content: layer},
	}
	if duplicateManifest {
		entries = append(entries, tarFixtureEntry{name: "manifest.json", content: manifestBytes})
	}
	path := filepath.Join(t.TempDir(), "template.tar")
	writeTarFixture(t, path, entries)
	return path, configDigest, labels
}

func archiveFixtureLabels() map[string]string {
	return map[string]string{
		"io.solutionforest.epar.schema":       "1",
		"io.solutionforest.epar.installation": "installation",
		"io.solutionforest.epar.provider":     "docker-sandboxes",
		"io.solutionforest.epar.role":         "template-staging",
		"io.solutionforest.epar.manifest":     strings.Repeat("a", 64),
	}
}

func writeTarFixture(t *testing.T, path string, entries []tarFixtureEntry) {
	t.Helper()
	var content bytes.Buffer
	writer := tar.NewWriter(&content)
	for _, entry := range entries {
		typeflag := entry.typeflag
		if typeflag == 0 {
			typeflag = tar.TypeReg
		}
		header := &tar.Header{Name: entry.name, Mode: 0o600, Size: int64(len(entry.content)), Typeflag: typeflag, Linkname: entry.linkname}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if typeflag == tar.TypeReg {
			if _, err := writer.Write(entry.content); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

func digestBytes(content []byte) string {
	sum := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(sum[:])
}
