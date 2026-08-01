package provider

import (
	"strings"
	"testing"
)

func TestWizardContributionKindsRejectMissingAndUnknownValues(t *testing.T) {
	for name, valid := range map[string]bool{
		"missing prerequisite": WizardPrerequisiteKind("").Valid(),
		"unknown prerequisite": WizardPrerequisiteKind("future").Valid(),
		"missing onboarding":   WizardOnboardingKind("").Valid(),
		"unknown onboarding":   WizardOnboardingKind("future").Valid(),
		"missing host trust":   WizardHostTrustKind("").Valid(),
		"unknown host trust":   WizardHostTrustKind("future").Valid(),
		"missing review":       WizardReviewKind("").Valid(),
		"unknown review":       WizardReviewKind("future").Valid(),
	} {
		if valid {
			t.Errorf("%s contribution was accepted", name)
		}
	}
}

func TestWizardContributionKindsAcceptRegisteredStrategies(t *testing.T) {
	for name, valid := range map[string]bool{
		"Docker prerequisite":           WizardPrerequisiteDocker.Valid(),
		"Docker Sandboxes prerequisite": WizardPrerequisiteDockerSandboxes.Valid(),
		"WSL2 prerequisite":             WizardPrerequisiteWSL2.Valid(),
		"Tart prerequisite":             WizardPrerequisiteTart.Valid(),
		"no onboarding":                 WizardOnboardingNone.Valid(),
		"Catthehacker onboarding":       WizardOnboardingCatthehackerDocker.Valid(),
		"no host trust":                 WizardHostTrustNone.Valid(),
		"overlay host trust":            WizardHostTrustOverlay.Valid(),
		"Docker image review":           WizardReviewDockerImage.Valid(),
		"native image review":           WizardReviewNativeImage.Valid(),
	} {
		if !valid {
			t.Errorf("%s contribution was rejected", name)
		}
	}
}

func TestValidateWizardContributionsRejectsIncompatibleStrategies(t *testing.T) {
	base := Descriptor{
		WizardSupported:     true,
		WizardNumber:        "1",
		WizardLabel:         "Example",
		WizardAliases:       []string{"example"},
		ImageMode:           ImageModeDocker,
		GuidedArtifacts:     true,
		WizardImageProfiles: []WizardImageProfile{{Name: "full", Tag: "full-latest"}},
		WizardPrerequisite:  WizardPrerequisiteDocker,
		WizardOnboarding:    WizardOnboardingCatthehackerDocker,
		WizardHostTrust:     WizardHostTrustOverlay,
		WizardReview:        WizardReviewDockerImage,
	}
	tests := map[string]Descriptor{
		"onboarding without compatible review": func() Descriptor {
			value := base
			value.WizardReview = WizardReviewNativeImage
			return value
		}(),
		"review without compatible onboarding": func() Descriptor {
			value := base
			value.WizardOnboarding = WizardOnboardingNone
			return value
		}(),
		"Docker onboarding without profiles": func() Descriptor {
			value := base
			value.WizardImageProfiles = nil
			return value
		}(),
	}
	for name, descriptor := range tests {
		t.Run(name, func(t *testing.T) {
			if err := ValidateWizardContributions(descriptor); err == nil {
				t.Fatal("incompatible wizard contributions were accepted")
			}
		})
	}
}

func TestValidateWizardContributionsAcceptsNativeProviderWithoutOnboarding(t *testing.T) {
	descriptor := Descriptor{
		WizardSupported:    true,
		WizardNumber:       "4",
		WizardLabel:        "Native",
		WizardAliases:      []string{"native"},
		ImageMode:          ImageModeNative,
		WizardPrerequisite: WizardPrerequisiteTart,
		WizardOnboarding:   WizardOnboardingNone,
		WizardHostTrust:    WizardHostTrustNone,
		WizardReview:       WizardReviewNativeImage,
		WizardReviewSource: "example/source",
		WizardReviewOutput: "example-output",
	}
	if err := ValidateWizardContributions(descriptor); err != nil {
		t.Fatalf("compatible native provider was rejected: %v", err)
	}
}

func TestValidateWizardContributionsExplainsMissingAndUnknownContributions(t *testing.T) {
	base := Descriptor{
		WizardSupported:     true,
		WizardNumber:        "1",
		WizardLabel:         "Example",
		WizardAliases:       []string{"example"},
		ImageMode:           ImageModeDocker,
		GuidedArtifacts:     true,
		WizardImageProfiles: []WizardImageProfile{{Name: "full", Tag: "full-latest"}},
		WizardPrerequisite:  WizardPrerequisiteDocker,
		WizardOnboarding:    WizardOnboardingCatthehackerDocker,
		WizardHostTrust:     WizardHostTrustOverlay,
		WizardReview:        WizardReviewDockerImage,
	}
	tests := []struct {
		name string
		want string
		edit func(*Descriptor)
	}{
		{name: "missing prerequisite", want: "prerequisite", edit: func(value *Descriptor) { value.WizardPrerequisite = "" }},
		{name: "unknown onboarding", want: "onboarding", edit: func(value *Descriptor) { value.WizardOnboarding = "future" }},
		{name: "missing host trust", want: "host-trust", edit: func(value *Descriptor) { value.WizardHostTrust = "" }},
		{name: "unknown review", want: "review", edit: func(value *Descriptor) { value.WizardReview = "future" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			descriptor := base
			test.edit(&descriptor)
			err := ValidateWizardContributions(descriptor)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validation error = %v, want a clear %s error", err, test.want)
			}
		})
	}
}
