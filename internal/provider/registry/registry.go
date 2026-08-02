package registry

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
	"github.com/solutionforest/ephemeral-action-runner/internal/provider/dockercontainer"
	"github.com/solutionforest/ephemeral-action-runner/internal/provider/dockersandboxes"
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
		descriptor: provider.Descriptor{Type: "docker-container", DisplayName: "Docker Container", WizardSupported: true, WizardNumber: "1", WizardLabel: "Docker Container — private daemon", WizardAliases: []string{"docker", "docker-container"}, ConfigurationDecoder: true, ConfigurationDefaults: true, ConfigurationValidator: true, LifecycleSupported: true, StorageSupported: true, ImageMode: provider.ImageModeDocker, GuidedArtifacts: true, WizardImageProfiles: catthehackerProfiles(), WizardCustomImageTags: true, WizardPrerequisite: provider.WizardPrerequisiteDocker, WizardOnboarding: provider.WizardOnboardingCatthehackerDocker, WizardHostTrust: provider.WizardHostTrustOverlay, WizardReview: provider.WizardReviewDockerImage},
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
		descriptor: provider.Descriptor{
			Type:                   "docker-sandboxes",
			DisplayName:            "Docker Sandboxes",
			WizardSupported:        true,
			WizardNumber:           "2",
			WizardLabel:            "Docker Sandboxes — recommended when ready",
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
			sandboxes := dockersandboxes.NewWithDryRun("", dryRun)
			return Runtime{Lifecycle: sandboxes, PolicyManager: sandboxes, Storage: providerStorage(cfg, projectRoot)}
		},
	},
	{
		descriptor: provider.Descriptor{Type: "wsl", DisplayName: "WSL2", WizardSupported: true, WizardNumber: "3", WizardLabel: "WSL2", WizardAliases: []string{"wsl", "wsl2"}, ConfigurationDecoder: true, ConfigurationDefaults: true, ConfigurationValidator: true, LifecycleSupported: true, StorageSupported: true, ImageMode: provider.ImageModeDocker, GuidedArtifacts: true, WizardImageProfiles: catthehackerProfiles(), WizardCustomImageTags: true, WizardPrerequisite: provider.WizardPrerequisiteWSL2, WizardOnboarding: provider.WizardOnboardingCatthehackerDocker, WizardHostTrust: provider.WizardHostTrustNone, WizardReview: provider.WizardReviewDockerImage},
		factory: func(cfg config.Config, projectRoot string, dryRun bool) Runtime {
			installRoot := config.ProjectPath(projectRoot, cfg.Provider.InstallRoot)
			return adaptLegacy(wsl.New("", installRoot, projectRoot, dryRun), providerStorage(cfg, projectRoot), dryRun)
		},
	},
	{
		descriptor: provider.Descriptor{Type: "tart", DisplayName: "Tart (experimental)", WizardSupported: true, WizardNumber: "4", WizardLabel: "Tart (experimental)", WizardAliases: []string{"tart"}, ConfigurationDecoder: true, ConfigurationDefaults: true, ConfigurationValidator: true, LifecycleSupported: true, StorageSupported: true, ImageMode: provider.ImageModeNative, WizardPrerequisite: provider.WizardPrerequisiteTart, WizardOnboarding: provider.WizardOnboardingNone, WizardHostTrust: provider.WizardHostTrustNone, WizardReview: provider.WizardReviewNativeImage, WizardReviewSource: "ghcr.io/cirruslabs/ubuntu:latest", WizardReviewOutput: "epar-ubuntu-24-arm64"},
		factory: func(cfg config.Config, projectRoot string, dryRun bool) Runtime {
			return adaptLegacy(tart.New("", dryRun), providerStorage(cfg, projectRoot), dryRun)
		},
	},
}

func catthehackerProfiles() []provider.WizardImageProfile {
	return []provider.WizardImageProfile{
		{Name: "full", Tag: "full-latest"},
		{Name: "act", Tag: "act-latest"},
		{Name: "dotnet", Tag: "dotnet-latest"},
		{Name: "js", Tag: "js-latest"},
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
	roots := []provider.StorageRoot{{ID: cfg.Provider.Type + "-project", Location: projectRoot}}
	minimumExpansions := map[string]uint64{}
	switch cfg.Provider.Type {
	case "docker-container":
		roots = append(roots, provider.StorageRoot{ID: "docker-engine-backing", Kind: storage.SurfaceDockerEngine, Location: dockerBackingRoot()})
	case "docker-sandboxes":
		roots = append(roots,
			provider.StorageRoot{
				ID:       "docker-engine-backing",
				Kind:     storage.SurfaceDockerEngine,
				Location: dockerBackingRoot(),
				MinimumExpansions: map[string]uint64{
					"image-pull":     0,
					"image-build":    0,
					"source-update":  0,
					"template-build": 0,
				},
			},
			provider.StorageRoot{
				ID:                "docker-sandboxes-backing",
				Kind:              storage.SurfaceSandboxCache,
				Location:          dockerSandboxesBackingRoot(),
				MinimumExpansions: map[string]uint64{"instance-create": 0, "template-build": 0},
			},
			provider.StorageRoot{ID: "docker-sandboxes-staging", Location: config.ProjectPath(projectRoot, cfg.DockerSandboxes.StagingRoot)},
		)
	case "wsl":
		roots = append(roots,
			provider.StorageRoot{ID: "wsl-install-root", Location: config.ProjectPath(projectRoot, cfg.Provider.InstallRoot)},
			provider.StorageRoot{ID: "docker-engine-backing", Kind: storage.SurfaceDockerEngine, Location: dockerBackingRoot()},
		)
	case "tart":
		roots = append(roots, provider.StorageRoot{ID: "tart-vm-store", Location: tartBackingRoot()})
	}
	return provider.NewMultiFilesystemStorageWithMinimumExpansions(cfg.Provider.Type, roots, minimumExpansions)
}

func dockerBackingRoot() string {
	switch runtime.GOOS {
	case "windows":
		return filepath.Join(os.Getenv("LOCALAPPDATA"), "Docker", "wsl", "disk")
	case "darwin":
		home, _ := os.UserHomeDir()
		return filepath.Join(home, "Library", "Containers", "com.docker.docker", "Data", "vms", "0", "data")
	default:
		return "/var/lib/docker"
	}
}

func dockerSandboxesBackingRoot() string {
	switch runtime.GOOS {
	case "windows":
		return filepath.Join(os.Getenv("LOCALAPPDATA"), "DockerSandboxes", "sandboxes", "data")
	case "darwin":
		home, _ := os.UserHomeDir()
		return filepath.Join(home, "Library", "Containers", "com.docker.docker", "Data", "docker-sandboxes")
	default:
		return "/var/lib/docker-sandboxes"
	}
}

func tartBackingRoot() string {
	if root := os.Getenv("TART_HOME"); root != "" {
		return filepath.Join(root, "vms")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".tart", "vms")
}

func adaptLegacy(legacy provider.Provider, storageContribution provider.StorageContribution, dryRun bool) Runtime {
	return Runtime{Legacy: legacy, Lifecycle: provider.AdaptLegacy(legacy, dryRun), Storage: storageContribution}
}
