package image

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

const (
	ManifestSchemaVersion = 2
	ManifestGuestPath     = "/opt/epar/image-manifest.json"
	ManifestLabel         = "org.solutionforest.epar.manifest-sha256"
)

type HostTrustMetadata struct {
	Mode             string   `json:"mode"`
	HostOS           string   `json:"hostOS"`
	Scopes           []string `json:"scopes"`
	Generation       string   `json:"generation"`
	CertificateCount int      `json:"certificateCount"`
}

type FileDigest struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type Manifest struct {
	SchemaVersion         int                `json:"schemaVersion"`
	ProviderType          string             `json:"providerType"`
	ProviderPlatform      string             `json:"providerPlatform,omitempty"`
	ProviderRosettaTag    string             `json:"providerRosettaTag,omitempty"`
	SourceType            string             `json:"sourceType,omitempty"`
	SourceImage           string             `json:"sourceImage"`
	SourcePlatform        string             `json:"sourcePlatform,omitempty"`
	SourceDigest          string             `json:"sourceDigest,omitempty"`
	SourcePlatformDigest  string             `json:"sourcePlatformDigest,omitempty"`
	OutputImage           string             `json:"outputImage"`
	RunnerSelector        string             `json:"runnerSelector"`
	RunnerVersion         string             `json:"runnerVersion"`
	RunnerAssetName       string             `json:"runnerAssetName,omitempty"`
	RunnerAssetURL        string             `json:"runnerAssetUrl,omitempty"`
	RunnerAssetDigest     string             `json:"runnerAssetDigest,omitempty"`
	UpstreamCommit        string             `json:"upstreamCommit,omitempty"`
	EPARScripts           []FileDigest       `json:"eparScripts,omitempty"`
	TemplateInputs        []FileDigest       `json:"templateInputs,omitempty"`
	CustomInstallScripts  []FileDigest       `json:"customInstallScripts,omitempty"`
	TrustedCACertificates []FileDigest       `json:"trustedCaCertificates,omitempty"`
	HostTrust             *HostTrustMetadata `json:"hostTrust,omitempty"`
}

type StoredManifest struct {
	Hash     string   `json:"hash"`
	Manifest Manifest `json:"manifest"`
}

type SourceCacheManifest struct {
	SourceImage    string `json:"sourceImage"`
	SourcePlatform string `json:"sourcePlatform,omitempty"`
	SourceDigest   string `json:"sourceDigest,omitempty"`
}

func ManifestHash(manifest Manifest) (string, error) {
	content, err := json.Marshal(manifest)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:]), nil
}

func StoredManifestContent(manifest Manifest) (string, string, error) {
	hash, err := ManifestHash(manifest)
	if err != nil {
		return "", "", err
	}
	content, err := json.MarshalIndent(StoredManifest{Hash: hash, Manifest: manifest}, "", "  ")
	if err != nil {
		return "", "", err
	}
	return string(content) + "\n", hash, nil
}

func ReadStoredManifest(path string) (StoredManifest, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return StoredManifest{}, err
	}
	var stored StoredManifest
	if err := json.Unmarshal(content, &stored); err != nil {
		return StoredManifest{}, err
	}
	return stored, nil
}

func WriteStoredManifest(path string, manifest Manifest) error {
	content, _, err := StoredManifestContent(manifest)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o644)
}

func SourceCacheManifestPath(rootfsPath string) string {
	return rootfsPath + ".source.json"
}

func WSLSourceRootfsPath(outputPath string) string {
	switch {
	case strings.HasSuffix(outputPath, ".tar.gz"):
		return strings.TrimSuffix(outputPath, ".tar.gz") + ".source.rootfs.tar"
	case strings.HasSuffix(outputPath, ".tgz"):
		return strings.TrimSuffix(outputPath, ".tgz") + ".source.rootfs.tar"
	case strings.HasSuffix(outputPath, ".tar"):
		return strings.TrimSuffix(outputPath, ".tar") + ".source.rootfs.tar"
	default:
		return outputPath + ".source.rootfs.tar"
	}
}

func WSLImageManifestPath(outputPath string) string {
	return outputPath + ".epar-manifest.json"
}

func SourceCacheMatches(path string, want SourceCacheManifest) bool {
	content, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var got SourceCacheManifest
	if err := json.Unmarshal(content, &got); err != nil {
		return false
	}
	return got == want
}

func WriteSourceCacheManifest(path string, manifest SourceCacheManifest) error {
	content, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(content, '\n'), 0o644)
}

func DockerInspectMeansMissing(err error) bool {
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "no such image") || strings.Contains(text, "no such object") || strings.Contains(text, "not found")
}
