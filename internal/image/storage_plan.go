package image

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
	"github.com/solutionforest/ephemeral-action-runner/internal/storage"
)

const (
	ExpandedSizeFallbackMultiplier uint64 = 5
	CustomizationAllowanceBytes           = 5 * storage.GiB
	SandboxWritableHeadroomBytes          = 20 * storage.GiB
	SandboxRootRoundingBytes              = 10 * storage.GiB
	SandboxMinimumRootBytes               = 20 * storage.GiB
)

type EstimateConfidence string

const (
	EstimateExact    EstimateConfidence = "exact"
	EstimateDerived  EstimateConfidence = "derived"
	EstimateFallback EstimateConfidence = "conservative-fallback"
)

type SourceSizeEstimate struct {
	CompressedBytes uint64             `json:"compressedBytes"`
	ExpandedBytes   uint64             `json:"expandedBytes"`
	Confidence      EstimateConfidence `json:"confidence"`
}

type ArtifactStoragePlan struct {
	Provider                string                `json:"provider"`
	CompressedDownloadBytes uint64                `json:"compressedDownloadBytes"`
	ExpandedSourceBytes     uint64                `json:"expandedSourceBytes"`
	CustomizationBytes      uint64                `json:"customizationBytes"`
	OperationPlan           storage.OperationPlan `json:"operationPlan"`
	// EstimatedIncrementalPeak is retained only for the onboarding wizard while
	// it moves to capacity-domain-aware operation plans. Runtime preflight must
	// use OperationPlan, whose role allocations preserve their phase overlap.
	EstimatedIncrementalPeak  uint64             `json:"estimatedIncrementalPeak"`
	Confidence                EstimateConfidence `json:"confidence"`
	LogicalRootMaximumBytes   uint64             `json:"logicalRootMaximumBytes,omitempty"`
	LogicalDockerMaximumBytes uint64             `json:"logicalDockerMaximumBytes,omitempty"`
	LogicalLimitsSparse       bool               `json:"logicalLimitsSparse,omitempty"`
	Notes                     []string           `json:"notes,omitempty"`
}

const projectWorkspaceAllowanceBytes = 5 * storage.GiB

func EstimateSourceSize(compressedBytes, exactExpandedBytes uint64) (SourceSizeEstimate, error) {
	if compressedBytes == 0 && exactExpandedBytes == 0 {
		return SourceSizeEstimate{}, errors.New("source size cannot be estimated without compressed or expanded bytes")
	}
	if exactExpandedBytes > 0 {
		return SourceSizeEstimate{CompressedBytes: compressedBytes, ExpandedBytes: exactExpandedBytes, Confidence: EstimateExact}, nil
	}
	if compressedBytes > math.MaxUint64/ExpandedSizeFallbackMultiplier {
		return SourceSizeEstimate{}, errors.New("expanded source-size estimate overflows uint64")
	}
	return SourceSizeEstimate{CompressedBytes: compressedBytes, ExpandedBytes: compressedBytes * ExpandedSizeFallbackMultiplier, Confidence: EstimateFallback}, nil
}

func AutomaticDockerSandboxesRootBytes(expandedSourceBytes uint64) (uint64, error) {
	if expandedSourceBytes > math.MaxUint64-CustomizationAllowanceBytes {
		return 0, errors.New("Docker Sandboxes customization allowance overflows uint64")
	}
	required := expandedSourceBytes + CustomizationAllowanceBytes
	if required > math.MaxUint64-SandboxWritableHeadroomBytes {
		return 0, errors.New("Docker Sandboxes writable headroom overflows uint64")
	}
	required += SandboxWritableHeadroomBytes
	if required < SandboxMinimumRootBytes {
		required = SandboxMinimumRootBytes
	}
	remainder := required % SandboxRootRoundingBytes
	if remainder == 0 {
		return required, nil
	}
	increment := SandboxRootRoundingBytes - remainder
	if required > math.MaxUint64-increment {
		return 0, errors.New("Docker Sandboxes root-disk rounding overflows uint64")
	}
	return required + increment, nil
}

func PlanArtifactStorage(providerType string, source SourceSizeEstimate, cached bool, dockerDiskBytes uint64) (ArtifactStoragePlan, error) {
	plan := ArtifactStoragePlan{
		Provider:                providerType,
		CompressedDownloadBytes: source.CompressedBytes,
		ExpandedSourceBytes:     source.ExpandedBytes,
		CustomizationBytes:      CustomizationAllowanceBytes,
		Confidence:              source.Confidence,
		OperationPlan: storage.OperationPlan{
			ID:       "artifact-build",
			Provider: providerType,
		},
	}
	if cached {
		plan.OperationPlan.Phases = []storage.OperationPhase{{ID: "build-export"}}
		plan.Notes = append(plan.Notes, "A verified current artifact can be reused.")
		return plan, nil
	}
	appendPhase := func(id string, allocations ...storage.Allocation) {
		plan.OperationPlan.Phases = append(plan.OperationPlan.Phases, storage.OperationPhase{ID: id, Allocations: allocations})
	}
	switch providerType {
	case "docker-container":
		engineBytes, err := sumStorageBytes(source.CompressedBytes, source.ExpandedBytes, CustomizationAllowanceBytes)
		if err != nil {
			return ArtifactStoragePlan{}, err
		}
		appendPhase("build-export", storage.Allocation{ID: "docker-image-store-build", Role: storage.StorageRoleContainerdStore, Bytes: engineBytes})
		plan.Notes = append(plan.Notes, "Docker Container source, output, and customization growth are allocated only to the active Docker image-store role.")
	case "docker-sandboxes":
		rootBytes, err := AutomaticDockerSandboxesRootBytes(source.ExpandedBytes)
		if err != nil {
			return ArtifactStoragePlan{}, err
		}
		engineBytes, err := sumStorageBytes(source.CompressedBytes, source.ExpandedBytes, CustomizationAllowanceBytes)
		if err != nil {
			return ArtifactStoragePlan{}, err
		}
		projectBytes, err := sumStorageBytes(source.ExpandedBytes, CustomizationAllowanceBytes, projectWorkspaceAllowanceBytes)
		if err != nil {
			return ArtifactStoragePlan{}, err
		}
		cacheBytes, err := sumStorageBytes(source.ExpandedBytes, CustomizationAllowanceBytes)
		if err != nil {
			return ArtifactStoragePlan{}, err
		}
		appendPhase("build-export",
			storage.Allocation{ID: "docker-image-store-build", Role: storage.StorageRoleContainerdStore, Bytes: engineBytes},
			storage.Allocation{ID: "project-build-export", Role: storage.StorageRoleProject, Bytes: projectBytes},
		)
		appendPhase("import",
			storage.Allocation{ID: "docker-image-store-import", Role: storage.StorageRoleContainerdStore, Bytes: engineBytes},
			storage.Allocation{ID: "project-import", Role: storage.StorageRoleProject, Bytes: projectBytes},
			storage.Allocation{ID: "sandbox-template-cache-import", Role: storage.StorageRoleSandboxTemplateCache, Bytes: cacheBytes},
		)
		plan.LogicalRootMaximumBytes = rootBytes
		plan.LogicalDockerMaximumBytes = dockerDiskBytes
		plan.LogicalLimitsSparse = true
		plan.Notes = append(plan.Notes, "Build/export and import preserve their actual overlap across the active Docker image store, project workspace, and Sandbox template-cache roles; no staging role is allocated.", "The root and inner-Docker sizes are independent sparse logical limits and are not added to immediate host growth.")
	case "wsl":
		engineBytes, err := sumStorageBytes(source.CompressedBytes, source.ExpandedBytes, CustomizationAllowanceBytes)
		if err != nil {
			return ArtifactStoragePlan{}, err
		}
		projectBytes, err := sumStorageBytes(source.ExpandedBytes, CustomizationAllowanceBytes, projectWorkspaceAllowanceBytes)
		if err != nil {
			return ArtifactStoragePlan{}, err
		}
		distributionBytes, err := sumStorageBytes(source.ExpandedBytes, CustomizationAllowanceBytes)
		if err != nil {
			return ArtifactStoragePlan{}, err
		}
		appendPhase("build-export",
			storage.Allocation{ID: "docker-image-store-build", Role: storage.StorageRoleContainerdStore, Bytes: engineBytes},
			storage.Allocation{ID: "project-rootfs", Role: storage.StorageRoleProject, Bytes: projectBytes},
			storage.Allocation{ID: "wsl-distribution-build", Role: storage.StorageRoleWSLDistribution, Bytes: distributionBytes},
		)
		plan.Notes = append(plan.Notes, "WSL Docker image-store, project rootfs, and temporary distribution allocations overlap during build/export.")
	case "tart":
		tartBytes, err := sumStorageBytes(source.ExpandedBytes, CustomizationAllowanceBytes)
		if err != nil {
			return ArtifactStoragePlan{}, err
		}
		appendPhase("build-clone", storage.Allocation{ID: "tart-store-build-clone", Role: storage.StorageRoleTartStore, Bytes: tartBytes})
		plan.Notes = append(plan.Notes, "Tart image clone and customization growth are allocated to the Tart store role.")
	default:
		return ArtifactStoragePlan{}, fmt.Errorf("provider %q does not use the shared Docker-image storage plan", providerType)
	}
	peak, err := operationPlanAggregatePeak(plan.OperationPlan)
	if err != nil {
		return ArtifactStoragePlan{}, err
	}
	plan.EstimatedIncrementalPeak = peak
	return plan, nil
}

func sourceUpdateOperationPlan() storage.OperationPlan {
	return storage.OperationPlan{
		ID:       "source-update",
		Provider: "shared",
		Phases: []storage.OperationPhase{{
			ID: "source-update",
			Allocations: []storage.Allocation{{
				ID: "project-source-update", Role: storage.StorageRoleProject, Bytes: sourceUpdateExpansionBytes,
			}},
		}},
	}
}

func PlanDockerSandboxesImportStorage(source SourceSizeEstimate, archiveBytes uint64) (ArtifactStoragePlan, error) {
	plannedCacheBytes, err := sumStorageBytes(source.ExpandedBytes, CustomizationAllowanceBytes)
	if err != nil {
		return ArtifactStoragePlan{}, err
	}
	archiveDerivedBytes, err := sumStorageBytes(archiveBytes, CustomizationAllowanceBytes)
	if err != nil {
		return ArtifactStoragePlan{}, errors.New("Docker Sandboxes verified archive import estimate overflows uint64")
	}
	cacheBytes := plannedCacheBytes
	if archiveDerivedBytes > cacheBytes {
		cacheBytes = archiveDerivedBytes
	}
	operationPlan := storage.OperationPlan{
		ID:       "template-import",
		Provider: "docker-sandboxes",
		Phases: []storage.OperationPhase{{
			ID: "import-only",
			Allocations: []storage.Allocation{{
				ID: "sandbox-template-cache-import-only", Role: storage.StorageRoleSandboxTemplateCache, Bytes: cacheBytes,
			}},
		}},
	}
	return ArtifactStoragePlan{
		Provider:                 "docker-sandboxes",
		CompressedDownloadBytes:  source.CompressedBytes,
		ExpandedSourceBytes:      source.ExpandedBytes,
		CustomizationBytes:       CustomizationAllowanceBytes,
		OperationPlan:            operationPlan,
		EstimatedIncrementalPeak: cacheBytes,
		Confidence:               source.Confidence,
		Notes:                    []string{"Import-only preflight reserves the larger of the planned Sandbox cache allocation and verified archive-derived cache estimate."},
	}, nil
}

func sumStorageBytes(values ...uint64) (uint64, error) {
	var total uint64
	for _, value := range values {
		if total > math.MaxUint64-value {
			return 0, errors.New("artifact storage estimate overflows uint64")
		}
		total += value
	}
	return total, nil
}

func operationPlanAggregatePeak(operationPlan storage.OperationPlan) (uint64, error) {
	var peak uint64
	for _, phase := range operationPlan.Phases {
		var phaseTotal uint64
		for _, allocation := range phase.Allocations {
			if phaseTotal > math.MaxUint64-allocation.Bytes {
				return 0, errors.New("artifact storage estimate overflows uint64")
			}
			phaseTotal += allocation.Bytes
		}
		if phaseTotal > peak {
			peak = phaseTotal
		}
	}
	return peak, nil
}

func (m *Coordinator) configuredArtifactStoragePlan(ctx context.Context, cached bool) (ArtifactStoragePlan, error) {
	if m.Config.Provider.Type == "tart" {
		return PlanArtifactStorage(m.Config.Provider.Type, SourceSizeEstimate{Confidence: EstimateDerived}, cached, 0)
	}
	if m.Config.Provider.Type == "wsl" && m.Config.Image.SourceType == config.ImageSourceRootFSTar {
		sourcePath := config.ProjectPath(m.ProjectRoot, m.Config.Image.SourceImage)
		info, err := os.Stat(filepath.Clean(sourcePath))
		if err != nil {
			return ArtifactStoragePlan{}, fmt.Errorf("measure WSL rootfs source %s: %w", sourcePath, err)
		}
		if info.Size() < 0 {
			return ArtifactStoragePlan{}, fmt.Errorf("measure WSL rootfs source %s: negative size", sourcePath)
		}
		estimate, err := EstimateSourceSize(uint64(info.Size()), uint64(info.Size()))
		if err != nil {
			return ArtifactStoragePlan{}, err
		}
		return PlanArtifactStorage(m.Config.Provider.Type, estimate, cached, 0)
	}
	source, err := m.resolveDockerSandboxesSource(ctx)
	if err != nil {
		return ArtifactStoragePlan{}, err
	}
	var exactExpanded uint64
	if output, inspectErr := m.runHostOutput(ctx, "docker", "image", "inspect", "--format", "{{.Size}}", source.Reference); inspectErr == nil {
		exactExpanded, _ = strconv.ParseUint(strings.TrimSpace(output), 10, 64)
	}
	estimate, err := EstimateSourceSize(source.CompressedLayerBytes, exactExpanded)
	if err != nil {
		return ArtifactStoragePlan{}, err
	}
	dockerDiskBytes := uint64(0)
	if m.Config.Provider.Type == "docker-sandboxes" {
		parsed, err := config.ParseByteSize(m.Config.DockerSandboxes.DockerDisk)
		if err != nil {
			return ArtifactStoragePlan{}, err
		}
		dockerDiskBytes = uint64(parsed)
	}
	return PlanArtifactStorage(m.Config.Provider.Type, estimate, cached, dockerDiskBytes)
}
