package inventory

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/solutionforest/ephemeral-action-runner/internal/storage"
)

func collectLogs(root, projectRoot string) ([]storage.Artifact, []string) {
	target, exists, err := inspectOptionalRoot(root)
	if !exists {
		if err == nil {
			return nil, nil
		}
		artifact := unknownRootArtifact("logs", root, "", err)
		return []storage.Artifact{artifact}, []string{fmt.Sprintf("logs root was not inventoried safely: %v", err)}
	}
	info, err := os.Lstat(target.Locator)
	if err != nil {
		return nil, []string{fmt.Sprintf("logs root metadata is unavailable: %v", err)}
	}
	size, sizeErr := directoryBytes(target.Locator)
	ownership := storage.Ownership{
		Kind:     storage.OwnershipExact,
		OwnerID:  stableID("epar-logs", projectRoot),
		Evidence: "explicit EPAR logging root",
	}
	protections := []storage.Protection{{Kind: storage.ProtectionOperator, Detail: "logging subsystem owns retention"}}
	var warnings []string
	if sizeErr != nil {
		size = 0
		ownership = storage.Ownership{Kind: storage.OwnershipUnknown}
		protections = append(protections, storage.Protection{Kind: storage.ProtectionUncertain, Detail: "unsafe or unreadable descendant"})
		warnings = append(warnings, fmt.Sprintf("logs root size and ownership are uncertain: %v", sizeErr))
	}
	defaultRoot := filepath.Join(projectRoot, "work", "logs")
	if !samePath(target.Locator, defaultRoot) {
		protections = append(protections, storage.Protection{Kind: storage.ProtectionCustomRoot, Detail: "configured logging root"})
	}
	return []storage.Artifact{{
		ID:          stableID("logs", target.Locator),
		SurfaceID:   ProjectSurfaceID,
		Kind:        storage.ArtifactOther,
		Target:      target,
		Ownership:   ownership,
		SizeBytes:   size,
		CreatedAt:   info.ModTime().UTC(),
		Protections: protections,
	}}, warnings
}

func unknownRootArtifact(kind, root, provider string, cause error) storage.Artifact {
	detail := "root identity is unsafe or unreadable"
	if cause != nil {
		detail = cause.Error()
	}
	return storage.Artifact{
		ID:        stableID(kind+"-unknown", root),
		Provider:  provider,
		SurfaceID: ProjectSurfaceID,
		Kind:      storage.ArtifactOther,
		Target:    unknownTarget(root, true),
		Ownership: storage.Ownership{Kind: storage.OwnershipUnknown},
		Protections: []storage.Protection{{
			Kind:   storage.ProtectionUncertain,
			Detail: detail,
		}},
	}
}
