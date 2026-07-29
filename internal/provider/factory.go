package provider

import "fmt"

// Descriptor is the provider-neutral registration record consumed by
// onboarding, configuration, lifecycle, image, and contract tests. A provider
// must not appear in SupportedTypes unless all required contributions exist.
type Descriptor struct {
	Type                   string
	DisplayName            string
	WizardSupported        bool
	WizardNumber           string
	WizardLabel            string
	WizardAliases          []string
	ConfigurationDecoder   bool
	ConfigurationDefaults  bool
	ConfigurationValidator bool
	LifecycleSupported     bool
	StorageSupported       bool
	ImageMode              string
	GuidedArtifacts        bool
	WizardImageProfiles    []WizardImageProfile
}

type WizardImageProfile struct {
	Name string
	Tag  string
}

const (
	ImageModeDocker   = "docker-image"
	ImageModeNative   = "native-image"
	ImageModeTemplate = "sandbox-template"
)

func UnsupportedTypeError(providerType string) error {
	return fmt.Errorf("unsupported provider.type %q", providerType)
}
