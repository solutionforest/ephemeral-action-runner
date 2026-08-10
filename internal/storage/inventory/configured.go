package inventory

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/storage"
)

func collectConfiguredFiles(files []ConfiguredFile, providerFilter, projectRoot string, snapshot *Snapshot, now time.Time) ([]storage.Artifact, []string) {
	var artifacts []storage.Artifact
	var warnings []string
	for _, configured := range files {
		if !includeProvider(providerFilter, configured.Provider) {
			continue
		}
		path := resolveRoot(projectRoot, configured.Path, configured.Path)
		info, err := os.Stat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			artifacts = append(artifacts, unknownConfiguredFile(configured, path, err))
			warnings = append(warnings, fmt.Sprintf("configured %s artifact %q was not inventoried safely: %v", configured.Role, path, err))
			continue
		}
		if !info.Mode().IsRegular() {
			err = fmt.Errorf("configured artifact is not a regular file")
			artifacts = append(artifacts, unknownConfiguredFile(configured, path, err))
			warnings = append(warnings, fmt.Sprintf("configured %s artifact %q was not inventoried safely: %v", configured.Role, path, err))
			continue
		}
		target, err := storage.SnapshotFilesystemTarget(path)
		if err != nil {
			artifacts = append(artifacts, unknownConfiguredFile(configured, path, err))
			warnings = append(warnings, fmt.Sprintf("configured %s artifact %q was not inventoried safely: %v", configured.Role, path, err))
			continue
		}
		after, err := os.Stat(target.Locator)
		if err != nil || !after.Mode().IsRegular() || after.Size() < 0 {
			if err == nil {
				err = fmt.Errorf("configured artifact changed while being inventoried")
			}
			artifacts = append(artifacts, unknownConfiguredFile(configured, path, err))
			warnings = append(warnings, fmt.Sprintf("configured %s artifact %q was not inventoried safely: %v", configured.Role, path, err))
			continue
		}
		confirmed, err := storage.SnapshotFilesystemTarget(target.Locator)
		if err != nil || confirmed != target {
			if err == nil {
				err = fmt.Errorf("configured artifact identity or metadata drifted while being inventoried")
			}
			artifacts = append(artifacts, unknownConfiguredFile(configured, path, err))
			warnings = append(warnings, fmt.Sprintf("configured %s artifact %q was not inventoried safely: %v", configured.Role, path, err))
			continue
		}
		kind := configured.Kind
		if kind == "" {
			kind = storage.ArtifactOther
		}
		protectionKind := configured.ProtectionKind
		if protectionKind == "" {
			protectionKind = storage.ProtectionConfiguration
		}
		protectionDetail := strings.TrimSpace(configured.ProtectionDetail)
		if protectionDetail == "" {
			protectionDetail = "explicit provider artifact configuration"
		}
		surfaceID := ensureFilesystemSurface(snapshot, filepath.Dir(target.Locator), projectRoot, configured.Provider, now)
		artifact := storage.Artifact{
			ID:             stableID("configured-file", configured.Provider+"\x00"+configured.Role+"\x00"+target.Identity),
			Provider:       configured.Provider,
			SurfaceID:      surfaceID,
			Kind:           kind,
			RetentionGroup: configured.Provider + ":" + configured.Role,
			Target:         target,
			Ownership: storage.Ownership{
				Kind:     storage.OwnershipExact,
				OwnerID:  stableID("configured-owner", configured.Provider+"\x00"+configured.Role+"\x00"+target.Identity),
				Evidence: "exact path persisted in EPAR configuration",
			},
			SizeBytes:  uint64(after.Size()),
			CreatedAt:  after.ModTime().UTC(),
			LastUsedAt: configured.ConfiguredAt.UTC(),
			Current:    configured.Current,
			Protections: []storage.Protection{{
				Kind:   protectionKind,
				Detail: protectionDetail,
			}},
		}
		if artifact.Current {
			artifact.Protections = append(artifact.Protections, storage.Protection{Kind: storage.ProtectionCurrent, Detail: "current configured generation"})
		}
		artifacts = append(artifacts, artifact)
	}
	return artifacts, warnings
}

func unknownConfiguredFile(configured ConfiguredFile, path string, cause error) storage.Artifact {
	artifact := unknownEntryArtifact("configured-file", path, configured.Provider, false, configured.Kind, cause)
	artifact.RetentionGroup = configured.Provider + ":" + configured.Role
	artifact.Protections = append(artifact.Protections, storage.Protection{
		Kind:   storage.ProtectionConfiguration,
		Detail: "configured provider artifact could not be bound to an exact identity",
	})
	return artifact
}
