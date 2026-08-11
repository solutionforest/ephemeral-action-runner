package provider

import "fmt"

// Descriptor is the provider-neutral registration record consumed by
// onboarding, configuration, lifecycle, image, and contract tests. A provider
// must not appear in SupportedTypes unless all required contributions exist.
type Descriptor struct {
	Type                   string
	DisplayName            string
	WizardSupported        bool
	WizardTier             WizardTier
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
	WizardArtifactSources  []WizardArtifactSource
	WizardCustomImageTags  bool
	WizardPrerequisite     WizardPrerequisiteKind
	WizardOnboarding       WizardOnboardingKind
	WizardHostTrust        WizardHostTrustKind
	WizardReview           WizardReviewKind
}

type WizardImageProfile struct {
	Name        string
	Tag         string
	Label       string
	Description string
}

// WizardArtifactSource describes a reusable runner artifact offered during
// onboarding. A provider with artifact sources must identify exactly one
// default across those sources; local image profiles follow them in the menu.
type WizardArtifactSource struct {
	Distribution string
	Profile      string
	Reference    string
	Label        string
	Description  string
	Preview      bool
	Default      bool
}

const (
	ImageModeDocker   = "docker-image"
	ImageModeNative   = "native-image"
	ImageModeTemplate = "sandbox-template"
)

type WizardPrerequisiteKind string

const (
	WizardPrerequisiteDocker          WizardPrerequisiteKind = "docker"
	WizardPrerequisiteDockerSandboxes WizardPrerequisiteKind = "docker-sandboxes"
	WizardPrerequisiteWSL2            WizardPrerequisiteKind = "wsl2"
)

func (kind WizardPrerequisiteKind) Valid() bool {
	switch kind {
	case WizardPrerequisiteDocker, WizardPrerequisiteDockerSandboxes, WizardPrerequisiteWSL2:
		return true
	default:
		return false
	}
}

type WizardOnboardingKind string

const (
	WizardOnboardingCatthehackerDocker WizardOnboardingKind = "catthehacker-docker"
)

func (kind WizardOnboardingKind) Valid() bool {
	switch kind {
	case WizardOnboardingCatthehackerDocker:
		return true
	default:
		return false
	}
}

type WizardHostTrustKind string

const (
	WizardHostTrustNone    WizardHostTrustKind = "none"
	WizardHostTrustOverlay WizardHostTrustKind = "overlay"
)

func (kind WizardHostTrustKind) Valid() bool {
	switch kind {
	case WizardHostTrustNone, WizardHostTrustOverlay:
		return true
	default:
		return false
	}
}

type WizardReviewKind string

const (
	WizardReviewDockerImage WizardReviewKind = "docker-image"
)

func (kind WizardReviewKind) Valid() bool {
	switch kind {
	case WizardReviewDockerImage:
		return true
	default:
		return false
	}
}

type WizardTier string

const (
	WizardTierPrimary       WizardTier = "primary"
	WizardTierCompatibility WizardTier = "compatibility"
)

func (tier WizardTier) Valid() bool {
	switch tier {
	case WizardTierPrimary, WizardTierCompatibility:
		return true
	default:
		return false
	}
}

// ValidateWizardContributions rejects incomplete or incompatible onboarding
// metadata before a provider can enter the first-run wizard.
func ValidateWizardContributions(descriptor Descriptor) error {
	if !descriptor.WizardSupported {
		return nil
	}
	if !descriptor.WizardTier.Valid() {
		return fmt.Errorf("unknown onboarding tier %q", descriptor.WizardTier)
	}
	if descriptor.WizardNumber == "" || descriptor.WizardLabel == "" || len(descriptor.WizardAliases) == 0 {
		return fmt.Errorf("wizard number, label, and aliases are required")
	}
	if !descriptor.WizardPrerequisite.Valid() {
		return fmt.Errorf("unknown prerequisite strategy %q", descriptor.WizardPrerequisite)
	}
	if !descriptor.WizardOnboarding.Valid() {
		return fmt.Errorf("unknown onboarding strategy %q", descriptor.WizardOnboarding)
	}
	if !descriptor.WizardHostTrust.Valid() {
		return fmt.Errorf("unknown host-trust contribution %q", descriptor.WizardHostTrust)
	}
	if !descriptor.WizardReview.Valid() {
		return fmt.Errorf("unknown review contribution %q", descriptor.WizardReview)
	}

	switch descriptor.WizardOnboarding {
	case WizardOnboardingCatthehackerDocker:
		if descriptor.WizardReview != WizardReviewDockerImage {
			return fmt.Errorf("Catthehacker onboarding requires the Docker-image review contribution")
		}
		if descriptor.ImageMode != ImageModeDocker && descriptor.ImageMode != ImageModeTemplate {
			return fmt.Errorf("Catthehacker onboarding requires Docker or template image mode, got %q", descriptor.ImageMode)
		}
		if !descriptor.GuidedArtifacts || len(descriptor.WizardImageProfiles) == 0 {
			return fmt.Errorf("Catthehacker onboarding requires guided artifact profiles")
		}
		for _, profile := range descriptor.WizardImageProfiles {
			if profile.Name == "" || profile.Tag == "" {
				return fmt.Errorf("Catthehacker onboarding image profiles require name and tag")
			}
		}
		defaults := 0
		for _, source := range descriptor.WizardArtifactSources {
			if source.Distribution == "" || source.Profile == "" || source.Reference == "" || source.Label == "" {
				return fmt.Errorf("Catthehacker onboarding artifact sources require distribution, profile, reference, and label")
			}
			if source.Default {
				defaults++
			}
		}
		if len(descriptor.WizardArtifactSources) > 0 && defaults != 1 {
			return fmt.Errorf("Catthehacker onboarding artifact sources require exactly one default, got %d", defaults)
		}
	}
	return nil
}

func UnsupportedTypeError(providerType string) error {
	return fmt.Errorf("unsupported provider.type %q", providerType)
}
