package inventory

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/storage"
)

// Collect inventories only explicitly supplied or project-local EPAR roots.
// Missing optional roots are empty inventory, while unsafe roots are skipped
// with warnings.
func Collect(options Options) (Snapshot, error) {
	if err := options.validate(); err != nil {
		return Snapshot{}, err
	}
	now := options.Now.UTC()
	projectTarget, err := storage.SnapshotFilesystemTarget(options.ProjectRoot)
	if err != nil {
		return Snapshot{}, fmt.Errorf("inspect storage inventory project root: %w", err)
	}
	if projectTarget.Kind != storage.TargetDirectory {
		return Snapshot{}, fmt.Errorf("storage inventory project root is not a real directory")
	}
	projectRoot := projectTarget.Locator
	capacity, capacityErr := storage.ProbeFilesystemCapacity(projectRoot, now)
	snapshot := Snapshot{
		CollectedAt:    now,
		ProjectRoot:    projectRoot,
		ProviderFilter: options.Provider,
		Surfaces: []storage.Surface{{
			ID:       ProjectSurfaceID,
			Kind:     storage.SurfaceHostFilesystem,
			Location: projectRoot,
			Capacity: capacity,
		}},
	}
	if capacityErr != nil {
		snapshot.Surfaces[0].Capacity = storage.Capacity{ObservedAt: now}
		snapshot.Warnings = append(snapshot.Warnings, fmt.Sprintf("project filesystem capacity is unknown: %v", capacityErr))
	}

	logsRoot := resolveRoot(projectRoot, options.LogsRoot, filepath.Join("work", "logs"))
	nativeRoot := resolveRoot(projectRoot, options.NativeRoot, filepath.Join(".local", "bin"))
	templateRoot := resolveRoot(projectRoot, options.TemplateRoot, filepath.Join("work", "template-builds", "docker-sandboxes"))

	if includeProvider(options.Provider, "") {
		logsSurfaceID := ensureFilesystemSurface(&snapshot, logsRoot, projectRoot, "", now)
		artifacts, warnings := collectLogs(logsRoot, projectRoot)
		assignSurface(artifacts, logsSurfaceID)
		snapshot.Artifacts = append(snapshot.Artifacts, artifacts...)
		snapshot.Warnings = append(snapshot.Warnings, warnings...)

		nativeSurfaceID := ensureFilesystemSurface(&snapshot, nativeRoot, projectRoot, "", now)
		artifacts, warnings = collectNative(nativeOptions{
			Root:              nativeRoot,
			CurrentExecutable: options.CurrentExecutable,
			CurrentRevision:   options.CurrentRevision,
		})
		assignSurface(artifacts, nativeSurfaceID)
		snapshot.Artifacts = append(snapshot.Artifacts, artifacts...)
		snapshot.Warnings = append(snapshot.Warnings, warnings...)
	}
	if includeProvider(options.Provider, ProviderDockerSandboxes) {
		templateSurfaceID := ensureFilesystemSurface(&snapshot, templateRoot, projectRoot, ProviderDockerSandboxes, now)
		artifacts, warnings, err := collectTemplates(templateOptions{
			Root:        templateRoot,
			Selections:  options.ConfiguredTemplates,
			Protections: options.TemplateProtections,
		})
		if err != nil {
			return Snapshot{}, err
		}
		assignSurface(artifacts, templateSurfaceID)
		snapshot.Artifacts = append(snapshot.Artifacts, artifacts...)
		snapshot.Warnings = append(snapshot.Warnings, warnings...)
	}
	artifacts, warnings := collectConfiguredFiles(options.ConfiguredFiles, options.Provider, projectRoot, &snapshot, now)
	snapshot.Artifacts = append(snapshot.Artifacts, artifacts...)
	snapshot.Warnings = append(snapshot.Warnings, warnings...)
	snapshot.normalize()
	return snapshot, nil
}

func assignSurface(artifacts []storage.Artifact, surfaceID string) {
	for index := range artifacts {
		artifacts[index].SurfaceID = surfaceID
	}
}

func ensureFilesystemSurface(snapshot *Snapshot, root, projectRoot, provider string, now time.Time) string {
	absolute, err := filepath.Abs(root)
	if err != nil {
		absolute = root
	}
	absolute = filepath.Clean(absolute)
	if pathWithin(projectRoot, absolute) {
		return ProjectSurfaceID
	}
	id := stableID("filesystem", absolute)
	for index := range snapshot.Surfaces {
		if snapshot.Surfaces[index].ID == id {
			if snapshot.Surfaces[index].Provider != provider {
				snapshot.Surfaces[index].Provider = ""
			}
			return id
		}
	}
	capacity, probeErr := storage.ProbeFilesystemCapacity(absolute, now)
	if probeErr != nil {
		capacity = storage.Capacity{ObservedAt: now}
		snapshot.Warnings = append(snapshot.Warnings, fmt.Sprintf("filesystem capacity for configured root %q is unknown: %v", absolute, probeErr))
	}
	snapshot.Surfaces = append(snapshot.Surfaces, storage.Surface{
		ID:       id,
		Provider: provider,
		Kind:     storage.SurfaceHostFilesystem,
		Location: absolute,
		Capacity: capacity,
	})
	return id
}

func pathWithin(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	if err != nil {
		return false
	}
	return relative == "." || (relative != ".." && !filepath.IsAbs(relative) && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))
}

func resolveRoot(projectRoot, configured, fallback string) string {
	if configured == "" {
		return filepath.Join(projectRoot, fallback)
	}
	if filepath.IsAbs(configured) {
		return filepath.Clean(configured)
	}
	return filepath.Join(projectRoot, configured)
}

func inspectOptionalRoot(path string) (storage.Target, bool, error) {
	_, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return storage.Target{}, false, nil
	}
	if err != nil {
		return storage.Target{}, false, err
	}
	target, err := storage.SnapshotFilesystemTarget(path)
	if err != nil {
		return storage.Target{}, false, err
	}
	if target.Kind != storage.TargetDirectory {
		return storage.Target{}, false, fmt.Errorf("%q is not a real directory", path)
	}
	return target, true, nil
}
