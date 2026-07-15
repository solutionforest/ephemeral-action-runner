package pool

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
	"github.com/solutionforest/ephemeral-action-runner/internal/hosttrust"
	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
)

const (
	imageManifestSchemaVersion = 1
	imageManifestGuestPath     = "/opt/epar/image-manifest.json"
	imageManifestLabel         = "org.solutionforest.epar.manifest-sha256"
)

type ImageManifest struct {
	SchemaVersion         int                     `json:"schemaVersion"`
	ProviderType          string                  `json:"providerType"`
	ProviderPlatform      string                  `json:"providerPlatform,omitempty"`
	ProviderRosettaTag    string                  `json:"providerRosettaTag,omitempty"`
	SourceType            string                  `json:"sourceType,omitempty"`
	SourceImage           string                  `json:"sourceImage"`
	SourcePlatform        string                  `json:"sourcePlatform,omitempty"`
	SourceDigest          string                  `json:"sourceDigest,omitempty"`
	OutputImage           string                  `json:"outputImage"`
	RunnerVersion         string                  `json:"runnerVersion"`
	UpstreamCommit        string                  `json:"upstreamCommit,omitempty"`
	EPARScripts           []fileDigest            `json:"eparScripts,omitempty"`
	CustomInstallScripts  []fileDigest            `json:"customInstallScripts,omitempty"`
	TrustedCACertificates []fileDigest            `json:"trustedCaCertificates,omitempty"`
	HostTrust             *hostTrustImageMetadata `json:"hostTrust,omitempty"`
}

type fileDigest struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type storedImageManifest struct {
	Hash     string        `json:"hash"`
	Manifest ImageManifest `json:"manifest"`
}

type sourceCacheManifest struct {
	SourceImage    string `json:"sourceImage"`
	SourcePlatform string `json:"sourcePlatform,omitempty"`
	SourceDigest   string `json:"sourceDigest,omitempty"`
}

func (m *Manager) EnsureImage(ctx context.Context) error {
	manifest, err := m.desiredImageManifest(ctx)
	if err != nil {
		return err
	}
	hash, err := imageManifestHash(manifest)
	if err != nil {
		return err
	}
	if m.DryRun {
		fmt.Printf("[dry-run] would ensure image %s has manifest %s\n", m.Config.Image.OutputImage, hash)
		return m.BuildImage(ctx, ImageBuildOptions{Replace: true, Manifest: &manifest})
	}
	state, err := m.currentImageState(ctx, hash)
	if err != nil {
		return err
	}
	switch state {
	case imageStateCurrent:
		fmt.Printf("image is current: %s\n", m.Config.Image.OutputImage)
		return nil
	case imageStateMissing:
		fmt.Printf("image is missing; building %s\n", m.Config.Image.OutputImage)
	case imageStateOutdated:
		fmt.Printf("image is outdated or not aligned with config; rebuilding %s\n", m.Config.Image.OutputImage)
	}
	return m.BuildImage(ctx, ImageBuildOptions{Replace: true, Manifest: &manifest})
}

type imageState int

const (
	imageStateMissing imageState = iota
	imageStateOutdated
	imageStateCurrent
)

func (m *Manager) currentImageState(ctx context.Context, wantHash string) (imageState, error) {
	switch m.Config.Provider.Type {
	case "docker-dind":
		got, exists, err := m.currentDockerDindManifestHash(ctx)
		if err != nil {
			return imageStateMissing, err
		}
		if !exists {
			return imageStateMissing, nil
		}
		if got != wantHash {
			return imageStateOutdated, nil
		}
		return imageStateCurrent, nil
	case "wsl":
		got, exists, err := m.currentWSLManifestHash()
		if err != nil {
			return imageStateMissing, err
		}
		if !exists {
			return imageStateMissing, nil
		}
		if got != wantHash {
			return imageStateOutdated, nil
		}
		return imageStateCurrent, nil
	case "tart":
		return m.currentTartImageState(ctx)
	default:
		return imageStateMissing, fmt.Errorf("unsupported provider.type %q", m.Config.Provider.Type)
	}
}

func (m *Manager) currentDockerDindManifestHash(ctx context.Context) (string, bool, error) {
	output := strings.TrimSpace(m.Config.Image.OutputImage)
	if output == "" {
		return "", false, fmt.Errorf("image.outputImage is required")
	}
	out, err := runHostOutputCommand(ctx, "docker", "image", "inspect", "--format", "{{json .Config.Labels}}", output)
	if err != nil {
		if dockerInspectMeansMissing(err) {
			return "", false, nil
		}
		return "", false, err
	}
	labels := map[string]string{}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &labels); err != nil {
		return "", true, fmt.Errorf("parse Docker image labels for %s: %w", output, err)
	}
	return labels[imageManifestLabel], true, nil
}

func (m *Manager) currentWSLManifestHash() (string, bool, error) {
	outputPath := config.ProjectPath(m.ProjectRoot, m.Config.Image.OutputImage)
	if _, err := os.Stat(outputPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}
		return "", false, err
	}
	stored, err := readStoredImageManifest(wslImageManifestSidecarPath(outputPath))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", true, nil
		}
		return "", true, err
	}
	return stored.Hash, true, nil
}

func (m *Manager) currentTartImageState(ctx context.Context) (imageState, error) {
	instances, err := m.Provider.List(ctx)
	if err != nil {
		return imageStateMissing, err
	}
	for _, instance := range instances {
		if instance.Name == m.Config.Image.OutputImage || instance.Source == m.Config.Image.OutputImage {
			return imageStateCurrent, nil
		}
	}
	return imageStateMissing, nil
}

func (m *Manager) desiredImageManifest(ctx context.Context) (ImageManifest, error) {
	snapshot, err := m.resolveHostTrust(ctx)
	if err != nil {
		return ImageManifest{}, err
	}
	return m.desiredImageManifestWithHostTrust(ctx, snapshot)
}

func (m *Manager) desiredImageManifestWithHostTrust(ctx context.Context, snapshot hosttrust.Snapshot) (ImageManifest, error) {
	sourceType := m.Config.Image.SourceType
	if sourceType == "" {
		sourceType = config.ImageSourceRootFSTar
		if m.Config.Provider.Type == "docker-dind" {
			sourceType = config.ImageSourceDockerImage
		}
	}
	manifest := ImageManifest{
		SchemaVersion:      imageManifestSchemaVersion,
		ProviderType:       m.Config.Provider.Type,
		ProviderPlatform:   m.Config.Provider.Platform,
		ProviderRosettaTag: m.Config.Provider.RosettaTag,
		SourceType:         sourceType,
		SourceImage:        m.Config.Image.SourceImage,
		SourcePlatform:     m.Config.Image.SourcePlatform,
		OutputImage:        m.Config.Image.OutputImage,
		RunnerVersion:      m.Config.Image.RunnerVersion,
		HostTrust:          hostTrustMetadata(snapshot),
	}
	switch sourceType {
	case config.ImageSourceDockerImage:
		if m.Config.Provider.Type == "docker-dind" || m.Config.Provider.Type == "wsl" {
			digest, err := m.refreshDockerSourceDigest(ctx)
			if err != nil {
				return manifest, err
			}
			manifest.SourceDigest = digest
		}
	case config.ImageSourceRootFSTar:
		if m.Config.Provider.Type == "wsl" {
			digest, err := m.fileSHA256(config.ProjectPath(m.ProjectRoot, m.Config.Image.SourceImage))
			if err != nil {
				return manifest, err
			}
			manifest.SourceDigest = digest
		}
	case "":
	default:
		return manifest, fmt.Errorf("unsupported image.sourceType %q", sourceType)
	}
	scripts, err := m.eparScriptDigests()
	if err != nil {
		return manifest, err
	}
	manifest.EPARScripts = scripts
	customScripts, err := m.customInstallScriptDigests()
	if err != nil {
		return manifest, err
	}
	manifest.CustomInstallScripts = customScripts
	trustedCACertificates, err := m.trustedCACertificateDigests()
	if err != nil {
		return manifest, err
	}
	manifest.TrustedCACertificates = trustedCACertificates
	if m.runnerImagesCopyMode() != runnerImagesCopyNone {
		commit, err := m.runnerImagesCommit()
		if err != nil {
			return manifest, err
		}
		manifest.UpstreamCommit = commit
	}
	return manifest, nil
}

func (m *Manager) refreshDockerSourceDigest(ctx context.Context) (string, error) {
	var digest string
	err := m.timeStartupStage("source_image_pull", func() error {
		var err error
		digest, err = m.refreshDockerSourceDigestUntimed(ctx)
		return err
	})
	return digest, err
}

func (m *Manager) refreshDockerSourceDigestUntimed(ctx context.Context) (string, error) {
	if m.DryRun {
		return "dry-run", nil
	}
	image := strings.TrimSpace(m.Config.Image.SourceImage)
	if image == "" {
		return "", fmt.Errorf("image.sourceImage is required when image.sourceType=docker-image")
	}
	platform := strings.TrimSpace(m.Config.Image.SourcePlatform)
	logPath := filepath.Join(config.ProjectPath(m.ProjectRoot, m.Config.Pool.LogDir), imageLogStem(m.Config.Image.OutputImage)+".source.log")
	fmt.Printf("refreshing Docker source image %s\n", image)
	if err := pullDockerSourceCommand(m, ctx, dockerSourcePullOptions{
		Image:              image,
		Platform:           platform,
		LogPath:            logPath,
		AnnounceRemoteSize: true,
	}); err != nil {
		return "", fmt.Errorf("refresh Docker source image %s: %w", image, err)
	}
	digestsJSON, err := runHostOutputCommand(ctx, "docker", "image", "inspect", "--format", "{{json .RepoDigests}}", image)
	if err != nil {
		return "", err
	}
	var digests []string
	if err := json.Unmarshal([]byte(strings.TrimSpace(digestsJSON)), &digests); err != nil {
		return "", fmt.Errorf("parse Docker source RepoDigests for %s: %w", image, err)
	}
	sort.Strings(digests)
	if len(digests) > 0 {
		digest := digests[0]
		m.writeDockerPullNotice(logPath, "Docker source image digest: "+digest)
		return digest, nil
	}
	imageID, err := runHostOutputCommand(ctx, "docker", "image", "inspect", "--format", "{{.Id}}", image)
	if err != nil {
		return "", err
	}
	digest := strings.TrimSpace(imageID)
	m.writeDockerPullNotice(logPath, "Docker source image ID: "+digest)
	return digest, nil
}

func (m *Manager) eparScriptDigests() ([]fileDigest, error) {
	var roots []string
	switch m.Config.Provider.Type {
	case "docker-dind":
		roots = []string{
			filepath.Join(m.ProjectRoot, "scripts", "guest", "ubuntu"),
			filepath.Join(m.ProjectRoot, "scripts", "container", "ubuntu"),
		}
	case "wsl", "tart":
		roots = []string{filepath.Join(m.ProjectRoot, "scripts", "guest", "ubuntu")}
	default:
		return nil, nil
	}
	var out []fileDigest
	for _, root := range roots {
		digests, err := m.fileDigestsUnder(root)
		if err != nil {
			return nil, err
		}
		out = append(out, digests...)
	}
	sortFileDigests(out)
	return out, nil
}

func (m *Manager) customInstallScriptDigests() ([]fileDigest, error) {
	var out []fileDigest
	for _, script := range m.Config.Image.CustomInstallScripts {
		path, err := m.customInstallScriptHostPath(script)
		if err != nil {
			return nil, err
		}
		digest, err := m.fileDigest(path)
		if err != nil {
			return nil, err
		}
		out = append(out, digest)
	}
	sortFileDigests(out)
	return out, nil
}

func (m *Manager) trustedCACertificateDigests() ([]fileDigest, error) {
	if _, err := m.trustedCACertificates(); err != nil {
		return nil, err
	}
	var out []fileDigest
	for _, configuredPath := range m.Config.Image.TrustedCACertificatePaths {
		path := config.ProjectPath(m.ProjectRoot, strings.TrimSpace(configuredPath))
		digest, err := m.fileDigest(path)
		if err != nil {
			return nil, err
		}
		out = append(out, digest)
	}
	sortFileDigests(out)
	return out, nil
}

func (m *Manager) fileDigestsUnder(root string) ([]fileDigest, error) {
	var out []fileDigest
	if err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".sh") {
			return nil
		}
		digest, err := m.fileDigest(path)
		if err != nil {
			return err
		}
		out = append(out, digest)
		return nil
	}); err != nil {
		return nil, err
	}
	sortFileDigests(out)
	return out, nil
}

func (m *Manager) fileDigest(path string) (fileDigest, error) {
	sha, err := m.fileSHA256(path)
	if err != nil {
		return fileDigest{}, err
	}
	rel, err := filepath.Rel(m.ProjectRoot, path)
	if err != nil || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." || filepath.IsAbs(rel) {
		rel = path
	}
	return fileDigest{Path: filepath.ToSlash(filepath.Clean(rel)), SHA256: sha}, nil
}

func (m *Manager) fileSHA256(path string) (string, error) {
	if m.DryRun {
		return "dry-run", nil
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:]), nil
}

func sortFileDigests(values []fileDigest) {
	sort.Slice(values, func(i, j int) bool {
		return values[i].Path < values[j].Path
	})
}

func imageManifestHash(manifest ImageManifest) (string, error) {
	content, err := json.Marshal(manifest)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:]), nil
}

func storedImageManifestContent(manifest ImageManifest) (string, string, error) {
	hash, err := imageManifestHash(manifest)
	if err != nil {
		return "", "", err
	}
	content, err := json.MarshalIndent(storedImageManifest{Hash: hash, Manifest: manifest}, "", "  ")
	if err != nil {
		return "", "", err
	}
	return string(content) + "\n", hash, nil
}

func readStoredImageManifest(path string) (storedImageManifest, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return storedImageManifest{}, err
	}
	var stored storedImageManifest
	if err := json.Unmarshal(content, &stored); err != nil {
		return storedImageManifest{}, err
	}
	return stored, nil
}

func writeStoredImageManifest(path string, manifest ImageManifest) error {
	content, _, err := storedImageManifestContent(manifest)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0644)
}

func (m *Manager) installImageManifest(ctx context.Context, vmName string, manifest ImageManifest) error {
	content, _, err := storedImageManifestContent(manifest)
	if err != nil {
		return err
	}
	return provider.CopyText(ctx, m.Provider, vmName, imageManifestGuestPath, "0644", content)
}

func wslImageManifestSidecarPath(outputPath string) string {
	return outputPath + ".epar-manifest.json"
}

func sourceCacheManifestPath(rootfsPath string) string {
	return rootfsPath + ".source.json"
}

func sourceCacheMatches(path string, want sourceCacheManifest) bool {
	content, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var got sourceCacheManifest
	if err := json.Unmarshal(content, &got); err != nil {
		return false
	}
	return got == want
}

func writeSourceCacheManifest(path string, manifest sourceCacheManifest) error {
	content, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(content, '\n'), 0644)
}

func dockerInspectMeansMissing(err error) bool {
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "no such image") ||
		strings.Contains(text, "no such object") ||
		strings.Contains(text, "not found")
}
