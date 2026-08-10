package provider

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/storage"
)

type filesystemStorage struct {
	providerType string
	roots        []StorageRoot
	discoveries  []StorageRootDiscovery
	domainProbe  func(string, time.Time) (storage.CapacityDomain, error)
}

type StorageRoot struct {
	ID                        string
	Role                      storage.StorageRole
	Kind                      storage.SurfaceKind
	Path                      string
	CapacityPath              string
	CapacityUnavailableReason string
	Provenance                string
	Confidence                string
	Warnings                  []string
	ReportOnly                bool
}

// StorageRootDiscovery performs read-only provider backing-path discovery at
// snapshot time. Its results are capacity evidence only and do not establish
// cleanup ownership over any returned path.
type StorageRootDiscovery func(context.Context, StorageRequest) ([]StorageRoot, error)

// NewFilesystemStorage creates the conservative common contribution used by
// providers whose EPAR-owned staging/install root is on a host filesystem.
// Provider-specific external stores are added to storage status as report-only
// surfaces until they expose an authoritative capacity API.
func NewFilesystemStorage(providerType, root string) StorageContribution {
	return NewMultiFilesystemStorage(providerType, []StorageRoot{{ID: providerType + "-host-filesystem", Role: storage.StorageRoleProject, Kind: storage.SurfaceHostFilesystem, Path: root}})
}

func NewMultiFilesystemStorage(providerType string, roots []StorageRoot) StorageContribution {
	return NewFilesystemStorageWithDiscovery(providerType, roots)
}

func NewFilesystemStorageWithDiscovery(providerType string, roots []StorageRoot, discoveries ...StorageRootDiscovery) StorageContribution {
	return &filesystemStorage{
		providerType: providerType,
		roots:        append([]StorageRoot(nil), roots...),
		discoveries:  append([]StorageRootDiscovery(nil), discoveries...),
		domainProbe:  storage.ProbeFilesystemCapacityDomain,
	}
}

func (contribution *filesystemStorage) StorageSnapshot(ctx context.Context, request StorageRequest) (StorageSnapshot, error) {
	if contribution == nil || contribution.providerType == "" {
		return StorageSnapshot{}, fmt.Errorf("provider storage contribution is incomplete")
	}
	if len(contribution.roots) == 0 && len(contribution.discoveries) == 0 {
		return StorageSnapshot{}, fmt.Errorf("provider %s has no required storage roots", contribution.providerType)
	}
	specifications := append([]StorageRoot(nil), contribution.roots...)
	requiresProviderDiscovery := len(request.OperationPlan.Phases) == 0
	for _, phase := range request.OperationPlan.Phases {
		for _, allocation := range phase.Allocations {
			if allocation.SurfaceID != "" || (allocation.Role != "" && allocation.Role != storage.StorageRoleProject) {
				requiresProviderDiscovery = true
			}
		}
	}
	if requiresProviderDiscovery {
		for index, discover := range contribution.discoveries {
			if discover == nil {
				return StorageSnapshot{}, fmt.Errorf("provider %s has nil storage discovery %d", contribution.providerType, index)
			}
			discovered, err := discover(ctx, request)
			if err != nil {
				return StorageSnapshot{}, fmt.Errorf("discover %s storage roots: %w", contribution.providerType, err)
			}
			specifications = append(specifications, discovered...)
		}
	}
	if len(specifications) == 0 {
		return StorageSnapshot{}, fmt.Errorf("provider %s discovered no required storage roots", contribution.providerType)
	}
	snapshot := StorageSnapshot{}
	seen := make(map[string]struct{}, len(specifications))
	seenRoles := make(map[storage.StorageRole]string, len(specifications))
	domainByIdentity := make(map[string]storage.CapacityDomain, len(specifications))
	domainByID := make(map[string]storage.CapacityDomain, len(specifications))
	unknownDomainByLocator := make(map[string]storage.CapacityDomain, len(specifications))
	for _, specification := range specifications {
		if strings.TrimSpace(specification.ID) == "" || strings.TrimSpace(specification.Path) == "" {
			return StorageSnapshot{}, fmt.Errorf("provider %s has an incomplete storage root", contribution.providerType)
		}
		if _, duplicate := seen[specification.ID]; duplicate {
			return StorageSnapshot{}, fmt.Errorf("provider %s has duplicate storage surface %q", contribution.providerType, specification.ID)
		}
		seen[specification.ID] = struct{}{}
		if specification.Role != "" {
			if previous, duplicate := seenRoles[specification.Role]; duplicate {
				return StorageSnapshot{}, fmt.Errorf("provider %s maps storage role %q to both %q and %q", contribution.providerType, specification.Role, previous, specification.ID)
			}
			seenRoles[specification.Role] = specification.ID
		}
		capacityPath := specification.CapacityPath
		if capacityPath == "" {
			capacityPath = specification.Path
		}
		kind := specification.Kind
		if kind == "" {
			kind = storage.SurfaceHostFilesystem
		}
		provenance := specification.Provenance
		if provenance == "" {
			provenance = "provider-root"
		}
		confidence := specification.Confidence
		root := capacityPath
		unavailableReason := strings.TrimSpace(specification.CapacityUnavailableReason)
		if unavailableReason == "" {
			resolvedRoot, resolveErr := nearestExistingDirectory(capacityPath)
			if resolveErr != nil {
				unavailableReason = fmt.Sprintf("resolve %s storage root %s: %v", contribution.providerType, specification.ID, resolveErr)
			} else {
				root = resolvedRoot
			}
		}

		var domain storage.CapacityDomain
		if unavailableReason == "" {
			probedDomain, probeErr := contribution.domainProbe(root, request.Now)
			if probeErr != nil {
				unavailableReason = fmt.Sprintf("measure %s storage root %s: %v", contribution.providerType, specification.ID, probeErr)
			} else if strings.TrimSpace(probedDomain.ID) == "" || strings.TrimSpace(probedDomain.Identity) == "" {
				return StorageSnapshot{}, fmt.Errorf("provider %s storage root %s has an incomplete capacity domain", contribution.providerType, specification.ID)
			} else {
				var exists bool
				domain, exists = domainByIdentity[probedDomain.Identity]
				if !exists {
					if previous, duplicateID := domainByID[probedDomain.ID]; duplicateID && previous.Identity != probedDomain.Identity {
						return StorageSnapshot{}, fmt.Errorf("provider %s capacity domain ID %q maps conflicting identities %q and %q", contribution.providerType, probedDomain.ID, previous.Identity, probedDomain.Identity)
					}
					domain = probedDomain
					domainByIdentity[probedDomain.Identity] = domain
					domainByID[domain.ID] = domain
				} else if domain.ID != probedDomain.ID {
					return StorageSnapshot{}, fmt.Errorf("provider %s capacity domain identity %q resolved to conflicting IDs %q and %q", contribution.providerType, probedDomain.Identity, domain.ID, probedDomain.ID)
				}
			}
		}
		if unavailableReason != "" {
			if confidence == "" {
				confidence = "unavailable"
			}
			locator := filepath.Clean(capacityPath)
			var exists bool
			domain, exists = unknownDomainByLocator[locator]
			if !exists {
				domain = storage.CapacityDomain{
					ID:                        contribution.providerType + "-" + specification.ID + "-unmeasured",
					Kind:                      kind,
					Path:                      capacityPath,
					Provenance:                provenance,
					Confidence:                confidence,
					CapacityUnavailableReason: unavailableReason,
					Capacity:                  storage.Capacity{ObservedAt: request.Now},
				}
				if previous, duplicateID := domainByID[domain.ID]; duplicateID {
					return StorageSnapshot{}, fmt.Errorf("provider %s capacity domain ID %q conflicts between measured path %q and unavailable path %q", contribution.providerType, domain.ID, previous.Path, capacityPath)
				}
				unknownDomainByLocator[locator] = domain
				domainByID[domain.ID] = domain
			} else if !strings.Contains(domain.CapacityUnavailableReason, unavailableReason) {
				domain.CapacityUnavailableReason += "; " + unavailableReason
				unknownDomainByLocator[locator] = domain
				domainByID[domain.ID] = domain
			}
			snapshot.Warnings = append(snapshot.Warnings, fmt.Sprintf("provider %s storage capacity is unavailable for surface %s at %s: %s", contribution.providerType, specification.ID, capacityPath, unavailableReason))
		} else if confidence == "" {
			confidence = "authoritative-filesystem-probe"
		}
		snapshot.Surfaces = append(snapshot.Surfaces, storage.Surface{
			ID:                     specification.ID,
			Provider:               contribution.providerType,
			Role:                   specification.Role,
			Kind:                   kind,
			DomainID:               domain.ID,
			Path:                   specification.Path,
			Location:               root,
			Classification:         "logical-provider-root",
			Provenance:             provenance,
			Confidence:             confidence,
			AdmissionAuthoritative: !specification.ReportOnly,
			Capacity:               domain.Capacity,
		})
		snapshot.Warnings = append(snapshot.Warnings, specification.Warnings...)
	}
	for _, domain := range domainByID {
		snapshot.Domains = append(snapshot.Domains, domain)
	}
	sort.Slice(snapshot.Surfaces, func(i, j int) bool { return snapshot.Surfaces[i].ID < snapshot.Surfaces[j].ID })
	sort.Slice(snapshot.Domains, func(i, j int) bool { return snapshot.Domains[i].ID < snapshot.Domains[j].ID })
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
			canonical, evalErr := filepath.EvalSymlinks(absolute)
			if evalErr != nil {
				return "", evalErr
			}
			return filepath.Clean(canonical), nil
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
