package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
	artifactimage "github.com/solutionforest/ephemeral-action-runner/internal/image"
	"github.com/solutionforest/ephemeral-action-runner/internal/provider/dockersandboxes"
	"github.com/solutionforest/ephemeral-action-runner/internal/storage"
	"github.com/solutionforest/ephemeral-action-runner/internal/storage/inventory"
)

func collectExternalStorage(snapshot *inventory.Snapshot, providerFilter string) {
	if providerFilter == "" || providerFilter == "docker-container" || providerFilter == "docker-sandboxes" || providerFilter == "wsl" {
		collectDockerStorage(snapshot, providerFilter)
	}
	if providerFilter == "" || providerFilter == "docker-sandboxes" {
		collectDockerSandboxesStorage(snapshot)
	}
	if runtime.GOOS == "windows" && (providerFilter == "" || providerFilter == "wsl") {
		collectWSLStorage(snapshot)
	}
	if runtime.GOOS == "darwin" && (providerFilter == "" || providerFilter == "tart") {
		collectTartStorage(snapshot)
	}
}

func collectDockerStorage(snapshot *inventory.Snapshot, providerFilter string) {
	const surfaceID = "docker-engine"
	snapshot.Surfaces = append(snapshot.Surfaces, storage.Surface{
		ID:       surfaceID,
		Kind:     storage.SurfaceDockerEngine,
		Location: "docker-engine",
		Capacity: storage.Capacity{ObservedAt: snapshot.CollectedAt},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "docker", "image", "ls", "--all", "--no-trunc", "--format", "{{.Repository}}\t{{.Tag}}\t{{.ID}}\t{{.Size}}").Output()
	if err != nil {
		snapshot.Warnings = append(snapshot.Warnings, fmt.Sprintf("Docker image inventory is unavailable and remains report-only: %v", err))
	} else {
		for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
			parts := strings.Split(strings.TrimSpace(line), "\t")
			if len(parts) != 4 || !strings.HasPrefix(parts[0], "epar-") {
				continue
			}
			reference := parts[0] + ":" + parts[1]
			artifactProvider := dockerImageProvider(parts[0])
			if !storageProviderMatches(providerFilter, artifactProvider) {
				continue
			}
			snapshot.Artifacts = append(snapshot.Artifacts, storage.Artifact{
				ID:        externalStorageID("docker-image", reference, parts[2]),
				Provider:  artifactProvider,
				SurfaceID: surfaceID,
				Kind:      storage.ArtifactDockerImage,
				Target: storage.Target{
					Kind:     storage.TargetDockerImageTag,
					Locator:  reference,
					Identity: parts[2],
					Match:    storage.MatchExact,
				},
				Ownership:   storage.Ownership{Kind: storage.OwnershipUnknown, Evidence: "EPAR prefix is not exact ownership"},
				SizeBytes:   parseDockerSize(parts[3]),
				Protections: []storage.Protection{{Kind: storage.ProtectionUncertain, Detail: "image lacks persisted EPAR owner metadata; explicit prune only"}},
			})
		}
	}
	output, err = exec.CommandContext(ctx, "docker", "system", "df", "-v", "--format", "json").Output()
	if err != nil {
		snapshot.Warnings = append(snapshot.Warnings, fmt.Sprintf("Docker volume size inventory is unavailable and remains report-only: %v", err))
		collectDockerVolumeNames(snapshot, surfaceID)
	} else {
		volumes, parseErr := parseDockerDiskUsageVolumes(output)
		if parseErr != nil {
			snapshot.Warnings = append(snapshot.Warnings, fmt.Sprintf("Docker volume size inventory is unreadable and remains report-only: %v", parseErr))
			collectDockerVolumeNames(snapshot, surfaceID)
		} else {
			collectDockerVolumeRecords(snapshot, surfaceID, volumes)
		}
	}
	collectDedicatedBuildxStorage(snapshot, surfaceID)
}

type dockerDiskUsageVolume struct {
	Name   string `json:"Name"`
	Size   string `json:"Size"`
	Labels string `json:"Labels"`
}

func parseDockerDiskUsageVolumes(output []byte) ([]dockerDiskUsageVolume, error) {
	var usage struct {
		Volumes []dockerDiskUsageVolume `json:"Volumes"`
	}
	if err := json.Unmarshal(output, &usage); err != nil {
		return nil, err
	}
	return usage.Volumes, nil
}

func collectDockerVolumeNames(snapshot *inventory.Snapshot, surfaceID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "docker", "volume", "ls", "--format", "{{.Name}}").Output()
	if err != nil {
		snapshot.Warnings = append(snapshot.Warnings, fmt.Sprintf("Docker volume inventory is unavailable and remains report-only: %v", err))
		return
	}
	volumes := make([]dockerDiskUsageVolume, 0)
	for _, name := range strings.Fields(string(output)) {
		volumes = append(volumes, dockerDiskUsageVolume{Name: name})
	}
	collectDockerVolumeRecords(snapshot, surfaceID, volumes)
}

func collectDockerVolumeRecords(snapshot *inventory.Snapshot, surfaceID string, volumes []dockerDiskUsageVolume) {
	for _, volume := range volumes {
		if !strings.HasPrefix(volume.Name, "epar-") {
			continue
		}
		labels := parseDockerLabels(volume.Labels)
		role := labels["io.solutionforest.epar.cache"]
		if labels["io.solutionforest.epar.project"] == storageProjectID(snapshot.ProjectRoot) &&
			labels["io.solutionforest.epar.schema"] == "1" &&
			sameStorageProjectRoot(labels["io.solutionforest.epar.root"], snapshot.ProjectRoot) &&
			(role == "gomod" || role == "gobuild") {
			snapshot.Artifacts = append(snapshot.Artifacts, storage.Artifact{
				ID:         externalStorageID("go-cache-volume", volume.Name, role, labels["io.solutionforest.epar.root"]),
				SurfaceID:  surfaceID,
				Kind:       storage.ArtifactGoCache,
				Target:     storage.Target{Kind: storage.TargetDockerVolume, Locator: volume.Name, Identity: volume.Name, Fingerprint: labels["io.solutionforest.epar.project"] + "\x00" + role + "\x001", Match: storage.MatchExact},
				Ownership:  storage.Ownership{Kind: storage.OwnershipExact, OwnerID: labels["io.solutionforest.epar.project"], Evidence: "exact Docker volume labels"},
				SizeBytes:  parseDockerSize(volume.Size),
				LastUsedAt: snapshot.CollectedAt,
				Protections: []storage.Protection{{
					Kind:   storage.ProtectionLock,
					Detail: "project-scoped Go cache is bounded by the native-controller wrapper",
				}},
			})
			continue
		}
		snapshot.Artifacts = append(snapshot.Artifacts, storage.Artifact{
			ID:          externalStorageID("docker-volume", volume.Name),
			SurfaceID:   surfaceID,
			Kind:        storage.ArtifactDockerVolume,
			Target:      storage.Target{Kind: storage.TargetDockerVolume, Locator: volume.Name, Identity: volume.Name, Match: storage.MatchExact},
			Ownership:   storage.Ownership{Kind: storage.OwnershipUnknown, Evidence: "name prefix or incomplete labels are not exact ownership"},
			SizeBytes:   parseDockerSize(volume.Size),
			Protections: []storage.Protection{{Kind: storage.ProtectionUncertain, Detail: "volume lacks exact persisted EPAR ownership labels; explicit operator review only"}},
		})
	}
}

func parseDockerLabels(value string) map[string]string {
	labels := make(map[string]string)
	for _, field := range strings.Split(value, ",") {
		key, item, found := strings.Cut(strings.TrimSpace(field), "=")
		if found && key != "" {
			labels[key] = item
		}
	}
	return labels
}

func storageProjectID(projectRoot string) string {
	canonical := filepath.Clean(projectRoot)
	if runtime.GOOS == "windows" {
		canonical = strings.ToLower(canonical)
	}
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:6])
}

func sameStorageProjectRoot(labelRoot, projectRoot string) bool {
	if labelRoot == "" {
		return false
	}
	left, right := filepath.Clean(labelRoot), filepath.Clean(projectRoot)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(left, right)
	}
	return left == right
}

func collectDedicatedBuildxStorage(snapshot *inventory.Snapshot, surfaceID string) {
	metadata, err := artifactimage.LoadBuildxMetadata(snapshot.ProjectRoot)
	if err != nil {
		if !os.IsNotExist(err) {
			snapshot.Warnings = append(snapshot.Warnings, fmt.Sprintf("EPAR Buildx ownership metadata is invalid; no builder cache is trusted: %v", err))
		}
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "docker", "buildx", "du", "--builder", metadata.Builder, "--format", "json").Output()
	if err != nil {
		snapshot.Warnings = append(snapshot.Warnings, fmt.Sprintf("EPAR Buildx cache inventory for %q is unavailable: %v", metadata.Builder, err))
		return
	}
	total, err := parseBuildxDiskUsage(output)
	if err != nil {
		snapshot.Warnings = append(snapshot.Warnings, fmt.Sprintf("EPAR Buildx cache inventory for %q is unreadable: %v", metadata.Builder, err))
		return
	}
	cacheLimit, limitErr := config.ParseByteSize(metadata.CacheLimit)
	if limitErr != nil {
		snapshot.Warnings = append(snapshot.Warnings, fmt.Sprintf("EPAR Buildx cache limit for %q is invalid: %v", metadata.Builder, limitErr))
	}
	artifact := storage.Artifact{
		ID:          externalStorageID("buildx-cache", metadata.Builder, metadata.ProjectRoot),
		SurfaceID:   surfaceID,
		Kind:        storage.ArtifactBuildKitCache,
		Target:      storage.Target{Kind: storage.TargetBuildKitRecord, Locator: metadata.Builder, Identity: metadata.Builder, Fingerprint: metadata.ProjectRoot + "\x00" + metadata.CacheLimit, Match: storage.MatchExact},
		Ownership:   storage.Ownership{Kind: storage.OwnershipExact, OwnerID: metadata.ProjectRoot, Evidence: artifactimage.BuildxMetadataPath(snapshot.ProjectRoot)},
		SizeBytes:   total,
		LastUsedAt:  snapshot.CollectedAt,
		Protections: []storage.Protection{{Kind: storage.ProtectionLock, Detail: "dedicated BuildKit enforces its configured garbage-collection ceiling"}},
	}
	snapshot.Artifacts = append(snapshot.Artifacts, artifact)
	if limitErr == nil && total > uint64(cacheLimit) {
		snapshot.Warnings = append(snapshot.Warnings, fmt.Sprintf("EPAR Buildx cache %q currently uses %d bytes above its %d-byte configured ceiling; BuildKit garbage collection is authoritative", metadata.Builder, total, uint64(cacheLimit)))
	}
}

func parseBuildxDiskUsage(output []byte) (uint64, error) {
	type record struct {
		ID    string          `json:"ID"`
		Size  json.RawMessage `json:"Size"`
		Total json.RawMessage `json:"Total"`
	}
	parseBytes := func(raw json.RawMessage) (uint64, bool) {
		if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
			return 0, false
		}
		var numeric uint64
		if err := json.Unmarshal(raw, &numeric); err == nil {
			return numeric, true
		}
		var text string
		if err := json.Unmarshal(raw, &text); err == nil {
			value := parseDockerSize(text)
			return value, value > 0 || strings.TrimSpace(text) == "0B"
		}
		return 0, false
	}
	trimmed := bytes.TrimSpace(output)
	if len(trimmed) == 0 {
		return 0, nil
	}
	var records []record
	if trimmed[0] == '[' {
		if err := json.Unmarshal(trimmed, &records); err != nil {
			return 0, err
		}
	} else {
		scanner := bufio.NewScanner(bytes.NewReader(trimmed))
		for scanner.Scan() {
			var value record
			if err := json.Unmarshal(scanner.Bytes(), &value); err != nil {
				return 0, err
			}
			records = append(records, value)
		}
		if err := scanner.Err(); err != nil {
			return 0, err
		}
	}
	var sum uint64
	for _, value := range records {
		if total, found := parseBytes(value.Total); found {
			return total, nil
		}
		if value.ID == "" {
			continue
		}
		size, found := parseBytes(value.Size)
		if found {
			sum += size
		}
	}
	return sum, nil
}

func parseDockerSize(value string) uint64 {
	value = strings.TrimSpace(value)
	index := 0
	for index < len(value) && (value[index] == '.' || value[index] >= '0' && value[index] <= '9') {
		index++
	}
	if index == 0 || index == len(value) {
		return 0
	}
	number, err := strconv.ParseFloat(value[:index], 64)
	if err != nil || number < 0 {
		return 0
	}
	multiplier := float64(0)
	switch strings.ToUpper(strings.TrimSpace(value[index:])) {
	case "B":
		multiplier = 1
	case "KB":
		multiplier = 1e3
	case "MB":
		multiplier = 1e6
	case "GB":
		multiplier = 1e9
	case "TB":
		multiplier = 1e12
	case "KIB":
		multiplier = 1 << 10
	case "MIB":
		multiplier = 1 << 20
	case "GIB":
		multiplier = 1 << 30
	case "TIB":
		multiplier = 1 << 40
	default:
		return 0
	}
	bytes := number * multiplier
	if math.IsInf(bytes, 0) || bytes > math.MaxUint64 {
		return 0
	}
	return uint64(bytes)
}

func dockerImageProvider(repository string) string {
	switch {
	case strings.HasPrefix(repository, "epar-docker-sandboxes-"):
		return "docker-sandboxes"
	case strings.HasPrefix(repository, "epar-docker-container-"):
		return "docker-container"
	default:
		// Development images, retired naming schemes, and cache volumes can be
		// shared by more than one provider. Keep them provider-neutral.
		return ""
	}
}

func storageProviderMatches(filter, artifactProvider string) bool {
	return filter == "" || artifactProvider == "" || filter == artifactProvider
}

func collectDockerSandboxesStorage(snapshot *inventory.Snapshot) {
	const surfaceID = "docker-sandboxes-template-cache"
	snapshot.Surfaces = append(snapshot.Surfaces, storage.Surface{
		ID:       surfaceID,
		Provider: "docker-sandboxes",
		Kind:     storage.SurfaceSandboxCache,
		Location: "docker-sandboxes-containerd",
		Capacity: storage.Capacity{ObservedAt: snapshot.CollectedAt},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	templates, err := dockersandboxes.New("").CachedTemplates(ctx)
	if err != nil {
		snapshot.Warnings = append(snapshot.Warnings, fmt.Sprintf("Docker Sandboxes template inventory is unavailable and remains report-only: %v", err))
		return
	}
	for _, template := range templates {
		snapshot.Artifacts = append(snapshot.Artifacts, storage.Artifact{
			ID:          externalStorageID("sandbox-template", template.Reference, template.CacheID),
			Provider:    "docker-sandboxes",
			SurfaceID:   surfaceID,
			Kind:        storage.ArtifactSandboxTemplate,
			Target:      storage.Target{Kind: storage.TargetSandboxTemplate, Locator: template.Reference, Identity: template.CacheID, Match: storage.MatchExact},
			Ownership:   storage.Ownership{Kind: storage.OwnershipUnknown, Evidence: "imported template has no persisted EPAR owner receipt"},
			SizeBytes:   uint64(maxInt64(template.SizeBytes, 0)),
			Protections: []storage.Protection{{Kind: storage.ProtectionUncertain, Detail: "imported templates require explicit prune execution"}},
		})
	}
}

func collectWSLStorage(snapshot *inventory.Snapshot) {
	const surfaceID = "wsl-distributions"
	snapshot.Surfaces = append(snapshot.Surfaces, storage.Surface{ID: surfaceID, Provider: "wsl", Kind: storage.SurfaceExternal, Location: "wsl", Capacity: storage.Capacity{ObservedAt: snapshot.CollectedAt}})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "wsl.exe", "--list", "--quiet").Output()
	if err != nil {
		snapshot.Warnings = append(snapshot.Warnings, fmt.Sprintf("WSL distribution inventory is unavailable and remains report-only: %v", err))
		return
	}
	for _, name := range strings.Fields(strings.ReplaceAll(string(output), "\x00", "")) {
		if !strings.HasPrefix(strings.ToLower(name), "epar-") {
			continue
		}
		snapshot.Artifacts = append(snapshot.Artifacts, externalReportOnlyArtifact("wsl-distribution", "wsl", surfaceID, name))
	}
}

func collectTartStorage(snapshot *inventory.Snapshot) {
	const surfaceID = "tart-images"
	snapshot.Surfaces = append(snapshot.Surfaces, storage.Surface{ID: surfaceID, Provider: "tart", Kind: storage.SurfaceExternal, Location: "tart", Capacity: storage.Capacity{ObservedAt: snapshot.CollectedAt}})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "tart", "list", "--format", "json").Output()
	if err != nil {
		snapshot.Warnings = append(snapshot.Warnings, fmt.Sprintf("Tart image inventory is unavailable and remains report-only: %v", err))
		return
	}
	snapshot.Artifacts = append(snapshot.Artifacts, externalReportOnlyArtifact("tart-inventory", "tart", surfaceID, strings.TrimSpace(string(output))))
}

func externalReportOnlyArtifact(kind, providerName, surfaceID, identity string) storage.Artifact {
	return storage.Artifact{
		ID:          externalStorageID(kind, identity),
		Provider:    providerName,
		SurfaceID:   surfaceID,
		Kind:        storage.ArtifactOther,
		Target:      storage.Target{Kind: storage.TargetExternal, Locator: kind + ":" + identity, Identity: identity, Match: storage.MatchExact},
		Ownership:   storage.Ownership{Kind: storage.OwnershipUnknown},
		Protections: []storage.Protection{{Kind: storage.ProtectionUncertain, Detail: "external provider resource requires explicit operator-reviewed prune"}},
	}
}

func externalStorageID(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return "external-" + hex.EncodeToString(sum[:12])
}

func maxInt64(value, minimum int64) int64 {
	if value < minimum {
		return minimum
	}
	return value
}
