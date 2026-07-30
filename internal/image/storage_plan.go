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
	Provider                  string             `json:"provider"`
	CompressedDownloadBytes   uint64             `json:"compressedDownloadBytes"`
	ExpandedSourceBytes       uint64             `json:"expandedSourceBytes"`
	CustomizationBytes        uint64             `json:"customizationBytes"`
	EstimatedIncrementalPeak  uint64             `json:"estimatedIncrementalPeak"`
	Confidence                EstimateConfidence `json:"confidence"`
	LogicalRootMaximumBytes   uint64             `json:"logicalRootMaximumBytes,omitempty"`
	LogicalDockerMaximumBytes uint64             `json:"logicalDockerMaximumBytes,omitempty"`
	LogicalLimitsSparse       bool               `json:"logicalLimitsSparse,omitempty"`
	Notes                     []string           `json:"notes,omitempty"`
}

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
	}
	if cached {
		plan.EstimatedIncrementalPeak = 0
		plan.Notes = append(plan.Notes, "A verified current artifact can be reused.")
		return plan, nil
	}
	add := func(value uint64) error {
		if plan.EstimatedIncrementalPeak > math.MaxUint64-value {
			return errors.New("artifact storage estimate overflows uint64")
		}
		plan.EstimatedIncrementalPeak += value
		return nil
	}
	switch providerType {
	case "docker-container":
		if err := add(source.CompressedBytes); err != nil {
			return ArtifactStoragePlan{}, err
		}
		if err := add(source.ExpandedBytes); err != nil {
			return ArtifactStoragePlan{}, err
		}
		if err := add(CustomizationAllowanceBytes); err != nil {
			return ArtifactStoragePlan{}, err
		}
		plan.Notes = append(plan.Notes, "Physical estimate covers Docker Engine source/output growth and customization.")
	case "docker-sandboxes":
		rootBytes, err := AutomaticDockerSandboxesRootBytes(source.ExpandedBytes)
		if err != nil {
			return ArtifactStoragePlan{}, err
		}
		for _, value := range []uint64{source.CompressedBytes, source.ExpandedBytes, source.ExpandedBytes, CustomizationAllowanceBytes} {
			if err := add(value); err != nil {
				return ArtifactStoragePlan{}, err
			}
		}
		plan.LogicalRootMaximumBytes = rootBytes
		plan.LogicalDockerMaximumBytes = dockerDiskBytes
		plan.LogicalLimitsSparse = true
		plan.Notes = append(plan.Notes, "Physical estimate covers dedicated BuildKit state, one directly exported archive, Sandbox template-cache import, and customization; no Docker Engine output image is created.", "The root and inner-Docker sizes are independent sparse logical limits and are not added to immediate host growth.")
	case "wsl":
		for _, value := range []uint64{source.CompressedBytes, source.ExpandedBytes, source.ExpandedBytes, CustomizationAllowanceBytes} {
			if err := add(value); err != nil {
				return ArtifactStoragePlan{}, err
			}
		}
		plan.Notes = append(plan.Notes, "Physical estimate covers Docker Engine build data, rootfs export, temporary build distribution, and customization.")
	default:
		return ArtifactStoragePlan{}, fmt.Errorf("provider %q does not use the shared Docker-image storage plan", providerType)
	}
	return plan, nil
}

func (m *Coordinator) configuredArtifactStoragePlan(ctx context.Context, cached bool) (ArtifactStoragePlan, error) {
	if m.Config.Provider.Type == "tart" {
		return ArtifactStoragePlan{
			Provider:                 m.Config.Provider.Type,
			CustomizationBytes:       CustomizationAllowanceBytes,
			EstimatedIncrementalPeak: CustomizationAllowanceBytes,
			Confidence:               EstimateDerived,
			Notes:                    []string{"Tart does not use the shared Docker-image plan; only the customization allowance is admitted here."},
		}, nil
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
