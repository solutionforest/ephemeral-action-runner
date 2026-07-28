package registry

import (
	"testing"

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
		if !descriptor.WizardSupported {
			t.Errorf("%s has no ./start wizard contribution", descriptor.Type)
		}
		if descriptor.WizardNumber == "" || descriptor.WizardLabel == "" || len(descriptor.WizardAliases) == 0 {
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
	}
	if len(seen) != len(SupportedTypes()) {
		t.Fatalf("descriptors=%d supported types=%d", len(seen), len(SupportedTypes()))
	}
}
