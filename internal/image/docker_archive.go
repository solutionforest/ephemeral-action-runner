package image

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/solutionforest/ephemeral-action-runner/internal/storage"
)

const (
	maxTemplateArchiveEntries  = 100000
	maxTemplateControlFileSize = 16 << 20
	maxTemplateControlBytes    = 64 << 20
)

type dockerArchiveVerification struct {
	ImageDigest   string
	ArchiveSHA256 string
	ArchiveBytes  uint64
}

type archivedFile struct {
	digest string
	diffID string
	size   uint64
	data   []byte
}

type dockerSaveManifestEntry struct {
	Config   string   `json:"Config"`
	RepoTags []string `json:"RepoTags"`
	Layers   []string `json:"Layers"`
}

type dockerImageConfig struct {
	Architecture string `json:"architecture"`
	OS           string `json:"os"`
	Config       struct {
		Labels map[string]string `json:"Labels"`
	} `json:"config"`
	RootFS struct {
		DiffIDs []string `json:"diff_ids"`
	} `json:"rootfs"`
}

type ociDescriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        uint64            `json:"size"`
	Annotations map[string]string `json:"annotations"`
	Platform    *struct {
		OS           string `json:"os"`
		Architecture string `json:"architecture"`
	} `json:"platform,omitempty"`
}

type ociIndex struct {
	SchemaVersion int             `json:"schemaVersion"`
	Manifests     []ociDescriptor `json:"manifests"`
}

type ociManifest struct {
	SchemaVersion int             `json:"schemaVersion"`
	Config        ociDescriptor   `json:"config"`
	Layers        []ociDescriptor `json:"layers"`
}

func verifyDockerSandboxesArchive(archivePath, expectedTag, expectedPlatform, expectedBuildDigest string, expectedLabels map[string]string) (dockerArchiveVerification, error) {
	target, err := storage.SnapshotFilesystemTarget(archivePath)
	if err != nil {
		return dockerArchiveVerification{}, fmt.Errorf("inspect Docker Sandboxes template archive: %w", err)
	}
	if target.Kind != storage.TargetFile {
		return dockerArchiveVerification{}, errors.New("Docker Sandboxes template archive is not a regular file")
	}
	file, err := os.Open(archivePath)
	if err != nil {
		return dockerArchiveVerification{}, err
	}
	files, err := scanTemplateArchive(file)
	closeErr := file.Close()
	if err != nil {
		return dockerArchiveVerification{}, err
	}
	if closeErr != nil {
		return dockerArchiveVerification{}, closeErr
	}

	var imageDigest string
	switch {
	case files["index.json"].data != nil:
		imageDigest, err = verifyOCIArchive(files, expectedTag, expectedPlatform, expectedLabels)
	case files["manifest.json"].data != nil:
		imageDigest, err = verifyDockerSaveArchive(files, expectedTag, expectedPlatform, expectedLabels)
	default:
		err = errors.New("template archive contains neither OCI index.json nor Docker manifest.json")
	}
	if err != nil {
		return dockerArchiveVerification{}, err
	}
	if imageDigest != expectedBuildDigest {
		return dockerArchiveVerification{}, fmt.Errorf("template archive identity %s does not match Buildx metadata identity %s", imageDigest, expectedBuildDigest)
	}
	archiveSHA, archiveBytes, err := hashFile(archivePath)
	if err != nil {
		return dockerArchiveVerification{}, err
	}
	return dockerArchiveVerification{ImageDigest: imageDigest, ArchiveSHA256: archiveSHA, ArchiveBytes: archiveBytes}, nil
}

func scanTemplateArchive(reader io.Reader) (map[string]archivedFile, error) {
	result := make(map[string]archivedFile)
	tarReader := tar.NewReader(reader)
	var controlBytes uint64
	for entries := 0; ; entries++ {
		if entries >= maxTemplateArchiveEntries {
			return nil, errors.New("template archive contains too many entries")
		}
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read template archive: %w", err)
		}
		name, err := safeArchiveName(header.Name)
		if err != nil {
			return nil, err
		}
		if _, duplicate := result[name]; duplicate {
			return nil, fmt.Errorf("template archive contains duplicate entry %q", name)
		}
		switch header.Typeflag {
		case tar.TypeDir:
			result[name] = archivedFile{}
			continue
		case tar.TypeReg, tar.TypeRegA:
		default:
			return nil, fmt.Errorf("template archive entry %q has unsafe type %d", name, header.Typeflag)
		}
		if header.Size < 0 {
			return nil, fmt.Errorf("template archive entry %q has a negative size", name)
		}
		hasher := sha256.New()
		var capture []byte
		var diffID string
		if shouldCaptureArchiveControl(name, header.Size) {
			if uint64(header.Size) > maxTemplateControlFileSize || controlBytes > maxTemplateControlBytes-uint64(header.Size) {
				return nil, errors.New("template archive control data exceeds the bounded verification limit")
			}
			capture = make([]byte, header.Size)
			if _, err := io.ReadFull(io.TeeReader(tarReader, hasher), capture); err != nil {
				return nil, fmt.Errorf("read template archive entry %q: %w", name, err)
			}
			controlBytes += uint64(header.Size)
		} else if strings.HasSuffix(name, ".tar.gz") {
			// go-containerregistry emits Docker-save layers in compressed form,
			// while image config rootfs.diff_ids bind the uncompressed stream.
			// Hash both in one pass so cache reuse proves the exact archive bytes
			// and the semantic layer identity expected by the image config.
			compressed := io.TeeReader(tarReader, hasher)
			reader, err := gzip.NewReader(compressed)
			if err != nil {
				return nil, fmt.Errorf("open compressed Docker archive layer %q: %w", name, err)
			}
			uncompressedHasher := sha256.New()
			if _, err := io.Copy(uncompressedHasher, reader); err != nil {
				_ = reader.Close()
				return nil, fmt.Errorf("verify compressed Docker archive layer %q: %w", name, err)
			}
			if err := reader.Close(); err != nil {
				return nil, fmt.Errorf("close compressed Docker archive layer %q: %w", name, err)
			}
			diffID = "sha256:" + hex.EncodeToString(uncompressedHasher.Sum(nil))
		} else {
			if _, err := io.Copy(hasher, tarReader); err != nil {
				return nil, fmt.Errorf("hash template archive entry %q: %w", name, err)
			}
		}
		result[name] = archivedFile{
			digest: "sha256:" + hex.EncodeToString(hasher.Sum(nil)),
			diffID: diffID,
			size:   uint64(header.Size),
			data:   capture,
		}
	}
	return result, nil
}

func safeArchiveName(value string) (string, error) {
	if value == "" || strings.ContainsRune(value, 0) || strings.Contains(value, "\\") || strings.HasPrefix(value, "/") {
		return "", fmt.Errorf("template archive contains unsafe path %q", value)
	}
	normalized := strings.TrimSuffix(value, "/")
	clean := path.Clean(normalized)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || clean != normalized {
		return "", fmt.Errorf("template archive contains unsafe path %q", value)
	}
	return clean, nil
}

func shouldCaptureArchiveControl(name string, size int64) bool {
	if size > maxTemplateControlFileSize {
		return false
	}
	// go-containerregistry's Docker tar writer uses the canonical config digest
	// (for example sha256:<hex>) as the config filename. Capture that bounded
	// control blob so a verified OCI image can be materialized without rewriting
	// the library-produced archive into a second, non-canonical layout.
	return name == "manifest.json" || name == "index.json" || name == "oci-layout" || validSHA256(name) || strings.HasSuffix(name, ".json") || strings.HasPrefix(name, "blobs/sha256/")
}

func verifyDockerSaveArchive(files map[string]archivedFile, expectedTag, expectedPlatform string, expectedLabels map[string]string) (string, error) {
	var entries []dockerSaveManifestEntry
	if err := json.Unmarshal(files["manifest.json"].data, &entries); err != nil {
		return "", fmt.Errorf("decode Docker archive manifest: %w", err)
	}
	if len(entries) != 1 {
		return "", fmt.Errorf("Docker archive must contain exactly one image, found %d", len(entries))
	}
	entry := entries[0]
	if len(entry.RepoTags) != 1 || !sameDockerReference(entry.RepoTags[0], expectedTag) {
		return "", fmt.Errorf("Docker archive tag %q does not match expected tag %q", strings.Join(entry.RepoTags, ", "), expectedTag)
	}
	configFile, ok := files[entry.Config]
	if !ok || configFile.data == nil {
		return "", fmt.Errorf("Docker archive is missing configuration %q", entry.Config)
	}
	configDigest := configFile.digest
	configName := strings.TrimSuffix(filepath.Base(entry.Config), ".json")
	configNameDigest := ""
	if validSHA256(configName) {
		configNameDigest = configName
	} else if len(configName) == 64 {
		configNameDigest = "sha256:" + configName
	}
	if configNameDigest == "" || configDigest != configNameDigest {
		return "", errors.New("Docker archive configuration filename does not match its recomputed digest")
	}
	var config dockerImageConfig
	if err := json.Unmarshal(configFile.data, &config); err != nil {
		return "", fmt.Errorf("decode Docker archive configuration: %w", err)
	}
	if err := verifyArchivePlatformAndLabels(config.OS, config.Architecture, config.Config.Labels, expectedPlatform, expectedLabels); err != nil {
		return "", err
	}
	if len(entry.Layers) == 0 || len(entry.Layers) != len(config.RootFS.DiffIDs) {
		return "", errors.New("Docker archive layer list does not match configuration rootfs")
	}
	seen := make(map[string]bool, len(entry.Layers))
	for index, layerName := range entry.Layers {
		if seen[layerName] {
			return "", fmt.Errorf("Docker archive references layer %q more than once", layerName)
		}
		seen[layerName] = true
		layer, ok := files[layerName]
		if !ok {
			return "", fmt.Errorf("Docker archive is missing layer %q", layerName)
		}
		layerDiffID := layer.diffID
		if layerDiffID == "" {
			layerDiffID = layer.digest
		}
		if layerDiffID != config.RootFS.DiffIDs[index] {
			return "", fmt.Errorf("Docker archive layer %q digest does not match configuration diff ID", layerName)
		}
	}
	return configDigest, nil
}

func verifyOCIArchive(files map[string]archivedFile, expectedTag, expectedPlatform string, expectedLabels map[string]string) (string, error) {
	if layout, ok := files["oci-layout"]; !ok || layout.data == nil {
		return "", errors.New("OCI archive is missing oci-layout")
	} else {
		var version struct {
			ImageLayoutVersion string `json:"imageLayoutVersion"`
		}
		if err := json.Unmarshal(layout.data, &version); err != nil || version.ImageLayoutVersion != "1.0.0" {
			return "", errors.New("OCI archive has an unsupported image layout version")
		}
	}
	var index ociIndex
	if err := json.Unmarshal(files["index.json"].data, &index); err != nil {
		return "", fmt.Errorf("decode OCI archive index: %w", err)
	}
	if index.SchemaVersion != 2 {
		return "", errors.New("OCI archive index has an unsupported schema version")
	}
	var selected *ociDescriptor
	for i := range index.Manifests {
		descriptor := &index.Manifests[i]
		fullName := descriptor.Annotations["io.containerd.image.name"]
		shortTag := descriptor.Annotations["org.opencontainers.image.ref.name"]
		_, expectedSuffix, _ := strings.Cut(expectedTag, ":")
		matches := fullName != "" && sameDockerReference(fullName, expectedTag)
		if fullName == "" {
			matches = shortTag != "" && shortTag == expectedSuffix
		}
		if matches {
			if selected != nil {
				return "", fmt.Errorf("OCI archive contains duplicate tag %q", expectedTag)
			}
			selected = descriptor
		}
	}
	if selected == nil {
		return "", fmt.Errorf("OCI archive does not contain exact tag %q", expectedTag)
	}
	manifestBytes, err := verifyOCIDescriptor(files, *selected)
	if err != nil {
		return "", err
	}
	var manifest ociManifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil || manifest.SchemaVersion != 2 {
		return "", errors.New("OCI archive contains an invalid image manifest")
	}
	configBytes, err := verifyOCIDescriptor(files, manifest.Config)
	if err != nil {
		return "", err
	}
	var config dockerImageConfig
	if err := json.Unmarshal(configBytes, &config); err != nil {
		return "", fmt.Errorf("decode OCI image configuration: %w", err)
	}
	if err := verifyArchivePlatformAndLabels(config.OS, config.Architecture, config.Config.Labels, expectedPlatform, expectedLabels); err != nil {
		return "", err
	}
	if selected.Platform != nil && expectedPlatform != selected.Platform.OS+"/"+selected.Platform.Architecture {
		return "", fmt.Errorf("OCI index platform %s/%s does not match expected platform %s", selected.Platform.OS, selected.Platform.Architecture, expectedPlatform)
	}
	for _, layer := range manifest.Layers {
		if _, err := verifyOCIDescriptor(files, layer); err != nil {
			return "", err
		}
	}
	return selected.Digest, nil
}

func verifyOCIDescriptor(files map[string]archivedFile, descriptor ociDescriptor) ([]byte, error) {
	if !validSHA256(descriptor.Digest) {
		return nil, fmt.Errorf("OCI archive contains invalid descriptor digest %q", descriptor.Digest)
	}
	name := "blobs/sha256/" + strings.TrimPrefix(descriptor.Digest, "sha256:")
	file, ok := files[name]
	if !ok {
		return nil, fmt.Errorf("OCI archive is missing blob %s", descriptor.Digest)
	}
	if file.digest != descriptor.Digest || file.size != descriptor.Size {
		return nil, fmt.Errorf("OCI archive blob %s does not match its descriptor", descriptor.Digest)
	}
	return file.data, nil
}

func verifyArchivePlatformAndLabels(osName, architecture string, labels map[string]string, expectedPlatform string, expectedLabels map[string]string) error {
	if osName+"/"+architecture != expectedPlatform {
		return fmt.Errorf("template archive platform %s/%s does not match expected platform %s", osName, architecture, expectedPlatform)
	}
	for name, expected := range expectedLabels {
		if expected == "*" && labels[name] == "" {
			return fmt.Errorf("template archive label %s is missing", name)
		}
		if expected != "*" && labels[name] != expected {
			return fmt.Errorf("template archive label %s=%q does not match expected value %q", name, labels[name], expected)
		}
	}
	return nil
}

func sameDockerReference(left, right string) bool {
	normalize := func(value string) string {
		value = strings.TrimPrefix(value, "docker.io/")
		if !strings.Contains(strings.SplitN(value, ":", 2)[0], "/") {
			value = "library/" + value
		}
		return value
	}
	return normalize(left) == normalize(right)
}
