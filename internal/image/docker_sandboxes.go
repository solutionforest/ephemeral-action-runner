package image

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
	"github.com/solutionforest/ephemeral-action-runner/internal/hosttrust"
	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
	"github.com/solutionforest/ephemeral-action-runner/internal/storage"
)

const (
	dockerSandboxesReceiptSchema  = 2
	dockerSandboxesMetadataSchema = 4
)

var dockerTagPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)

type ResolvedDockerSource struct {
	Reference            string `json:"reference"`
	ImmutableReference   string `json:"immutableReference"`
	IndexDigest          string `json:"indexDigest"`
	PlatformDigest       string `json:"platformDigest"`
	Platform             string `json:"platform"`
	CompressedLayerBytes uint64 `json:"compressedLayerBytes"`
}

type dockerManifestDescriptor struct {
	Digest   string `json:"digest"`
	Size     uint64 `json:"size"`
	Platform struct {
		OS           string `json:"os"`
		Architecture string `json:"architecture"`
	} `json:"platform"`
}

type dockerManifestDocument struct {
	MediaType string                     `json:"mediaType"`
	Manifests []dockerManifestDescriptor `json:"manifests"`
	Layers    []dockerManifestDescriptor `json:"layers"`
}

type dockerSandboxesReceipt struct {
	SchemaVersion  int                       `json:"schemaVersion"`
	ManifestHash   string                    `json:"manifestHash"`
	Manifest       Manifest                  `json:"manifest"`
	Source         ResolvedDockerSource      `json:"source"`
	Artifact       provider.TemplateArtifact `json:"artifact"`
	MetadataPath   string                    `json:"metadataPath"`
	MetadataSHA256 string                    `json:"metadataSha256"`
	ArchivePath    string                    `json:"archivePath"`
	ArchiveSHA256  string                    `json:"archiveSha256"`
	ActivatedAt    time.Time                 `json:"activatedAt"`
}

type dockerSandboxesSourceLock struct {
	SchemaVersion      int `json:"schemaVersion"`
	DockerfileFrontend struct {
		Reference string `json:"reference"`
	} `json:"dockerfileFrontend"`
	SBOMGenerator struct {
		InspectionReference string `json:"inspectionReference"`
	} `json:"sbomGenerator"`
	GoBuilder struct {
		Version     string `json:"version"`
		IndexDigest string `json:"indexDigest"`
	} `json:"goBuilder"`
	HookLauncher struct {
		SHA256 string `json:"sha256"`
	} `json:"hookLauncher"`
	ActionsRunner struct {
		Version string `json:"version"`
	} `json:"actionsRunner"`
	Tini struct {
		Version string `json:"version"`
	} `json:"tini"`
	Platforms map[string]struct {
		GoBuilderReference               string `json:"goBuilderReference"`
		GoBuilderManifestDigest          string `json:"goBuilderManifestDigest"`
		SBOMGeneratorReference           string `json:"sbomGeneratorReference"`
		SBOMGeneratorManifestDigest      string `json:"sbomGeneratorManifestDigest"`
		DockerfileFrontendManifestDigest string `json:"dockerfileFrontendManifestDigest"`
		ActionsRunner                    struct {
			URL    string `json:"url"`
			SHA256 string `json:"sha256"`
		} `json:"actionsRunner"`
		Tini struct {
			URL    string `json:"url"`
			SHA256 string `json:"sha256"`
		} `json:"tini"`
	} `json:"platforms"`
}

type dockerSandboxesBuildMetadata struct {
	ImageDigest string          `json:"containerimage.digest"`
	Provenance  json.RawMessage `json:"buildx.build.provenance"`
	BuildRef    string          `json:"buildx.build.ref"`
}

type buildxAttachmentDocument struct {
	Manifests []struct {
		MediaType   string            `json:"mediaType"`
		Digest      string            `json:"digest"`
		Annotations map[string]string `json:"annotations"`
	} `json:"manifests"`
	Layers []struct {
		MediaType   string            `json:"mediaType"`
		Digest      string            `json:"digest"`
		Size        uint64            `json:"size"`
		Annotations map[string]string `json:"annotations"`
	} `json:"layers"`
}

type buildxHistoryEntry struct {
	Ref    string `json:"ref"`
	Status string `json:"status"`
}

type buildxHistoryInspection struct {
	Ref       string `json:"Ref"`
	Status    string `json:"Status"`
	BuildArgs []struct {
		Name  string `json:"Name"`
		Value string `json:"Value"`
	} `json:"BuildArgs"`
	Attachments []struct {
		Digest string `json:"Digest"`
		Type   string `json:"Type"`
	} `json:"Attachments"`
}

type dockerSandboxesTemplateMetadata struct {
	SchemaVersion int    `json:"schemaVersion"`
	Profile       string `json:"profile"`
	Platform      string `json:"platform"`
	ManifestHash  string `json:"manifestHash"`
	Template      struct {
		Tag           string `json:"tag"`
		Digest        string `json:"digest"`
		CacheID       string `json:"cacheID"`
		RootDisk      string `json:"rootDisk"`
		Archive       string `json:"archive"`
		ArchiveSHA256 string `json:"archiveSha256"`
		ArchiveBytes  uint64 `json:"archiveBytes"`
	} `json:"template"`
	Source        ResolvedDockerSource `json:"source"`
	Compatibility struct {
		TemplateSchemaVersion     int    `json:"templateSchemaVersion"`
		RunnerExecution           string `json:"runnerExecution"`
		DockerDaemonOwner         string `json:"dockerDaemonOwner"`
		ExpectedDockerDaemonCount int    `json:"expectedDockerDaemonCount"`
	} `json:"compatibility"`
	Artifacts map[string]artifactEvidence `json:"artifacts"`
}

type artifactEvidence struct {
	Path         string `json:"path"`
	SHA256       string `json:"sha256"`
	SourceDigest string `json:"sourceDigest,omitempty"`
}

// NormalizeCatthehackerSource accepts the user-facing family shorthand or one
// exact tag while keeping the supported source repository explicit.
func NormalizeCatthehackerSource(input string) (string, error) {
	value := strings.TrimSpace(input)
	if value == "" {
		value = "full"
	}
	switch value {
	case "full", "act", "dotnet", "js":
		value += "-latest"
	}
	const repository = "ghcr.io/catthehacker/ubuntu"
	if strings.HasPrefix(value, repository+":") {
		value = strings.TrimPrefix(value, repository+":")
	}
	if !dockerTagPattern.MatchString(value) || strings.Contains(value, "/") || strings.Contains(value, "@") {
		return "", fmt.Errorf("Docker Sandboxes source must be a catthehacker/ubuntu tag such as full, act, dotnet, js, or go-24.04")
	}
	return repository + ":" + value, nil
}

// ResolveCatthehackerSource resolves a mutable tag to the exact OCI index and
// platform manifest identities that will be recorded in the artifact receipt.
func ResolveCatthehackerSource(ctx context.Context, input, platform string) (ResolvedDockerSource, error) {
	reference, err := NormalizeCatthehackerSource(input)
	if err != nil {
		return ResolvedDockerSource{}, err
	}
	indexRaw, err := exec.CommandContext(ctx, "docker", "buildx", "imagetools", "inspect", "--raw", reference).Output()
	if err != nil {
		return ResolvedDockerSource{}, fmt.Errorf("resolve source image %s: %w", reference, err)
	}
	var index dockerManifestDocument
	if err := json.Unmarshal(indexRaw, &index); err != nil {
		return ResolvedDockerSource{}, fmt.Errorf("parse source image index for %s: %w", reference, err)
	}
	parts := strings.Split(platform, "/")
	var platformDigest string
	if len(parts) == 2 {
		for _, descriptor := range index.Manifests {
			if descriptor.Platform.OS == parts[0] && descriptor.Platform.Architecture == parts[1] {
				if platformDigest != "" {
					return ResolvedDockerSource{}, fmt.Errorf("source image %s contains multiple %s manifests", reference, platform)
				}
				platformDigest = descriptor.Digest
			}
		}
	}
	if platformDigest == "" {
		return ResolvedDockerSource{}, fmt.Errorf("source image %s does not provide %s", reference, platform)
	}
	tagSeparator := strings.LastIndex(reference, ":")
	if tagSeparator < 0 {
		return ResolvedDockerSource{}, fmt.Errorf("source image %s has no tag", reference)
	}
	platformRaw, err := exec.CommandContext(ctx, "docker", "buildx", "imagetools", "inspect", "--raw", reference[:tagSeparator]+"@"+platformDigest).Output()
	if err != nil {
		return ResolvedDockerSource{}, fmt.Errorf("resolve source image %s for %s: %w", reference, platform, err)
	}
	return parseResolvedDockerSource(reference, platform, indexRaw, platformRaw)
}

func sourceProfile(reference string) string {
	_, tag, found := strings.Cut(reference, ":")
	if !found || tag == "" {
		return "custom"
	}
	return strings.ToLower(tag)
}

func parseResolvedDockerSource(reference, platform string, indexRaw, platformRaw []byte) (ResolvedDockerSource, error) {
	indexRaw = trimManifestCommandNewline(indexRaw)
	platformRaw = trimManifestCommandNewline(platformRaw)
	var index dockerManifestDocument
	if err := json.Unmarshal(indexRaw, &index); err != nil {
		return ResolvedDockerSource{}, fmt.Errorf("parse source image manifest index: %w", err)
	}
	parts := strings.Split(platform, "/")
	if len(parts) != 2 || parts[0] != "linux" || (parts[1] != "amd64" && parts[1] != "arm64") {
		return ResolvedDockerSource{}, fmt.Errorf("unsupported Docker Sandboxes source platform %q", platform)
	}
	var matching []dockerManifestDescriptor
	for _, descriptor := range index.Manifests {
		if descriptor.Platform.OS == parts[0] && descriptor.Platform.Architecture == parts[1] {
			matching = append(matching, descriptor)
		}
	}
	if len(matching) != 1 {
		return ResolvedDockerSource{}, fmt.Errorf("source image %s must contain exactly one %s manifest; found %d", reference, platform, len(matching))
	}
	indexSum := sha256.Sum256(indexRaw)
	indexDigest := "sha256:" + hex.EncodeToString(indexSum[:])
	if !validSHA256(matching[0].Digest) {
		return ResolvedDockerSource{}, fmt.Errorf("source image %s omitted a valid %s manifest digest", reference, platform)
	}
	var manifest dockerManifestDocument
	if err := json.Unmarshal(platformRaw, &manifest); err != nil {
		return ResolvedDockerSource{}, fmt.Errorf("parse source image %s manifest: %w", platform, err)
	}
	var compressed uint64
	for _, layer := range manifest.Layers {
		if layer.Size > ^uint64(0)-compressed {
			return ResolvedDockerSource{}, fmt.Errorf("source image compressed layer size overflows")
		}
		compressed += layer.Size
	}
	tagSeparator := strings.LastIndex(reference, ":")
	if tagSeparator < 0 {
		return ResolvedDockerSource{}, fmt.Errorf("source image %s has no tag", reference)
	}
	return ResolvedDockerSource{
		Reference:            reference,
		ImmutableReference:   reference[:tagSeparator] + "@" + indexDigest,
		IndexDigest:          indexDigest,
		PlatformDigest:       matching[0].Digest,
		Platform:             platform,
		CompressedLayerBytes: compressed,
	}, nil
}

func trimManifestCommandNewline(content []byte) []byte {
	if len(content) != 0 && content[len(content)-1] == '\n' {
		content = content[:len(content)-1]
		if len(content) != 0 && content[len(content)-1] == '\r' {
			content = content[:len(content)-1]
		}
	}
	return content
}

func (m *Coordinator) resolveDockerSandboxesSource(ctx context.Context) (ResolvedDockerSource, error) {
	reference := strings.TrimSpace(m.Config.Image.SourceImage)
	if m.Config.Provider.Type == "docker-sandboxes" {
		var err error
		reference, err = NormalizeCatthehackerSource(reference)
		if err != nil {
			return ResolvedDockerSource{}, err
		}
	} else if reference == "" {
		return ResolvedDockerSource{}, errors.New("image.sourceImage is required")
	}
	platform := strings.TrimSpace(m.Config.Image.SourcePlatform)
	if platform == "" {
		platform = strings.TrimSpace(m.Config.Provider.Platform)
	}
	indexRaw, err := m.runHostOutput(ctx, "docker", "buildx", "imagetools", "inspect", "--raw", reference)
	if err != nil {
		return ResolvedDockerSource{}, fmt.Errorf("resolve source image %s: %w", reference, err)
	}
	var index dockerManifestDocument
	if err := json.Unmarshal([]byte(indexRaw), &index); err != nil {
		return ResolvedDockerSource{}, fmt.Errorf("parse source image index for %s: %w", reference, err)
	}
	parts := strings.Split(platform, "/")
	var platformDigest string
	if len(parts) == 2 {
		for _, descriptor := range index.Manifests {
			if descriptor.Platform.OS == parts[0] && descriptor.Platform.Architecture == parts[1] {
				if platformDigest != "" {
					return ResolvedDockerSource{}, fmt.Errorf("source image %s contains multiple %s manifests", reference, platform)
				}
				platformDigest = descriptor.Digest
			}
		}
	}
	if platformDigest == "" {
		return ResolvedDockerSource{}, fmt.Errorf("source image %s does not provide %s", reference, platform)
	}
	tagSeparator := strings.LastIndex(reference, ":")
	if tagSeparator <= strings.LastIndex(reference, "/") {
		return ResolvedDockerSource{}, fmt.Errorf("source image %s must include a tag", reference)
	}
	repository := reference[:tagSeparator]
	platformRaw, err := m.runHostOutput(ctx, "docker", "buildx", "imagetools", "inspect", "--raw", repository+"@"+platformDigest)
	if err != nil {
		return ResolvedDockerSource{}, fmt.Errorf("resolve source platform manifest %s: %w", platform, err)
	}
	return parseResolvedDockerSource(reference, platform, []byte(indexRaw), []byte(platformRaw))
}

func (m *Coordinator) dockerSandboxesDesiredManifest(ctx context.Context) (Manifest, ResolvedDockerSource, error) {
	source, err := m.resolveDockerSandboxesSource(ctx)
	if err != nil {
		return Manifest{}, ResolvedDockerSource{}, err
	}
	lock, err := loadDockerSandboxesSourceLock(m.ProjectRoot, source.Platform)
	if err != nil {
		return Manifest{}, ResolvedDockerSource{}, err
	}
	configuredRunnerVersion := strings.TrimSpace(m.Config.Image.RunnerVersion)
	if configuredRunnerVersion != "" && configuredRunnerVersion != "latest" && configuredRunnerVersion != lock.ActionsRunner.Version {
		return Manifest{}, ResolvedDockerSource{}, fmt.Errorf("Docker Sandboxes build inputs pin Actions runner %s; image.runnerVersion %q is not available", lock.ActionsRunner.Version, configuredRunnerVersion)
	}
	snapshot, err := m.resolveHostTrust(ctx)
	if err != nil {
		return Manifest{}, ResolvedDockerSource{}, err
	}
	customScripts, err := m.customInstallScriptDigests()
	if err != nil {
		return Manifest{}, ResolvedDockerSource{}, err
	}
	trustedCertificates, err := m.trustedCACertificateDigests()
	if err != nil {
		return Manifest{}, ResolvedDockerSource{}, err
	}
	templateInputs, err := fileDigestsRecursive(filepath.Join(m.ProjectRoot, "templates", "docker-sandboxes"))
	if err != nil {
		return Manifest{}, ResolvedDockerSource{}, err
	}
	manifest := Manifest{
		SchemaVersion:         ManifestSchemaVersion,
		ProviderType:          "docker-sandboxes",
		ProviderPlatform:      source.Platform,
		SourceType:            config.ImageSourceDockerImage,
		SourceImage:           source.Reference,
		SourcePlatform:        source.Platform,
		SourceDigest:          source.IndexDigest,
		SourcePlatformDigest:  source.PlatformDigest,
		OutputImage:           "docker-sandboxes-template",
		RunnerVersion:         lock.ActionsRunner.Version,
		TemplateInputs:        templateInputs,
		CustomInstallScripts:  customScripts,
		TrustedCACertificates: trustedCertificates,
		HostTrust:             hostTrustMetadata(snapshot),
	}
	return manifest, source, nil
}

func fileDigestsRecursive(root string) ([]FileDigest, error) {
	var result []FileDigest
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("template input %s is not a regular file", path)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(content)
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		result = append(result, FileDigest{Path: filepath.ToSlash(relative), SHA256: hex.EncodeToString(sum[:])})
		return nil
	})
	sort.Slice(result, func(i, j int) bool { return result[i].Path < result[j].Path })
	return result, err
}

func DockerSandboxesReceiptPath(projectRoot string) string {
	return filepath.Join(projectRoot, ".local", "state", "image", "docker-sandboxes", "active.json")
}

func LoadDockerSandboxesReceipt(projectRoot string) (provider.TemplateArtifact, string, time.Time, error) {
	receipt, err := readDockerSandboxesReceipt(projectRoot)
	if err != nil {
		return provider.TemplateArtifact{}, "", time.Time{}, err
	}
	return receipt.Artifact, receipt.MetadataSHA256, receipt.ActivatedAt, nil
}

func readDockerSandboxesReceipt(projectRoot string) (dockerSandboxesReceipt, error) {
	content, err := os.ReadFile(DockerSandboxesReceiptPath(projectRoot))
	if err != nil {
		return dockerSandboxesReceipt{}, err
	}
	var receipt dockerSandboxesReceipt
	if err := json.Unmarshal(content, &receipt); err != nil {
		return dockerSandboxesReceipt{}, err
	}
	if receipt.SchemaVersion != dockerSandboxesReceiptSchema || receipt.ManifestHash == "" || receipt.Artifact.Reference == "" || receipt.Artifact.Digest == "" || receipt.Artifact.RootDisk == "" || receipt.MetadataSHA256 == "" || receipt.ArchiveSHA256 == "" || receipt.ActivatedAt.IsZero() {
		return dockerSandboxesReceipt{}, fmt.Errorf("invalid Docker Sandboxes active artifact receipt")
	}
	return receipt, nil
}

func (m *Coordinator) ensureDockerSandboxesTemplate(ctx context.Context, force bool) error {
	runtime, ok := m.Lifecycle.(provider.TemplateArtifactRuntime)
	if !ok {
		return fmt.Errorf("docker-sandboxes provider is missing required template artifact integration")
	}
	manifest, source, err := m.dockerSandboxesDesiredManifest(ctx)
	if err != nil {
		return err
	}
	manifestHash, err := ManifestHash(manifest)
	if err != nil {
		return err
	}
	rootDisk, err := m.effectiveDockerSandboxesRootDisk(source)
	if err != nil {
		return err
	}
	if !force {
		receipt, receiptErr := readDockerSandboxesReceipt(m.ProjectRoot)
		if receiptErr == nil && receipt.ManifestHash == manifestHash && receipt.Artifact.RootDisk == rootDisk {
			if err := runtime.VerifyTemplate(ctx, receipt.Artifact); err != nil {
				return fmt.Errorf("configured Docker Sandboxes artifact is not available exactly as recorded: %w", err)
			}
			if err := runtime.ActivateTemplate(receipt.Artifact); err != nil {
				return err
			}
			m.infof("Docker Sandboxes runner template is current: %s@%s\n", receipt.Artifact.Reference, receipt.Artifact.Digest)
			return nil
		}
		if receiptErr != nil && !errors.Is(receiptErr, os.ErrNotExist) {
			return fmt.Errorf("read Docker Sandboxes active artifact receipt: %w", receiptErr)
		}
	}
	if m.DryRun {
		m.infof("[dry-run] would build and import Docker Sandboxes runner template from %s for %s\n", source.Reference, source.Platform)
		return nil
	}
	return m.buildDockerSandboxesTemplate(ctx, manifest, source, manifestHash, rootDisk, runtime)
}

func estimatedDockerSandboxesExpansion(source ResolvedDockerSource) uint64 {
	estimate, err := EstimateSourceSize(source.CompressedLayerBytes, 0)
	if err != nil {
		return ^uint64(0)
	}
	dockerDisk, _ := config.ParseByteSize(config.DockerSandboxesDefaultDockerDisk)
	plan, err := PlanArtifactStorage("docker-sandboxes", estimate, false, uint64(dockerDisk))
	if err != nil {
		return ^uint64(0)
	}
	return plan.EstimatedIncrementalPeak
}

func (m *Coordinator) effectiveDockerSandboxesRootDisk(source ResolvedDockerSource) (string, error) {
	estimate, err := EstimateSourceSize(source.CompressedLayerBytes, 0)
	if err != nil {
		return "", err
	}
	required, err := AutomaticDockerSandboxesRootBytes(estimate.ExpandedBytes)
	if err != nil {
		return "", err
	}
	if m.Config.DockerSandboxes.RootDisk != config.DockerSandboxesAutomaticRootDisk {
		configured, err := config.ParseByteSize(m.Config.DockerSandboxes.RootDisk)
		if err != nil {
			return "", err
		}
		if uint64(configured) < required {
			return "", fmt.Errorf("dockerSandboxes.rootDisk %s is too small for %s; use rootDisk: auto or at least %dGiB", m.Config.DockerSandboxes.RootDisk, source.Reference, required/storage.GiB)
		}
		return m.Config.DockerSandboxes.RootDisk, nil
	}
	return fmt.Sprintf("%dGiB", required/storage.GiB), nil
}

func (m *Coordinator) buildDockerSandboxesTemplate(ctx context.Context, manifest Manifest, source ResolvedDockerSource, manifestHash, rootDisk string, runtime provider.TemplateArtifactRuntime) error {
	if err := m.preflightStorage("template-build", estimatedDockerSandboxesExpansion(source)); err != nil {
		return err
	}
	if err := m.verifyDockerSandboxesNativeBuilder(ctx, source.Platform); err != nil {
		return err
	}
	lock, err := loadDockerSandboxesSourceLock(m.ProjectRoot, source.Platform)
	if err != nil {
		return err
	}
	profile := sourceProfile(source.Reference)
	architecture := strings.TrimPrefix(source.Platform, "linux/")
	tagProfile := sanitizeTemplateTag(profile)
	templateTag := fmt.Sprintf("epar-docker-sandboxes-catthehacker-%s:%s-%s", tagProfile, manifestHash[:16], architecture)
	artifactRoot := filepath.Join(m.ProjectRoot, "work", "template-builds", "docker-sandboxes", manifestHash)
	if err := os.MkdirAll(artifactRoot, 0o755); err != nil {
		return err
	}
	platformLock := lock.Platforms[source.Platform]
	builder, err := m.ensureBuildxBuilder(ctx, []string{
		source.ImmutableReference,
		lock.DockerfileFrontend.Reference,
		platformLock.GoBuilderReference,
		platformLock.SBOMGeneratorReference,
	})
	if err != nil {
		return err
	}
	buildTrust, err := m.resolveBuildTrust(ctx)
	if err != nil {
		return err
	}
	downloadClient, err := buildTrustHTTPClient(buildTrust)
	if err != nil {
		return err
	}
	inputRoot := filepath.Join(artifactRoot, "inputs")
	actionsRunnerPath := filepath.Join(inputRoot, "actions-runner.tar.gz")
	tiniPath := filepath.Join(inputRoot, "tini")
	if err := verifiedDownload(ctx, downloadClient, platformLock.ActionsRunner.URL, actionsRunnerPath, platformLock.ActionsRunner.SHA256, 0o600); err != nil {
		return fmt.Errorf("acquire locked Actions runner: %w", err)
	}
	if err := verifiedDownload(ctx, downloadClient, platformLock.Tini.URL, tiniPath, platformLock.Tini.SHA256, 0o700); err != nil {
		return fmt.Errorf("acquire locked tini: %w", err)
	}
	buildMetadataPath := filepath.Join(artifactRoot, "build-metadata.json")
	provenancePath := filepath.Join(artifactRoot, "provenance.json")
	sbomPath := filepath.Join(artifactRoot, "sbom.intoto.json")
	inventoryPath := filepath.Join(artifactRoot, "software-inventory.txt")
	compatibilityEvidencePath := filepath.Join(artifactRoot, "compatibility.json")
	archivePath := filepath.Join(artifactRoot, "runner-template.tar")
	metadataPath := filepath.Join(artifactRoot, "template-metadata.json")
	resumed, err := m.resumeDockerSandboxesTemplate(ctx, manifest, source, manifestHash, rootDisk, artifactRoot, metadataPath, archivePath, runtime)
	if err != nil {
		return err
	}
	if resumed {
		return nil
	}
	buildMetadata, localDigest, recoveredBuild, err := m.recoverDockerSandboxesBuild(ctx, builder, templateTag, source, manifestHash, architecture, lock, compatibilityEvidencePath)
	if err != nil {
		return err
	}
	if recoveredBuild {
		m.infof("reusing completed exact Docker Sandboxes Buildx result %s\n", localDigest)
		if err := writeJSONFile(buildMetadataPath, buildMetadata); err != nil {
			return err
		}
		if err := writeAtomicFile(provenancePath, append(buildMetadata.Provenance, '\n'), 0o644); err != nil {
			return err
		}
	} else {
		contextRoot, err := os.MkdirTemp(filepath.Join(m.ProjectRoot, ".local"), "docker-sandboxes-context-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(contextRoot)
		if err := copyDirectory(filepath.Join(m.ProjectRoot, "templates", "docker-sandboxes"), contextRoot); err != nil {
			return err
		}
		compatibilityPath := filepath.Join(contextRoot, "profiles", "generated.compatibility.json")
		compatibility := map[string]any{
			"schemaVersion":             2,
			"templateSchemaVersion":     1,
			"profile":                   profile,
			"platform":                  source.Platform,
			"runnerExecution":           "direct-actions-listener",
			"dockerDaemonOwner":         "docker-sandboxes-runtime",
			"expectedDockerDaemonCount": 1,
		}
		if err := writeJSONFile(compatibilityPath, compatibility); err != nil {
			return err
		}
		if err := copyFile(compatibilityPath, compatibilityEvidencePath, 0o644); err != nil {
			return err
		}
		if err := m.prepareDockerSandboxesCustomScripts(contextRoot); err != nil {
			return err
		}
		runnerTrust, err := m.resolveHostTrust(ctx)
		if err != nil {
			return err
		}
		if err := m.prepareDockerSandboxesTrustPolicy(contextRoot, runnerTrust); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Join(contextRoot, "inputs"), 0o755); err != nil {
			return err
		}
		if err := copyFile(actionsRunnerPath, filepath.Join(contextRoot, "inputs", "actions-runner.tar.gz"), 0o600); err != nil {
			return err
		}
		if err := copyFile(tiniPath, filepath.Join(contextRoot, "inputs", "tini"), 0o755); err != nil {
			return err
		}
		for _, path := range []string{buildMetadataPath, provenancePath, sbomPath, inventoryPath, archivePath, metadataPath} {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
		args := []string{
			"buildx", "build", "--builder", builder, "--platform", source.Platform, "--pull", "--progress", "plain", "--load",
			"--provenance", "mode=max", "--sbom", "generator=" + platformLock.SBOMGeneratorReference,
			"--metadata-file", buildMetadataPath, "--tag", templateTag,
		}
		for _, buildArg := range []string{
			"TEMPLATE_PLATFORM=" + source.Platform,
			"SOURCE_IMAGE=" + source.ImmutableReference,
			"GO_BUILDER_IMAGE=" + platformLock.GoBuilderReference,
			"HOOK_LAUNCHER_SHA256=" + lock.HookLauncher.SHA256,
			"SOURCE_PROFILE=" + profile,
			"SOURCE_INDEX_DIGEST=" + source.IndexDigest,
			"SOURCE_MANIFEST_DIGEST=" + source.PlatformDigest,
			"SOURCE_REVISION=" + source.IndexDigest,
			"TEMPLATE_VERSION=" + manifestHash[:16] + "-" + architecture,
			"COMPATIBILITY_FILE=generated.compatibility.json",
			"ACTIONS_RUNNER_SHA256=sha256:" + strings.TrimPrefix(platformLock.ActionsRunner.SHA256, "sha256:"),
			"TINI_SHA256=sha256:" + strings.TrimPrefix(platformLock.Tini.SHA256, "sha256:"),
		} {
			args = append(args, "--build-arg", buildArg)
		}
		args = append(args, "--file", filepath.Join(contextRoot, "Dockerfile"), contextRoot)
		buildLogPath := m.buildLogPath("docker-sandboxes-" + manifestHash[:16] + ".docker-build.log")
		defer m.releaseTranscript(buildLogPath)
		if err := resetLogs(buildLogPath); err != nil {
			return err
		}
		m.infof("building Docker Sandboxes runner template from %s for %s\n", source.Reference, source.Platform)
		m.infof("full Docker Sandboxes Buildx progress: %s\n", buildLogPath)
		if err := m.runHostLogged(ctx, buildLogPath, "docker", args...); err != nil {
			return fmt.Errorf("build Docker Sandboxes runner template: %w%s", err, boundedRedactedLogTail(buildLogPath, 32*1024))
		}
		if err := readJSONFile(buildMetadataPath, &buildMetadata); err != nil {
			return fmt.Errorf("read Docker Sandboxes Buildx metadata: %w", err)
		}
		localDigest, err = m.runHostOutput(ctx, "docker", "image", "inspect", "--format", "{{.Id}}", templateTag)
		if err != nil {
			return err
		}
		localDigest = strings.TrimSpace(localDigest)
		if !validSHA256(localDigest) || buildMetadata.ImageDigest != localDigest {
			return fmt.Errorf("Docker Sandboxes template Buildx digest does not match the full local Docker image identity")
		}
		if len(buildMetadata.Provenance) == 0 || string(buildMetadata.Provenance) == "null" {
			return fmt.Errorf("Docker Sandboxes template Buildx metadata omitted max-mode provenance")
		}
		if err := writeAtomicFile(provenancePath, append(buildMetadata.Provenance, '\n'), 0o644); err != nil {
			return err
		}
	}
	sbomSourceDigest, err := m.persistBuildxSBOM(ctx, builder, buildMetadata, sbomPath)
	if err != nil {
		return fmt.Errorf("persist Docker Sandboxes template SBOM attestation: %w", err)
	}
	inventory, err := m.runHostOutput(ctx, "docker", "run", "--rm", "--pull", "never", "--platform", source.Platform, "--entrypoint", "/opt/epar/collect-software-inventory.sh", templateTag)
	if err != nil {
		return fmt.Errorf("collect Docker Sandboxes template software inventory: %w", err)
	}
	if err := writeAtomicFile(inventoryPath, []byte(strings.TrimSpace(inventory)+"\n"), 0o644); err != nil {
		return err
	}
	if err := m.runHost(ctx, "docker", "image", "save", "--output", archivePath, templateTag); err != nil {
		return fmt.Errorf("save Docker Sandboxes template archive: %w", err)
	}
	archiveSHA, archiveBytes, err := hashFile(archivePath)
	if err != nil {
		return err
	}
	artifact := provider.TemplateArtifact{
		Reference: "docker.io/library/" + templateTag,
		Digest:    localDigest,
		CacheID:   strings.TrimPrefix(localDigest, "sha256:")[:12],
		Platform:  source.Platform,
		RootDisk:  rootDisk,
	}
	metadata := dockerSandboxesTemplateMetadata{
		SchemaVersion: dockerSandboxesMetadataSchema,
		Profile:       profile,
		Platform:      source.Platform,
		ManifestHash:  manifestHash,
		Source:        source,
		Artifacts:     make(map[string]artifactEvidence),
	}
	metadata.Template.Tag = artifact.Reference
	metadata.Template.Digest = localDigest
	metadata.Template.CacheID = artifact.CacheID
	metadata.Template.RootDisk = artifact.RootDisk
	metadata.Template.Archive = filepath.Base(archivePath)
	metadata.Template.ArchiveSHA256 = archiveSHA
	metadata.Template.ArchiveBytes = archiveBytes
	metadata.Compatibility.TemplateSchemaVersion = 1
	metadata.Compatibility.RunnerExecution = "direct-actions-listener"
	metadata.Compatibility.DockerDaemonOwner = "docker-sandboxes-runtime"
	metadata.Compatibility.ExpectedDockerDaemonCount = 1
	for name, path := range map[string]string{
		"buildMetadata":     buildMetadataPath,
		"provenance":        provenancePath,
		"sbom":              sbomPath,
		"softwareInventory": inventoryPath,
		"compatibility":     compatibilityEvidencePath,
	} {
		digest, _, err := hashFile(path)
		if err != nil {
			return err
		}
		evidence := artifactEvidence{Path: filepath.Base(path), SHA256: digest}
		if name == "sbom" {
			evidence.SourceDigest = sbomSourceDigest
		}
		metadata.Artifacts[name] = evidence
	}
	if err := writeJSONFile(metadataPath, metadata); err != nil {
		return err
	}
	metadataSHA, _, err := hashFile(metadataPath)
	if err != nil {
		return err
	}
	if err := runtime.VerifyTemplate(ctx, artifact); err != nil {
		m.infof("importing verified Docker Sandboxes runner template %s\n", artifact.Reference)
		if err := runtime.ImportTemplate(ctx, archivePath); err != nil {
			return err
		}
	}
	if err := runtime.VerifyTemplate(ctx, artifact); err != nil {
		return fmt.Errorf("verify imported Docker Sandboxes runner template: %w", err)
	}
	return m.activateDockerSandboxesTemplate(manifest, source, manifestHash, artifact, metadataPath, metadataSHA, archivePath, archiveSHA, runtime)
}

func (m *Coordinator) recoverDockerSandboxesBuild(ctx context.Context, builder, templateTag string, source ResolvedDockerSource, manifestHash, architecture string, lock dockerSandboxesSourceLock, compatibilityPath string) (dockerSandboxesBuildMetadata, string, bool, error) {
	compatibilityInfo, err := os.Lstat(compatibilityPath)
	if err != nil {
		if os.IsNotExist(err) {
			return dockerSandboxesBuildMetadata{}, "", false, nil
		}
		return dockerSandboxesBuildMetadata{}, "", false, err
	}
	if !compatibilityInfo.Mode().IsRegular() {
		return dockerSandboxesBuildMetadata{}, "", false, fmt.Errorf("Docker Sandboxes compatibility evidence is not a regular file: %s", compatibilityPath)
	}
	localDigest, err := m.runHostOutput(ctx, "docker", "image", "inspect", "--format", "{{.Id}}", templateTag)
	if err != nil {
		return dockerSandboxesBuildMetadata{}, "", false, nil
	}
	localDigest = strings.TrimSpace(localDigest)
	if !validSHA256(localDigest) {
		return dockerSandboxesBuildMetadata{}, "", false, nil
	}
	history, err := m.runHostOutput(ctx, "docker", "buildx", "history", "ls", "--builder", builder, "--no-trunc", "--format", "json")
	if err != nil {
		m.warnf("could not inspect owned Buildx history for interrupted-build recovery; rebuilding: %v\n", err)
		return dockerSandboxesBuildMetadata{}, "", false, nil
	}
	platformLock := lock.Platforms[source.Platform]
	expectedArgs := map[string]string{
		"TEMPLATE_PLATFORM":      source.Platform,
		"SOURCE_IMAGE":           source.ImmutableReference,
		"GO_BUILDER_IMAGE":       platformLock.GoBuilderReference,
		"HOOK_LAUNCHER_SHA256":   lock.HookLauncher.SHA256,
		"SOURCE_PROFILE":         sourceProfile(source.Reference),
		"SOURCE_INDEX_DIGEST":    source.IndexDigest,
		"SOURCE_MANIFEST_DIGEST": source.PlatformDigest,
		"SOURCE_REVISION":        source.IndexDigest,
		"TEMPLATE_VERSION":       manifestHash[:16] + "-" + architecture,
		"COMPATIBILITY_FILE":     "generated.compatibility.json",
		"ACTIONS_RUNNER_SHA256":  "sha256:" + strings.TrimPrefix(platformLock.ActionsRunner.SHA256, "sha256:"),
		"TINI_SHA256":            "sha256:" + strings.TrimPrefix(platformLock.Tini.SHA256, "sha256:"),
	}
	for _, line := range strings.Split(strings.TrimSpace(history), "\n") {
		var entry buildxHistoryEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil || entry.Status != "Completed" {
			continue
		}
		buildRef := entry.Ref
		if separator := strings.LastIndex(buildRef, "/"); separator >= 0 {
			buildRef = buildRef[separator+1:]
		}
		if matched, _ := regexp.MatchString(`^[a-z0-9]{12,128}$`, buildRef); !matched {
			continue
		}
		inspectionJSON, err := m.runHostOutput(ctx, "docker", "buildx", "history", "inspect", "--builder", builder, buildRef, "--format", "json")
		if err != nil {
			continue
		}
		var inspection buildxHistoryInspection
		if err := json.Unmarshal([]byte(inspectionJSON), &inspection); err != nil || inspection.Status != "completed" {
			continue
		}
		actualArgs := make(map[string]string, len(inspection.BuildArgs))
		for _, argument := range inspection.BuildArgs {
			actualArgs[argument.Name] = argument.Value
		}
		matches := true
		for name, expected := range expectedArgs {
			if actualArgs[name] != expected {
				matches = false
				break
			}
		}
		if !matches {
			continue
		}
		exactImage := false
		for _, attachment := range inspection.Attachments {
			if attachment.Type == "application/vnd.oci.image.index.v1+json" && attachment.Digest == localDigest {
				exactImage = true
				break
			}
		}
		if !exactImage {
			continue
		}
		provenance, err := m.runHostOutput(ctx, "docker", "buildx", "history", "inspect", "attachment", "--builder", builder, "--type", "provenance", buildRef)
		if err != nil || !json.Valid([]byte(provenance)) {
			continue
		}
		return dockerSandboxesBuildMetadata{
			ImageDigest: localDigest,
			Provenance:  json.RawMessage(provenance),
			BuildRef:    entry.Ref,
		}, localDigest, true, nil
	}
	return dockerSandboxesBuildMetadata{}, "", false, nil
}

func (m *Coordinator) persistBuildxSBOM(ctx context.Context, builder string, metadata dockerSandboxesBuildMetadata, destination string) (string, error) {
	buildRef := metadata.BuildRef
	if separator := strings.LastIndex(buildRef, "/"); separator >= 0 {
		buildRef = buildRef[separator+1:]
	}
	if matched, _ := regexp.MatchString(`^[a-z0-9]{12,128}$`, buildRef); !matched {
		return "", fmt.Errorf("Buildx metadata contains invalid build reference %q", metadata.BuildRef)
	}
	if !validSHA256(metadata.ImageDigest) {
		return "", fmt.Errorf("Buildx metadata contains invalid image digest %q", metadata.ImageDigest)
	}
	indexJSON, err := m.runHostOutput(ctx, "docker", "buildx", "history", "inspect", "attachment", "--builder", builder, buildRef, metadata.ImageDigest)
	if err != nil {
		return "", fmt.Errorf("inspect Buildx image-index attachment: %w", err)
	}
	attestationDigest, err := selectBuildxAttestationDigest([]byte(indexJSON))
	if err != nil {
		return "", err
	}
	attestationJSON, err := m.runHostOutput(ctx, "docker", "buildx", "history", "inspect", "attachment", "--builder", builder, buildRef, attestationDigest)
	if err != nil {
		return "", fmt.Errorf("inspect Buildx attestation manifest: %w", err)
	}
	sbomDigest, sbomSize, err := selectBuildxSBOMDigest([]byte(attestationJSON))
	if err != nil {
		return "", err
	}
	if sbomSize == 0 || sbomSize > storage.GiB {
		return "", fmt.Errorf("Buildx SBOM attachment has invalid size %d bytes", sbomSize)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return "", err
	}
	temporary, err := os.CreateTemp(filepath.Dir(destination), "."+filepath.Base(destination)+".partial-")
	if err != nil {
		return "", err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return "", err
	}
	if err := m.streamLocalDockerContentBlob(ctx, builder, sbomDigest, temporary); err != nil {
		_ = temporary.Close()
		return "", fmt.Errorf("extract Buildx SBOM attachment: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return "", err
	}
	if err := temporary.Close(); err != nil {
		return "", err
	}
	actualDigest, actualSize, err := hashFile(temporaryPath)
	if err != nil {
		return "", err
	}
	if actualDigest != sbomDigest || actualSize != sbomSize {
		return "", fmt.Errorf("Buildx SBOM attachment readback is %s/%d bytes, expected %s/%d bytes", actualDigest, actualSize, sbomDigest, sbomSize)
	}
	if err := validateInTotoSPDX(temporaryPath); err != nil {
		return "", fmt.Errorf("validate Buildx SBOM attachment: %w", err)
	}
	if err := os.Rename(temporaryPath, destination); err != nil {
		return "", err
	}
	return sbomDigest, nil
}

func (m *Coordinator) streamLocalDockerContentBlob(ctx context.Context, builder, digest string, destination *os.File) error {
	if !validSHA256(digest) {
		return fmt.Errorf("invalid Docker content digest %q", digest)
	}
	helperImage, err := m.runHostOutput(ctx, "docker", "inspect", "--format", "{{.Image}}", buildxControlContainer(builder))
	if err != nil {
		return fmt.Errorf("inspect owned BuildKit control container: %w", err)
	}
	helperImage = strings.TrimSpace(helperImage)
	if !validSHA256(helperImage) {
		return fmt.Errorf("owned BuildKit control container reported invalid image identity %q", helperImage)
	}
	var failures []string
	for _, source := range dockerContentBlobCandidates(digest) {
		if _, err := destination.Seek(0, io.SeekStart); err != nil {
			return err
		}
		if err := destination.Truncate(0); err != nil {
			return err
		}
		mount := "type=bind,src=" + source + ",dst=/epar-build-attestation,readonly"
		err := m.runHostOutputTo(
			ctx,
			destination,
			"docker",
			"run",
			"--rm",
			"--pull=never",
			"--network=none",
			"--entrypoint",
			"cat",
			"--mount",
			mount,
			helperImage,
			"/epar-build-attestation",
		)
		if err == nil {
			return nil
		}
		failures = append(failures, err.Error())
	}
	return fmt.Errorf("exact content blob %s was not readable from Docker's local containerd image store: %s", digest, strings.Join(failures, "; "))
}

func dockerContentBlobCandidates(digest string) []string {
	hexDigest := strings.TrimPrefix(digest, "sha256:")
	return []string{
		"/var/lib/desktop-containerd/daemon/io.containerd.content.v1.content/blobs/sha256/" + hexDigest,
		"/var/lib/docker/containerd/daemon/io.containerd.content.v1.content/blobs/sha256/" + hexDigest,
	}
}

func selectBuildxAttestationDigest(content []byte) (string, error) {
	var document buildxAttachmentDocument
	if err := json.Unmarshal(content, &document); err != nil {
		return "", fmt.Errorf("parse Buildx image-index attachment: %w", err)
	}
	var selected string
	for _, manifest := range document.Manifests {
		if manifest.Annotations["vnd.docker.reference.type"] != "attestation-manifest" {
			continue
		}
		if !validSHA256(manifest.Digest) || selected != "" {
			return "", fmt.Errorf("Buildx image index does not contain exactly one valid attestation manifest")
		}
		selected = manifest.Digest
	}
	if selected == "" {
		return "", fmt.Errorf("Buildx image index omitted its attestation manifest")
	}
	return selected, nil
}

func selectBuildxSBOMDigest(content []byte) (string, uint64, error) {
	var document buildxAttachmentDocument
	if err := json.Unmarshal(content, &document); err != nil {
		return "", 0, fmt.Errorf("parse Buildx attestation manifest: %w", err)
	}
	var selected string
	var size uint64
	for _, layer := range document.Layers {
		if layer.MediaType != "application/vnd.in-toto+json" || layer.Annotations["in-toto.io/predicate-type"] != "https://spdx.dev/Document" {
			continue
		}
		if !validSHA256(layer.Digest) || selected != "" {
			return "", 0, fmt.Errorf("Buildx attestation manifest does not contain exactly one valid SPDX attachment")
		}
		selected = layer.Digest
		size = layer.Size
	}
	if selected == "" {
		return "", 0, fmt.Errorf("Buildx attestation manifest omitted its SPDX attachment")
	}
	return selected, size, nil
}

func validateInTotoSPDX(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	open, err := decoder.Token()
	if err != nil {
		return err
	}
	if open != json.Delim('{') {
		return fmt.Errorf("SBOM attachment is not an in-toto JSON object")
	}
	var statementType string
	var predicateType string
	var spdxID string
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return err
		}
		switch key {
		case "_type":
			if err := decoder.Decode(&statementType); err != nil {
				return err
			}
		case "predicateType":
			if err := decoder.Decode(&predicateType); err != nil {
				return err
			}
		case "predicate":
			spdxID, err = scanSPDXPredicate(decoder)
			if err != nil {
				return err
			}
		default:
			if err := skipJSONValue(decoder); err != nil {
				return err
			}
		}
	}
	if _, err := decoder.Token(); err != nil {
		return err
	}
	if statementType != "https://in-toto.io/Statement/v0.1" && statementType != "https://in-toto.io/Statement/v1" {
		return fmt.Errorf("unsupported in-toto statement type %q", statementType)
	}
	if predicateType != "https://spdx.dev/Document" {
		return fmt.Errorf("unexpected SBOM predicate type %q", predicateType)
	}
	if spdxID != "SPDXRef-DOCUMENT" {
		return fmt.Errorf("unexpected SPDX document identity %q", spdxID)
	}
	if token, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return fmt.Errorf("SBOM attachment contains trailing JSON token %v", token)
		}
		return err
	}
	return nil
}

func scanSPDXPredicate(decoder *json.Decoder) (string, error) {
	open, err := decoder.Token()
	if err != nil {
		return "", err
	}
	if open != json.Delim('{') {
		return "", fmt.Errorf("SPDX predicate is not a JSON object")
	}
	var spdxID string
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return "", err
		}
		if key == "SPDXID" {
			if err := decoder.Decode(&spdxID); err != nil {
				return "", err
			}
			continue
		}
		if err := skipJSONValue(decoder); err != nil {
			return "", err
		}
	}
	_, err = decoder.Token()
	return spdxID, err
}

func skipJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, compound := token.(json.Delim)
	if !compound || (delim != '{' && delim != '[') {
		return nil
	}
	for decoder.More() {
		if delim == '{' {
			if _, err := decoder.Token(); err != nil {
				return err
			}
		}
		if err := skipJSONValue(decoder); err != nil {
			return err
		}
	}
	_, err = decoder.Token()
	return err
}

func (m *Coordinator) verifyDockerSandboxesNativeBuilder(ctx context.Context, platform string) error {
	reported, err := m.runHostOutput(ctx, "docker", "info", "--format", "{{.OSType}}/{{.Architecture}}")
	if err != nil {
		return fmt.Errorf("determine Docker server platform for Docker Sandboxes template build: %w", err)
	}
	native := strings.ToLower(strings.TrimSpace(reported))
	switch native {
	case "linux/x86_64":
		native = "linux/amd64"
	case "linux/aarch64":
		native = "linux/arm64"
	}
	if native != platform {
		return fmt.Errorf("Docker Sandboxes template builds require a native %s Docker server; Docker reports %s and EPAR does not use emulation for this artifact", platform, native)
	}
	return nil
}

func (m *Coordinator) resumeDockerSandboxesTemplate(ctx context.Context, manifest Manifest, source ResolvedDockerSource, manifestHash, rootDisk, artifactRoot, metadataPath, archivePath string, runtime provider.TemplateArtifactRuntime) (bool, error) {
	metadata, artifact, metadataSHA, archiveSHA, valid, err := verifiedDockerSandboxesBuildArtifact(artifactRoot, metadataPath, archivePath, manifestHash, source)
	if err != nil {
		return false, err
	}
	if !valid {
		return false, nil
	}
	if artifact.RootDisk != rootDisk {
		return false, nil
	}
	localDigest, inspectErr := m.runHostOutput(ctx, "docker", "image", "inspect", "--format", "{{.Id}}", artifact.Reference)
	if inspectErr != nil || strings.TrimSpace(localDigest) != artifact.Digest {
		m.infof("restoring verified Docker Sandboxes runner template image from interrupted build evidence\n")
		if err := m.runHost(ctx, "docker", "image", "load", "--input", archivePath); err != nil {
			return false, fmt.Errorf("restore verified Docker Sandboxes template image: %w", err)
		}
		localDigest, err = m.runHostOutput(ctx, "docker", "image", "inspect", "--format", "{{.Id}}", artifact.Reference)
		if err != nil || strings.TrimSpace(localDigest) != artifact.Digest {
			return false, fmt.Errorf("restored Docker Sandboxes template image does not match verified build evidence")
		}
	}
	if err := runtime.VerifyTemplate(ctx, artifact); err != nil {
		m.infof("resuming Docker Sandboxes template import from verified build evidence\n")
		if err := runtime.ImportTemplate(ctx, archivePath); err != nil {
			return false, err
		}
	}
	if err := runtime.VerifyTemplate(ctx, artifact); err != nil {
		return false, fmt.Errorf("verify resumed Docker Sandboxes runner template: %w", err)
	}
	if metadata.ManifestHash != manifestHash {
		return false, fmt.Errorf("verified Docker Sandboxes build evidence changed during resume")
	}
	if err := m.activateDockerSandboxesTemplate(manifest, source, manifestHash, artifact, metadataPath, metadataSHA, archivePath, archiveSHA, runtime); err != nil {
		return false, err
	}
	m.infof("resumed Docker Sandboxes runner template from verified interrupted build evidence\n")
	return true, nil
}

func verifiedDockerSandboxesBuildArtifact(artifactRoot, metadataPath, archivePath, manifestHash string, source ResolvedDockerSource) (dockerSandboxesTemplateMetadata, provider.TemplateArtifact, string, string, bool, error) {
	var metadata dockerSandboxesTemplateMetadata
	info, err := os.Lstat(metadataPath)
	if errors.Is(err, os.ErrNotExist) {
		return metadata, provider.TemplateArtifact{}, "", "", false, nil
	}
	if err != nil {
		return metadata, provider.TemplateArtifact{}, "", "", false, err
	}
	if !info.Mode().IsRegular() {
		return metadata, provider.TemplateArtifact{}, "", "", false, nil
	}
	if err := readJSONFile(metadataPath, &metadata); err != nil {
		return metadata, provider.TemplateArtifact{}, "", "", false, nil
	}
	if metadata.SchemaVersion != dockerSandboxesMetadataSchema || metadata.ManifestHash != manifestHash || metadata.Source != source || metadata.Platform != source.Platform || metadata.Profile != sourceProfile(source.Reference) {
		return metadata, provider.TemplateArtifact{}, "", "", false, nil
	}
	if !validSHA256(metadata.Template.Digest) || metadata.Template.CacheID != strings.TrimPrefix(metadata.Template.Digest, "sha256:")[:12] || metadata.Template.Tag == "" || metadata.Template.RootDisk == "" || metadata.Template.Archive != filepath.Base(archivePath) {
		return metadata, provider.TemplateArtifact{}, "", "", false, nil
	}
	if metadata.Compatibility.TemplateSchemaVersion != 1 || metadata.Compatibility.RunnerExecution != "direct-actions-listener" || metadata.Compatibility.DockerDaemonOwner != "docker-sandboxes-runtime" || metadata.Compatibility.ExpectedDockerDaemonCount != 1 {
		return metadata, provider.TemplateArtifact{}, "", "", false, nil
	}
	archiveInfo, err := os.Lstat(archivePath)
	if err != nil || !archiveInfo.Mode().IsRegular() {
		return metadata, provider.TemplateArtifact{}, "", "", false, nil
	}
	archiveSHA, archiveBytes, err := hashFile(archivePath)
	if err != nil {
		return metadata, provider.TemplateArtifact{}, "", "", false, err
	}
	if archiveSHA != metadata.Template.ArchiveSHA256 || archiveBytes != metadata.Template.ArchiveBytes {
		return metadata, provider.TemplateArtifact{}, "", "", false, nil
	}
	requiredEvidence := []string{"buildMetadata", "provenance", "sbom", "softwareInventory", "compatibility"}
	for _, name := range requiredEvidence {
		evidence, found := metadata.Artifacts[name]
		if !found || filepath.Base(evidence.Path) != evidence.Path || !validSHA256(evidence.SHA256) {
			return metadata, provider.TemplateArtifact{}, "", "", false, nil
		}
		evidencePath := filepath.Join(artifactRoot, evidence.Path)
		evidenceInfo, err := os.Lstat(evidencePath)
		if err != nil || !evidenceInfo.Mode().IsRegular() {
			return metadata, provider.TemplateArtifact{}, "", "", false, nil
		}
		digest, _, err := hashFile(evidencePath)
		if err != nil {
			return metadata, provider.TemplateArtifact{}, "", "", false, err
		}
		if digest != evidence.SHA256 {
			return metadata, provider.TemplateArtifact{}, "", "", false, nil
		}
	}
	metadataSHA, _, err := hashFile(metadataPath)
	if err != nil {
		return metadata, provider.TemplateArtifact{}, "", "", false, err
	}
	return metadata, provider.TemplateArtifact{
		Reference: metadata.Template.Tag,
		Digest:    metadata.Template.Digest,
		CacheID:   metadata.Template.CacheID,
		Platform:  metadata.Platform,
		RootDisk:  metadata.Template.RootDisk,
	}, metadataSHA, archiveSHA, true, nil
}

func (m *Coordinator) activateDockerSandboxesTemplate(manifest Manifest, source ResolvedDockerSource, manifestHash string, artifact provider.TemplateArtifact, metadataPath, metadataSHA, archivePath, archiveSHA string, runtime provider.TemplateArtifactRuntime) error {
	if err := runtime.ActivateTemplate(artifact); err != nil {
		return err
	}
	receipt := dockerSandboxesReceipt{
		SchemaVersion:  dockerSandboxesReceiptSchema,
		ManifestHash:   manifestHash,
		Manifest:       manifest,
		Source:         source,
		Artifact:       artifact,
		MetadataPath:   metadataPath,
		MetadataSHA256: metadataSHA,
		ArchivePath:    archivePath,
		ArchiveSHA256:  archiveSHA,
		ActivatedAt:    time.Now().UTC(),
	}
	if err := writeJSONFile(DockerSandboxesReceiptPath(m.ProjectRoot), receipt); err != nil {
		return err
	}
	m.infof("activated Docker Sandboxes runner template %s@%s\n", artifact.Reference, artifact.Digest)
	return nil
}

func loadDockerSandboxesSourceLock(projectRoot, platform string) (dockerSandboxesSourceLock, error) {
	var lock dockerSandboxesSourceLock
	path := filepath.Join(projectRoot, "templates", "docker-sandboxes", "sources.lock.json")
	if err := readJSONFile(path, &lock); err != nil {
		return lock, fmt.Errorf("read Docker Sandboxes source lock: %w", err)
	}
	if lock.SchemaVersion != 2 {
		return lock, fmt.Errorf("unsupported Docker Sandboxes source lock schema %d", lock.SchemaVersion)
	}
	if lock.DockerfileFrontend.Reference == "" || lock.SBOMGenerator.InspectionReference == "" || lock.GoBuilder.Version == "" || lock.GoBuilder.IndexDigest == "" || lock.HookLauncher.SHA256 == "" || lock.ActionsRunner.Version == "" || lock.Tini.Version == "" {
		return lock, errors.New("Docker Sandboxes source lock has incomplete shared build inputs")
	}
	platformLock, ok := lock.Platforms[platform]
	if !ok || platformLock.GoBuilderReference == "" || platformLock.GoBuilderManifestDigest == "" || platformLock.SBOMGeneratorReference == "" || platformLock.SBOMGeneratorManifestDigest == "" || platformLock.DockerfileFrontendManifestDigest == "" || platformLock.ActionsRunner.URL == "" || platformLock.ActionsRunner.SHA256 == "" || platformLock.Tini.URL == "" || platformLock.Tini.SHA256 == "" {
		return lock, fmt.Errorf("Docker Sandboxes source lock has incomplete build inputs for %s", platform)
	}
	return lock, nil
}

func sanitizeTemplateTag(value string) string {
	value = strings.ToLower(value)
	var builder strings.Builder
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') || character == '.' || character == '_' || character == '-' {
			builder.WriteRune(character)
		} else {
			builder.WriteByte('-')
		}
	}
	result := strings.Trim(builder.String(), "-.")
	if result == "" {
		return "custom"
	}
	return result
}

func (m *Coordinator) prepareDockerSandboxesCustomScripts(contextRoot string) error {
	directory := filepath.Join(contextRoot, "custom-install")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return err
	}
	var runner strings.Builder
	runner.WriteString("#!/usr/bin/env bash\nset -euo pipefail\n")
	for index, configured := range m.Config.Image.CustomInstallScripts {
		source, err := m.customInstallScriptHostPath(configured)
		if err != nil {
			return err
		}
		name := fmt.Sprintf("%03d-%s", index+1, guestScriptName(filepath.Base(source)))
		if err := copyFile(source, filepath.Join(directory, name), 0o755); err != nil {
			return err
		}
		fmt.Fprintf(&runner, "EPAR_CONTAINER_IMAGE_BUILD=true bash /opt/epar/custom-install/%s\n", name)
	}
	return writeAtomicFile(filepath.Join(directory, "run.sh"), []byte(runner.String()), 0o755)
}

func (m *Coordinator) prepareDockerSandboxesTrustPolicy(contextRoot string, snapshot hosttrust.Snapshot) error {
	hostDirectory := filepath.Join(contextRoot, "host-trust-certificates")
	if err := copyHostTrustCertificatesToDir(hostDirectory, snapshot); err != nil {
		return err
	}
	explicitDirectory := filepath.Join(contextRoot, "trusted-ca-certificates")
	if err := m.copyTrustedCACertificatesToDir(explicitDirectory); err != nil {
		return err
	}
	metadataDirectory := filepath.Join(contextRoot, "host-trust-metadata")
	if err := os.MkdirAll(metadataDirectory, 0o755); err != nil {
		return err
	}
	marker := struct {
		SchemaVersion    int      `json:"schemaVersion"`
		Generation       string   `json:"generation"`
		HostOS           string   `json:"hostOS"`
		Mode             string   `json:"mode"`
		Scopes           []string `json:"scopes"`
		CertificateCount int      `json:"certificateCount"`
	}{
		SchemaVersion: 1,
		Generation:    "disabled",
		Mode:          hosttrust.ModeDisabled,
		Scopes:        []string{},
	}
	if snapshot.Generation != "" {
		marker.Generation = snapshot.Generation
		marker.HostOS = snapshot.HostOS
		marker.Mode = hosttrust.ModeOverlay
		marker.Scopes = append([]string(nil), snapshot.Scopes...)
		marker.CertificateCount = len(snapshot.Certificates)
	}
	content, err := json.MarshalIndent(marker, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomicFile(filepath.Join(metadataDirectory, filepath.Base(hostTrustMarkerGuest)), append(content, '\n'), 0o644)
}

func copyHostTrustCertificatesToDir(destination string, snapshot hosttrust.Snapshot) error {
	if err := os.MkdirAll(destination, 0o755); err != nil {
		return err
	}
	for _, certificate := range snapshot.Certificates {
		if err := writeAtomicFile(filepath.Join(destination, certificate.Name), certificate.PEM, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func boundedRedactedLogTail(path string, maximumBytes int64) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return ""
	}
	start := info.Size() - maximumBytes
	if start < 0 {
		start = 0
	}
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return ""
	}
	content, err := io.ReadAll(io.LimitReader(file, maximumBytes))
	if err != nil {
		return ""
	}
	text := strings.TrimSpace(provider.RedactText(string(content)))
	if text == "" {
		return ""
	}
	if start > 0 {
		text = "[earlier Buildx output omitted]\n" + text
	}
	return "\nBuildx error tail:\n" + text
}

func copyDirectory(source, destination string) error {
	return filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("refusing non-regular Docker Sandboxes template input %s", path)
		}
		return copyFile(path, target, info.Mode().Perm())
	})
}

func writeJSONFile(path string, value any) error {
	content, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomicFile(path, append(content, '\n'), 0o600)
}

func readJSONFile(path string, value any) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 16<<20))
	if err := decoder.Decode(value); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("unexpected trailing JSON value")
		}
		return err
	}
	return nil
}

func hashFile(path string) (string, uint64, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil {
		return "", 0, err
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), uint64(size), nil
}

func validSHA256(value string) bool {
	if len(value) != len("sha256:")+64 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil && value == strings.ToLower(value)
}
