package registry

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
	"github.com/solutionforest/ephemeral-action-runner/internal/provider/dockercontainer"
	"github.com/solutionforest/ephemeral-action-runner/internal/provider/dockersandboxes"
	"github.com/solutionforest/ephemeral-action-runner/internal/provider/storagepath"
	"github.com/solutionforest/ephemeral-action-runner/internal/provider/tart"
	"github.com/solutionforest/ephemeral-action-runner/internal/provider/wsl"
	"github.com/solutionforest/ephemeral-action-runner/internal/storage"
)

// Runtime is the complete provider wiring needed by the pool manager.
// Legacy is populated only for providers that still use the compatibility
// adapter.
type Runtime struct {
	Legacy        provider.Provider
	Lifecycle     provider.Lifecycle
	PolicyManager provider.PolicyManager
	Storage       provider.StorageContribution
}

type factory func(cfg config.Config, projectRoot string, dryRun bool) Runtime

type entry struct {
	descriptor provider.Descriptor
	factory    factory
}

var entries = []entry{
	{
		descriptor: provider.Descriptor{
			Type:                   "docker-sandboxes",
			DisplayName:            "Docker Sandboxes",
			WizardSupported:        true,
			WizardTier:             provider.WizardTierPrimary,
			WizardNumber:           "1",
			WizardLabel:            "Docker Sandboxes — recommended",
			WizardAliases:          []string{"docker-sandboxes", "sandboxes"},
			ConfigurationDecoder:   true,
			ConfigurationDefaults:  true,
			ConfigurationValidator: true,
			LifecycleSupported:     true,
			StorageSupported:       true,
			ImageMode:              provider.ImageModeTemplate,
			GuidedArtifacts:        true,
			WizardImageProfiles:    dockerSandboxesProfiles(),
			WizardPrerequisite:     provider.WizardPrerequisiteDockerSandboxes,
			WizardOnboarding:       provider.WizardOnboardingCatthehackerDocker,
			WizardHostTrust:        provider.WizardHostTrustOverlay,
			WizardReview:           provider.WizardReviewDockerImage,
		},
		factory: func(cfg config.Config, projectRoot string, dryRun bool) Runtime {
			sandboxes := dockersandboxes.NewWithArchitectureMode("", dryRun, cfg.DockerSandboxes.ArchitectureEmulation, cfg.Provider.Platform)
			sandboxes.ConfigureHostTrustRelay(runtime.GOOS == "windows" && cfg.Image.HostTrustMode == config.HostTrustModeOverlay, filepath.Clean(projectRoot)+"\x00"+cfg.Pool.NamePrefix)
			return Runtime{Lifecycle: sandboxes, PolicyManager: sandboxes, Storage: providerStorage(cfg, projectRoot)}
		},
	},
	{
		descriptor: provider.Descriptor{Type: "docker-container", DisplayName: "Docker Container", WizardSupported: true, WizardTier: provider.WizardTierCompatibility, WizardNumber: "2", WizardLabel: "Docker Container — private daemon", WizardAliases: []string{"docker", "docker-container"}, ConfigurationDecoder: true, ConfigurationDefaults: true, ConfigurationValidator: true, LifecycleSupported: true, StorageSupported: true, ImageMode: provider.ImageModeDocker, GuidedArtifacts: true, WizardImageProfiles: catthehackerProfiles(), WizardCustomImageTags: true, WizardPrerequisite: provider.WizardPrerequisiteDocker, WizardOnboarding: provider.WizardOnboardingCatthehackerDocker, WizardHostTrust: provider.WizardHostTrustOverlay, WizardReview: provider.WizardReviewDockerImage},
		factory: func(cfg config.Config, projectRoot string, dryRun bool) Runtime {
			hostGateway := config.DockerConfigNeedsHostGateway(cfg.Docker)
			environment := map[string]string{
				"HTTP_PROXY":  cfg.Docker.HTTPProxy,
				"HTTPS_PROXY": cfg.Docker.HTTPSProxy,
				"NO_PROXY":    cfg.Docker.NoProxy,
			}
			return adaptLegacy(dockercontainer.NewWithOptions("", cfg.Provider.Platform, hostGateway, environment, dryRun), providerStorage(cfg, projectRoot), dryRun)
		},
	},
	{
		descriptor: provider.Descriptor{Type: "wsl", DisplayName: "WSL2", WizardSupported: true, WizardTier: provider.WizardTierCompatibility, WizardNumber: "3", WizardLabel: "WSL2", WizardAliases: []string{"wsl", "wsl2"}, ConfigurationDecoder: true, ConfigurationDefaults: true, ConfigurationValidator: true, LifecycleSupported: true, StorageSupported: true, ImageMode: provider.ImageModeDocker, GuidedArtifacts: true, WizardImageProfiles: catthehackerProfiles(), WizardCustomImageTags: true, WizardPrerequisite: provider.WizardPrerequisiteWSL2, WizardOnboarding: provider.WizardOnboardingCatthehackerDocker, WizardHostTrust: provider.WizardHostTrustNone, WizardReview: provider.WizardReviewDockerImage},
		factory: func(cfg config.Config, projectRoot string, dryRun bool) Runtime {
			installRoot := config.ProjectPath(projectRoot, cfg.Provider.InstallRoot)
			return adaptLegacy(wsl.New("", installRoot, projectRoot, dryRun), providerStorage(cfg, projectRoot), dryRun)
		},
	},
	{
		descriptor: provider.Descriptor{Type: "tart", DisplayName: "Tart (retired)", WizardSupported: false, ConfigurationDecoder: true, ConfigurationDefaults: true, ConfigurationValidator: true, LifecycleSupported: true, StorageSupported: true, ImageMode: provider.ImageModeNative},
		factory: func(cfg config.Config, projectRoot string, dryRun bool) Runtime {
			return adaptLegacy(tart.New("", dryRun), providerStorage(cfg, projectRoot), dryRun)
		},
	},
}

func catthehackerProfiles() []provider.WizardImageProfile {
	return []provider.WizardImageProfile{
		{Name: "full", Tag: "full-latest"},
		{Name: "act", Tag: "act-latest"},
	}
}

func dockerSandboxesProfiles() []provider.WizardImageProfile {
	return []provider.WizardImageProfile{
		{Name: "full", Tag: "full-latest"},
		{Name: "act", Tag: "act-latest"},
	}
}

func Descriptors() []provider.Descriptor {
	result := make([]provider.Descriptor, 0, len(entries))
	for _, registered := range entries {
		descriptor := registered.descriptor
		descriptor.WizardAliases = append([]string(nil), descriptor.WizardAliases...)
		descriptor.WizardImageProfiles = append([]provider.WizardImageProfile(nil), descriptor.WizardImageProfiles...)
		result = append(result, descriptor)
	}
	return result
}

func DescriptorFor(providerType string) (provider.Descriptor, bool) {
	for _, registered := range entries {
		if registered.descriptor.Type == providerType {
			descriptor := registered.descriptor
			descriptor.WizardAliases = append([]string(nil), descriptor.WizardAliases...)
			descriptor.WizardImageProfiles = append([]provider.WizardImageProfile(nil), descriptor.WizardImageProfiles...)
			return descriptor, true
		}
	}
	return provider.Descriptor{}, false
}

func SupportedTypes() []string {
	result := make([]string, 0, len(entries))
	for _, registered := range entries {
		result = append(result, registered.descriptor.Type)
	}
	return result
}

// New is the single construction point for concrete providers.
func New(cfg config.Config, projectRoot string, dryRun bool) (Runtime, error) {
	var registered *entry
	for index := range entries {
		if entries[index].descriptor.Type == cfg.Provider.Type {
			registered = &entries[index]
			break
		}
	}
	if registered == nil {
		return Runtime{}, provider.UnsupportedTypeError(cfg.Provider.Type)
	}
	descriptor := registered.descriptor
	if !descriptor.ConfigurationDecoder || !descriptor.ConfigurationDefaults || !descriptor.ConfigurationValidator || !descriptor.LifecycleSupported || !descriptor.StorageSupported || descriptor.ImageMode == "" {
		return Runtime{}, fmt.Errorf("provider %q has an incomplete registry entry", cfg.Provider.Type)
	}
	if err := provider.ValidateWizardContributions(descriptor); err != nil {
		return Runtime{}, fmt.Errorf("provider %q has incomplete wizard contributions: %w", cfg.Provider.Type, err)
	}
	runtime := registered.factory(cfg, projectRoot, dryRun)
	if runtime.Lifecycle == nil || runtime.Storage == nil {
		return Runtime{}, fmt.Errorf("provider %q registry entry did not construct required lifecycle and storage behavior", cfg.Provider.Type)
	}
	if descriptor.ImageMode == provider.ImageModeTemplate {
		if _, ok := runtime.Lifecycle.(provider.TemplateArtifactRuntime); !ok {
			return Runtime{}, fmt.Errorf("template-backed provider %q did not construct required artifact runtime behavior", cfg.Provider.Type)
		}
	}
	return runtime, nil
}

func providerStorage(cfg config.Config, projectRoot string) provider.StorageContribution {
	roots := []provider.StorageRoot{{ID: cfg.Provider.Type + "-project", Role: storage.StorageRoleProject, Path: projectRoot}}
	discoveries := []provider.StorageRootDiscovery{}
	switch cfg.Provider.Type {
	case "docker-container":
		discoveries = append(discoveries, dockerStorageRoots)
	case "docker-sandboxes":
		roots = append(roots, provider.StorageRoot{ID: "docker-sandboxes-staging", Path: config.ProjectPath(projectRoot, cfg.DockerSandboxes.StagingRoot)})
		discoveries = append(discoveries, dockerStorageRoots, dockerSandboxesStorageRoots)
	case "wsl":
		roots = append(roots,
			provider.StorageRoot{ID: "wsl-install-root", Role: storage.StorageRoleWSLDistribution, Path: config.ProjectPath(projectRoot, cfg.Provider.InstallRoot)},
		)
		discoveries = append(discoveries, dockerStorageRoots)
	case "tart":
		discoveries = append(discoveries, tartStorageRoots)
	}
	return provider.NewFilesystemStorageWithDiscovery(cfg.Provider.Type, roots, discoveries...)
}

var discoverCurrentDockerStorage = storagepath.DiscoverCurrentDockerStorage
var currentStorageEnvironment = storagepath.CurrentEnvironment

func dockerStorageRoots(ctx context.Context, request provider.StorageRequest) ([]provider.StorageRoot, error) {
	if !storageRequestUsesRole(request, storage.StorageRoleDockerEngine, storage.StorageRoleContainerdStore) {
		return nil, nil
	}
	discovered, err := discoverCurrentDockerStorage(ctx)
	if err != nil {
		return nil, err
	}
	roots := make([]provider.StorageRoot, 0, len(discovered.Roots)+1)
	containerdFound := false
	var engineRoot *provider.StorageRoot
	for _, root := range discovered.Roots {
		role := storage.StorageRoleDockerEngine
		id := "docker-engine-backing"
		if root.ID == "containerd" {
			containerdFound = true
			role = storage.StorageRoleContainerdStore
			id = "containerd-store-backing"
		}
		mapped := provider.StorageRoot{
			ID:           id,
			Role:         role,
			Kind:         storage.SurfaceDockerEngine,
			Path:         root.Path,
			CapacityPath: root.CapacityPath,
			Provenance:   string(root.Provenance),
			Confidence:   string(root.Confidence),
			Warnings:     append([]string(nil), root.Warnings...),
		}
		roots = append(roots, mapped)
		if root.ID == "engine" {
			copy := mapped
			engineRoot = &copy
		}
	}
	if !containerdFound && engineRoot != nil {
		imageStore := *engineRoot
		imageStore.ID = "containerd-store-backing"
		imageStore.Role = storage.StorageRoleContainerdStore
		imageStore.Provenance += "-image-store-alias"
		roots = append(roots, imageStore)
	}
	return roots, nil
}

func dockerSandboxesStorageRoots(_ context.Context, request provider.StorageRequest) ([]provider.StorageRoot, error) {
	if !storageRequestUsesRole(request, storage.StorageRoleSandboxRuntime, storage.StorageRoleSandboxTemplateCache) {
		return nil, nil
	}
	environment, err := currentStorageEnvironment()
	if err != nil {
		return nil, err
	}
	discovered, err := storagepath.DockerSandboxesRoots(environment)
	if err != nil {
		return nil, err
	}
	var roots []provider.StorageRoot
	for _, root := range discovered {
		base := provider.StorageRoot{
			Kind:       storage.SurfaceSandboxCache,
			Path:       root.Path,
			Provenance: string(root.Provenance),
			Confidence: string(root.Confidence),
		}
		switch root.ID {
		case "state":
			runtimeRoot := base
			runtimeRoot.ID = "docker-sandboxes-runtime"
			runtimeRoot.Role = storage.StorageRoleSandboxRuntime
			roots = append(roots, runtimeRoot)
			if environment.GOOS != "linux" {
				templateRoot := base
				templateRoot.ID = "docker-sandboxes-template-cache"
				templateRoot.Role = storage.StorageRoleSandboxTemplateCache
				roots = append(roots, templateRoot)
			}
		case "cache":
			base.ID = "docker-sandboxes-template-cache"
			base.Role = storage.StorageRoleSandboxTemplateCache
			roots = append(roots, base)
		case "config":
			base.ID = "docker-sandboxes-config"
			base.ReportOnly = true
			roots = append(roots, base)
		}
	}
	return roots, nil
}

func tartStorageRoots(_ context.Context, request provider.StorageRequest) ([]provider.StorageRoot, error) {
	if !storageRequestUsesRole(request, storage.StorageRoleTartStore) {
		return nil, nil
	}
	environment, err := currentStorageEnvironment()
	if err != nil {
		return nil, err
	}
	root, err := storagepath.TartRoot(environment)
	if err != nil {
		return nil, err
	}
	return []provider.StorageRoot{{
		ID:         "tart-vm-store",
		Role:       storage.StorageRoleTartStore,
		Path:       root.Path,
		Provenance: string(root.Provenance),
		Confidence: string(root.Confidence),
	}}, nil
}

func storageRequestUsesRole(request provider.StorageRequest, roles ...storage.StorageRole) bool {
	if len(request.OperationPlan.Phases) == 0 {
		return true
	}
	wanted := make(map[storage.StorageRole]struct{}, len(roles))
	for _, role := range roles {
		wanted[role] = struct{}{}
	}
	for _, phase := range request.OperationPlan.Phases {
		for _, allocation := range phase.Allocations {
			if allocation.SurfaceID != "" {
				return true
			}
			if _, found := wanted[allocation.Role]; found {
				return true
			}
		}
	}
	return false
}

func adaptLegacy(legacy provider.Provider, storageContribution provider.StorageContribution, dryRun bool) Runtime {
	return Runtime{Legacy: legacy, Lifecycle: provider.AdaptLegacy(legacy, dryRun), Storage: storageContribution}
}
