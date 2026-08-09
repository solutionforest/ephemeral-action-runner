package image

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
	"github.com/solutionforest/ephemeral-action-runner/internal/filelock"
	"github.com/solutionforest/ephemeral-action-runner/internal/hosttrust"
	storagecatalog "github.com/solutionforest/ephemeral-action-runner/internal/storage/catalog"
)

const (
	buildxMetadataSchemaVersion           = 5
	migratableBuildxMetadataSchemaVersion = 4
	legacyBuildxMaxSchemaVersion          = 3
	buildkitImageReference                = "moby/buildkit:buildx-stable-1"
	buildxReconcileAttempts               = 2
)

type BuildxMetadata struct {
	SchemaVersion     int       `json:"schemaVersion"`
	Builder           string    `json:"builder"`
	Driver            string    `json:"driver"`
	ProjectRoot       string    `json:"projectRoot"`
	ConfigID          string    `json:"configId,omitempty"`
	EPARConfigPath    string    `json:"eparConfigPath,omitempty"`
	BackendID         string    `json:"dockerBackendId,omitempty"`
	CacheLimit        string    `json:"cacheLimit"`
	ConfigPath        string    `json:"configPath"`
	ConfigSHA256      string    `json:"configSha256,omitempty"`
	TrustGeneration   string    `json:"trustGeneration,omitempty"`
	CertificateBundle string    `json:"certificateBundle,omitempty"`
	CertificateSHA256 string    `json:"certificateSha256,omitempty"`
	RegistryHosts     []string  `json:"registryHosts,omitempty"`
	BuildKitImageID   string    `json:"buildkitImageId,omitempty"`
	CreatedAt         time.Time `json:"createdAt"`
	LastReconciledAt  time.Time `json:"lastReconciledAt,omitempty"`
}

// BuildxMetadataPath returns the dedicated Buildx metadata path for the
// project's default configuration. Callers that accept --config must use
// BuildxMetadataPathForConfig so independent controllers never share mutable
// BuildKit state.
func BuildxMetadataPath(projectRoot string) string {
	path, err := BuildxMetadataPathForConfig(projectRoot, filepath.Join(projectRoot, ".local", "config.yml"))
	if err != nil {
		return filepath.Join(projectRoot, ".local", "storage", "buildx", "invalid", "metadata.json")
	}
	return path
}

func BuildxMetadataPathForConfig(projectRoot, configPath string) (string, error) {
	scope, err := resolveBuildxScope(projectRoot, configPath)
	if err != nil {
		return "", err
	}
	return scope.metadataPath, nil
}

// LegacyBuildxMetadataPath identifies the project-scoped metadata used before
// Buildx resources became config-scoped. It remains discoverable for exact
// inventory and operator-directed cleanup; current controllers never mutate
// or adopt the legacy builder implicitly.
func LegacyBuildxMetadataPath(projectRoot string) string {
	return filepath.Join(projectRoot, ".local", "storage", "buildx.json")
}

func LoadLegacyBuildxMetadata(projectRoot string) (BuildxMetadata, error) {
	content, err := os.ReadFile(LegacyBuildxMetadataPath(projectRoot))
	if err != nil {
		return BuildxMetadata{}, err
	}
	var metadata BuildxMetadata
	if err := json.Unmarshal(content, &metadata); err != nil {
		return BuildxMetadata{}, err
	}
	canonicalRoot, err := storagecatalog.CanonicalPath(projectRoot)
	if err != nil {
		return BuildxMetadata{}, err
	}
	metadataRoot, err := storagecatalog.CanonicalPath(metadata.ProjectRoot)
	if err != nil {
		return BuildxMetadata{}, fmt.Errorf("invalid legacy EPAR Buildx ownership metadata")
	}
	if metadata.SchemaVersion < 1 || metadata.SchemaVersion > legacyBuildxMaxSchemaVersion || metadata.Builder != legacyBuildxBuilderName(metadata.ProjectRoot) || metadata.Driver != "docker-container" || metadataRoot != canonicalRoot || strings.TrimSpace(metadata.ConfigPath) == "" {
		return BuildxMetadata{}, fmt.Errorf("invalid legacy EPAR Buildx ownership metadata")
	}
	return metadata, nil
}

func LoadBuildxMetadata(projectRoot string) (BuildxMetadata, error) {
	return LoadBuildxMetadataForConfig(projectRoot, filepath.Join(projectRoot, ".local", "config.yml"))
}

func LoadBuildxMetadataForConfig(projectRoot, configPath string) (BuildxMetadata, error) {
	scope, err := resolveBuildxScope(projectRoot, configPath)
	if err != nil {
		return BuildxMetadata{}, err
	}
	content, err := os.ReadFile(scope.metadataPath)
	if err != nil {
		return BuildxMetadata{}, err
	}
	var metadata BuildxMetadata
	if err := json.Unmarshal(content, &metadata); err != nil {
		return BuildxMetadata{}, err
	}
	if (metadata.SchemaVersion != buildxMetadataSchemaVersion && metadata.SchemaVersion != migratableBuildxMetadataSchemaVersion) || metadata.Builder != "epar-"+scope.configID || metadata.Driver != "docker-container" || filepath.Clean(metadata.ProjectRoot) != scope.projectRoot || metadata.ConfigID != scope.configID || filepath.Clean(metadata.EPARConfigPath) != scope.configPath || filepath.Clean(metadata.ConfigPath) != scope.buildkitConfig {
		return BuildxMetadata{}, fmt.Errorf("invalid EPAR Buildx ownership metadata")
	}
	if metadata.SchemaVersion == migratableBuildxMetadataSchemaVersion && metadata.BackendID != "" {
		return BuildxMetadata{}, fmt.Errorf("invalid EPAR Buildx ownership metadata")
	}
	if metadata.SchemaVersion == buildxMetadataSchemaVersion && !validDockerBackendID(metadata.BackendID) {
		return BuildxMetadata{}, fmt.Errorf("invalid EPAR Buildx ownership metadata")
	}
	return metadata, nil
}

func validDockerBackendID(value string) bool {
	if value != strings.TrimSpace(value) || !strings.HasPrefix(value, "docker:") {
		return false
	}
	identity := strings.TrimPrefix(value, "docker:")
	return identity != "" && strings.IndexFunc(identity, func(character rune) bool {
		return unicode.IsSpace(character) || unicode.IsControl(character)
	}) == -1
}

func buildxBuilderName(projectRoot string) string {
	builder, err := buildxBuilderNameForConfig(projectRoot, filepath.Join(projectRoot, ".local", "config.yml"))
	if err != nil {
		return "epar-invalid"
	}
	return builder
}

func buildxBuilderNameForConfig(projectRoot, configPath string) (string, error) {
	scope, err := resolveBuildxScope(projectRoot, configPath)
	if err != nil {
		return "", err
	}
	return "epar-" + scope.configID, nil
}

func legacyBuildxBuilderName(projectRoot string) string {
	canonical := filepath.Clean(projectRoot)
	if runtime.GOOS == "windows" {
		canonical = strings.ToLower(canonical)
	}
	sum := sha256.Sum256([]byte(canonical))
	return "epar-" + hex.EncodeToString(sum[:6])
}

type buildxScope struct {
	configID       string
	projectRoot    string
	configPath     string
	metadataPath   string
	lockPath       string
	buildkitConfig string
	certificateDir string
}

func resolveBuildxScope(projectRoot, configPath string) (buildxScope, error) {
	if strings.TrimSpace(configPath) == "" {
		configPath = filepath.Join(projectRoot, ".local", "config.yml")
	}
	configID, err := storagecatalog.ConfigID(projectRoot, configPath)
	if err != nil {
		return buildxScope{}, err
	}
	canonicalRoot, err := canonicalBuildxPath(projectRoot)
	if err != nil {
		return buildxScope{}, err
	}
	canonicalConfig, err := canonicalBuildxPath(configPath)
	if err != nil {
		return buildxScope{}, err
	}
	storageRoot := filepath.Join(canonicalRoot, ".local", "storage")
	buildxRoot := filepath.Join(storageRoot, "buildx", configID)
	return buildxScope{
		configID:       configID,
		projectRoot:    canonicalRoot,
		configPath:     canonicalConfig,
		metadataPath:   filepath.Join(buildxRoot, "metadata.json"),
		lockPath:       filepath.Join(buildxRoot, "reconcile.lock"),
		buildkitConfig: filepath.Join(storageRoot, "buildkit", configID, "buildkitd.toml"),
		certificateDir: filepath.Join(storageRoot, "buildkit-certs", configID),
	}, nil
}

func canonicalBuildxPath(path string) (string, error) {
	return storagecatalog.CanonicalPath(path)
}

func (m *Coordinator) ensureBuildxBuilder(ctx context.Context, registryReferences []string) (string, error) {
	scope, err := resolveBuildxScope(m.ProjectRoot, m.effectiveConfigPath())
	if err != nil {
		return "", fmt.Errorf("resolve Buildx configuration scope: %w", err)
	}
	builder := "epar-" + scope.configID
	if legacy, legacyErr := LoadLegacyBuildxMetadata(m.ProjectRoot); legacyErr == nil {
		m.warnf("Legacy project-scoped EPAR Buildx builder %q remains recorded at %s; it is not reused by config-scoped controllers and remains visible to storage inventory for explicit cleanup.\n", legacy.Builder, LegacyBuildxMetadataPath(m.ProjectRoot))
	} else if !os.IsNotExist(legacyErr) {
		m.warnf("Legacy EPAR Buildx metadata at %s is invalid and was left untouched: %v\n", LegacyBuildxMetadataPath(m.ProjectRoot), legacyErr)
	}
	cacheLimit := strings.TrimSpace(m.Config.Storage.BuildCacheLimit)
	if cacheLimit == "" {
		cacheLimit = "20GiB"
	}
	limitBytes, err := config.ParseByteSize(cacheLimit)
	if err != nil {
		return "", fmt.Errorf("parse storage.buildCacheLimit: %w", err)
	}
	registryHosts, err := buildRegistryHosts(registryReferences)
	if err != nil {
		return "", err
	}
	if m.DryRun {
		m.infof("[dry-run] ensure EPAR-owned Buildx builder %s with cache limit %s for registries %s\n", builder, cacheLimit, strings.Join(registryHosts, ", "))
		return builder, nil
	}
	trust, err := m.resolveBuildTrust(ctx)
	if err != nil {
		return "", err
	}
	if len(trust.Certificates) == 0 {
		return "", fmt.Errorf("operational BuildKit trust resolved no system CA certificates")
	}
	bundle, err := buildTrustBundle(trust)
	if err != nil {
		return "", err
	}
	bundleSHA := sha256.Sum256(bundle)
	certificatePath := filepath.Join(scope.certificateDir, trust.Generation, "ca.pem")
	configPath := scope.buildkitConfig
	configContent := buildkitConfig(uint64(limitBytes), trust.Generation, certificatePath, registryHosts)
	configSHA := sha256.Sum256(configContent)
	expected := BuildxMetadata{
		SchemaVersion:     buildxMetadataSchemaVersion,
		Builder:           builder,
		Driver:            "docker-container",
		ProjectRoot:       scope.projectRoot,
		ConfigID:          scope.configID,
		EPARConfigPath:    scope.configPath,
		CacheLimit:        cacheLimit,
		ConfigPath:        configPath,
		ConfigSHA256:      hex.EncodeToString(configSHA[:]),
		TrustGeneration:   trust.Generation,
		CertificateBundle: certificatePath,
		CertificateSHA256: hex.EncodeToString(bundleSHA[:]),
		RegistryHosts:     registryHosts,
	}
	storageDirectory := filepath.Join(scope.projectRoot, ".local", "storage")
	if err := validateRegularParent(storageDirectory, scope.projectRoot); err != nil {
		return "", fmt.Errorf("validate EPAR storage directory: %w", err)
	}
	if err := validateRegularParent(filepath.Dir(scope.lockPath), storageDirectory); err != nil {
		return "", fmt.Errorf("validate EPAR Buildx lock directory: %w", err)
	}
	if err := validateRegularParent(filepath.Dir(configPath), storageDirectory); err != nil {
		return "", fmt.Errorf("validate EPAR BuildKit configuration directory: %w", err)
	}
	reconcileLock, err := filelock.Acquire(scope.lockPath)
	if err != nil {
		if errors.Is(err, filelock.ErrLocked) {
			return "", fmt.Errorf("another EPAR process is reconciling the project Buildx builder")
		}
		return "", err
	}
	defer reconcileLock.Close()

	if err := validateRegularParent(filepath.Dir(certificatePath), storageDirectory); err != nil {
		return "", fmt.Errorf("validate BuildKit certificate generation directory: %w", err)
	}
	if err := writeAtomicFile(certificatePath, bundle, 0o600); err != nil {
		return "", fmt.Errorf("write operational BuildKit CA bundle: %w", err)
	}
	if err := writeAtomicFile(configPath, configContent, 0o600); err != nil {
		return "", fmt.Errorf("write EPAR BuildKit configuration: %w", err)
	}

	metadata, metadataErr := LoadBuildxMetadataForConfig(m.ProjectRoot, m.effectiveConfigPath())
	if metadataErr != nil && !errors.Is(metadataErr, os.ErrNotExist) {
		return "", fmt.Errorf("invalid EPAR Buildx ownership metadata; refusing automatic recovery: %w", metadataErr)
	}
	var ownedMetadata *BuildxMetadata
	if metadataErr == nil {
		if !buildxOwnershipMatches(metadata, expected) {
			return "", fmt.Errorf("EPAR Buildx metadata does not match this project; refusing to remove or reuse the builder")
		}
		ownedMetadata = &metadata
	}
	for attempt := 0; attempt < buildxReconcileAttempts; attempt++ {
		retry, attemptErr := m.reconcileBuildxBuilderAttempt(ctx, scope, builder, ownedMetadata, expected, attempt)
		if attemptErr != nil {
			return "", attemptErr
		}
		if !retry {
			return builder, nil
		}
	}
	return "", fmt.Errorf("EPAR Buildx builder %q recovery exhausted its bounded retry", builder)
}

func (m *Coordinator) reconcileBuildxBuilderAttempt(ctx context.Context, scope buildxScope, builder string, metadata *BuildxMetadata, expected BuildxMetadata, attempt int) (bool, error) {
	backendID, releaseBackend, err := m.acquireDockerBackendLock(ctx)
	if err != nil {
		return false, err
	}
	defer releaseBackend()
	expected.BackendID = backendID
	expected.BuildKitImageID, err = m.resolveBuildKitImageLocked(ctx, backendID)
	if err != nil {
		var backendChangedErr *buildxBackendChangedError
		if errors.As(err, &backendChangedErr) {
			if attempt+1 >= buildxReconcileAttempts {
				return false, fmt.Errorf("%w after one fresh retry; retry after the Docker context stabilizes", err)
			}
			m.warnf("%v; retrying once against the current backend\n", err)
			return true, nil
		}
		return false, err
	}

	needsCreate := false
	if metadata != nil && metadata.SchemaVersion == buildxMetadataSchemaVersion && metadata.BackendID != expected.BackendID {
		m.infof("recreating exact EPAR-owned Buildx builder %s because its recorded Docker backend %s differs from the current backend %s while preserving its daemon-local cache state\n", builder, metadata.BackendID, expected.BackendID)
		if err := m.removeOwnedBuildxBuilder(ctx, builder); err != nil {
			return false, err
		}
		needsCreate = true
	} else {
		inspection, err := m.inspectBuildxBuilder(ctx, builder)
		if err != nil {
			return false, err
		}
		if inspection.exists && metadata == nil {
			return false, fmt.Errorf("Buildx builder %q already exists without valid EPAR ownership metadata; refusing to adopt or remove it", builder)
		}
		needsCreate = !inspection.exists
		if inspection.exists {
			if reason := buildxRecreateReason(*metadata, expected, inspection.output); reason != "" {
				m.infof("recreating exact EPAR-owned Buildx builder %s because %s while preserving its daemon-local cache state\n", builder, reason)
				if err := m.removeOwnedBuildxBuilder(ctx, builder); err != nil {
					return false, err
				}
				needsCreate = true
			}
		}
	}
	if needsCreate {
		if err := m.runHost(ctx, "docker", "buildx", "create", "--name", builder, "--driver", "docker-container", "--driver-opt", "image="+buildkitImageReference, "--buildkitd-config", scope.buildkitConfig); err != nil {
			return false, fmt.Errorf("create EPAR Buildx builder %q: %w", builder, err)
		}
	}

	bootstrapOutput, bootstrapErr := m.runHostOutput(ctx, "docker", "buildx", "inspect", "--bootstrap", builder)
	verificationErr := bootstrapErr
	if bootstrapErr != nil {
		verificationErr = fmt.Errorf("bootstrap EPAR Buildx builder %q: %w", builder, bootstrapErr)
	} else if buildxInspectShowsNodeError(bootstrapOutput) {
		verificationErr = fmt.Errorf("bootstrap EPAR Buildx builder %q reported a node Error despite exiting successfully", builder)
	} else if err := m.verifyBuildxConfiguration(ctx, builder, expected); err != nil {
		verificationErr = err
	}
	if verificationErr != nil {
		if attempt+1 >= buildxReconcileAttempts {
			return false, fmt.Errorf("EPAR Buildx builder %q failed verification after one fresh retry: %w", builder, verificationErr)
		}
		if err := m.removeOwnedBuildxBuilder(ctx, builder); err != nil {
			return false, errors.Join(verificationErr, err)
		}
		m.warnf("EPAR Buildx builder %s verification failed; retrying once with a fresh exact definition while preserving cache state: %v\n", builder, verificationErr)
		return true, nil
	}

	confirmedBackendID, err := m.dockerBackendID(ctx)
	if err != nil {
		return false, fmt.Errorf("recheck Docker backend before publishing EPAR Buildx metadata: %w", err)
	}
	if confirmedBackendID != backendID {
		backendChangedErr := fmt.Errorf("Docker backend changed while reconciling EPAR Buildx builder %q (%s -> %s)", builder, backendID, confirmedBackendID)
		if err := m.removeOwnedBuildxBuilder(ctx, builder); err != nil {
			return false, errors.Join(backendChangedErr, err)
		}
		if attempt+1 >= buildxReconcileAttempts {
			return false, fmt.Errorf("%w after one fresh retry; retry after the Docker context stabilizes", backendChangedErr)
		}
		m.warnf("%v; retrying once against the current backend\n", backendChangedErr)
		return true, nil
	}

	now := time.Now().UTC()
	if metadata != nil && !metadata.CreatedAt.IsZero() {
		expected.CreatedAt = metadata.CreatedAt
	} else {
		expected.CreatedAt = now
	}
	expected.LastReconciledAt = now
	content, err := json.MarshalIndent(expected, "", "  ")
	if err != nil {
		return false, err
	}
	if err := writeAtomicFile(scope.metadataPath, append(content, '\n'), 0o600); err != nil {
		return false, fmt.Errorf("publish EPAR Buildx ownership metadata: %w", err)
	}
	return false, nil
}

func (m *Coordinator) resolveBuildKitImageLocked(ctx context.Context, backendID string) (string, error) {
	previousBuildKitImageID := ""
	if output, inspectErr := m.runHostOutput(ctx, "docker", "image", "inspect", "--format", "{{.Id}}", buildkitImageReference); inspectErr == nil {
		previousBuildKitImageID = strings.TrimSpace(output)
	}
	if err := m.beginDockerRoleAcquisition(backendID, "buildkit-image", buildkitImageReference, previousBuildKitImageID, time.Now().UTC()); err != nil {
		return "", fmt.Errorf("journal EPAR BuildKit image acquisition: %w", err)
	}
	pullCtx, cancelPull := boundedImageAttempt(ctx, dockerPullAttemptTimeout)
	pullErr := m.runHost(pullCtx, "docker", "pull", buildkitImageReference)
	cancelPull()
	if pullErr != nil {
		cause := fmt.Errorf("resolve EPAR BuildKit image %s: %w", buildkitImageReference, pullErr)
		return "", classifyImageCommandFailure("docker.io", "pull EPAR BuildKit image", cause, "", false)
	}
	output, err := m.runHostOutput(ctx, "docker", "image", "inspect", "--format", "{{.Id}}", buildkitImageReference)
	if err != nil {
		return "", fmt.Errorf("read EPAR BuildKit image identity: %w", err)
	}
	buildKitImageID := strings.TrimSpace(output)
	if buildKitImageID == "" {
		return "", errors.New("Docker Engine returned an empty EPAR BuildKit image identity")
	}
	confirmedBackendID, err := m.dockerBackendID(ctx)
	if err != nil {
		return "", fmt.Errorf("recheck Docker backend after resolving the EPAR BuildKit image: %w", err)
	}
	if confirmedBackendID != backendID {
		return "", &buildxBackendChangedError{builder: "EPAR BuildKit image", previous: backendID, current: confirmedBackendID}
	}
	if err := m.recordDockerRoleAcquisitionForBackend(backendID, "buildkit-image", buildkitImageReference, previousBuildKitImageID, buildKitImageID, time.Now().UTC()); err != nil {
		return "", fmt.Errorf("record EPAR BuildKit image acquisition: %w", err)
	}
	return buildKitImageID, nil
}

type buildxBackendChangedError struct {
	builder  string
	previous string
	current  string
}

func (err *buildxBackendChangedError) Error() string {
	return fmt.Sprintf("Docker backend changed while reconciling %s (%s -> %s)", err.builder, err.previous, err.current)
}

type buildxInspection struct {
	exists bool
	output string
}

func (m *Coordinator) inspectBuildxBuilder(ctx context.Context, builder string) (buildxInspection, error) {
	output, err := m.runHostOutput(ctx, "docker", "buildx", "inspect", builder)
	if err == nil {
		return buildxInspection{exists: true, output: output}, nil
	}
	if buildxInspectMeansMissing(builder, err) {
		return buildxInspection{}, nil
	}
	return buildxInspection{}, fmt.Errorf("inspect EPAR Buildx builder %q: %w", builder, err)
}

func buildxInspectMeansMissing(builder string, err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	builder = strings.ToLower(strings.TrimSpace(builder))
	return strings.Contains(message, "builder not found") ||
		(strings.Contains(message, "no builder") && strings.Contains(message, "found")) ||
		(strings.Contains(message, "builder") && strings.Contains(message, builder) && strings.Contains(message, "not found")) ||
		(strings.Contains(message, "failed to find instance") && strings.Contains(message, builder))
}

func buildxInspectShowsNodeError(output string) bool {
	for _, line := range strings.Split(output, "\n") {
		key, _, found := strings.Cut(strings.TrimSpace(line), ":")
		if found && strings.EqualFold(strings.TrimSpace(key), "error") {
			return true
		}
	}
	return false
}

func buildxInspectNodeCount(output string) int {
	inNodes := false
	count := 0
	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.EqualFold(trimmed, "nodes:") {
			inNodes = true
			continue
		}
		if !inNodes {
			continue
		}
		key, _, found := strings.Cut(trimmed, ":")
		if found && strings.EqualFold(strings.TrimSpace(key), "name") {
			count++
		}
	}
	return count
}

func buildxRecreateReason(actual, expected BuildxMetadata, inspectOutput string) string {
	if buildxInspectShowsNodeError(inspectOutput) {
		return "Buildx reported a node Error"
	}
	if nodeCount := buildxInspectNodeCount(inspectOutput); nodeCount > 1 {
		return fmt.Sprintf("Buildx reported %d nodes for an EPAR single-node builder", nodeCount)
	}
	if actual.SchemaVersion == buildxMetadataSchemaVersion && actual.BackendID != expected.BackendID {
		return fmt.Sprintf("its recorded Docker backend %s differs from the current backend %s", actual.BackendID, expected.BackendID)
	}
	if !buildxOperationalMetadataMatches(actual, expected) {
		return "its verified BuildKit configuration or image changed"
	}
	return ""
}

func (m *Coordinator) removeOwnedBuildxBuilder(ctx context.Context, builder string) error {
	rmErr := m.runHost(ctx, "docker", "buildx", "rm", "--keep-state", "--force", builder)
	inspection, inspectErr := m.inspectBuildxBuilder(ctx, builder)
	if inspectErr != nil {
		return errors.Join(fmt.Errorf("verify removal of exact EPAR-owned Buildx builder %q: %w", builder, inspectErr), rmErr)
	}
	if inspection.exists {
		remainingErr := fmt.Errorf("exact EPAR-owned Buildx builder %q remains after detach; refusing to recreate it", builder)
		return errors.Join(remainingErr, rmErr)
	}
	if rmErr != nil {
		m.warnf("Buildx detach for exact EPAR-owned builder %s returned an error, but exact inspect readback confirms that its definition is absent; continuing with cache state preserved: %v\n", builder, rmErr)
	}
	return nil
}

// stopBuildxBuilder releases the config-scoped BuildKit daemon's resident
// memory while preserving the builder definition, cache state, and ownership
// metadata. A later build automatically starts the same builder again.
func (m *Coordinator) stopBuildxBuilder(ctx context.Context, builder, reason string) error {
	builder = strings.TrimSpace(builder)
	if builder == "" {
		return errors.New("stop EPAR Buildx builder: builder name is required")
	}
	if m.DryRun {
		m.infof("[dry-run] stop EPAR-owned Buildx builder %s to %s while preserving cache state\n", builder, reason)
		return nil
	}
	if err := m.runHost(ctx, "docker", "buildx", "stop", builder); err != nil {
		return fmt.Errorf("stop EPAR-owned Buildx builder %q to %s while preserving cache state: %w", builder, reason, err)
	}
	m.infof("stopped EPAR-owned Buildx builder %s to %s; its definition and cache state were preserved\n", builder, reason)
	return nil
}

func buildxOwnershipMatches(actual, expected BuildxMetadata) bool {
	return actual.Builder == expected.Builder &&
		actual.Driver == expected.Driver &&
		filepath.Clean(actual.ProjectRoot) == expected.ProjectRoot &&
		actual.ConfigID == expected.ConfigID &&
		filepath.Clean(actual.EPARConfigPath) == expected.EPARConfigPath &&
		filepath.Clean(actual.ConfigPath) == expected.ConfigPath
}

func buildxMetadataMatches(actual, expected BuildxMetadata) bool {
	return actual.SchemaVersion == expected.SchemaVersion &&
		actual.BackendID == expected.BackendID &&
		buildxOperationalMetadataMatches(actual, expected)
}

func buildxOperationalMetadataMatches(actual, expected BuildxMetadata) bool {
	return buildxOwnershipMatches(actual, expected) &&
		actual.CacheLimit == expected.CacheLimit &&
		actual.ConfigSHA256 == expected.ConfigSHA256 &&
		actual.TrustGeneration == expected.TrustGeneration &&
		filepath.Clean(actual.CertificateBundle) == expected.CertificateBundle &&
		actual.CertificateSHA256 == expected.CertificateSHA256 &&
		actual.BuildKitImageID == expected.BuildKitImageID &&
		sameOrderedStrings(actual.RegistryHosts, expected.RegistryHosts)
}

func buildRegistryHosts(references []string) ([]string, error) {
	seen := make(map[string]struct{})
	for _, reference := range references {
		reference = strings.TrimSpace(reference)
		if reference == "" {
			continue
		}
		parts := strings.SplitN(reference, "/", 2)
		first := parts[0]
		host := "docker.io"
		if len(parts) == 2 && (strings.Contains(first, ".") || strings.Contains(first, ":") || first == "localhost") {
			host = strings.ToLower(first)
		}
		if host == "index.docker.io" || host == "registry-1.docker.io" {
			host = "docker.io"
		}
		if strings.ContainsAny(host, "\"'[] \t\r\n") {
			return nil, fmt.Errorf("invalid registry host derived from %q", reference)
		}
		seen[host] = struct{}{}
	}
	hosts := make([]string, 0, len(seen))
	for host := range seen {
		hosts = append(hosts, host)
	}
	sort.Strings(hosts)
	return hosts, nil
}

func buildTrustBundle(snapshot hosttrust.Snapshot) ([]byte, error) {
	canonical, err := hosttrust.Canonicalize(snapshot)
	if err != nil {
		return nil, err
	}
	var bundle bytes.Buffer
	for _, certificate := range canonical.Certificates {
		bundle.Write(certificate.PEM)
	}
	return bundle.Bytes(), nil
}

func buildkitConfig(cacheLimit uint64, generation, certificatePath string, registryHosts []string) []byte {
	var content strings.Builder
	fmt.Fprintf(&content, "# epar-build-trust-generation=%s\n", generation)
	content.WriteString("[worker.oci]\n")
	content.WriteString("  gc = true\n")
	fmt.Fprintf(&content, "  reservedSpace = %s\n", strconv.Quote("2GiB"))
	fmt.Fprintf(&content, "  maxUsedSpace = %s\n", strconv.Quote(strconv.FormatUint(cacheLimit, 10)+"B"))
	fmt.Fprintf(&content, "  minFreeSpace = %s\n", strconv.Quote("1GiB"))
	certificatePath = strings.ReplaceAll(filepath.Clean(certificatePath), `\`, "/")
	for _, host := range registryHosts {
		fmt.Fprintf(&content, "\n[registry.%s]\n", strconv.Quote(host))
		fmt.Fprintf(&content, "  ca = [%s]\n", strconv.Quote(certificatePath))
	}
	return []byte(content.String())
}

func (m *Coordinator) verifyBuildxConfiguration(ctx context.Context, builder string, expected BuildxMetadata) error {
	container := buildxControlContainer(builder)
	imageID, err := m.runHostOutput(ctx, "docker", "inspect", "--format", "{{.Image}}", container)
	if err != nil {
		return fmt.Errorf("verify EPAR Buildx builder %q image identity: %w", builder, err)
	}
	if strings.TrimSpace(imageID) != expected.BuildKitImageID {
		return fmt.Errorf("EPAR Buildx builder %q uses image %s, expected %s", builder, strings.TrimSpace(imageID), expected.BuildKitImageID)
	}
	content, err := m.runHostOutput(ctx, "docker", "exec", container, "cat", "/etc/buildkit/buildkitd.toml")
	if err != nil {
		return fmt.Errorf("verify EPAR Buildx builder %q configuration readback: %w", builder, err)
	}
	for _, host := range expected.RegistryHosts {
		doubleQuoted := "[registry." + strconv.Quote(host) + "]"
		singleQuoted := "[registry.'" + host + "']"
		if !strings.Contains(content, doubleQuoted) && !strings.Contains(content, singleQuoted) {
			return fmt.Errorf("EPAR Buildx builder %q did not install registry trust for %s", builder, host)
		}
		installedBundle, err := m.runHostOutput(ctx, "docker", "exec", container, "cat", "/etc/buildkit/certs/"+host+"/ca.pem")
		if err != nil {
			return fmt.Errorf("verify EPAR Buildx builder %q CA readback for %s: %w", builder, host, err)
		}
		sum := sha256.Sum256([]byte(installedBundle))
		if hex.EncodeToString(sum[:]) != expected.CertificateSHA256 {
			return fmt.Errorf("EPAR Buildx builder %q CA readback digest for %s does not match trust generation %s", builder, host, expected.TrustGeneration)
		}
	}
	return nil
}

func buildxControlContainer(builder string) string {
	return "buildx_buildkit_" + builder + "0"
}

func validateRegularParent(path, allowedRoot string) error {
	absoluteRoot, err := filepath.Abs(allowedRoot)
	if err != nil {
		return err
	}
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	relative, err := filepath.Rel(absoluteRoot, absolutePath)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%s is outside %s", absolutePath, absoluteRoot)
	}
	current := absoluteRoot
	for _, part := range strings.Split(relative, string(filepath.Separator)) {
		if part == "" || part == "." {
			continue
		}
		current = filepath.Join(current, part)
		info, statErr := os.Lstat(current)
		if os.IsNotExist(statErr) {
			if err := os.Mkdir(current, 0o700); err != nil {
				return err
			}
			continue
		}
		if statErr != nil {
			return statErr
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("%s is not a real directory", current)
		}
	}
	return nil
}

func sameOrderedStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func writeAtomicFile(path string, content []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	if existing, err := os.ReadFile(path); err == nil && bytes.Equal(existing, content) {
		return nil
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(content); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return replaceAtomicFile(temporaryPath, path)
}
