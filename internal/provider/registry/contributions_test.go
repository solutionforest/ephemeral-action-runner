package registry

import (
	"testing"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
)

func TestEveryProviderRegistersRequiredContributions(t *testing.T) {
	seen := make(map[string]struct{})
	for _, descriptor := range Descriptors() {
		if descriptor.Type == "" {
			t.Fatal("provider descriptor type is empty")
		}
		if _, exists := seen[descriptor.Type]; exists {
			t.Fatalf("duplicate provider descriptor %q", descriptor.Type)
		}
		seen[descriptor.Type] = struct{}{}
		if err := provider.ValidateWizardContributions(descriptor); err != nil {
			t.Errorf("%s has incomplete wizard contributions: %v", descriptor.Type, err)
		}
		if descriptor.WizardSupported && (descriptor.WizardNumber == "" || descriptor.WizardLabel == "" || len(descriptor.WizardAliases) == 0) {
			t.Errorf("%s has an incomplete ./start wizard contribution", descriptor.Type)
		}
		if !descriptor.ConfigurationDecoder || !descriptor.ConfigurationDefaults || !descriptor.ConfigurationValidator {
			t.Errorf("%s has an incomplete configuration contribution", descriptor.Type)
		}
		if !descriptor.LifecycleSupported {
			t.Errorf("%s has no shared lifecycle contribution", descriptor.Type)
		}
		if !descriptor.StorageSupported {
			t.Errorf("%s has no storage contribution", descriptor.Type)
		}
		switch descriptor.ImageMode {
		case provider.ImageModeDocker, provider.ImageModeNative, provider.ImageModeTemplate:
		default:
			t.Errorf("%s has unsupported image mode %q", descriptor.Type, descriptor.ImageMode)
		}
		if descriptor.WizardOnboarding == provider.WizardOnboardingCatthehackerDocker {
			if !descriptor.GuidedArtifacts || len(descriptor.WizardImageProfiles) == 0 {
				t.Errorf("%s has no guided Docker-image provisioning contribution", descriptor.Type)
			}
			if descriptor.WizardImageProfiles[0].Name != "full" || descriptor.WizardImageProfiles[0].Tag != "full-latest" {
				t.Errorf("%s does not register its default image profile first", descriptor.Type)
			}
		}
	}
	if len(seen) != len(SupportedTypes()) {
		t.Fatalf("descriptors=%d supported types=%d", len(seen), len(SupportedTypes()))
	}
}

func TestProviderOnboardingTiersAndRuntimeOnlyTart(t *testing.T) {
	want := map[string]struct {
		supported bool
		tier      provider.WizardTier
		number    string
	}{
		"docker-sandboxes": {supported: true, tier: provider.WizardTierPrimary, number: "1"},
		"docker-container": {supported: true, tier: provider.WizardTierCompatibility, number: "2"},
		"wsl":              {supported: true, tier: provider.WizardTierCompatibility, number: "3"},
		"tart":             {supported: false},
	}
	for _, descriptor := range Descriptors() {
		expected := want[descriptor.Type]
		if descriptor.WizardSupported != expected.supported || descriptor.WizardTier != expected.tier || descriptor.WizardNumber != expected.number {
			t.Errorf("%s wizard contribution = supported %t tier %q number %q, want supported %t tier %q number %q", descriptor.Type, descriptor.WizardSupported, descriptor.WizardTier, descriptor.WizardNumber, expected.supported, expected.tier, expected.number)
		}
	}
}

func TestDockerSandboxesRegistersVerifiedPrebuiltFullAndActSources(t *testing.T) {
	descriptor, found := DescriptorFor("docker-sandboxes")
	if !found {
		t.Fatal("docker-sandboxes descriptor not found")
	}
	if len(descriptor.WizardArtifactSources) != 2 {
		t.Fatalf("WizardArtifactSources = %d, want 2", len(descriptor.WizardArtifactSources))
	}
	full, act := descriptor.WizardArtifactSources[0], descriptor.WizardArtifactSources[1]
	if full.Distribution != "prebuilt" || full.Profile != "full" || full.Reference != config.DockerSandboxesPrebuiltFullReference || !full.Default || full.Preview {
		t.Fatalf("Full artifact source = %#v, want stable default prebuilt Full", full)
	}
	if act.Distribution != "prebuilt" || act.Profile != "act" || act.Reference != config.DockerSandboxesPrebuiltActReference || act.Default || act.Preview {
		t.Fatalf("Act artifact source = %#v, want stable non-default prebuilt Act", act)
	}
	for _, source := range descriptor.WizardArtifactSources {
		if source.Label == "" || source.Description == "" {
			t.Fatalf("artifact source is missing user-facing label/description: %#v", source)
		}
	}
}

func TestCompatibilityProvidersDoNotAdvertisePrebuiltSources(t *testing.T) {
	for _, providerType := range []string{"docker-container", "wsl"} {
		descriptor, found := DescriptorFor(providerType)
		if !found {
			t.Fatalf("%s descriptor not found", providerType)
		}
		if len(descriptor.WizardArtifactSources) != 0 {
			t.Fatalf("%s advertises prebuilt sources: %#v", providerType, descriptor.WizardArtifactSources)
		}
	}
}
