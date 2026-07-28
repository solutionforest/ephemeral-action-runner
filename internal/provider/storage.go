package provider

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/solutionforest/ephemeral-action-runner/internal/storage"
)

type filesystemStorage struct {
	providerType      string
	roots             []StorageRoot
	minimumExpansions map[string]uint64
}

type StorageRoot struct {
	ID                string
	Kind              storage.SurfaceKind
	Location          string
	MinimumExpansions map[string]uint64
}

// NewFilesystemStorage creates the conservative common contribution used by
// providers whose EPAR-owned staging/install root is on a host filesystem.
// Provider-specific external stores are added to storage status as report-only
// surfaces until they expose an authoritative capacity API.
func NewFilesystemStorage(providerType, root string) StorageContribution {
	return NewMultiFilesystemStorage(providerType, []StorageRoot{{ID: providerType + "-host-filesystem", Kind: storage.SurfaceHostFilesystem, Location: root}})
}

func NewMultiFilesystemStorage(providerType string, roots []StorageRoot) StorageContribution {
	return NewMultiFilesystemStorageWithMinimumExpansions(providerType, roots, nil)
}

// NewMultiFilesystemStorageWithMinimumExpansions records provider-specific
// lower bounds without leaking provider configuration into the common pool.
func NewMultiFilesystemStorageWithMinimumExpansions(providerType string, roots []StorageRoot, minimumExpansions map[string]uint64) StorageContribution {
	expansions := make(map[string]uint64, len(minimumExpansions))
	for operation, bytes := range minimumExpansions {
		expansions[operation] = bytes
	}
	return &filesystemStorage{
		providerType:      providerType,
		roots:             append([]StorageRoot(nil), roots...),
		minimumExpansions: expansions,
	}
}

func (contribution *filesystemStorage) StorageSnapshot(_ context.Context, request StorageRequest) (StorageSnapshot, error) {
	if contribution == nil || contribution.providerType == "" {
		return StorageSnapshot{}, fmt.Errorf("provider storage contribution is incomplete")
	}
	if len(contribution.roots) == 0 {
		return StorageSnapshot{}, fmt.Errorf("provider %s has no required storage roots", contribution.providerType)
	}
	peakBytes := request.PeakBytes
	if minimum := contribution.minimumExpansions[request.Operation]; minimum > peakBytes {
		peakBytes = minimum
	}
	snapshot := StorageSnapshot{}
	seen := make(map[string]struct{}, len(contribution.roots))
	for _, specification := range contribution.roots {
		if specification.ID == "" || specification.Location == "" {
			return StorageSnapshot{}, fmt.Errorf("provider %s has an incomplete storage root", contribution.providerType)
		}
		if _, duplicate := seen[specification.ID]; duplicate {
			return StorageSnapshot{}, fmt.Errorf("provider %s has duplicate storage surface %q", contribution.providerType, specification.ID)
		}
		seen[specification.ID] = struct{}{}
		surfacePeakBytes := peakBytes
		requiredForOperation := true
		if specification.MinimumExpansions != nil {
			minimum, found := specification.MinimumExpansions[request.Operation]
			requiredForOperation = found
			surfacePeakBytes = request.PeakBytes
			if minimum > surfacePeakBytes {
				surfacePeakBytes = minimum
			}
		}
		root, err := nearestExistingDirectory(specification.Location)
		if err != nil {
			return StorageSnapshot{}, fmt.Errorf("resolve %s storage root %s: %w", contribution.providerType, specification.ID, err)
		}
		capacity, err := storage.ProbeFilesystemCapacity(root, request.Now)
		if err != nil {
			return StorageSnapshot{}, err
		}
		kind := specification.Kind
		if kind == "" {
			kind = storage.SurfaceHostFilesystem
		}
		snapshot.Surfaces = append(snapshot.Surfaces, storage.Surface{
			ID:       specification.ID,
			Provider: contribution.providerType,
			Kind:     kind,
			Location: root,
			Capacity: capacity,
		})
		if !requiredForOperation {
			continue
		}
		snapshot.Requirements = append(snapshot.Requirements, storage.Requirement{
			ID:               request.Operation + "-" + specification.ID,
			Provider:         contribution.providerType,
			SurfaceID:        specification.ID,
			PeakBytes:        surfacePeakBytes,
			MinimumFreeBytes: request.MinimumFreeBytes,
		})
	}
	return snapshot, nil
}

func nearestExistingDirectory(path string) (string, error) {
	if path == "" {
		path = "."
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	for {
		info, statErr := os.Stat(absolute)
		if statErr == nil {
			if !info.IsDir() {
				absolute = filepath.Dir(absolute)
				continue
			}
			return absolute, nil
		}
		if !os.IsNotExist(statErr) {
			return "", statErr
		}
		parent := filepath.Dir(absolute)
		if parent == absolute {
			return "", statErr
		}
		absolute = parent
	}
}
