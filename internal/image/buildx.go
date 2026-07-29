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
)

const buildxMetadataSchemaVersion = 2

type BuildxMetadata struct {
	SchemaVersion     int       `json:"schemaVersion"`
	Builder           string    `json:"builder"`
	Driver            string    `json:"driver"`
	ProjectRoot       string    `json:"projectRoot"`
	CacheLimit        string    `json:"cacheLimit"`
	ConfigPath        string    `json:"configPath"`
	ConfigSHA256      string    `json:"configSha256,omitempty"`
	TrustGeneration   string    `json:"trustGeneration,omitempty"`
	CertificateBundle string    `json:"certificateBundle,omitempty"`
	CertificateSHA256 string    `json:"certificateSha256,omitempty"`
	RegistryHosts     []string  `json:"registryHosts,omitempty"`
	CreatedAt         time.Time `json:"createdAt"`
	LastReconciledAt  time.Time `json:"lastReconciledAt,omitempty"`
}

func BuildxMetadataPath(projectRoot string) string {
	return filepath.Join(projectRoot, ".local", "storage", "buildx.json")
}

func LoadBuildxMetadata(projectRoot string) (BuildxMetadata, error) {
	content, err := os.ReadFile(BuildxMetadataPath(projectRoot))
	if err != nil {
		return BuildxMetadata{}, err
	}
	var metadata BuildxMetadata
	if err := json.Unmarshal(content, &metadata); err != nil {
		return BuildxMetadata{}, err
	}
	if (metadata.SchemaVersion != 1 && metadata.SchemaVersion != buildxMetadataSchemaVersion) || metadata.Builder == "" || metadata.Driver != "docker-container" || metadata.ProjectRoot == "" || metadata.ConfigPath == "" {
		return BuildxMetadata{}, fmt.Errorf("invalid EPAR Buildx ownership metadata")
	}
	return metadata, nil
}

func buildxBuilderName(projectRoot string) string {
	canonical := filepath.Clean(projectRoot)
	if runtime.GOOS == "windows" {
		canonical = strings.ToLower(canonical)
	}
	sum := sha256.Sum256([]byte(canonical))
	return "epar-" + hex.EncodeToString(sum[:6])
}

func (m *Coordinator) ensureBuildxBuilder(ctx context.Context, registryReferences []string) (string, error) {
	builder := buildxBuilderName(m.ProjectRoot)
	cacheLimit := strings.TrimSpace(m.Config.Storage.BuildCacheLimit)
	if cacheLimit == "" {
		cacheLimit = "64GiB"
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
	certificatePath := filepath.Join(m.ProjectRoot, ".local", "storage", "buildkit-certs", trust.Generation, "ca.pem")
	configPath := filepath.Join(m.ProjectRoot, ".local", "storage", "buildkitd.toml")
	configContent := buildkitConfig(uint64(limitBytes), trust.Generation, certificatePath, registryHosts)
	configSHA := sha256.Sum256(configContent)
	expected := BuildxMetadata{
		SchemaVersion:     buildxMetadataSchemaVersion,
		Builder:           builder,
		Driver:            "docker-container",
		ProjectRoot:       filepath.Clean(m.ProjectRoot),
		CacheLimit:        cacheLimit,
		ConfigPath:        configPath,
		ConfigSHA256:      hex.EncodeToString(configSHA[:]),
		TrustGeneration:   trust.Generation,
		CertificateBundle: certificatePath,
		CertificateSHA256: hex.EncodeToString(bundleSHA[:]),
		RegistryHosts:     registryHosts,
	}
	storageDirectory := filepath.Join(m.ProjectRoot, ".local", "storage")
	if err := validateRegularParent(storageDirectory, m.ProjectRoot); err != nil {
		return "", fmt.Errorf("validate EPAR storage directory: %w", err)
	}
	reconcileLock, err := filelock.Acquire(filepath.Join(storageDirectory, "buildx.lock"))
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

	metadata, metadataErr := LoadBuildxMetadata(m.ProjectRoot)
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
		if err := m.runHost(ctx, "docker", "buildx", "create", "--name", builder, "--driver", "docker-container", "--buildkitd-config", configPath); err != nil {
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
	if err := writeAtomicFile(BuildxMetadataPath(m.ProjectRoot), append(content, '\n'), 0o600); err != nil {
		return "", fmt.Errorf("publish EPAR Buildx ownership metadata: %w", err)
	}
	return builder, nil
}

func buildxOwnershipMatches(actual, expected BuildxMetadata) bool {
	return actual.Builder == expected.Builder &&
		actual.Driver == expected.Driver &&
		filepath.Clean(actual.ProjectRoot) == expected.ProjectRoot &&
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
	fmt.Fprintf(&content, "  gckeepstorage = %d\n", cacheLimit)
	certificatePath = strings.ReplaceAll(filepath.Clean(certificatePath), `\`, "/")
	for _, host := range registryHosts {
		fmt.Fprintf(&content, "\n[registry.%s]\n", strconv.Quote(host))
		fmt.Fprintf(&content, "  ca = [%s]\n", strconv.Quote(certificatePath))
	}
	return []byte(content.String())
}

func (m *Coordinator) verifyBuildxConfiguration(ctx context.Context, builder string, expected BuildxMetadata) error {
	container := buildxControlContainer(builder)
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
