package image

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
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
	"github.com/solutionforest/ephemeral-action-runner/internal/hosttrust"
	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
	storagecatalog "github.com/solutionforest/ephemeral-action-runner/internal/storage/catalog"
)

const (
	imageManifestSchemaVersion = ManifestSchemaVersion
	imageManifestGuestPath     = ManifestGuestPath
	imageManifestLabel         = ManifestLabel
)

type ImageManifest = Manifest
type fileDigest = FileDigest
type sourceCacheManifest = SourceCacheManifest

func hostTrustMetadata(snapshot hosttrust.Snapshot) *HostTrustMetadata {
	if snapshot.Generation == "" {
		return nil
	}
	return &HostTrustMetadata{
		Mode:             hosttrust.ModeOverlay,
		HostOS:           snapshot.HostOS,
		Scopes:           append([]string(nil), snapshot.Scopes...),
		Generation:       snapshot.Generation,
		CertificateCount: len(snapshot.Certificates),
	}
}

func (m *Coordinator) EnsureImage(ctx context.Context) error {
	return m.ensureImage(ctx, false)
}

// UpdateImage forces an immediate remote freshness observation and rebuilds
// only when the resolved immutable inputs differ from the active artifact.
func (m *Coordinator) UpdateImage(ctx context.Context) error {
	return m.ensureImage(ctx, true)
}

func (m *Coordinator) ensureImage(ctx context.Context, forceRemote bool) error {
	if err := m.cleanupSupersededCatalog(ctx); err != nil {
		return fmt.Errorf("reconcile EPAR storage before image provisioning: %w", err)
	}
	m.logEffectiveUpdatePolicy(forceRemote)
	if m.Config.Provider.Type == "docker-sandboxes" {
		return m.ensureDockerSandboxesTemplateWithPolicy(ctx, forceRemote)
	}
	if artifactManager, ok := m.Lifecycle.(provider.ArtifactManager); ok {
		handled, err := artifactManager.EnsureArtifacts(ctx, m.DryRun)
		if handled || err != nil {
			return err
		}
	}
	localManifest, err := m.desiredLocalImageManifest(ctx)
	if err != nil {
		return err
	}
	localHash, err := imageManifestHash(localManifest)
	if err != nil {
		return err
	}
	if m.DryRun {
		m.infof("[dry-run] would ensure image %s has local-input manifest %s\n", m.Config.Image.OutputImage, localHash)
		manifest, err := m.resolveRemoteImageManifest(ctx, localManifest)
		if err != nil {
			return err
		}
		return m.BuildImage(ctx, ImageBuildOptions{Replace: true, Manifest: &manifest})
	}

	now := m.now()
	updateState, err := m.readUpdatePolicyState()
	if err != nil {
		m.warnf("ignoring stale image update state and performing an immediate check: %v\n", err)
		updateState = UpdatePolicyState{SchemaVersion: updatePolicyStateSchemaVersion}
	}
	if m.Config.Provider.Type == "wsl" {
		outputPath := config.ProjectPath(m.ProjectRoot, m.Config.Image.OutputImage)
		sidecarPath := wslImageManifestSidecarPath(outputPath)
		if stored, readErr := readStoredImageManifest(sidecarPath); readErr == nil {
			if info, statErr := os.Stat(sidecarPath); statErr == nil {
				bootstrapped, bootstrapErr := bootstrapUpdatePolicyState(&updateState, m.Config.Image, localManifest, stored.Manifest, nil, info.ModTime(), now.Location())
				if bootstrapErr != nil {
					return bootstrapErr
				}
				if bootstrapped {
					if err := m.writeUpdatePolicyState(updateState); err != nil {
						return err
					}
					m.infof("initialized image update schedule from the verified active WSL artifact\n")
				}
			}
		}
	}
	if recalculateScheduleForTimeZone(&updateState, m.Config.Image, now.Location()) {
		if err := m.writeUpdatePolicyState(updateState); err != nil {
			return err
		}
	}

	localChanged := updateState.LocalInputHash != "" && updateState.LocalInputHash != localHash
	var (
		currentState imageState
		currentHash  string
		haveResolved bool
	)
	if updateState.LocalInputHash == localHash && updateState.LastResolvedManifest != nil {
		currentHash, err = imageManifestHash(*updateState.LastResolvedManifest)
		if err != nil {
			return err
		}
		currentState, err = m.currentImageState(ctx, currentHash)
		if err != nil {
			return err
		}
		haveResolved = true
	}
	if updateState.LocalInputHash == localHash && pendingUpdateReady(updateState, now) {
		if err := m.ApplyPendingUpdate(ctx, now); err != nil {
			if !forceRemote && haveResolved && currentState == imageStateCurrent {
				status, _ := m.UpdatePolicyStatus()
				m.warnf("pending scheduled image update failed; continuing with the previous verified artifact and retrying after %s: %v\n", formatUpdateTime(status.NextRetryAt), err)
				return nil
			}
			return err
		}
		m.infof("pending image update activated\n")
		return nil
	}

	remoteDue := updateCheckDue(updateState, m.Config.Image, now)
	needsRemote := forceRemote || !haveResolved || localChanged || currentState != imageStateCurrent || remoteDue
	if !needsRemote {
		m.infof("image is current: %s; next remote check %s\n", m.Config.Image.OutputImage, formatUpdateTime(updateState.NextEligibleAt))
		if err := m.recordCurrentArtifact(ctx, currentHash); err != nil {
			return fmt.Errorf("record current EPAR artifact ownership: %w", err)
		}
		return m.cleanupSupersededCatalog(ctx)
	}

	updateState.LastAttemptAt = now.UTC()
	manifest, resolveErr := m.resolveRemoteImageManifest(ctx, localManifest)
	if resolveErr != nil {
		scheduleUpdateFailure(&updateState, now, resolveErr)
		_ = m.writeUpdatePolicyState(updateState)
		if !forceRemote && !localChanged && haveResolved && currentState == imageStateCurrent {
			m.warnf("scheduled image update check failed; continuing with the last verified artifact and retrying after %s: %v\n", formatUpdateTime(updateState.NextRetryAt), resolveErr)
			return nil
		}
		return resolveErr
	}
	resolvedHash, err := imageManifestHash(manifest)
	if err != nil {
		return err
	}
	resolvedState, err := m.currentImageState(ctx, resolvedHash)
	if err != nil {
		return err
	}
	if resolvedState == imageStateCurrent {
		updateState.LocalInputHash = localHash
		updateState.LastResolvedManifest = &manifest
		updateState.PendingManifest = nil
		if err := scheduleNextSuccess(&updateState, m.Config.Image, now); err != nil {
			return err
		}
		if err := m.writeUpdatePolicyState(updateState); err != nil {
			return err
		}
		m.infof("image is current: %s; next remote check %s\n", m.Config.Image.OutputImage, formatUpdateTime(updateState.NextEligibleAt))
		if err := m.recordCurrentArtifact(ctx, resolvedHash); err != nil {
			return fmt.Errorf("record current EPAR artifact ownership: %w", err)
		}
		return m.cleanupSupersededCatalog(ctx)
	}
	if resolvedState == imageStateMissing {
		m.infof("image is missing; building %s\n", m.Config.Image.OutputImage)
	} else {
		m.infof("image inputs changed; rebuilding %s\n", m.Config.Image.OutputImage)
	}
	updateState.LocalInputHash = localHash
	updateState.PendingManifest = &manifest
	updateState.DeferredReason = "artifact build and activation pending"
	if err := m.writeUpdatePolicyState(updateState); err != nil {
		return err
	}
	if err := m.buildResolvedImage(ctx, manifest); err != nil {
		scheduleUpdateFailure(&updateState, now, err)
		_ = m.writeUpdatePolicyState(updateState)
		if !forceRemote && !localChanged && haveResolved && currentState == imageStateCurrent {
			m.warnf("scheduled image update failed; restoring the last verified artifact and retrying after %s: %v\n", formatUpdateTime(updateState.NextRetryAt), err)
			return nil
		}
		return err
	}
	if err := m.recordCurrentArtifact(ctx, resolvedHash); err != nil {
		return fmt.Errorf("record current EPAR artifact ownership: %w", err)
	}
	updateState.LastResolvedManifest = &manifest
	updateState.PendingManifest = nil
	if err := scheduleNextSuccess(&updateState, m.Config.Image, m.now()); err != nil {
		return err
	}
	if err := m.writeUpdatePolicyState(updateState); err != nil {
		return err
	}
	return m.cleanupSupersededCatalog(ctx)
}

func (m *Coordinator) logEffectiveUpdatePolicy(forceRemote bool) {
	if forceRemote {
		m.infof("image update policy: immediate manual check requested\n")
		return
	}
	if m.Config.Image.UpdateFrequency == config.ImageUpdateFrequencyManual {
		m.infof("image update policy: manual; remote image and Actions runner checks run only when requested\n")
		return
	}
	m.infof("image update policy: %s at %s local time\n", m.Config.Image.UpdateFrequency, m.Config.Image.UpdateTime)
}

func (m *Coordinator) buildResolvedImage(ctx context.Context, manifest Manifest) error {
	originalSource := m.Config.Image.SourceImage
	if manifest.SourceType == config.ImageSourceDockerImage && strings.Contains(manifest.SourceDigest, "@sha256:") {
		m.Config.Image.SourceImage = manifest.SourceDigest
	}
	defer func() {
		m.Config.Image.SourceImage = originalSource
	}()
	return m.BuildImage(ctx, ImageBuildOptions{Replace: true, Manifest: &manifest})
}

type imageState int

const (
	imageStateMissing imageState = iota
	imageStateOutdated
	imageStateCurrent
)

func (m *Coordinator) currentImageState(ctx context.Context, wantHash string) (imageState, error) {
	switch m.Config.Provider.Type {
	case "docker-container":
		got, exists, err := m.currentDockerContainerManifestHash(ctx)
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
		return m.currentTartImageState(ctx, wantHash)
	default:
		return imageStateMissing, fmt.Errorf("unsupported provider.type %q", m.Config.Provider.Type)
	}
}

func (m *Coordinator) currentDockerContainerManifestHash(ctx context.Context) (string, bool, error) {
	output := strings.TrimSpace(m.Config.Image.OutputImage)
	if output == "" {
		return "", false, fmt.Errorf("image.outputImage is required")
	}
	out, err := m.runHostOutput(ctx, "docker", "image", "inspect", "--format", "{{json .Config.Labels}}", output)
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

func (m *Coordinator) currentWSLManifestHash() (string, bool, error) {
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

func (m *Coordinator) currentTartImageState(ctx context.Context, wantHash string) (imageState, error) {
	instances, err := m.Provider.List(ctx)
	if err != nil {
		return imageStateMissing, err
	}
	var current provider.Instance
	for _, instance := range instances {
		if instance.Name == m.Config.Image.OutputImage || instance.Source == m.Config.Image.OutputImage {
			current = instance
			break
		}
	}
	if current.Name == "" {
		return imageStateMissing, nil
	}
	if current.ProviderID == "" {
		return imageStateOutdated, nil
	}
	store, err := m.hostCatalog()
	if err != nil {
		return imageStateMissing, err
	}
	value, err := store.Load(time.Now().UTC())
	if err != nil {
		return imageStateMissing, err
	}
	configID, err := storagecatalog.ConfigID(m.ProjectRoot, m.effectiveConfigPath())
	if err != nil {
		return imageStateMissing, err
	}
	if tartCatalogReferenceMatches(value, configID, strings.TrimSpace(m.Config.Image.OutputImage), current.ProviderID, wantHash) {
		return imageStateCurrent, nil
	}
	return imageStateOutdated, nil
}

func tartCatalogReferenceMatches(value storagecatalog.Catalog, configID, locator, identity, manifestHash string) bool {
	for _, resource := range value.Resources {
		if resource.Kind != catalogTartImageKind || resource.Locator != locator || resource.Identity != identity {
			continue
		}
		for _, reference := range resource.References {
			if reference.ConfigID == configID && reference.Role == "provider-artifact" && reference.ManifestHash == manifestHash {
				return true
			}
		}
	}
	return false
}

func (m *Coordinator) desiredImageManifest(ctx context.Context) (ImageManifest, error) {
	snapshot, err := m.resolveHostTrust(ctx)
	if err != nil {
		return ImageManifest{}, err
	}
	return m.desiredImageManifestWithHostTrust(ctx, snapshot)
}

func (m *Coordinator) desiredImageManifestWithHostTrust(ctx context.Context, snapshot hosttrust.Snapshot) (ImageManifest, error) {
	manifest, err := m.desiredLocalImageManifestWithHostTrust(snapshot)
	if err != nil {
		return ImageManifest{}, err
	}
	return m.resolveRemoteImageManifest(ctx, manifest)
}

func (m *Coordinator) desiredLocalImageManifest(ctx context.Context) (ImageManifest, error) {
	snapshot, err := m.resolveHostTrust(ctx)
	if err != nil {
		return ImageManifest{}, err
	}
	return m.desiredLocalImageManifestWithHostTrust(snapshot)
}

func (m *Coordinator) desiredLocalImageManifestWithHostTrust(snapshot hosttrust.Snapshot) (ImageManifest, error) {
	sourceType := m.Config.Image.SourceType
	if sourceType == "" {
		sourceType = config.ImageSourceRootFSTar
		if m.Config.Provider.Type == "docker-container" {
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
		RunnerSelector:     normalizedRunnerSelector(m.Config.Image.RunnerVersion),
		HostTrust:          hostTrustMetadata(snapshot),
	}
	switch sourceType {
	case config.ImageSourceDockerImage:
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

func (m *Coordinator) resolveRemoteImageManifest(ctx context.Context, manifest ImageManifest) (ImageManifest, error) {
	if manifest.SourceType == config.ImageSourceDockerImage {
		switch m.Config.Provider.Type {
		case "docker-container", "wsl":
			source, err := m.resolveDockerSandboxesSource(ctx)
			if err != nil {
				return manifest, err
			}
			manifest.SourceImage = source.Reference
			manifest.SourcePlatform = source.Platform
			manifest.SourceDigest = source.ImmutableReference
			manifest.SourcePlatformDigest = source.PlatformDigest
		case "tart":
			source, err := m.resolveTartOCIReference(ctx, manifest.SourceImage)
			if err != nil {
				return manifest, err
			}
			manifest.SourceDigest = source
		default:
			return manifest, fmt.Errorf("unsupported provider.type %q for Docker image source resolution", m.Config.Provider.Type)
		}
	}
	return m.resolveActionsRunner(ctx, manifest)
}

func (m *Coordinator) refreshDockerSourceDigest(ctx context.Context) (string, error) {
	var digest string
	err := m.timeStartupStage("source_image_pull", func() error {
		var err error
		digest, err = m.refreshDockerSourceDigestUntimed(ctx)
		return err
	})
	return digest, err
}

func (m *Coordinator) refreshDockerSourceDigestUntimed(ctx context.Context) (string, error) {
	if m.DryRun {
		return "dry-run", nil
	}
	image := strings.TrimSpace(m.Config.Image.SourceImage)
	if image == "" {
		return "", fmt.Errorf("image.sourceImage is required when image.sourceType=docker-image")
	}
	platform := strings.TrimSpace(m.Config.Image.SourcePlatform)
	logPath := m.buildLogPath(imageLogStem(m.Config.Image.OutputImage) + ".source.log")
	defer m.releaseTranscript(logPath)
	m.infof("refreshing Docker source image %s\n", image)
	backendID, releaseBackend, err := m.acquireDockerBackendLock(ctx)
	if err != nil {
		return "", err
	}
	defer releaseBackend()
	previousID := ""
	if value, inspectErr := m.runHostOutput(ctx, "docker", "image", "inspect", "--format", "{{.Id}}", image); inspectErr == nil {
		previousID = strings.TrimSpace(value)
	}
	if err := m.beginDockerRoleAcquisition(backendID, "build-source", image, previousID, time.Now().UTC()); err != nil {
		return "", fmt.Errorf("journal Docker source acquisition: %w", err)
	}
	if err := m.pullDockerSource(ctx, DockerSourcePullOptions{
		Image:              image,
		Platform:           platform,
		LogPath:            logPath,
		AnnounceRemoteSize: true,
	}); err != nil {
		return "", fmt.Errorf("refresh Docker source image %s: %w", image, err)
	}
	currentID, err := m.runHostOutput(ctx, "docker", "image", "inspect", "--format", "{{.Id}}", image)
	if err != nil {
		return "", err
	}
	currentID = strings.TrimSpace(currentID)
	if err := m.recordDockerSourceAcquisition(ctx, image, previousID, currentID, time.Now().UTC()); err != nil {
		return "", fmt.Errorf("record Docker source acquisition: %w", err)
	}
	digestsJSON, err := m.runHostOutput(ctx, "docker", "image", "inspect", "--format", "{{json .RepoDigests}}", image)
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
		m.WriteDockerPullNotice(logPath, "Docker source image digest: "+digest)
		return digest, nil
	}
	imageID, err := m.runHostOutput(ctx, "docker", "image", "inspect", "--format", "{{.Id}}", image)
	if err != nil {
		return "", err
	}
	digest := strings.TrimSpace(imageID)
	m.WriteDockerPullNotice(logPath, "Docker source image ID: "+digest)
	return digest, nil
}

func (m *Coordinator) eparScriptDigests() ([]fileDigest, error) {
	var roots []string
	switch m.Config.Provider.Type {
	case "docker-container":
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

func (m *Coordinator) customInstallScriptDigests() ([]fileDigest, error) {
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

func (m *Coordinator) trustedCACertificateDigests() ([]fileDigest, error) {
	if err := m.validateTrustedCACertificates(); err != nil {
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

func (m *Coordinator) fileDigestsUnder(root string) ([]fileDigest, error) {
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

func (m *Coordinator) fileDigest(path string) (fileDigest, error) {
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

func (m *Coordinator) fileSHA256(path string) (string, error) {
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
	return ManifestHash(manifest)
}

func storedImageManifestContent(manifest ImageManifest) (string, string, error) {
	return StoredManifestContent(manifest)
}

func readStoredImageManifest(path string) (StoredManifest, error) {
	return ReadStoredManifest(path)
}

func writeStoredImageManifest(path string, manifest ImageManifest) error {
	return WriteStoredManifest(path, manifest)
}

func (m *Coordinator) installImageManifest(ctx context.Context, vmName string, manifest ImageManifest) error {
	content, _, err := storedImageManifestContent(manifest)
	if err != nil {
		return err
	}
	return provider.CopyText(ctx, m.Provider, vmName, imageManifestGuestPath, "0644", content)
}

func wslImageManifestSidecarPath(outputPath string) string {
	return WSLImageManifestPath(outputPath)
}

func sourceCacheManifestPath(rootfsPath string) string {
	return SourceCacheManifestPath(rootfsPath)
}

func sourceCacheMatches(path string, want sourceCacheManifest) bool {
	return SourceCacheMatches(path, want)
}

func writeSourceCacheManifest(path string, manifest sourceCacheManifest) error {
	return WriteSourceCacheManifest(path, manifest)
}

func dockerInspectMeansMissing(err error) bool {
	return DockerInspectMeansMissing(err)
}
