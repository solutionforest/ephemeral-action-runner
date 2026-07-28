package image

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
)

const buildxMetadataSchemaVersion = 1

type BuildxMetadata struct {
	SchemaVersion int       `json:"schemaVersion"`
	Builder       string    `json:"builder"`
	Driver        string    `json:"driver"`
	ProjectRoot   string    `json:"projectRoot"`
	CacheLimit    string    `json:"cacheLimit"`
	ConfigPath    string    `json:"configPath"`
	CreatedAt     time.Time `json:"createdAt"`
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
	if metadata.SchemaVersion != buildxMetadataSchemaVersion || metadata.Builder == "" || metadata.Driver != "docker-container" || metadata.ProjectRoot == "" || metadata.ConfigPath == "" {
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

func (m *Coordinator) ensureBuildxBuilder(ctx context.Context) (string, error) {
	builder := buildxBuilderName(m.ProjectRoot)
	cacheLimit := strings.TrimSpace(m.Config.Storage.BuildCacheLimit)
	if cacheLimit == "" {
		cacheLimit = "64GiB"
	}
	limitBytes, err := config.ParseByteSize(cacheLimit)
	if err != nil {
		return "", fmt.Errorf("parse storage.buildCacheLimit: %w", err)
	}
	configPath := filepath.Join(m.ProjectRoot, ".local", "storage", "buildkitd.toml")
	expected := BuildxMetadata{
		SchemaVersion: buildxMetadataSchemaVersion,
		Builder:       builder,
		Driver:        "docker-container",
		ProjectRoot:   filepath.Clean(m.ProjectRoot),
		CacheLimit:    cacheLimit,
		ConfigPath:    configPath,
	}
	if m.DryRun {
		m.infof("[dry-run] ensure EPAR-owned Buildx builder %s with cache limit %s\n", builder, cacheLimit)
		return builder, nil
	}

	metadata, metadataErr := LoadBuildxMetadata(m.ProjectRoot)
	_, inspectErr := m.runHostOutput(ctx, "docker", "buildx", "inspect", builder)
	if inspectErr == nil {
		if metadataErr != nil {
			return "", fmt.Errorf("Buildx builder %q already exists without valid EPAR ownership metadata; refusing to adopt it: %w", builder, metadataErr)
		}
		if !buildxMetadataMatches(metadata, expected) {
			return "", fmt.Errorf("Buildx builder %q ownership metadata does not match this project and cache policy", builder)
		}
		if err := m.runHost(ctx, "docker", "buildx", "inspect", "--bootstrap", builder); err != nil {
			return "", fmt.Errorf("bootstrap EPAR Buildx builder %q: %w", builder, err)
		}
		return builder, nil
	}
	if metadataErr == nil && !buildxMetadataMatches(metadata, expected) {
		return "", fmt.Errorf("EPAR Buildx metadata does not match this project and cache policy")
	}
	if err := os.MkdirAll(filepath.Dir(configPath), 0755); err != nil {
		return "", err
	}
	configContent := fmt.Sprintf("[worker.oci]\n  gc = true\n  gckeepstorage = %d\n", uint64(limitBytes))
	if err := writeAtomicFile(configPath, []byte(configContent), 0644); err != nil {
		return "", fmt.Errorf("write EPAR BuildKit configuration: %w", err)
	}
	if metadataErr != nil {
		expected.CreatedAt = time.Now().UTC()
		content, err := json.MarshalIndent(expected, "", "  ")
		if err != nil {
			return "", err
		}
		content = append(content, '\n')
		if err := writeAtomicFile(BuildxMetadataPath(m.ProjectRoot), content, 0644); err != nil {
			return "", fmt.Errorf("write EPAR Buildx ownership metadata: %w", err)
		}
	}
	if err := m.runHost(ctx, "docker", "buildx", "create", "--name", builder, "--driver", "docker-container", "--buildkitd-config", configPath); err != nil {
		return "", fmt.Errorf("create EPAR Buildx builder %q: %w", builder, err)
	}
	if err := m.runHost(ctx, "docker", "buildx", "inspect", "--bootstrap", builder); err != nil {
		return "", fmt.Errorf("bootstrap EPAR Buildx builder %q: %w", builder, err)
	}
	return builder, nil
}

func buildxMetadataMatches(actual, expected BuildxMetadata) bool {
	return actual.SchemaVersion == expected.SchemaVersion &&
		actual.Builder == expected.Builder &&
		actual.Driver == expected.Driver &&
		filepath.Clean(actual.ProjectRoot) == expected.ProjectRoot &&
		actual.CacheLimit == expected.CacheLimit &&
		filepath.Clean(actual.ConfigPath) == expected.ConfigPath
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
