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
	initWizardHostTrust
	initWizardUpdates
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
	CustomScripts             []string
	DownloadBytes             uint64
	ExpandedBytes             uint64
	IncrementalPeakBytes      uint64
	AvailablePhysicalSpace    string
	LogicalRootMaximumBytes   uint64
	LogicalDockerMaximumBytes uint64
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
	PoolChosen      bool
	HostTrustChosen bool
	UpdatesChosen   bool
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
			result, err := promptInitProviderWizard(opts.Context, opts.ProjectRoot, opts.Out, reader, opts.SkipDockerCheck, true, draft.ProviderType)
			if err != nil {
				return initWizardOutcome{}, err
			}
			action = result.Action
			if action == initWizardNext && result.Value != draft.ProviderType {
				oldUsesHostTrust := initProviderUsesHostTrust(draft.ProviderType)
				draft.ProviderType = result.Value
				draft.Profile = nil
				draft.Estimate = nil
				newUsesHostTrust := initProviderUsesHostTrust(draft.ProviderType)
				if !newUsesHostTrust || !oldUsesHostTrust {
					draft.HostTrustMode = config.HostTrustModeDisabled
					draft.HostTrustScopes = []string{config.HostTrustScopeSystem}
					draft.HostTrustChosen = false
				}
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
				draft.PoolChosen = true
			}
		case initWizardHostTrust:
			defaultEnabled := true
			if draft.HostTrustChosen {
				defaultEnabled = draft.HostTrustMode == config.HostTrustModeOverlay
			}
			result, err := promptHostTrustWizard(opts, reader, defaultEnabled)
			if err != nil {
				return initWizardOutcome{}, err
			}
			action = result.Action
			if action == initWizardNext {
				draft.HostTrustMode = config.HostTrustModeDisabled
				draft.HostTrustScopes = []string{config.HostTrustScopeSystem}
				if result.Value {
					draft.HostTrustMode = config.HostTrustModeOverlay
					draft.HostTrustScopes = hostTrustScopesForOS(initHostTrustOS)
				}
				draft.HostTrustChosen = true
			}
		case initWizardUpdates:
			result, err := promptImageUpdatePolicyWizard(opts.Out, reader, draft.UpdatePolicy)
			if err != nil {
				return initWizardOutcome{}, err
			}
			action = result.Action
			if action == initWizardNext {
				draft.UpdatePolicy = result.Value
				draft.UpdatesChosen = true
			}
		case initWizardReview:
			result, err := promptInitReview(opts.Out, reader, draft)
			if err != nil {
				return initWizardOutcome{}, err
			}
			switch result.Action {
			case initWizardNext:
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
			if initProviderUsesHostTrust(draft.ProviderType) && !draft.HostTrustChosen {
				return initWizardHostTrust
			}
			return initWizardReview
		case initWizardProviderSetup:
			if initProviderUsesHostTrust(draft.ProviderType) && !draft.HostTrustChosen {
				return initWizardHostTrust
			}
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
		if initProviderUsesHostTrust(draft.ProviderType) {
			return initWizardHostTrust
		}
		return initWizardUpdates
	case initWizardHostTrust:
		return initWizardUpdates
	default:
		return initWizardReview
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

func promptWizardOptional(out io.Writer, reader *bufio.Reader, label string) (initWizardResult[string], error) {
	fmt.Fprintf(out, "%s (press Enter for none; /back to return): ", label)
	value, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return initWizardResult[string]{}, err
	}
	value = strings.TrimSpace(value)
	if strings.EqualFold(value, "/back") {
		return initWizardResult[string]{Action: initWizardBack}, nil
	}
	return initWizardResult[string]{Action: initWizardNext, Value: value}, nil
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

func promptWizardYesNo(out io.Writer, reader *bufio.Reader, label string, defaultYes bool) (initWizardResult[bool], error) {
	defaultValue := "Y"
	if !defaultYes {
		defaultValue = "N"
	}
	for {
		result, hitEOF, err := promptWizardDefault(out, reader, label+" [Y/n]", defaultValue)
		if err != nil || result.Action == initWizardBack {
			return initWizardResult[bool]{Action: result.Action}, err
		}
		switch strings.ToLower(result.Value) {
		case "y", "yes":
			return initWizardResult[bool]{Action: initWizardNext, Value: true}, nil
		case "n", "no":
			return initWizardResult[bool]{Action: initWizardNext, Value: false}, nil
		default:
			fmt.Fprintln(out, "Please answer yes or no, or enter /back to return.")
			if hitEOF {
				return initWizardResult[bool]{}, fmt.Errorf("invalid yes/no response %q", result.Value)
			}
		}
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

func promptHostTrustWizard(opts initOptions, reader *bufio.Reader, defaultEnabled bool) (initWizardResult[bool], error) {
	fmt.Fprintln(opts.Out, "Runners need this host's trusted TLS roots to access services that this machine trusts.")
	result, err := promptWizardYesNo(opts.Out, reader, "Inherit this host's trusted TLS roots into disposable runners?", defaultEnabled)
	if err != nil || result.Action != initWizardNext || !result.Value {
		return result, err
	}
	deferred := os.Getenv("EPAR_HOST_TRUST_INIT_DEFERRED") == "1"
	if !opts.SkipHostTrustCheck && !deferred {
		preflightCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_, collectErr := initResolveHostTrust(preflightCtx, hosttrust.Options{Mode: config.HostTrustModeOverlay, Scopes: hostTrustScopesForOS(initHostTrustOS), ControllerHostOS: initHostTrustOS})
		cancel()
		if collectErr != nil {
			return initWizardResult[bool]{}, fmt.Errorf("collect host trusted TLS roots before writing config: %w", collectErr)
		}
	}
	return result, nil
}

func renderInitArtifactEstimate(out io.Writer, estimate *initArtifactEstimate) {
	if estimate == nil {
		return
	}
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Runner artifact estimate:")
	fmt.Fprintf(out, "  Source: %s\n", estimate.Source)
	fmt.Fprintf(out, "  Platform: %s\n", estimate.Platform)
	if len(estimate.CustomScripts) == 0 {
		fmt.Fprintln(out, "  Custom install scripts: none")
	} else {
		fmt.Fprintf(out, "  Custom install scripts: %s\n", strings.Join(estimate.CustomScripts, ", "))
	}
	fmt.Fprintf(out, "  Estimated download: %s compressed layers\n", formatInitUintByteCount(estimate.DownloadBytes))
	fmt.Fprintf(out, "  Estimated expanded source: %s\n", formatInitUintByteCount(estimate.ExpandedBytes))
	fmt.Fprintf(out, "  Estimated incremental physical peak: %s\n", formatInitUintByteCount(estimate.IncrementalPeakBytes))
	fmt.Fprintf(out, "  Available physical space: %s\n", estimate.AvailablePhysicalSpace)
	fmt.Fprintln(out, "  Fixed free-space reserve: 1GiB")
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
	case provider.WizardOnboardingNone:
		return initWizardResult[*initDockerSandboxesProfile]{Action: initWizardNext}, nil, nil
	case provider.WizardOnboardingCatthehackerDocker:
		if existing != nil && existing.Provider == providerType {
			fmt.Fprintln(out, "")
			fmt.Fprintln(out, "Current runner artifact setup:")
			fmt.Fprintf(out, "  Image: %s\n", existing.SourceImage)
			if len(existing.CustomScripts) == 0 {
				fmt.Fprintln(out, "  Custom install scripts: none")
			} else {
				fmt.Fprintf(out, "  Custom install scripts: %s\n", strings.Join(existing.CustomScripts, ", "))
			}
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

func promptInitProviderWizard(ctx context.Context, projectRoot string, out io.Writer, reader *bufio.Reader, skipDockerCheck, allowBack bool, preferredProvider string) (initWizardResult[string], error) {
	hostPlatform := initSandboxPromotionPlatform()
	record, promoted := initSandboxPromotionLookup(hostPlatform)
	for {
		result, _, err := promptInitProviderChoiceWizard(ctx, projectRoot, hostPlatform, record, promoted, out, reader, skipDockerCheck, allowBack, preferredProvider)
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

func promptImageUpdatePolicyWizard(out io.Writer, reader *bufio.Reader, current initImageUpdatePolicy) (initWizardResult[initImageUpdatePolicy], error) {
	defaultChoice := "1"
	switch current.Frequency {
	case config.ImageUpdateFrequencyDaily:
		defaultChoice = "2"
	case config.ImageUpdateFrequencyBiweekly:
		defaultChoice = "3"
	case config.ImageUpdateFrequencyMonthly:
		defaultChoice = "4"
	case config.ImageUpdateFrequencyManual:
		defaultChoice = "5"
	}
	for {
		fmt.Fprintln(out, "")
		fmt.Fprintln(out, "Automatic image and Actions runner updates:")
		fmt.Fprintln(out, "  1. Weekly")
		fmt.Fprintln(out, "  2. Daily")
		fmt.Fprintln(out, "  3. Every two weeks")
		fmt.Fprintln(out, "  4. Monthly")
		fmt.Fprintln(out, "  5. Manual — check only on demand")
		fmt.Fprintln(out, "     Command: ./start image update")
		fmt.Fprintln(out, "  0. Back")
		result, hitEOF, err := promptWizardDefault(out, reader, "Update frequency", defaultChoice)
		if err != nil {
			return initWizardResult[initImageUpdatePolicy]{}, err
		}
		if result.Action == initWizardBack || result.Value == "0" {
			return initWizardResult[initImageUpdatePolicy]{Action: initWizardBack}, nil
		}
		frequency := ""
		switch strings.ToLower(result.Value) {
		case "1", "weekly":
			frequency = config.ImageUpdateFrequencyWeekly
		case "2", "daily":
			frequency = config.ImageUpdateFrequencyDaily
		case "3", "biweekly", "every two weeks":
			frequency = config.ImageUpdateFrequencyBiweekly
		case "4", "monthly":
			frequency = config.ImageUpdateFrequencyMonthly
		case "5", "manual":
			return initWizardResult[initImageUpdatePolicy]{Action: initWizardNext, Value: initImageUpdatePolicy{Frequency: config.ImageUpdateFrequencyManual, Time: config.DefaultImageUpdateTime}}, nil
		default:
			fmt.Fprintln(out, "  Choose 1–5, 0 to go back, or enter daily, weekly, biweekly, monthly, or manual.")
			if hitEOF {
				return initWizardResult[initImageUpdatePolicy]{}, fmt.Errorf("invalid image update frequency %q", result.Value)
			}
			continue
		}
		updateTimeDefault := current.Time
		if updateTimeDefault == "" {
			updateTimeDefault = config.DefaultImageUpdateTime
		}
		for {
			timeResult, _, promptErr := promptWizardDefault(out, reader, "Local update time (24-hour HH:MM)", updateTimeDefault)
			if promptErr != nil {
				return initWizardResult[initImageUpdatePolicy]{}, promptErr
			}
			if timeResult.Action == initWizardBack {
				return initWizardResult[initImageUpdatePolicy]{Action: initWizardBack}, nil
			}
			policy := initImageUpdatePolicy{Frequency: frequency, Time: timeResult.Value}
			image := config.Default().Image
			image.UpdateFrequency = policy.Frequency
			image.UpdateTime = policy.Time
			if validationErr := config.ValidateImageUpdatePolicy(image); validationErr != nil {
				fmt.Fprintf(out, "  %v\n", validationErr)
				continue
			}
			return initWizardResult[initImageUpdatePolicy]{Action: initWizardNext, Value: policy}, nil
		}
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
		if len(draft.Profile.CustomScripts) == 0 {
			fmt.Fprintln(out, "  Custom install scripts: none")
		} else {
			fmt.Fprintf(out, "  Custom install scripts: %s\n", strings.Join(draft.Profile.CustomScripts, ", "))
		}
	case provider.WizardReviewNativeImage:
		fmt.Fprintf(out, "  Runner image: %s\n", descriptor.WizardReviewSource)
		fmt.Fprintf(out, "  Reusable artifact: %s\n", descriptor.WizardReviewOutput)
	default:
		return initWizardResult[initWizardStep]{}, fmt.Errorf("provider %q has unknown wizard review contribution %q", draft.ProviderType, descriptor.WizardReview)
	}
	fmt.Fprintf(out, "  Pool name prefix: %s\n", draft.PoolNamePrefix)
	if initProviderUsesHostTrust(draft.ProviderType) {
		if draft.HostTrustMode == config.HostTrustModeOverlay {
			fmt.Fprintln(out, "  Host trusted TLS roots: inherited")
		} else {
			fmt.Fprintln(out, "  Host trusted TLS roots: not inherited")
		}
	} else {
		fmt.Fprintln(out, "  Host trusted TLS roots: not applicable")
	}
	fmt.Fprintf(out, "  Updates: %s", draft.UpdatePolicy.Frequency)
	if draft.UpdatePolicy.Frequency != config.ImageUpdateFrequencyManual {
		fmt.Fprintf(out, " at %s local time", draft.UpdatePolicy.Time)
	}
	fmt.Fprintln(out)
	renderInitArtifactEstimate(out, draft.Estimate)

	options := []initReviewOption{
		{Label: "Looks good, proceed to create configuration"},
		{Label: "Change runner group", Step: initWizardRunnerGroup},
		{Label: "Change provider", Step: initWizardProvider},
	}
	if initProviderNeedsSetup(draft.ProviderType) {
		options = append(options, initReviewOption{Label: "Change runner image or install scripts", Step: initWizardProviderSetup})
	}
	options = append(options, initReviewOption{Label: "Change pool name prefix", Step: initWizardPool})
	if initProviderUsesHostTrust(draft.ProviderType) {
		options = append(options, initReviewOption{Label: "Change host trust", Step: initWizardHostTrust})
	}
	options = append(options, initReviewOption{Label: "Change update frequency", Step: initWizardUpdates})

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
	case "tart":
		return defaultTartConfig(draft.AppID, draft.Organization, draft.PrivateKeyPath, draft.PoolNamePrefix, draft.RunnerGroup, draft.UpdatePolicy), nil
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
