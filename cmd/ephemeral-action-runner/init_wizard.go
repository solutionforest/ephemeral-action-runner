package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
	"github.com/solutionforest/ephemeral-action-runner/internal/hosttrust"
	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
	providerregistry "github.com/solutionforest/ephemeral-action-runner/internal/provider/registry"
)

type initWizardStep uint8

const (
	initWizardRunnerGroup initWizardStep = iota
	initWizardProvider
	initWizardProviderSetup
	initWizardPool
	initWizardReview
)

type initWizardAction uint8

const (
	initWizardNext initWizardAction = iota
	initWizardBack
	initWizardRefresh
	initWizardQuit
	initWizardEdit
)

type initWizardResult[T any] struct {
	Action initWizardAction
	Value  T
}

type initWizardHistory []initWizardStep

func (history *initWizardHistory) push(step initWizardStep) {
	*history = append(*history, step)
}

func (history *initWizardHistory) pop() (initWizardStep, bool) {
	if len(*history) == 0 {
		return 0, false
	}
	last := len(*history) - 1
	step := (*history)[last]
	*history = (*history)[:last]
	return step, true
}

func (history *initWizardHistory) reset(step initWizardStep) {
	*history = append((*history)[:0], step)
}

type initArtifactEstimate struct {
	Source                    string
	Platform                  string
	DownloadBytes             uint64
	ExpandedBytes             uint64
	CapacityDomains           []initCapacityDomainEstimate
	CapacityWarnings          []string
	LogicalRootMaximumBytes   uint64
	LogicalDockerMaximumBytes uint64
}

type initCapacityDomainEstimate struct {
	Roles          string
	Location       string
	AvailableBytes uint64
	AvailableKnown bool
	PhasePeakBytes uint64
	ReserveBytes   uint64
	Confidence     string
	Status         string
}

type initWizardDraft struct {
	AppID           int64
	Organization    string
	PrivateKeyPath  string
	RunnerGroup     initRunnerGroupSelection
	ProviderType    string
	Profile         *initDockerSandboxesProfile
	Estimate        *initArtifactEstimate
	PoolNamePrefix  string
	HostTrustMode   string
	HostTrustScopes []string
	UpdatePolicy    initImageUpdatePolicy
}

type initWizardOutcome struct {
	Draft  initWizardDraft
	Create bool
}

func runInitConfigurationWizard(opts initOptions, reader *bufio.Reader, githubConfig config.GitHubConfig, appID int64, organization, privateKeyPath, defaultPrefix string) (initWizardOutcome, error) {
	draft := initWizardDraft{
		AppID:           appID,
		Organization:    organization,
		PrivateKeyPath:  privateKeyPath,
		PoolNamePrefix:  defaultPrefix,
		HostTrustMode:   config.HostTrustModeDisabled,
		HostTrustScopes: []string{config.HostTrustScopeSystem},
		UpdatePolicy:    initImageUpdatePolicy{Frequency: config.ImageUpdateFrequencyWeekly, Time: config.DefaultImageUpdateTime},
	}
	client := newInitRunnerGroupClient(githubConfig)
	step := initWizardRunnerGroup
	history := make(initWizardHistory, 0, 8)
	editHistory := make(initWizardHistory, 0, 3)
	editing := false
	providerMenuState := initProviderMenuState{}

	for {
		var action initWizardAction
		switch step {
		case initWizardRunnerGroup:
			result, err := promptRunnerGroupWizard(opts.Context, opts.Out, reader, client, len(history) > 0)
			if err != nil {
				return initWizardOutcome{}, err
			}
			action = result.Action
			if action == initWizardNext {
				draft.RunnerGroup = result.Value
			}
		case initWizardProvider:
			result, err := promptInitProviderWizard(opts.Context, opts.ProjectRoot, opts.Out, reader, opts.SkipDockerCheck, true, &providerMenuState)
			if err != nil {
				return initWizardOutcome{}, err
			}
			action = result.Action
			if action == initWizardNext {
				if result.Value != draft.ProviderType {
					draft.ProviderType = result.Value
					draft.Profile = nil
					draft.Estimate = nil
				}
				applyInitWizardProviderDefaults(&draft)
			}
		case initWizardProviderSetup:
			result, estimate, err := promptProviderSetupWizard(opts.Context, opts.ProjectRoot, draft.ProviderType, draft.Profile, draft.Estimate, opts.Out, reader)
			if err != nil {
				return initWizardOutcome{}, err
			}
			action = result.Action
			if action == initWizardNext {
				draft.Profile = result.Value
				draft.Estimate = estimate
			}
		case initWizardPool:
			fmt.Fprintln(opts.Out, "")
			fmt.Fprintln(opts.Out, "Pool name prefix must be unique for this machine/config within the GitHub organization.")
			fmt.Fprintln(opts.Out, "EPAR cleanup deletes GitHub runner records matching this prefix.")
			result, err := promptPoolNamePrefixWizard(opts.Out, reader, draft.PoolNamePrefix)
			if err != nil {
				return initWizardOutcome{}, err
			}
			action = result.Action
			if action == initWizardNext {
				draft.PoolNamePrefix = result.Value
			}
		case initWizardReview:
			result, err := promptInitReview(opts.Out, reader, draft)
			if err != nil {
				return initWizardOutcome{}, err
			}
			switch result.Action {
			case initWizardNext:
				if err := preflightInitWizardHostTrust(opts, draft); err != nil {
					return initWizardOutcome{}, err
				}
				return initWizardOutcome{Draft: draft, Create: true}, nil
			case initWizardQuit:
				return initWizardOutcome{Draft: draft}, nil
			case initWizardBack:
				action = initWizardBack
			default:
				editHistory.reset(initWizardReview)
				step = result.Value
				editing = true
				continue
			}
		}

		switch action {
		case initWizardQuit:
			return initWizardOutcome{Draft: draft}, nil
		case initWizardBack:
			if editing {
				if len(editHistory) == 0 {
					step = initWizardReview
					editing = false
					continue
				}
				step, _ = editHistory.pop()
				if step == initWizardReview {
					editing = false
				}
				continue
			}
			if len(history) == 0 {
				continue
			}
			step, _ = history.pop()
			if step == initWizardReview {
				editing = false
			}
			continue
		case initWizardRefresh:
			continue
		}

		next := nextInitWizardStep(step, draft, editing)
		if editing {
			if next == initWizardReview {
				step = initWizardReview
				editHistory = editHistory[:0]
				editing = false
				continue
			}
			editHistory.push(step)
			step = next
			continue
		}
		history.push(step)
		step = next
		if step == initWizardReview {
			editing = false
		}
	}
}

func nextInitWizardStep(current initWizardStep, draft initWizardDraft, editing bool) initWizardStep {
	if editing {
		switch current {
		case initWizardProvider:
			if initProviderNeedsSetup(draft.ProviderType) {
				return initWizardProviderSetup
			}
			return initWizardReview
		case initWizardProviderSetup:
			return initWizardReview
		default:
			return initWizardReview
		}
	}
	switch current {
	case initWizardRunnerGroup:
		return initWizardProvider
	case initWizardProvider:
		if initProviderNeedsSetup(draft.ProviderType) {
			return initWizardProviderSetup
		}
		return initWizardPool
	case initWizardProviderSetup:
		return initWizardPool
	case initWizardPool:
		return initWizardReview
	default:
		return initWizardReview
	}
}

func applyInitWizardProviderDefaults(draft *initWizardDraft) {
	draft.HostTrustMode = config.HostTrustModeDisabled
	draft.HostTrustScopes = []string{config.HostTrustScopeSystem}
	if initProviderUsesHostTrust(draft.ProviderType) {
		draft.HostTrustMode = config.HostTrustModeOverlay
		draft.HostTrustScopes = hostTrustScopesForOS(initHostTrustOS)
	}
}

func initProviderNeedsSetup(providerType string) bool {
	descriptor, found := providerregistry.DescriptorFor(providerType)
	return found && descriptor.WizardOnboarding != "none"
}

func initProviderUsesHostTrust(providerType string) bool {
	descriptor, found := providerregistry.DescriptorFor(providerType)
	return found && descriptor.WizardHostTrust == provider.WizardHostTrustOverlay
}

func promptWizardDefault(out io.Writer, reader *bufio.Reader, label, defaultValue string) (initWizardResult[string], bool, error) {
	fmt.Fprintf(out, "%s (press Enter to use %s; /back to return): ", label, defaultValue)
	value, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return initWizardResult[string]{}, false, err
	}
	hitEOF := errors.Is(err, io.EOF)
	value = strings.TrimSpace(value)
	if strings.EqualFold(value, "/back") {
		return initWizardResult[string]{Action: initWizardBack}, hitEOF, nil
	}
	if value == "" {
		value = defaultValue
	}
	return initWizardResult[string]{Action: initWizardNext, Value: value}, hitEOF, nil
}

func promptWizardRequired(out io.Writer, reader *bufio.Reader, label string) (initWizardResult[string], error) {
	for {
		fmt.Fprintf(out, "%s (/back to return): ", label)
		value, err := reader.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return initWizardResult[string]{}, err
		}
		value = strings.TrimSpace(value)
		if strings.EqualFold(value, "/back") {
			return initWizardResult[string]{Action: initWizardBack}, nil
		}
		if value != "" {
			return initWizardResult[string]{Action: initWizardNext, Value: value}, nil
		}
		if errors.Is(err, io.EOF) {
			return initWizardResult[string]{}, fmt.Errorf("%s is required", label)
		}
		fmt.Fprintf(out, "%s is required.\n", label)
	}
}

func promptPoolNamePrefixWizard(out io.Writer, reader *bufio.Reader, defaultValue string) (initWizardResult[string], error) {
	for {
		result, hitEOF, err := promptWizardDefault(out, reader, "Pool name prefix", defaultValue)
		if err != nil || result.Action == initWizardBack {
			return result, err
		}
		if validationErr := config.ValidatePrefix(result.Value); validationErr != nil {
			fmt.Fprintf(out, "Pool name prefix is invalid: %v\n", validationErr)
			if hitEOF {
				return initWizardResult[string]{}, validationErr
			}
			continue
		}
		return result, nil
	}
}

func preflightInitWizardHostTrust(opts initOptions, draft initWizardDraft) error {
	if !initProviderUsesHostTrust(draft.ProviderType) {
		return nil
	}
	deferred := os.Getenv("EPAR_HOST_TRUST_INIT_DEFERRED") == "1"
	if !opts.SkipHostTrustCheck && !deferred {
		preflightCtx, cancel := context.WithTimeout(opts.Context, 30*time.Second)
		_, collectErr := initResolveHostTrust(preflightCtx, hosttrust.Options{Mode: draft.HostTrustMode, Scopes: draft.HostTrustScopes, ControllerHostOS: initHostTrustOS})
		cancel()
		if collectErr != nil {
			return fmt.Errorf("collect host trusted TLS roots before writing config: %w", collectErr)
		}
	}
	return nil
}

func renderInitArtifactEstimate(out io.Writer, estimate *initArtifactEstimate) {
	if estimate == nil {
		return
	}
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Runner artifact estimate:")
	fmt.Fprintf(out, "  Source: %s\n", estimate.Source)
	fmt.Fprintf(out, "  Platform: %s\n", estimate.Platform)
	fmt.Fprintf(out, "  Estimated download: %s compressed layers\n", formatInitUintByteCount(estimate.DownloadBytes))
	fmt.Fprintf(out, "  Estimated expanded source: %s\n", formatInitUintByteCount(estimate.ExpandedBytes))
	fmt.Fprintln(out, "  Capacity domains (non-blocking estimate; startup admission remains authoritative):")
	if len(estimate.CapacityDomains) == 0 {
		fmt.Fprintln(out, "    unavailable — review the warning below and use storage status after creating the configuration")
	}
	for _, domain := range estimate.CapacityDomains {
		available := "unknown"
		if domain.AvailableKnown {
			available = formatInitUintByteCount(domain.AvailableBytes)
		}
		fmt.Fprintf(out, "    role=%s\n", domain.Roles)
		fmt.Fprintf(out, "      location=%s\n", domain.Location)
		fmt.Fprintf(out, "      available=%s  phase peak=%s  reserve=%s  confidence=%s  status=%s\n", available, formatInitUintByteCount(domain.PhasePeakBytes), formatInitUintByteCount(domain.ReserveBytes), domain.Confidence, domain.Status)
	}
	for _, warning := range estimate.CapacityWarnings {
		fmt.Fprintf(out, "  *** STORAGE DISCOVERY WARNING *** %s\n", warning)
	}
	if estimate.LogicalRootMaximumBytes > 0 {
		fmt.Fprintf(out, "  Automatic sandbox root limit: %s (sparse logical maximum)\n", formatInitUintByteCount(estimate.LogicalRootMaximumBytes))
		fmt.Fprintf(out, "  Inner Docker limit: %s (independent sparse logical maximum)\n", formatInitUintByteCount(estimate.LogicalDockerMaximumBytes))
	}
}

func promptProviderSetupWizard(ctx context.Context, projectRoot, providerType string, existing *initDockerSandboxesProfile, existingEstimate *initArtifactEstimate, out io.Writer, reader *bufio.Reader) (initWizardResult[*initDockerSandboxesProfile], *initArtifactEstimate, error) {
	descriptor, found := providerregistry.DescriptorFor(providerType)
	if !found {
		return initWizardResult[*initDockerSandboxesProfile]{}, nil, fmt.Errorf("unsupported provider.type %q", providerType)
	}
	switch descriptor.WizardOnboarding {
	case provider.WizardOnboardingCatthehackerDocker:
		if existing != nil && existing.Provider == providerType {
			fmt.Fprintln(out, "")
			fmt.Fprintln(out, "Current runner artifact setup:")
			fmt.Fprintf(out, "  Image: %s\n", existing.SourceImage)
			fmt.Fprintln(out, "  1. Continue with this setup (default)")
			fmt.Fprintln(out, "  2. Change runner artifact setup")
			fmt.Fprintln(out, "  0. Back")
			for {
				choice, hitEOF, err := promptDefault(out, reader, "Runner artifact setup", "1")
				if err != nil {
					return initWizardResult[*initDockerSandboxesProfile]{}, nil, err
				}
				switch strings.ToLower(strings.TrimSpace(choice)) {
				case "1", "continue":
					return initWizardResult[*initDockerSandboxesProfile]{Action: initWizardNext, Value: existing}, existingEstimate, nil
				case "2", "change":
					return promptDockerImageProfileWizard(ctx, projectRoot, providerType, initSandboxPromotionPlatform(), out, reader)
				case "0", "back":
					return initWizardResult[*initDockerSandboxesProfile]{Action: initWizardBack}, nil, nil
				default:
					fmt.Fprintln(out, "Choose 1 to continue, 2 to change, or 0 to go back.")
					if hitEOF {
						return initWizardResult[*initDockerSandboxesProfile]{}, nil, fmt.Errorf("invalid runner artifact setup choice %q", choice)
					}
				}
			}
		}
		return promptDockerImageProfileWizard(ctx, projectRoot, providerType, initSandboxPromotionPlatform(), out, reader)
	default:
		return initWizardResult[*initDockerSandboxesProfile]{}, nil, fmt.Errorf("provider %q has unsupported onboarding strategy %q", providerType, descriptor.WizardOnboarding)
	}
}

func promptInitProviderWizard(ctx context.Context, projectRoot string, out io.Writer, reader *bufio.Reader, skipDockerCheck, allowBack bool, menuState *initProviderMenuState) (initWizardResult[string], error) {
	hostPlatform := initSandboxPromotionPlatform()
	record, promoted := initSandboxPromotionLookup(hostPlatform)
	for {
		result, _, err := promptInitProviderChoiceWizard(ctx, projectRoot, hostPlatform, record, promoted, out, reader, skipDockerCheck, allowBack, menuState)
		if err != nil {
			return initWizardResult[string]{}, err
		}
		if result.Action == initWizardRefresh {
			fmt.Fprintln(out, "Refreshing provider prerequisites...")
			continue
		}
		return result, nil
	}
}

type initReviewOption struct {
	Label string
	Step  initWizardStep
}

func promptInitReview(out io.Writer, reader *bufio.Reader, draft initWizardDraft) (initWizardResult[initWizardStep], error) {
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Configuration review:")
	fmt.Fprintf(out, "  GitHub App: %d for %s\n", draft.AppID, draft.Organization)
	fmt.Fprintf(out, "  Private key path: %s\n", draft.PrivateKeyPath)
	fmt.Fprintf(out, "  Runner group: %s\n", draft.RunnerGroup.Group.Name)
	fmt.Fprintf(out, "  Provider: %s\n", providerDisplayName(draft.ProviderType))
	descriptor, found := providerregistry.DescriptorFor(draft.ProviderType)
	if !found || !descriptor.WizardReview.Valid() {
		return initWizardResult[initWizardStep]{}, fmt.Errorf("provider %q has no valid wizard review contribution", draft.ProviderType)
	}
	switch descriptor.WizardReview {
	case provider.WizardReviewDockerImage:
		if draft.Profile == nil {
			return initWizardResult[initWizardStep]{}, fmt.Errorf("provider %q review requires an image profile", draft.ProviderType)
		}
		fmt.Fprintf(out, "  Runner image: %s\n", draft.Profile.SourceImage)
	default:
		return initWizardResult[initWizardStep]{}, fmt.Errorf("provider %q has unknown wizard review contribution %q", draft.ProviderType, descriptor.WizardReview)
	}
	fmt.Fprintf(out, "  Pool name prefix: %s\n", draft.PoolNamePrefix)
	renderInitArtifactEstimate(out, draft.Estimate)

	options := []initReviewOption{
		{Label: "Looks good, proceed to create configuration"},
		{Label: "Change runner group", Step: initWizardRunnerGroup},
		{Label: "Change provider", Step: initWizardProvider},
	}
	if initProviderNeedsSetup(draft.ProviderType) {
		options = append(options, initReviewOption{Label: "Change runner image", Step: initWizardProviderSetup})
	}
	options = append(options, initReviewOption{Label: "Change pool name prefix", Step: initWizardPool})

	fmt.Fprintln(out, "")
	for index, option := range options {
		defaultLabel := ""
		if index == 0 {
			defaultLabel = " (default)"
		}
		fmt.Fprintf(out, "  %d. %s%s\n", index+1, option.Label, defaultLabel)
	}
	fmt.Fprintln(out, "  0. Back")
	fmt.Fprintln(out, "  Q. Quit without writing a config")
	for {
		value, hitEOF, err := promptDefault(out, reader, "Review choice", "1")
		if err != nil {
			return initWizardResult[initWizardStep]{}, err
		}
		normalized := strings.ToLower(strings.TrimSpace(value))
		switch normalized {
		case "0", "back":
			return initWizardResult[initWizardStep]{Action: initWizardBack}, nil
		case "q", "quit":
			return initWizardResult[initWizardStep]{Action: initWizardQuit}, nil
		}
		index, parseErr := strconv.Atoi(normalized)
		if parseErr == nil && index >= 1 && index <= len(options) {
			if index == 1 {
				return initWizardResult[initWizardStep]{Action: initWizardNext}, nil
			}
			return initWizardResult[initWizardStep]{Action: initWizardEdit, Value: options[index-1].Step}, nil
		}
		fmt.Fprintln(out, "Choose a review action, 0 to go back, or Q to quit.")
		if hitEOF {
			return initWizardResult[initWizardStep]{}, fmt.Errorf("invalid review choice %q", value)
		}
	}
}

func renderInitWizardConfig(draft initWizardDraft) (string, error) {
	descriptor, found := providerregistry.DescriptorFor(draft.ProviderType)
	if !found {
		return "", fmt.Errorf("unsupported provider.type %q", draft.ProviderType)
	}
	if descriptor.WizardOnboarding == provider.WizardOnboardingCatthehackerDocker && draft.Profile == nil {
		return "", fmt.Errorf("%s image selection did not produce a provisioning profile", draft.ProviderType)
	}
	switch draft.ProviderType {
	case "docker-container":
		return defaultDockerContainerConfig(draft.AppID, draft.Organization, draft.PrivateKeyPath, draft.PoolNamePrefix, draft.HostTrustMode, draft.HostTrustScopes, draft.RunnerGroup, *draft.Profile, draft.UpdatePolicy), nil
	case "wsl":
		return defaultWSLConfig(draft.AppID, draft.Organization, draft.PrivateKeyPath, draft.PoolNamePrefix, draft.RunnerGroup, *draft.Profile, draft.UpdatePolicy), nil
	case "docker-sandboxes":
		guestPlatform, runnerArchitectureLabel, err := dockerSandboxesPlatform(draft.Profile.HostPlatform)
		if err != nil {
			return "", err
		}
		return defaultDockerSandboxesConfig(draft.AppID, draft.Organization, draft.PrivateKeyPath, draft.PoolNamePrefix, draft.HostTrustMode, draft.HostTrustScopes, draft.RunnerGroup, *draft.Profile, draft.UpdatePolicy, guestPlatform, runnerArchitectureLabel), nil
	default:
		return "", fmt.Errorf("unsupported provider.type %q", draft.ProviderType)
	}
}
