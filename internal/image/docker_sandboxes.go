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
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
	"github.com/solutionforest/ephemeral-action-runner/internal/hosttrust"
	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
	"github.com/solutionforest/ephemeral-action-runner/internal/storage"
	storagecatalog "github.com/solutionforest/ephemeral-action-runner/internal/storage/catalog"
)

const (
	dockerSandboxesReceiptSchema  = 3
	dockerSandboxesMetadataSchema = 6
)

var dockerTagPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)

var dockerSandboxesCompactEvidenceFiles = map[string]string{
	"attestationMetadata": "attestation-metadata.json",
	"buildMetadata":       "build-metadata.json",
	"compatibility":       "compatibility.json",
	"provenance":          "provenance.json",
	"sbomDescriptor":      "sbom-descriptor.json",
	"softwareInventory":   "software-inventory.txt",
	"templateMetadata":    "template-metadata.json",
}

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
	SchemaVersion  int                         `json:"schemaVersion"`
	ManifestHash   string                      `json:"manifestHash"`
	Manifest       Manifest                    `json:"manifest"`
	Source         ResolvedDockerSource        `json:"source"`
	Artifact       provider.TemplateArtifact   `json:"artifact"`
	MetadataSHA256 string                      `json:"metadataSha256"`
	ArchiveSHA256  string                      `json:"archiveSha256"`
	ArchiveBytes   uint64                      `json:"archiveBytes"`
	Evidence       map[string]artifactEvidence `json:"evidence"`
	ActivatedAt    time.Time                   `json:"activatedAt"`
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
	Tini struct {
		Version string `json:"version"`
	} `json:"tini"`
	Emulation dockerSandboxesEmulationLock `json:"emulation"`
	Platforms map[string]struct {
		GoBuilderReference               string `json:"goBuilderReference"`
		GoBuilderManifestDigest          string `json:"goBuilderManifestDigest"`
		SBOMGeneratorReference           string `json:"sbomGeneratorReference"`
		SBOMGeneratorManifestDigest      string `json:"sbomGeneratorManifestDigest"`
		DockerfileFrontendManifestDigest string `json:"dockerfileFrontendManifestDigest"`
		Tini                             struct {
			URL    string `json:"url"`
			SHA256 string `json:"sha256"`
		} `json:"tini"`
	} `json:"platforms"`
}

type dockerSandboxesEmulationLock struct {
	SchemaVersion int    `json:"schemaVersion"`
	Backend       string `json:"backend"`
	Source        struct {
		Repository     string `json:"repository"`
		Release        string `json:"release"`
		Revision       string `json:"revision"`
		QEMUVersion    string `json:"qemuVersion"`
		IndexReference string `json:"indexReference"`
		IndexDigest    string `json:"indexDigest"`
		Licenses       []struct {
			Name   string `json:"name"`
			URL    string `json:"url"`
			SHA256 string `json:"sha256"`
		} `json:"licenses"`
	} `json:"source"`
	Platforms map[string]struct {
		ManifestDigest       string `json:"manifestDigest"`
		SourceReference      string `json:"sourceReference"`
		CompressedLayerBytes uint64 `json:"compressedLayerBytes"`
	} `json:"platforms"`
}

type dockerSandboxesBuildMetadata struct {
	ImageDigest string          `json:"containerimage.digest"`
	Provenance  json.RawMessage `json:"buildx.build.provenance"`
	BuildRef    string          `json:"buildx.build.ref"`
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
		EmulationBackend          string `json:"emulationBackend"`
		EmulationPolicy           string `json:"emulationPolicy"`
		EmulationRelease          string `json:"emulationRelease"`
		EmulationSourceDigest     string `json:"emulationSourceDigest"`
		EmulationManifestDigest   string `json:"emulationManifestDigest"`
		QEMUVersion               string `json:"qemuVersion"`
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
	const repository = catthehackerUbuntuRepository
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
	repository, err := dockerRepository(reference)
	if err != nil {
		return ResolvedDockerSource{}, err
	}
	return ResolvedDockerSource{
		Reference:            reference,
		ImmutableReference:   repository + "@" + indexDigest,
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
	if platform == "" {
		architecture := runtime.GOARCH
		if architecture == "386" {
			architecture = "amd64"
		}
		if architecture != "amd64" && architecture != "arm64" {
			return ResolvedDockerSource{}, fmt.Errorf("cannot infer a Linux source-image platform from host architecture %s; set image.sourcePlatform explicitly", runtime.GOARCH)
		}
		platform = "linux/" + architecture
	}
	indexRaw, err := m.runHostOutput(ctx, "docker", "buildx", "imagetools", "inspect", "--raw", reference)
	if err != nil {
		err = m.explainBuiltInCatthehackerAuthFailure(ctx, reference, err)
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
	repository, err := dockerRepository(reference)
	if err != nil {
		return ResolvedDockerSource{}, err
	}
	platformRaw, err := m.runHostOutput(ctx, "docker", "buildx", "imagetools", "inspect", "--raw", repository+"@"+platformDigest)
	if err != nil {
		platformReference := repository + "@" + platformDigest
		err = m.explainBuiltInCatthehackerAuthFailure(ctx, platformReference, err)
		return ResolvedDockerSource{}, fmt.Errorf("resolve source platform manifest %s: %w", platform, err)
	}
	return parseResolvedDockerSource(reference, platform, []byte(indexRaw), []byte(platformRaw))
}

func dockerRepository(reference string) (string, error) {
	if separator := strings.Index(reference, "@"); separator > 0 {
		return reference[:separator], nil
	}
	tagSeparator := strings.LastIndex(reference, ":")
	if tagSeparator <= strings.LastIndex(reference, "/") {
		return "", fmt.Errorf("source image %s must include a tag or digest", reference)
	}
	return reference[:tagSeparator], nil
}

func (m *Coordinator) dockerSandboxesDesiredManifest(ctx context.Context) (Manifest, ResolvedDockerSource, error) {
	manifest, err := m.dockerSandboxesLocalManifest(ctx)
	if err != nil {
		return Manifest{}, ResolvedDockerSource{}, err
	}
	source, err := m.resolveDockerSandboxesSource(ctx)
	if err != nil {
		return Manifest{}, ResolvedDockerSource{}, err
	}
	manifest.ProviderPlatform = source.Platform
	manifest.SourceImage = source.Reference
	manifest.SourcePlatform = source.Platform
	manifest.SourceDigest = source.IndexDigest
	manifest.SourcePlatformDigest = source.PlatformDigest
	manifest, err = m.resolveActionsRunner(ctx, manifest)
	if err != nil {
		return Manifest{}, ResolvedDockerSource{}, err
	}
	return manifest, source, nil
}

func (m *Coordinator) dockerSandboxesLocalManifest(ctx context.Context) (Manifest, error) {
	reference, err := NormalizeCatthehackerSource(strings.TrimSpace(m.Config.Image.SourceImage))
	if err != nil {
		return Manifest{}, err
	}
	platform := strings.TrimSpace(m.Config.Image.SourcePlatform)
	if platform == "" {
		platform = strings.TrimSpace(m.Config.Provider.Platform)
	}
	configuredRunnerVersion := normalizedRunnerSelector(m.Config.Image.RunnerVersion)
	snapshot, err := m.resolveHostTrust(ctx)
	if err != nil {
		return Manifest{}, err
	}
	customScripts, err := m.customInstallScriptDigests()
	if err != nil {
		return Manifest{}, err
	}
	trustedCertificates, err := m.trustedCACertificateDigests()
	if err != nil {
		return Manifest{}, err
	}
	templateInputs, err := fileDigestsRecursive(filepath.Join(m.ProjectRoot, "templates", "docker-sandboxes"))
	if err != nil {
		return Manifest{}, err
	}
	manifest := Manifest{
		SchemaVersion:         ManifestSchemaVersion,
		ProviderType:          "docker-sandboxes",
		ProviderPlatform:      platform,
		SourceType:            config.ImageSourceDockerImage,
		SourceImage:           reference,
		SourcePlatform:        platform,
		OutputImage:           "docker-sandboxes-template",
		RunnerSelector:        configuredRunnerVersion,
		TemplateInputs:        templateInputs,
		CustomInstallScripts:  customScripts,
		TrustedCACertificates: trustedCertificates,
		HostTrust:             hostTrustMetadata(snapshot),
	}
	return manifest, nil
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

func DockerSandboxesReceiptPathForConfig(projectRoot, configPath string) (string, error) {
	if strings.TrimSpace(configPath) == "" {
		configPath = filepath.Join(projectRoot, ".local", "config.yml")
	}
	configID, err := storagecatalog.ConfigID(projectRoot, configPath)
	if err != nil {
		return "", err
	}
	return filepath.Join(projectRoot, ".local", "state", "image", configID, "docker-sandboxes", "active.json"), nil
}

func (m *Coordinator) dockerSandboxesReceiptPath() (string, error) {
	return DockerSandboxesReceiptPathForConfig(m.ProjectRoot, m.ConfigPath)
}

func LoadDockerSandboxesReceipt(projectRoot string) (provider.TemplateArtifact, string, time.Time, error) {
	receipt, err := readDockerSandboxesReceiptPath(DockerSandboxesReceiptPath(projectRoot))
	if err != nil {
		return provider.TemplateArtifact{}, "", time.Time{}, err
	}
	return receipt.Artifact, receipt.MetadataSHA256, receipt.ActivatedAt, nil
}

func LoadDockerSandboxesReceiptForConfig(projectRoot, configPath string) (provider.TemplateArtifact, string, time.Time, error) {
	path, err := DockerSandboxesReceiptPathForConfig(projectRoot, configPath)
	if err != nil {
		return provider.TemplateArtifact{}, "", time.Time{}, err
	}
	receipt, err := readDockerSandboxesReceiptPath(path)
	if err != nil {
		return provider.TemplateArtifact{}, "", time.Time{}, err
	}
	return receipt.Artifact, receipt.MetadataSHA256, receipt.ActivatedAt, nil
}

func (m *Coordinator) readDockerSandboxesReceipt() (dockerSandboxesReceipt, error) {
	path, err := m.dockerSandboxesReceiptPath()
	if err != nil {
		return dockerSandboxesReceipt{}, err
	}
	return readDockerSandboxesReceiptPath(path)
}

func readDockerSandboxesReceiptPath(path string) (dockerSandboxesReceipt, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return dockerSandboxesReceipt{}, err
	}
	var receipt dockerSandboxesReceipt
	if err := json.Unmarshal(content, &receipt); err != nil {
		return dockerSandboxesReceipt{}, err
	}
	if receipt.SchemaVersion != dockerSandboxesReceiptSchema || receipt.ManifestHash == "" || receipt.Artifact.Reference == "" || receipt.Artifact.Digest == "" || receipt.Artifact.RootDisk == "" || receipt.MetadataSHA256 == "" || receipt.ArchiveSHA256 == "" || receipt.ArchiveBytes == 0 || len(receipt.Evidence) == 0 || receipt.ActivatedAt.IsZero() {
		return dockerSandboxesReceipt{}, fmt.Errorf("invalid Docker Sandboxes active artifact receipt")
	}
	return receipt, nil
}

func (m *Coordinator) ensureDockerSandboxesTemplate(ctx context.Context, force bool) error {
	manifest, source, err := m.dockerSandboxesDesiredManifest(ctx)
	if err != nil {
		return err
	}
	return m.ensureDockerSandboxesTemplateResolved(ctx, force, manifest, source)
}

func (m *Coordinator) ensureDockerSandboxesTemplateResolved(ctx context.Context, force bool, manifest Manifest, source ResolvedDockerSource) error {
	runtime, ok := m.Lifecycle.(provider.TemplateArtifactRuntime)
	if !ok {
		return fmt.Errorf("docker-sandboxes provider is missing required template artifact integration")
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
		receipt, receiptErr := m.readDockerSandboxesReceipt()
		if receiptErr == nil && receipt.ManifestHash == manifestHash && receipt.Artifact.RootDisk == rootDisk {
			if err := runtime.VerifyImportedTemplate(ctx, receipt.Artifact); err != nil {
				if !errors.Is(err, provider.ErrTemplateNotFound) {
					return fmt.Errorf("measure configured Docker Sandboxes artifact availability: %w", err)
				}
				m.warnf("recorded Docker Sandboxes template is absent from the authoritative Sandbox cache; rebuilding it\n")
			} else {
				if err := runtime.ActivateTemplate(receipt.Artifact); err != nil {
					return err
				}
				if err := m.recordCurrentSandboxArtifact(ctx, receipt.Artifact, manifestHash, receipt.ActivatedAt); err != nil {
					return fmt.Errorf("record current Docker Sandboxes template ownership: %w", err)
				}
				if err := m.cleanupSupersededCatalog(ctx); err != nil {
					return err
				}
				m.infof("Docker Sandboxes runner template is current: %s@%s\n", receipt.Artifact.Reference, receipt.Artifact.Digest)
				return nil
			}
		}
		if receiptErr != nil && !errors.Is(receiptErr, os.ErrNotExist) {
			m.warnf("ignoring stale unpublished Docker Sandboxes receipt and rebuilding: %v\n", receiptErr)
		}
		adopted := false
		if err := m.withSandboxBackendLock(ctx, func() error {
			var adoptErr error
			adopted, adoptErr = m.adoptReusableDockerSandboxesTemplateLocked(ctx, manifest, source, manifestHash, rootDisk, runtime)
			return adoptErr
		}); err != nil {
			return err
		}
		if adopted {
			m.infof("adopted shared verified Docker Sandboxes runner template for manifest %s\n", manifestHash)
			if err := m.cleanupSupersededCatalog(ctx); err != nil {
				return err
			}
			return nil
		}
	}
	if m.DryRun {
		m.infof("[dry-run] would build and import Docker Sandboxes runner template from %s for %s\n", source.Reference, source.Platform)
		return nil
	}
	return m.buildDockerSandboxesTemplate(ctx, manifest, source, manifestHash, rootDisk, !force, runtime)
}

func (m *Coordinator) ensureDockerSandboxesTemplateWithPolicy(ctx context.Context, forceRemote bool) error {
	localManifest, err := m.dockerSandboxesLocalManifest(ctx)
	if err != nil {
		return err
	}
	localHash, err := ManifestHash(localManifest)
	if err != nil {
		return err
	}
	if m.DryRun {
		manifest, source, err := m.dockerSandboxesDesiredManifest(ctx)
		if err != nil {
			return err
		}
		return m.ensureDockerSandboxesTemplateResolved(ctx, false, manifest, source)
	}
	now := m.now()
	state, err := m.readUpdatePolicyState()
	if err != nil {
		m.warnf("ignoring stale image update state and performing an immediate check: %v\n", err)
		state = UpdatePolicyState{SchemaVersion: updatePolicyStateSchemaVersion}
	}
	if receipt, receiptErr := m.readDockerSandboxesReceipt(); receiptErr == nil {
		runtime, ok := m.Lifecycle.(provider.TemplateArtifactRuntime)
		if !ok {
			return fmt.Errorf("docker-sandboxes provider is missing required template artifact integration")
		}
		if verifyErr := runtime.VerifyImportedTemplate(ctx, receipt.Artifact); verifyErr == nil {
			bootstrapped, bootstrapErr := bootstrapUpdatePolicyState(&state, m.Config.Image, localManifest, receipt.Manifest, &receipt.Source, receipt.ActivatedAt, now.Location())
			if bootstrapErr != nil {
				return bootstrapErr
			}
			if bootstrapped {
				if err := m.writeUpdatePolicyState(state); err != nil {
					return err
				}
				m.infof("initialized image update schedule from the verified active Docker Sandboxes template\n")
			}
		} else if !errors.Is(verifyErr, provider.ErrTemplateNotFound) {
			return fmt.Errorf("measure configured Docker Sandboxes artifact availability: %w", verifyErr)
		}
	}
	if recalculateScheduleForTimeZone(&state, m.Config.Image, now.Location()) {
		if err := m.writeUpdatePolicyState(state); err != nil {
			return err
		}
	}
	localChanged := state.LocalInputHash != "" && state.LocalInputHash != localHash
	currentVerified := false
	if state.LocalInputHash == localHash && state.LastResolvedManifest != nil {
		receipt, receiptErr := m.readDockerSandboxesReceipt()
		if receiptErr == nil {
			wantHash, hashErr := ManifestHash(*state.LastResolvedManifest)
			if hashErr != nil {
				return hashErr
			}
			runtime, ok := m.Lifecycle.(provider.TemplateArtifactRuntime)
			if !ok {
				return fmt.Errorf("docker-sandboxes provider is missing required template artifact integration")
			}
			if receipt.ManifestHash == wantHash {
				if verifyErr := runtime.VerifyImportedTemplate(ctx, receipt.Artifact); verifyErr == nil {
					currentVerified = true
				} else if !errors.Is(verifyErr, provider.ErrTemplateNotFound) {
					return fmt.Errorf("measure configured Docker Sandboxes artifact availability: %w", verifyErr)
				}
			}
		}
	}
	if state.LocalInputHash == localHash && pendingUpdateReady(state, now) {
		if err := m.ApplyPendingUpdate(ctx, now); err != nil {
			if !forceRemote && currentVerified {
				status, _ := m.UpdatePolicyStatus()
				m.warnf("pending scheduled Docker Sandboxes update failed; continuing with the previous verified template and retrying after %s: %v\n", formatUpdateTime(status.NextRetryAt), err)
				return m.ensureDockerSandboxesTemplateFromState(ctx, state)
			}
			return err
		}
		m.infof("pending Docker Sandboxes update activated\n")
		return nil
	}
	if !forceRemote && currentVerified && !updateCheckDue(state, m.Config.Image, now) {
		if err := m.ensureDockerSandboxesTemplateFromState(ctx, state); err != nil {
			return err
		}
		m.infof("Docker Sandboxes runner template is current; next remote check %s\n", formatUpdateTime(state.NextEligibleAt))
		return nil
	}

	state.LastAttemptAt = now.UTC()
	manifest, source, resolveErr := m.dockerSandboxesDesiredManifest(ctx)
	if resolveErr != nil {
		scheduleUpdateFailure(&state, now, resolveErr)
		_ = m.writeUpdatePolicyState(state)
		if !forceRemote && !localChanged && currentVerified {
			m.warnf("scheduled image update check failed; continuing with the last verified Docker Sandboxes template and retrying after %s: %v\n", formatUpdateTime(state.NextRetryAt), resolveErr)
			return m.ensureDockerSandboxesTemplateFromState(ctx, state)
		}
		return resolveErr
	}
	state.LocalInputHash = localHash
	state.PendingManifest = &manifest
	state.PendingSource = &source
	state.DeferredReason = "template build and activation pending"
	if err := m.writeUpdatePolicyState(state); err != nil {
		return err
	}
	if err := m.ensureDockerSandboxesTemplateResolved(ctx, false, manifest, source); err != nil {
		scheduleUpdateFailure(&state, now, err)
		_ = m.writeUpdatePolicyState(state)
		if !forceRemote && !localChanged && currentVerified {
			m.warnf("scheduled Docker Sandboxes update failed; restoring the last verified template and retrying after %s: %v\n", formatUpdateTime(state.NextRetryAt), err)
			return m.ensureDockerSandboxesTemplateFromState(ctx, state)
		}
		return err
	}
	state.LastResolvedManifest = &manifest
	state.LastResolvedSource = &source
	state.PendingManifest = nil
	state.PendingSource = nil
	if err := scheduleNextSuccess(&state, m.Config.Image, m.now()); err != nil {
		return err
	}
	return m.writeUpdatePolicyState(state)
}

func (m *Coordinator) ensureDockerSandboxesTemplateFromState(ctx context.Context, state UpdatePolicyState) error {
	if state.LastResolvedManifest == nil || state.LastResolvedSource == nil {
		return fmt.Errorf("Docker Sandboxes update state is missing its resolved artifact inputs")
	}
	return m.ensureDockerSandboxesTemplateResolved(ctx, false, *state.LastResolvedManifest, *state.LastResolvedSource)
}

const dockerSandboxesExpandedEmulationAllowanceBytes = 64 * storage.MiB

func (m *Coordinator) dockerSandboxesSourceEstimate(source ResolvedDockerSource) (SourceSizeEstimate, error) {
	estimate, err := EstimateSourceSize(source.CompressedLayerBytes, 0)
	if err != nil {
		return SourceSizeEstimate{}, err
	}
	lock, err := loadDockerSandboxesSourceLock(m.ProjectRoot, source.Platform)
	if err != nil {
		return SourceSizeEstimate{}, err
	}
	platform := lock.Emulation.Platforms[source.Platform]
	estimate.CompressedBytes, err = sumStorageBytes(estimate.CompressedBytes, platform.CompressedLayerBytes)
	if err != nil {
		return SourceSizeEstimate{}, errors.New("Docker Sandboxes emulation download estimate overflows uint64")
	}
	estimate.ExpandedBytes, err = sumStorageBytes(estimate.ExpandedBytes, dockerSandboxesExpandedEmulationAllowanceBytes)
	if err != nil {
		return SourceSizeEstimate{}, errors.New("Docker Sandboxes expanded emulation estimate overflows uint64")
	}
	estimate.Confidence = EstimateFallback
	return estimate, nil
}

func (m *Coordinator) dockerSandboxesStoragePlan(source ResolvedDockerSource, cached bool, operationID string) (ArtifactStoragePlan, error) {
	estimate, err := m.dockerSandboxesSourceEstimate(source)
	if err != nil {
		return ArtifactStoragePlan{}, err
	}
	dockerDisk, err := config.ParseByteSize(m.Config.DockerSandboxes.DockerDisk)
	if err != nil {
		return ArtifactStoragePlan{}, err
	}
	plan, err := PlanArtifactStorage("docker-sandboxes", estimate, cached, uint64(dockerDisk))
	if err != nil {
		return ArtifactStoragePlan{}, err
	}
	plan.OperationPlan.ID = operationID
	plan.Notes = append(plan.Notes, "Emulation storage includes the exact compressed size of the pinned binfmt manifest layers plus a conservative 64MiB expanded allowance for the installer, static QEMU interpreters, and license evidence; the estimate is not marked exact.")
	return plan, nil
}

func (m *Coordinator) dockerSandboxesImportStoragePlan(source ResolvedDockerSource, archiveBytes uint64) (ArtifactStoragePlan, error) {
	estimate, err := m.dockerSandboxesSourceEstimate(source)
	if err != nil {
		return ArtifactStoragePlan{}, err
	}
	return PlanDockerSandboxesImportStorage(estimate, archiveBytes)
}

func (m *Coordinator) effectiveDockerSandboxesRootDisk(source ResolvedDockerSource) (string, error) {
	estimate, err := m.dockerSandboxesSourceEstimate(source)
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

func (m *Coordinator) buildDockerSandboxesTemplate(ctx context.Context, manifest Manifest, source ResolvedDockerSource, manifestHash, rootDisk string, allowReusable bool, runtime provider.TemplateArtifactRuntime) error {
	plan, err := m.dockerSandboxesStoragePlan(source, false, "template-build")
	if err != nil {
		return err
	}
	if err := m.preflightStorage(plan.OperationPlan); err != nil {
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
	artifactRoot, err := m.dockerSandboxesArtifactRoot(manifestHash)
	if err != nil {
		return fmt.Errorf("resolve Docker Sandboxes template workspace: %w", err)
	}
	if err := os.MkdirAll(artifactRoot, 0o755); err != nil {
		return err
	}
	if err := m.recordSandboxWorkspace(ctx, artifactRoot, manifestHash, storagecatalog.StateStaging, time.Now().UTC()); err != nil {
		return fmt.Errorf("record Docker Sandboxes archive workspace ownership: %w", err)
	}
	platformLock := lock.Platforms[source.Platform]
	emulationPlatformLock := lock.Emulation.Platforms[source.Platform]
	builder, err := m.ensureBuildxBuilder(ctx, []string{
		source.ImmutableReference,
		lock.DockerfileFrontend.Reference,
		platformLock.GoBuilderReference,
		platformLock.SBOMGeneratorReference,
		emulationPlatformLock.SourceReference,
	})
	if err != nil {
		return err
	}
	builderNeedsStop := !m.DryRun
	defer func() {
		if !builderNeedsStop {
			return
		}
		stopContext, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if stopErr := m.stopBuildxBuilder(stopContext, builder, "release resident memory after the Docker Sandboxes build"); stopErr != nil {
			m.warnf("EPAR Buildx builder shutdown warning: %v\n", stopErr)
		}
	}()
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
	emulationLicenseRoot := filepath.Join(inputRoot, "emulation-licenses")
	actionsRunnerCachePath, err := m.acquireActionsRunner(ctx, manifest)
	if err != nil {
		return err
	}
	if err := copyFile(actionsRunnerCachePath, actionsRunnerPath, 0o600); err != nil {
		return fmt.Errorf("stage GitHub Actions runner %s: %w", manifest.RunnerVersion, err)
	}
	if err := verifiedDownload(ctx, downloadClient, platformLock.Tini.URL, tiniPath, platformLock.Tini.SHA256, 0o700); err != nil {
		return fmt.Errorf("acquire locked tini: %w", err)
	}
	if err := os.MkdirAll(emulationLicenseRoot, 0o755); err != nil {
		return err
	}
	for _, license := range lock.Emulation.Source.Licenses {
		if err := verifiedDownload(ctx, downloadClient, license.URL, filepath.Join(emulationLicenseRoot, license.Name), license.SHA256, 0o600); err != nil {
			return fmt.Errorf("acquire locked emulation license %s: %w", license.Name, err)
		}
	}
	buildMetadataPath := filepath.Join(artifactRoot, "build-metadata.json")
	attestationMetadataPath := filepath.Join(artifactRoot, "attestation-metadata.json")
	provenancePath := filepath.Join(artifactRoot, "provenance.json")
	sbomPath := filepath.Join(artifactRoot, "sbom.intoto.json")
	inventoryPath := filepath.Join(artifactRoot, "software-inventory.txt")
	compatibilityEvidencePath := filepath.Join(artifactRoot, "compatibility.json")
	archivePath := filepath.Join(artifactRoot, "runner-template.tar")
	partialArchivePath := archivePath + ".partial"
	metadataPath := filepath.Join(artifactRoot, "template-metadata.json")
	resumed, err := m.resumeDockerSandboxesTemplate(ctx, manifest, source, manifestHash, rootDisk, artifactRoot, metadataPath, archivePath, runtime)
	if err != nil {
		return err
	}
	if resumed {
		return nil
	}
	var buildMetadata dockerSandboxesBuildMetadata
	var localDigest string
	var contextRoot string
	var buildArguments []string
	var archiveSHA string
	var archiveBytes uint64
	recoveredBuild := false
	if recoveredBuild {
		m.infof("reusing completed exact Docker Sandboxes Buildx result %s\n", localDigest)
		if err := writeJSONFile(buildMetadataPath, buildMetadata); err != nil {
			return err
		}
		if err := writeAtomicFile(provenancePath, append(buildMetadata.Provenance, '\n'), 0o644); err != nil {
			return err
		}
	} else {
		contextRoot, err = os.MkdirTemp(filepath.Join(m.ProjectRoot, ".local"), "docker-sandboxes-context-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(contextRoot)
		if err := copyDirectory(filepath.Join(m.ProjectRoot, "templates", "docker-sandboxes"), contextRoot); err != nil {
			return err
		}
		compatibilityPath := filepath.Join(contextRoot, "profiles", "generated.compatibility.json")
		compatibility := map[string]any{
			"schemaVersion":             3,
			"templateSchemaVersion":     2,
			"profile":                   profile,
			"platform":                  source.Platform,
			"runnerExecution":           "direct-actions-listener",
			"dockerDaemonOwner":         "docker-sandboxes-runtime",
			"expectedDockerDaemonCount": 1,
			"architectureEmulation": map[string]any{
				"backend":              lock.Emulation.Backend,
				"policy":               "automatic-binfmt-install-all",
				"release":              lock.Emulation.Source.Release,
				"sourceIndexDigest":    lock.Emulation.Source.IndexDigest,
				"sourceManifestDigest": emulationPlatformLock.ManifestDigest,
				"qemuVersion":          lock.Emulation.Source.QEMUVersion,
			},
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
		if err := copyDirectory(emulationLicenseRoot, filepath.Join(contextRoot, "inputs", "emulation-licenses")); err != nil {
			return err
		}
		for _, path := range []string{buildMetadataPath, attestationMetadataPath, provenancePath, sbomPath, inventoryPath, archivePath, partialArchivePath, metadataPath} {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
		installationID, err := m.catalogInstallationID(time.Now().UTC())
		if err != nil {
			return fmt.Errorf("resolve EPAR template ownership identity: %w", err)
		}
		args := []string{
			"buildx", "build", "--builder", builder, "--platform", source.Platform, "--pull", "--progress", "plain",
			"--target", "runner-template", "--output", "type=docker,dest=" + partialArchivePath,
			"--provenance=false", "--sbom=false",
			"--metadata-file", buildMetadataPath, "--tag", templateTag,
			"--label", "io.solutionforest.epar.schema=1",
			"--label", "io.solutionforest.epar.installation=" + installationID,
			"--label", "io.solutionforest.epar.provider=docker-sandboxes",
			"--label", "io.solutionforest.epar.role=template-staging",
			"--label", "io.solutionforest.epar.manifest=" + manifestHash,
		}
		buildArguments = []string{
			"TEMPLATE_PLATFORM=" + source.Platform,
			"SOURCE_IMAGE=" + source.ImmutableReference,
			"GO_BUILDER_IMAGE=" + platformLock.GoBuilderReference,
			"BINFMT_IMAGE=" + emulationPlatformLock.SourceReference,
			"HOOK_LAUNCHER_SHA256=" + lock.HookLauncher.SHA256,
			"SOURCE_PROFILE=" + profile,
			"SOURCE_INDEX_DIGEST=" + source.IndexDigest,
			"SOURCE_MANIFEST_DIGEST=" + source.PlatformDigest,
			"SOURCE_REVISION=" + source.IndexDigest,
			"TEMPLATE_VERSION=" + manifestHash[:16] + "-" + architecture,
			"COMPATIBILITY_FILE=generated.compatibility.json",
			"ACTIONS_RUNNER_VERSION=" + manifest.RunnerVersion,
			"ACTIONS_RUNNER_SHA256=sha256:" + strings.TrimPrefix(manifest.RunnerAssetDigest, "sha256:"),
			"TINI_SHA256=sha256:" + strings.TrimPrefix(platformLock.Tini.SHA256, "sha256:"),
		}
		for _, buildArg := range buildArguments {
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
		if err := m.runHostBuildxLogged(ctx, buildLogPath, "docker", args...); err != nil {
			return fmt.Errorf("build Docker Sandboxes runner template: %w%s", err, boundedRedactedLogTail(buildLogPath, 32*1024))
		}
		if err := readJSONFile(buildMetadataPath, &buildMetadata); err != nil {
			return fmt.Errorf("read Docker Sandboxes Buildx metadata: %w", err)
		}
		expectedLabels := map[string]string{
			"io.solutionforest.epar.schema":       "1",
			"io.solutionforest.epar.installation": installationID,
			"io.solutionforest.epar.provider":     "docker-sandboxes",
			"io.solutionforest.epar.role":         "template-staging",
			"io.solutionforest.epar.manifest":     manifestHash,
		}
		archiveVerification, err := verifyDockerSandboxesArchive(partialArchivePath, "docker.io/library/"+templateTag, source.Platform, buildMetadata.ImageDigest, expectedLabels)
		if err != nil {
			return fmt.Errorf("verify directly exported Docker Sandboxes archive: %w", err)
		}
		localDigest = archiveVerification.ImageDigest
		archiveSHA = archiveVerification.ArchiveSHA256
		archiveBytes = archiveVerification.ArchiveBytes
		if err := os.Rename(partialArchivePath, archivePath); err != nil {
			return fmt.Errorf("activate verified Docker Sandboxes archive: %w", err)
		}
	}
	evidenceExportRoot := filepath.Join(artifactRoot, "evidence-export")
	if err := os.RemoveAll(evidenceExportRoot); err != nil {
		return err
	}
	if err := m.stopBuildxBuilder(ctx, builder, "release archive-build memory before full-image SBOM generation"); err != nil {
		return err
	}
	builderNeedsStop = false
	attestationArgs := []string{
		"buildx", "build", "--builder", builder, "--platform", source.Platform, "--progress", "plain",
		"--target", "software-inventory-export", "--output", "type=local,dest=" + evidenceExportRoot,
		"--provenance", "mode=max", "--sbom", "generator=" + platformLock.SBOMGeneratorReference,
		"--metadata-file", attestationMetadataPath,
	}
	for _, buildArg := range buildArguments {
		attestationArgs = append(attestationArgs, "--build-arg", buildArg)
	}
	attestationArgs = append(attestationArgs, "--file", filepath.Join(contextRoot, "Dockerfile"), contextRoot)
	attestationLogPath := m.buildLogPath("docker-sandboxes-" + manifestHash[:16] + "-attestation.docker-build.log")
	defer m.releaseTranscript(attestationLogPath)
	if err := resetLogs(attestationLogPath); err != nil {
		return err
	}
	m.infof("full Docker Sandboxes provenance, SBOM, and software-inventory progress: %s\n", attestationLogPath)
	builderNeedsStop = !m.DryRun
	if err := m.runHostBuildxLogged(ctx, attestationLogPath, "docker", attestationArgs...); err != nil {
		return dockerSandboxesEvidenceBuildError(err, attestationLogPath)
	}
	var attestationMetadata dockerSandboxesBuildMetadata
	if err := readJSONFile(attestationMetadataPath, &attestationMetadata); err != nil {
		return fmt.Errorf("read Docker Sandboxes attestation metadata: %w", err)
	}
	if len(attestationMetadata.Provenance) == 0 || string(attestationMetadata.Provenance) == "null" {
		return fmt.Errorf("Docker Sandboxes attestation build omitted max-mode provenance")
	}
	if err := validateBuildxMaxProvenance(attestationMetadata.Provenance); err != nil {
		return fmt.Errorf("validate Docker Sandboxes Buildx provenance metadata: %w", err)
	}
	exportedProvenancePath := filepath.Join(evidenceExportRoot, "provenance.json")
	exportedProvenance, err := readVerifiedBuildEvidence(exportedProvenancePath, storage.GiB)
	if err != nil {
		return fmt.Errorf("read exported Docker Sandboxes provenance: %w", err)
	}
	if err := validateInTotoProvenance(exportedProvenance); err != nil {
		return fmt.Errorf("validate exported Docker Sandboxes provenance: %w", err)
	}
	if err := writeAtomicFile(provenancePath, append(exportedProvenance, '\n'), 0o644); err != nil {
		return err
	}
	exportedSBOMPath := filepath.Join(evidenceExportRoot, "sbom-runner-template.spdx.json")
	exportedSBOM, err := readVerifiedBuildEvidence(exportedSBOMPath, storage.GiB)
	if err != nil {
		return fmt.Errorf("read exported Docker Sandboxes runner-template SBOM: %w", err)
	}
	if err := writeAtomicFile(sbomPath, exportedSBOM, 0o644); err != nil {
		return err
	}
	if err := validateInTotoSPDX(sbomPath); err != nil {
		return fmt.Errorf("validate exported Docker Sandboxes runner-template SBOM: %w", err)
	}
	sbomSourceDigest, _, err := hashFile(sbomPath)
	if err != nil {
		return err
	}
	inventory, err := readVerifiedBuildEvidence(filepath.Join(evidenceExportRoot, "software-inventory.txt"), 64*storage.MiB)
	if err != nil {
		return fmt.Errorf("read exported Docker Sandboxes software inventory: %w", err)
	}
	if len(strings.TrimSpace(string(inventory))) == 0 {
		return errors.New("exported Docker Sandboxes software inventory is empty")
	}
	if err := writeAtomicFile(inventoryPath, inventory, 0o644); err != nil {
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
	metadata.Compatibility.TemplateSchemaVersion = 2
	metadata.Compatibility.RunnerExecution = "direct-actions-listener"
	metadata.Compatibility.DockerDaemonOwner = "docker-sandboxes-runtime"
	metadata.Compatibility.ExpectedDockerDaemonCount = 1
	metadata.Compatibility.EmulationBackend = lock.Emulation.Backend
	metadata.Compatibility.EmulationPolicy = "automatic-binfmt-install-all"
	metadata.Compatibility.EmulationRelease = lock.Emulation.Source.Release
	metadata.Compatibility.EmulationSourceDigest = lock.Emulation.Source.IndexDigest
	metadata.Compatibility.EmulationManifestDigest = emulationPlatformLock.ManifestDigest
	metadata.Compatibility.QEMUVersion = lock.Emulation.Source.QEMUVersion
	for name, path := range map[string]string{
		"buildMetadata":       buildMetadataPath,
		"attestationMetadata": attestationMetadataPath,
		"provenance":          provenancePath,
		"sbom":                sbomPath,
		"softwareInventory":   inventoryPath,
		"compatibility":       compatibilityEvidencePath,
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
	adoptedReusable, activatedAt, err := m.importOrAdoptDockerSandboxesTemplate(ctx, manifest, source, manifestHash, rootDisk, allowReusable, artifact, metadataPath, metadataSHA, archivePath, archiveSHA, archiveBytes, runtime)
	if err != nil {
		return err
	}
	if adoptedReusable {
		m.infof("adopted shared verified Docker Sandboxes runner template for manifest %s after concurrent build work\n", manifestHash)
		return m.finishDockerSandboxesTemplateActivation(ctx, archivePath, manifestHash, m.now().UTC())
	}
	if err := m.finishDockerSandboxesTemplateActivation(ctx, archivePath, manifestHash, activatedAt); err != nil {
		return err
	}
	m.infof("activated Docker Sandboxes runner template %s@%s\n", artifact.Reference, artifact.Digest)
	return nil
}

func (m *Coordinator) importOrAdoptDockerSandboxesTemplate(ctx context.Context, manifest Manifest, source ResolvedDockerSource, manifestHash, rootDisk string, allowReusable bool, artifact provider.TemplateArtifact, metadataPath, metadataSHA, archivePath, archiveSHA string, verifiedArchiveBytes uint64, runtime provider.TemplateArtifactRuntime) (bool, time.Time, error) {
	adoptedReusable := false
	activatedAt := time.Time{}
	err := m.withSandboxBackendLock(ctx, func() error {
		if allowReusable {
			var adoptErr error
			adoptedReusable, adoptErr = m.adoptReusableDockerSandboxesTemplateLocked(ctx, manifest, source, manifestHash, rootDisk, runtime)
			if adoptErr != nil || adoptedReusable {
				return adoptErr
			}
		}
		imported := false
		if err := runtime.VerifyImportedTemplate(ctx, artifact); err != nil {
			if !errors.Is(err, provider.ErrTemplateNotFound) {
				return err
			}
			label := fmt.Sprintf("Docker Sandboxes template-cache import for %s", artifact.Reference)
			importPlan, err := m.dockerSandboxesImportStoragePlan(source, verifiedArchiveBytes)
			if err != nil {
				return fmt.Errorf("plan Docker Sandboxes template-cache import storage: %w", err)
			}
			if err := m.preflightStorage(importPlan.OperationPlan); err != nil {
				return err
			}
			if err := m.runProgressOperation(label, nil, func() error {
				return runtime.ImportTemplate(ctx, archivePath)
			}); err != nil {
				return err
			}
			imported = true
		}
		if imported {
			if err := m.runProgressOperation("Docker Sandboxes imported-template verification", nil, func() error {
				return runtime.VerifyImportedTemplate(ctx, artifact)
			}); err != nil {
				return fmt.Errorf("verify imported Docker Sandboxes runner template: %w", err)
			}
		}
		var publishErr error
		activatedAt, publishErr = m.publishDockerSandboxesTemplateLocked(manifest, source, manifestHash, artifact, metadataPath, metadataSHA, archivePath, archiveSHA, runtime)
		return publishErr
	})
	return adoptedReusable, activatedAt, err
}

func (m *Coordinator) adoptReusableDockerSandboxesTemplateLocked(ctx context.Context, manifest Manifest, source ResolvedDockerSource, manifestHash, rootDisk string, runtime provider.TemplateArtifactRuntime) (bool, error) {
	receipt, sourceReceiptPath, found, err := m.reusableDockerSandboxesReceipt(manifest, source, manifestHash, rootDisk)
	if err != nil || !found {
		return false, err
	}
	if err := runtime.VerifyImportedTemplate(ctx, receipt.Artifact); err != nil {
		if errors.Is(err, provider.ErrTemplateNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("verify reusable Docker Sandboxes template from %s: %w", sourceReceiptPath, err)
	}
	destinationReceiptPath, err := m.dockerSandboxesReceiptPath()
	if err != nil {
		return false, err
	}
	if err := cloneDockerSandboxesReceiptEvidence(sourceReceiptPath, destinationReceiptPath, receipt); err != nil {
		return false, fmt.Errorf("adopt reusable Docker Sandboxes evidence: %w", err)
	}
	if err := runtime.ActivateTemplate(receipt.Artifact); err != nil {
		return false, err
	}
	if err := writeJSONFile(destinationReceiptPath, receipt); err != nil {
		return false, err
	}
	if err := m.recordCurrentSandboxArtifactLocked(receipt.Artifact, manifestHash, receipt.ActivatedAt); err != nil {
		return false, fmt.Errorf("record adopted Docker Sandboxes template ownership: %w", err)
	}
	return true, nil
}

func (m *Coordinator) reusableDockerSandboxesReceipt(manifest Manifest, source ResolvedDockerSource, manifestHash, rootDisk string) (dockerSandboxesReceipt, string, bool, error) {
	backendID, err := sandboxBackendID()
	if err != nil {
		return dockerSandboxesReceipt{}, "", false, err
	}
	store, err := m.hostCatalog()
	if err != nil {
		return dockerSandboxesReceipt{}, "", false, err
	}
	catalogValue, err := store.Load(m.now())
	if err != nil {
		return dockerSandboxesReceipt{}, "", false, err
	}
	projectRoot, err := storagecatalog.CanonicalPath(m.ProjectRoot)
	if err != nil {
		return dockerSandboxesReceipt{}, "", false, err
	}
	currentConfigID, err := storagecatalog.ConfigID(m.ProjectRoot, m.effectiveConfigPath())
	if err != nil {
		return dockerSandboxesReceipt{}, "", false, err
	}
	configs := make(map[string]storagecatalog.Config, len(catalogValue.Configs))
	for _, record := range catalogValue.Configs {
		configs[record.ID] = record
	}
	var selected dockerSandboxesReceipt
	selectedPath := ""
	for _, resource := range catalogValue.Resources {
		if resource.BackendID != backendID || resource.Kind != catalogSandboxTemplateKind || resource.Provider != "docker-sandboxes" || resource.Role != "runtime-template" || resource.Custody != storagecatalog.CustodyGenerated || resource.State != storagecatalog.StateCurrent || resource.ManifestHash != manifestHash || !validSHA256(resource.Fingerprint) || len(resource.Identity) != 12 {
			continue
		}
		for _, reference := range resource.References {
			if reference.ConfigID == currentConfigID || reference.Role != "provider-artifact" || (reference.ManifestHash != "" && reference.ManifestHash != manifestHash) {
				continue
			}
			configRecord, ok := configs[reference.ConfigID]
			if !ok || configRecord.ProjectRoot != projectRoot {
				continue
			}
			receiptPath := filepath.Join(configRecord.ProjectRoot, ".local", "state", "image", reference.ConfigID, "docker-sandboxes", "active.json")
			receipt, readErr := readDockerSandboxesReceiptPath(receiptPath)
			if readErr != nil {
				return dockerSandboxesReceipt{}, "", false, fmt.Errorf("read cataloged Docker Sandboxes receipt %s: %w", receiptPath, readErr)
			}
			recomputedHash, hashErr := ManifestHash(receipt.Manifest)
			if hashErr != nil {
				return dockerSandboxesReceipt{}, "", false, hashErr
			}
			if receipt.ManifestHash != manifestHash || recomputedHash != manifestHash || receipt.ManifestHash != resource.ManifestHash || receipt.Source != source || receipt.Artifact.Reference != resource.Locator || receipt.Artifact.CacheID != resource.Identity || receipt.Artifact.Digest != resource.Fingerprint || receipt.Artifact.Platform != source.Platform || receipt.Artifact.RootDisk != rootDisk {
				return dockerSandboxesReceipt{}, "", false, fmt.Errorf("cataloged Docker Sandboxes artifact for manifest %s disagrees with its active receipt", manifestHash)
			}
			requestedHash, hashErr := ManifestHash(manifest)
			if hashErr != nil {
				return dockerSandboxesReceipt{}, "", false, hashErr
			}
			if requestedHash != recomputedHash {
				return dockerSandboxesReceipt{}, "", false, fmt.Errorf("cataloged Docker Sandboxes receipt does not match the requested manifest")
			}
			if selectedPath != "" && (selected.Artifact != receipt.Artifact || selected.ArchiveSHA256 != receipt.ArchiveSHA256 || selected.MetadataSHA256 != receipt.MetadataSHA256) {
				return dockerSandboxesReceipt{}, "", false, fmt.Errorf("multiple conflicting current Docker Sandboxes artifacts claim manifest %s", manifestHash)
			}
			selected = receipt
			selectedPath = receiptPath
		}
	}
	if selectedPath == "" {
		return dockerSandboxesReceipt{}, "", false, nil
	}
	if err := validateDockerSandboxesReceiptEvidence(selectedPath, selected); err != nil {
		return dockerSandboxesReceipt{}, "", false, err
	}
	return selected, selectedPath, true, nil
}

func validateDockerSandboxesReceiptEvidence(receiptPath string, receipt dockerSandboxesReceipt) error {
	receiptDirectory := filepath.Dir(receiptPath)
	if len(receipt.Evidence) != len(dockerSandboxesCompactEvidenceFiles) {
		return fmt.Errorf("Docker Sandboxes receipt evidence is incomplete")
	}
	for name, filename := range dockerSandboxesCompactEvidenceFiles {
		evidence, found := receipt.Evidence[name]
		if !found {
			return fmt.Errorf("Docker Sandboxes receipt evidence %q is missing", name)
		}
		clean := filepath.Clean(filepath.FromSlash(evidence.Path))
		expectedPath := filepath.ToSlash(filepath.Join("evidence", receipt.ManifestHash, filename))
		if evidence.Path != expectedPath || filepath.IsAbs(clean) || clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || filepath.ToSlash(clean) != evidence.Path || !validSHA256(evidence.SHA256) {
			return fmt.Errorf("Docker Sandboxes receipt evidence %q has an invalid path or digest", name)
		}
		if name == "sbomDescriptor" && !validSHA256(evidence.SourceDigest) {
			return fmt.Errorf("Docker Sandboxes receipt evidence %q has an invalid source digest", name)
		}
		path := filepath.Join(receiptDirectory, clean)
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("Docker Sandboxes receipt evidence %q is missing or not a regular file", name)
		}
		digest, _, err := hashFile(path)
		if err != nil {
			return err
		}
		if digest != evidence.SHA256 {
			return fmt.Errorf("Docker Sandboxes receipt evidence %q digest does not match its receipt", name)
		}
	}
	return nil
}

func cloneDockerSandboxesReceiptEvidence(sourceReceiptPath, destinationReceiptPath string, receipt dockerSandboxesReceipt) error {
	if filepath.Clean(sourceReceiptPath) == filepath.Clean(destinationReceiptPath) {
		return validateDockerSandboxesReceiptEvidence(sourceReceiptPath, receipt)
	}
	if err := validateDockerSandboxesReceiptEvidence(sourceReceiptPath, receipt); err != nil {
		return err
	}
	sourceDirectory := filepath.Dir(sourceReceiptPath)
	destinationDirectory := filepath.Dir(destinationReceiptPath)
	for name, evidence := range receipt.Evidence {
		relative := filepath.FromSlash(evidence.Path)
		destination := filepath.Join(destinationDirectory, relative)
		if err := copyFile(filepath.Join(sourceDirectory, relative), destination, 0o600); err != nil {
			return fmt.Errorf("copy %s: %w", name, err)
		}
		digest, _, err := hashFile(destination)
		if err != nil {
			return err
		}
		if digest != evidence.SHA256 {
			return fmt.Errorf("copied Docker Sandboxes receipt evidence %q changed during publication", name)
		}
	}
	return nil
}

func (m *Coordinator) dockerSandboxesArtifactRoot(manifestHash string) (string, error) {
	configID, err := storagecatalog.ConfigID(m.ProjectRoot, m.effectiveConfigPath())
	if err != nil {
		return "", err
	}
	return filepath.Join(m.ProjectRoot, "work", "template-builds", "docker-sandboxes", configID, manifestHash), nil
}

func readVerifiedBuildEvidence(path string, maximumBytes uint64) ([]byte, error) {
	if maximumBytes == 0 || maximumBytes > uint64(^uint(0)>>1) {
		return nil, fmt.Errorf("invalid build-evidence size limit %d", maximumBytes)
	}
	before, err := storage.SnapshotFilesystemTarget(path)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || uint64(info.Size()) > maximumBytes {
		return nil, fmt.Errorf("build evidence %q has invalid size %d", path, info.Size())
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	after, err := storage.SnapshotFilesystemTarget(path)
	if err != nil {
		return nil, err
	}
	if before.Identity != after.Identity || before.Fingerprint != after.Fingerprint || uint64(len(content)) != uint64(info.Size()) {
		return nil, fmt.Errorf("build evidence %q changed during readback", path)
	}
	return content, nil
}

func validateBuildxMaxProvenance(content []byte) error {
	var provenance struct {
		BuildType  string            `json:"buildType"`
		Materials  []json.RawMessage `json:"materials"`
		Invocation struct {
			Parameters json.RawMessage `json:"parameters"`
		} `json:"invocation"`
	}
	if err := json.Unmarshal(content, &provenance); err != nil {
		return err
	}
	if provenance.BuildType == "" || len(provenance.Materials) == 0 || len(provenance.Invocation.Parameters) == 0 || string(provenance.Invocation.Parameters) == "null" {
		return errors.New("max-mode Buildx provenance omitted build type, materials, or invocation parameters")
	}
	return nil
}

func validateInTotoProvenance(content []byte) error {
	var statement struct {
		Type          string            `json:"_type"`
		PredicateType string            `json:"predicateType"`
		Subject       []json.RawMessage `json:"subject"`
		Predicate     json.RawMessage   `json:"predicate"`
	}
	if err := json.Unmarshal(content, &statement); err != nil {
		return err
	}
	if statement.Type != "https://in-toto.io/Statement/v1" || statement.PredicateType != "https://slsa.dev/provenance/v1" || len(statement.Subject) == 0 || len(statement.Predicate) == 0 || string(statement.Predicate) == "null" {
		return errors.New("exported provenance is not a complete in-toto SLSA v1 statement")
	}
	return nil
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
	activatedAt := time.Time{}
	if err := m.withSandboxBackendLock(ctx, func() error {
		if err := runtime.VerifyImportedTemplate(ctx, artifact); err != nil {
			if !errors.Is(err, provider.ErrTemplateNotFound) {
				return err
			}
			importPlan, err := m.dockerSandboxesImportStoragePlan(source, metadata.Template.ArchiveBytes)
			if err != nil {
				return fmt.Errorf("plan resumed Docker Sandboxes template-cache import storage: %w", err)
			}
			if err := m.preflightStorage(importPlan.OperationPlan); err != nil {
				return err
			}
			if err := m.runProgressOperation("Docker Sandboxes resumed template-cache import", nil, func() error {
				return runtime.ImportTemplate(ctx, archivePath)
			}); err != nil {
				return err
			}
		}
		if err := m.runProgressOperation("Docker Sandboxes resumed imported-template verification", nil, func() error {
			return runtime.VerifyImportedTemplate(ctx, artifact)
		}); err != nil {
			return fmt.Errorf("verify resumed Docker Sandboxes runner template: %w", err)
		}
		if metadata.ManifestHash != manifestHash {
			return fmt.Errorf("verified Docker Sandboxes build evidence changed during resume")
		}
		activatedAt, err = m.publishDockerSandboxesTemplateLocked(manifest, source, manifestHash, artifact, metadataPath, metadataSHA, archivePath, archiveSHA, runtime)
		return err
	}); err != nil {
		return false, err
	}
	if err := m.finishDockerSandboxesTemplateActivation(ctx, archivePath, manifestHash, activatedAt); err != nil {
		return false, err
	}
	m.infof("activated Docker Sandboxes runner template %s@%s\n", artifact.Reference, artifact.Digest)
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
	if metadata.Compatibility.TemplateSchemaVersion != 2 || metadata.Compatibility.RunnerExecution != "direct-actions-listener" || metadata.Compatibility.DockerDaemonOwner != "docker-sandboxes-runtime" || metadata.Compatibility.ExpectedDockerDaemonCount != 1 || metadata.Compatibility.EmulationBackend != "qemu" || metadata.Compatibility.EmulationPolicy != "automatic-binfmt-install-all" || metadata.Compatibility.EmulationRelease == "" || !validSHA256(metadata.Compatibility.EmulationSourceDigest) || !validSHA256(metadata.Compatibility.EmulationManifestDigest) || metadata.Compatibility.QEMUVersion == "" {
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
	var buildMetadata dockerSandboxesBuildMetadata
	buildMetadataEvidence, found := metadata.Artifacts["buildMetadata"]
	if !found || filepath.Base(buildMetadataEvidence.Path) != buildMetadataEvidence.Path {
		return metadata, provider.TemplateArtifact{}, "", "", false, nil
	}
	if err := readJSONFile(filepath.Join(artifactRoot, buildMetadataEvidence.Path), &buildMetadata); err != nil {
		return metadata, provider.TemplateArtifact{}, "", "", false, nil
	}
	archiveVerification, err := verifyDockerSandboxesArchive(archivePath, metadata.Template.Tag, source.Platform, buildMetadata.ImageDigest, map[string]string{
		"io.solutionforest.epar.schema":       "1",
		"io.solutionforest.epar.installation": "*",
		"io.solutionforest.epar.provider":     "docker-sandboxes",
		"io.solutionforest.epar.role":         "template-staging",
		"io.solutionforest.epar.manifest":     manifestHash,
	})
	if err != nil || archiveVerification.ArchiveSHA256 != archiveSHA || archiveVerification.ArchiveBytes != archiveBytes {
		return metadata, provider.TemplateArtifact{}, "", "", false, nil
	}
	requiredEvidence := []string{"buildMetadata", "attestationMetadata", "provenance", "sbom", "softwareInventory", "compatibility"}
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

func (m *Coordinator) publishDockerSandboxesTemplateLocked(manifest Manifest, source ResolvedDockerSource, manifestHash string, artifact provider.TemplateArtifact, metadataPath, metadataSHA, archivePath, archiveSHA string, runtime provider.TemplateArtifactRuntime) (time.Time, error) {
	if err := runtime.ActivateTemplate(artifact); err != nil {
		return time.Time{}, err
	}
	archiveInfo, err := os.Lstat(archivePath)
	if err != nil {
		return time.Time{}, fmt.Errorf("inspect verified Docker Sandboxes archive before activation: %w", err)
	}
	if !archiveInfo.Mode().IsRegular() {
		return time.Time{}, errors.New("verified Docker Sandboxes archive is not a regular file")
	}
	evidence, err := m.persistDockerSandboxesCompactEvidence(manifestHash, filepath.Dir(metadataPath))
	if err != nil {
		return time.Time{}, err
	}
	activatedAt := m.now().UTC()
	receipt := dockerSandboxesReceipt{
		SchemaVersion:  dockerSandboxesReceiptSchema,
		ManifestHash:   manifestHash,
		Manifest:       manifest,
		Source:         source,
		Artifact:       artifact,
		MetadataSHA256: metadataSHA,
		ArchiveSHA256:  archiveSHA,
		ArchiveBytes:   uint64(archiveInfo.Size()),
		Evidence:       evidence,
		ActivatedAt:    activatedAt,
	}
	receiptPath, err := m.dockerSandboxesReceiptPath()
	if err != nil {
		return time.Time{}, err
	}
	if err := writeJSONFile(receiptPath, receipt); err != nil {
		return time.Time{}, err
	}
	if err := m.recordCurrentSandboxArtifactLocked(artifact, manifestHash, activatedAt); err != nil {
		return time.Time{}, fmt.Errorf("record current Docker Sandboxes template ownership: %w", err)
	}
	return activatedAt, nil
}

func (m *Coordinator) finishDockerSandboxesTemplateActivation(ctx context.Context, archivePath, manifestHash string, activatedAt time.Time) error {
	if err := m.recordSandboxWorkspace(ctx, filepath.Dir(archivePath), manifestHash, storagecatalog.StateSuperseded, activatedAt); err != nil {
		return fmt.Errorf("record Docker Sandboxes staging ownership: %w", err)
	}
	if err := m.cleanupSupersededCatalog(ctx); err != nil {
		return err
	}
	return nil
}

func (m *Coordinator) persistDockerSandboxesCompactEvidence(manifestHash, artifactRoot string) (map[string]artifactEvidence, error) {
	receiptPath, err := m.dockerSandboxesReceiptPath()
	if err != nil {
		return nil, err
	}
	evidenceRoot := filepath.Join(filepath.Dir(receiptPath), "evidence", manifestHash)
	if err := os.MkdirAll(evidenceRoot, 0o700); err != nil {
		return nil, err
	}
	result := make(map[string]artifactEvidence)
	for name, filename := range dockerSandboxesCompactEvidenceFiles {
		if name == "sbomDescriptor" {
			continue
		}
		source := filepath.Join(artifactRoot, filename)
		destination := filepath.Join(evidenceRoot, filename)
		if err := copyFile(source, destination, 0o600); err != nil {
			return nil, fmt.Errorf("retain Docker Sandboxes %s evidence: %w", name, err)
		}
		digest, _, err := hashFile(destination)
		if err != nil {
			return nil, err
		}
		result[name] = artifactEvidence{Path: filepath.ToSlash(filepath.Join("evidence", manifestHash, filename)), SHA256: digest}
	}
	sbomPath := filepath.Join(artifactRoot, "sbom.intoto.json")
	sbomDigest, sbomBytes, err := hashFile(sbomPath)
	if err != nil {
		return nil, err
	}
	descriptorPath := filepath.Join(evidenceRoot, "sbom-descriptor.json")
	if err := writeJSONFile(descriptorPath, map[string]any{
		"schemaVersion": 1,
		"digest":        sbomDigest,
		"size":          sbomBytes,
	}); err != nil {
		return nil, err
	}
	descriptorDigest, _, err := hashFile(descriptorPath)
	if err != nil {
		return nil, err
	}
	result["sbomDescriptor"] = artifactEvidence{Path: filepath.ToSlash(filepath.Join("evidence", manifestHash, "sbom-descriptor.json")), SHA256: descriptorDigest, SourceDigest: sbomDigest}
	return result, nil
}

func loadDockerSandboxesSourceLock(projectRoot, platform string) (dockerSandboxesSourceLock, error) {
	var lock dockerSandboxesSourceLock
	path := filepath.Join(projectRoot, "templates", "docker-sandboxes", "sources.lock.json")
	if err := readJSONFile(path, &lock); err != nil {
		return lock, fmt.Errorf("read Docker Sandboxes source lock: %w", err)
	}
	if lock.SchemaVersion != 3 {
		return lock, fmt.Errorf("unsupported Docker Sandboxes source lock schema %d", lock.SchemaVersion)
	}
	expectedIndexReference := lock.Emulation.Source.Repository + ":" + lock.Emulation.Source.Release + "@" + lock.Emulation.Source.IndexDigest
	if lock.DockerfileFrontend.Reference == "" || lock.SBOMGenerator.InspectionReference == "" || lock.GoBuilder.Version == "" || lock.GoBuilder.IndexDigest == "" || lock.HookLauncher.SHA256 == "" || lock.Tini.Version == "" || lock.Emulation.SchemaVersion != 1 || lock.Emulation.Backend != "qemu" || lock.Emulation.Source.Repository != "docker.io/tonistiigi/binfmt" || lock.Emulation.Source.Release == "" || lock.Emulation.Source.Revision == "" || lock.Emulation.Source.QEMUVersion == "" || !validSHA256(lock.Emulation.Source.IndexDigest) || lock.Emulation.Source.IndexReference != expectedIndexReference {
		return lock, errors.New("Docker Sandboxes source lock has incomplete shared build inputs")
	}
	platformLock, ok := lock.Platforms[platform]
	if !ok || platformLock.GoBuilderReference == "" || platformLock.GoBuilderManifestDigest == "" || platformLock.SBOMGeneratorReference == "" || platformLock.SBOMGeneratorManifestDigest == "" || platformLock.DockerfileFrontendManifestDigest == "" || platformLock.Tini.URL == "" || platformLock.Tini.SHA256 == "" {
		return lock, fmt.Errorf("Docker Sandboxes source lock has incomplete build inputs for %s", platform)
	}
	emulationPlatform, ok := lock.Emulation.Platforms[platform]
	expectedManifestReference := lock.Emulation.Source.Repository + ":" + lock.Emulation.Source.Release + "@" + emulationPlatform.ManifestDigest
	if !ok || !validSHA256(emulationPlatform.ManifestDigest) || emulationPlatform.SourceReference != expectedManifestReference || emulationPlatform.CompressedLayerBytes == 0 {
		return lock, fmt.Errorf("Docker Sandboxes source lock has incomplete emulation inputs for %s", platform)
	}
	if len(lock.Emulation.Source.Licenses) < 2 {
		return lock, errors.New("Docker Sandboxes source lock has incomplete emulation license evidence")
	}
	for _, license := range lock.Emulation.Source.Licenses {
		if filepath.Base(license.Name) != license.Name || !strings.HasSuffix(license.Name, ".txt") || !strings.HasPrefix(license.URL, "https://") || !validSHA256("sha256:"+license.SHA256) {
			return lock, errors.New("Docker Sandboxes source lock has invalid emulation license evidence")
		}
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

func dockerSandboxesEvidenceBuildError(cause error, logPath string) error {
	tail := boundedRedactedLogTail(logPath, 16*1024)
	lower := strings.ToLower(tail)
	if strings.Contains(lower, "exit code: 137") && (strings.Contains(lower, "syft") || strings.Contains(lower, "generating sbom")) {
		return fmt.Errorf("generate Docker Sandboxes template evidence: the full-image SBOM scanner was killed with exit code 137, which indicates that the Docker or BuildKit VM exhausted memory; EPAR preserves other configurations' builders, so inspect other Docker workloads or increase the VM memory allocation if this repeats: %w%s", cause, tail)
	}
	return fmt.Errorf("generate Docker Sandboxes template evidence: %w%s", cause, tail)
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
