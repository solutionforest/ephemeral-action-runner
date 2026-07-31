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

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
	"github.com/solutionforest/ephemeral-action-runner/internal/filelock"
	"github.com/solutionforest/ephemeral-action-runner/internal/hosttrust"
	storagecatalog "github.com/solutionforest/ephemeral-action-runner/internal/storage/catalog"
)

const (
	buildxMetadataSchemaVersion  = 4
	legacyBuildxMaxSchemaVersion = 3
	buildkitImageReference       = "moby/buildkit:buildx-stable-1"
)

type BuildxMetadata struct {
	SchemaVersion     int       `json:"schemaVersion"`
	Builder           string    `json:"builder"`
	Driver            string    `json:"driver"`
	ProjectRoot       string    `json:"projectRoot"`
	ConfigID          string    `json:"configId,omitempty"`
	EPARConfigPath    string    `json:"eparConfigPath,omitempty"`
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
	if metadata.SchemaVersion != buildxMetadataSchemaVersion || metadata.Builder != "epar-"+scope.configID || metadata.Driver != "docker-container" || filepath.Clean(metadata.ProjectRoot) != scope.projectRoot || metadata.ConfigID != scope.configID || filepath.Clean(metadata.EPARConfigPath) != scope.configPath || filepath.Clean(metadata.ConfigPath) != scope.buildkitConfig {
		return BuildxMetadata{}, fmt.Errorf("invalid EPAR Buildx ownership metadata")
	}
	return metadata, nil
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
	var buildKitImageID string
	if err := func() error {
		backendID, releaseBackend, acquireErr := m.acquireDockerBackendLock(ctx)
		if acquireErr != nil {
			return acquireErr
		}
		defer releaseBackend()
		previousBuildKitImageID := ""
		if output, inspectErr := m.runHostOutput(ctx, "docker", "image", "inspect", "--format", "{{.Id}}", buildkitImageReference); inspectErr == nil {
			previousBuildKitImageID = strings.TrimSpace(output)
		}
		if journalErr := m.beginDockerRoleAcquisition(backendID, "buildkit-image", buildkitImageReference, previousBuildKitImageID, time.Now().UTC()); journalErr != nil {
			return fmt.Errorf("journal EPAR BuildKit image acquisition: %w", journalErr)
		}
		if pullErr := m.runHost(ctx, "docker", "pull", buildkitImageReference); pullErr != nil {
			return fmt.Errorf("resolve EPAR BuildKit image %s: %w", buildkitImageReference, pullErr)
		}
		output, inspectErr := m.runHostOutput(ctx, "docker", "image", "inspect", "--format", "{{.Id}}", buildkitImageReference)
		if inspectErr != nil {
			return fmt.Errorf("read EPAR BuildKit image identity: %w", inspectErr)
		}
		buildKitImageID = strings.TrimSpace(output)
		if recordErr := m.recordDockerRoleAcquisition(ctx, "buildkit-image", buildkitImageReference, previousBuildKitImageID, buildKitImageID, time.Now().UTC()); recordErr != nil {
			return fmt.Errorf("record EPAR BuildKit image acquisition: %w", recordErr)
		}
		return nil
	}(); err != nil {
		return "", err
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
		BuildKitImageID:   buildKitImageID,
	}
	storageDirectory := filepath.Join(scope.projectRoot, ".local", "storage")
	if err := validateRegularParent(storageDirectory, m.ProjectRoot); err != nil {
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
	_, inspectErr := m.runHostOutput(ctx, "docker", "buildx", "inspect", builder)
	if inspectErr == nil {
		if metadataErr != nil {
			return "", fmt.Errorf("Buildx builder %q already exists without valid EPAR ownership metadata; refusing to adopt it: %w", builder, metadataErr)
		}
		if !buildxOwnershipMatches(metadata, expected) {
			return "", fmt.Errorf("Buildx builder %q ownership metadata does not match this project; refusing to remove or adopt it", builder)
		}
		if !buildxMetadataMatches(metadata, expected) {
			m.infof("upgrading EPAR-owned Buildx builder %s for operational trust generation %s while preserving its cache state\n", builder, trust.Generation)
			if err := m.runHost(ctx, "docker", "buildx", "rm", "--keep-state", "--force", builder); err != nil {
				return "", fmt.Errorf("remove outdated EPAR Buildx builder %q while retaining state: %w", builder, err)
			}
			inspectErr = fmt.Errorf("builder removed for owned upgrade")
		}
	} else if metadataErr == nil && !buildxOwnershipMatches(metadata, expected) {
		return "", fmt.Errorf("EPAR Buildx metadata does not match this project; refusing to reuse it")
	}

	if inspectErr != nil {
		if err := m.runHost(ctx, "docker", "buildx", "create", "--name", builder, "--driver", "docker-container", "--driver-opt", "image="+buildkitImageReference, "--buildkitd-config", configPath); err != nil {
			return "", fmt.Errorf("create EPAR Buildx builder %q: %w", builder, err)
		}
	}
	if err := m.runHost(ctx, "docker", "buildx", "inspect", "--bootstrap", builder); err != nil {
		return "", fmt.Errorf("bootstrap EPAR Buildx builder %q: %w", builder, err)
	}
	if err := m.verifyBuildxConfiguration(ctx, builder, expected); err != nil {
		return "", err
	}
	now := time.Now().UTC()
	if metadataErr == nil && !metadata.CreatedAt.IsZero() {
		expected.CreatedAt = metadata.CreatedAt
	} else {
		expected.CreatedAt = now
	}
	expected.LastReconciledAt = now
	content, err := json.MarshalIndent(expected, "", "  ")
	if err != nil {
		return "", err
	}
	if err := writeAtomicFile(scope.metadataPath, append(content, '\n'), 0o600); err != nil {
		return "", fmt.Errorf("publish EPAR Buildx ownership metadata: %w", err)
	}
	return builder, nil
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
		buildxOwnershipMatches(actual, expected) &&
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
	if runtime.GOOS == "windows" {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return os.Rename(temporaryPath, path)
}
