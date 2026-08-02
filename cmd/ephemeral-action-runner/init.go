package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
	gh "github.com/solutionforest/ephemeral-action-runner/internal/github"
	"github.com/solutionforest/ephemeral-action-runner/internal/hosttrust"
	imageartifact "github.com/solutionforest/ephemeral-action-runner/internal/image"
	"github.com/solutionforest/ephemeral-action-runner/internal/logging"
	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
	"github.com/solutionforest/ephemeral-action-runner/internal/provider/dockersandboxes"
	sandboxcapacity "github.com/solutionforest/ephemeral-action-runner/internal/provider/dockersandboxes/capacity"
	sandboxpolicy "github.com/solutionforest/ephemeral-action-runner/internal/provider/dockersandboxes/policy"
	sandboxpromotion "github.com/solutionforest/ephemeral-action-runner/internal/provider/dockersandboxes/promotion"
	providerregistry "github.com/solutionforest/ephemeral-action-runner/internal/provider/registry"
	"github.com/solutionforest/ephemeral-action-runner/internal/storage"
)

var dockerAvailable = func(ctx context.Context) error {
	return exec.CommandContext(ctx, "docker", "version", "--format", "{{.Server.Version}}").Run()
}

var initHostname = config.HostName

var initRandomRead = rand.Read

var initGOOS = runtime.GOOS

var initHostTrustOS = detectedInitHostTrustOS()

var initWSLStatus = wslStatus

var initTartVersion = tartVersion

var initResolveHostTrust = hosttrust.Resolve

var initSandboxPromotionPlatform = sandboxpromotion.CurrentPlatform

var initSandboxPromotionLookup = sandboxpromotion.Lookup

var initDockerSandboxesPreflight = func(ctx context.Context, record sandboxpromotion.Record, projectRoot string) sandboxpromotion.PreflightResult {
	return sandboxpromotion.LocalPreflight(ctx, record, projectRoot, os.Getenv("EPAR_CONTROLLER_IN_DOCKER") != "1", sourceRevision)
}

var initDockerSandboxesLookPath = exec.LookPath

var initDockerSandboxesStartDaemon = func(ctx context.Context, binary string) error {
	return dockersandboxes.New(binary).StartDaemon(ctx)
}

var initDockerSandboxesDiagnose = func(ctx context.Context, binary string) (dockersandboxes.HostReadiness, error) {
	return dockersandboxes.New(binary).VerifyHostReadiness(ctx)
}

var initDockerSandboxesReadiness = prepareInitDockerSandboxesReadiness

func prepareInitDockerSandboxesReadiness(ctx context.Context) (dockersandboxes.HostReadiness, error) {
	binary := "sbx"
	var daemonStartErr error
	if installed, err := initDockerSandboxesLookPath(binary); err == nil {
		binary = installed
		daemonStartErr = initDockerSandboxesStartDaemon(ctx, binary)
	}
	readiness, readinessErr := initDockerSandboxesDiagnose(ctx, binary)
	if readinessErr != nil && daemonStartErr != nil {
		return dockersandboxes.HostReadiness{}, fmt.Errorf("automatic 'sbx daemon start --detach' failed: %v; diagnostics also failed: %w", daemonStartErr, readinessErr)
	}
	return readiness, readinessErr
}

type initDockerSandboxesTemplate struct {
	Reference     string
	Digest        string
	CacheID       string
	Platform      string
	Size          int64
	Label         string
	SourceChannel string
}

type dockerSandboxesSourceLock struct {
	SchemaVersion int                                   `json:"schemaVersion"`
	Profiles      map[string]dockerSandboxesLockProfile `json:"profiles"`
}

type dockerSandboxesLockProfile struct {
	ObservedTagReference string                                        `json:"observedTagReference"`
	Platforms            map[string]dockerSandboxesLockProfilePlatform `json:"platforms"`
}

type dockerSandboxesLockProfilePlatform struct {
	TemplateTag string `json:"templateTag"`
}

type dockerSandboxesActiveProfile struct {
	Name              string
	ObservedTag       string
	TemplateReference string
	DisplayLabel      string
}

type initDockerSandboxesDiscovery struct {
	Templates         []initDockerSandboxesTemplate
	PolicyFingerprint string
}

type initDockerSandboxesRootMeasurement struct {
	PeakBytes int64
	Evidence  string
}

type initDockerSandboxesCapacityResult struct {
	StorageRoot    string
	AvailableBytes uint64
	TotalBytes     uint64
	Reservation    uint64
	HostWatermark  uint64
	RequiredBytes  uint64
	DeficitBytes   uint64
	CapacityStatus storage.CapacityStatus
}

type initDockerImageProfile struct {
	Provider          string
	HostPlatform      sandboxpromotion.Platform
	GuestPlatform     string
	SourceImage       string
	CustomScripts     []string
	PolicyFingerprint string
	RootDisk          string
	DockerDisk        string
}

// Retain the established helper name while generated-config renderers and
// provider tests migrate to the provider-neutral Docker image profile name.
type initDockerSandboxesProfile = initDockerImageProfile

type initImageUpdatePolicy struct {
	Frequency string
	Time      string
}

var initDiscoverDockerSandboxes = discoverDockerSandboxes

var initDockerSandboxesRootMeasurementFor = dockerSandboxesRootMeasurement

var initDockerSandboxesCapacityCheck = checkInitDockerSandboxesCapacity

var initResolveDockerSandboxesSource = imageartifact.ResolveCatthehackerSource

var initDockerSandboxesPolicyFingerprint = func(ctx context.Context) (string, error) {
	adapter := dockersandboxes.New("")
	if err := adapter.VerifyAdmission(ctx); err != nil {
		return "", fmt.Errorf("Docker Sandboxes admission check failed: %w", err)
	}
	rules, err := adapter.ReadGlobalNetworkPolicy(ctx)
	if err != nil {
		return "", fmt.Errorf("read Docker Sandboxes global network policy: %w", err)
	}
	return sandboxpolicy.Fingerprint(rules)
}

var initEnsureDockerSandboxesTemplate = func(ctx context.Context, projectRoot, configPath string) error {
	manager, err := newImageProvisioningManager(configPath, projectRoot)
	if err != nil {
		return err
	}
	defer manager.Close()
	return manager.EnsureImage(ctx)
}

type initRunnerGroupClient interface {
	ListRunnerGroups(context.Context) ([]gh.RunnerGroup, error)
	ListRunnerGroupRepositories(context.Context, int64) ([]gh.RunnerGroupRepository, error)
}

var newInitRunnerGroupClient = func(cfg config.GitHubConfig) initRunnerGroupClient {
	return gh.New(cfg)
}

func detectedInitHostTrustOS() string {
	if hostOS := strings.TrimSpace(os.Getenv("EPAR_CONTROLLER_HOST_OS")); hostOS != "" {
		return hostOS
	}
	return runtime.GOOS
}

type initOptions struct {
	Context            context.Context
	ProjectRoot        string
	ConfigPath         string
	Force              bool
	SkipDockerCheck    bool
	SkipHostTrustCheck bool
	EmbeddedInStart    bool
	In                 io.Reader
	Reader             *bufio.Reader
	Out                io.Writer
}

func runInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	common := addCommonFlags(fs)
	force := fs.Bool("force", false, "overwrite an existing config file")
	skipDockerCheck := fs.Bool("skip-docker-check", false, "create the config without checking for Docker")
	skipHostTrustCheck := fs.Bool("skip-host-trust-check", false, "create the config without collecting host trust roots")
	if err := fs.Parse(args); err != nil {
		return err
	}

	projectRoot, err := filepath.Abs(*common.projectRoot)
	if err != nil {
		return err
	}
	configPath := *common.configPath
	if configPath == "" {
		configPath = filepath.Join(projectRoot, ".local", "config.yml")
	} else {
		configPath = config.ProjectPath(projectRoot, configPath)
	}

	return runInitWithOptions(initOptions{
		Context:            interruptContext(),
		ProjectRoot:        projectRoot,
		ConfigPath:         configPath,
		Force:              *force,
		SkipDockerCheck:    *skipDockerCheck,
		SkipHostTrustCheck: *skipHostTrustCheck,
		In:                 os.Stdin,
		Out:                os.Stdout,
	})
}

func runInitWithOptions(opts initOptions) error {
	if opts.Context == nil {
		opts.Context = context.Background()
	}
	if opts.In == nil {
		opts.In = os.Stdin
	}
	if opts.Out == nil {
		opts.Out = os.Stdout
	}
	if opts.ConfigPath == "" {
		opts.ConfigPath = filepath.Join(opts.ProjectRoot, ".local", "config.yml")
	}
	if !opts.Force {
		if _, err := os.Stat(opts.ConfigPath); err == nil {
			return fmt.Errorf("config already exists at %s; use init --force to overwrite it", opts.ConfigPath)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	fmt.Fprintln(opts.Out, "EPAR first-run setup")
	fmt.Fprintln(opts.Out, "")
	fmt.Fprintln(opts.Out, "This creates an EPAR runner configuration.")
	fmt.Fprintln(opts.Out, "Before continuing, create a GitHub App with organization self-hosted runner read/write access.")
	fmt.Fprintln(opts.Out, "See README.md and docs/github-app.md for the GitHub App steps.")
	fmt.Fprintln(opts.Out, "")

	reader := opts.Reader
	if reader == nil {
		reader = bufio.NewReader(opts.In)
	}
	appID, err := promptRequiredInt64(opts.Out, reader, "GitHub App ID")
	if err != nil {
		return err
	}
	organization, err := promptRequired(opts.Out, reader, "GitHub organization")
	if err != nil {
		return err
	}
	privateKeyPath, err := promptRequired(opts.Out, reader, "GitHub App private key path")
	if err != nil {
		return err
	}
	githubConfig := config.GitHubConfig{
		AppID:          appID,
		Organization:   organization,
		PrivateKeyPath: resolveInitPrivateKeyPath(opts.ProjectRoot, privateKeyPath),
		APIBaseURL:     "https://api.github.com",
		WebBaseURL:     "https://github.com",
	}
	defaultPrefix, err := generatedPoolNamePrefix()
	if err != nil {
		return err
	}
	outcome, err := runInitConfigurationWizard(opts, reader, githubConfig, appID, organization, privateKeyPath, defaultPrefix)
	if err != nil {
		return err
	}
	if !outcome.Create {
		fmt.Fprintln(opts.Out, "Setup cancelled. No config was written.")
		return nil
	}
	content, err := renderInitWizardConfig(outcome.Draft)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(opts.ConfigPath), 0755); err != nil {
		return err
	}
	if err := logging.WritePrivateFileAtomic(opts.ConfigPath, []byte(content)); err != nil {
		return err
	}

	fmt.Fprintf(opts.Out, "\nCreated %s\n", opts.ConfigPath)
	if opts.EmbeddedInStart {
		fmt.Fprintln(opts.Out, "Initialization succeeded. Startup will now provision the selected runner artifact and apply storage admission before side effects.")
		return nil
	}
	startCommand := "./start"
	if initGOOS == "windows" {
		startCommand = `.\start.ps1`
	}
	fmt.Fprintf(opts.Out, `
Next:
  %s

Manual/advanced:
  %s image build --replace
  %s pool verify --instances 2 --register-only --cleanup
  %s pool up --instances 2
`, startCommand, startCommand, startCommand, startCommand)
	return nil
}

type initRunnerGroupSelection struct {
	Group  gh.RunnerGroup
	Policy config.RunnerGroupSecurityConfig
}

func promptRunnerGroup(ctx context.Context, out io.Writer, reader *bufio.Reader, client initRunnerGroupClient) (initRunnerGroupSelection, error) {
	result, err := promptRunnerGroupWizard(ctx, out, reader, client, false)
	if err != nil {
		return initRunnerGroupSelection{}, err
	}
	if result.Action == initWizardQuit {
		return initRunnerGroupSelection{}, fmt.Errorf("runner-group selection cancelled; no config was written")
	}
	return result.Value, nil
}

func promptRunnerGroupWizard(ctx context.Context, out io.Writer, reader *bufio.Reader, client initRunnerGroupClient, allowBack bool) (initWizardResult[initRunnerGroupSelection], error) {
	var groups []gh.RunnerGroup
	var repositories map[int64][]gh.RunnerGroupRepository
	showBlockedGroups := false
	showGroupDetails := false
	for {
		if groups == nil {
			var err error
			groups, repositories, err = loadInitRunnerGroups(ctx, client)
			if err != nil {
				return initWizardResult[initRunnerGroupSelection]{}, fmt.Errorf("load GitHub runner groups: %w", err)
			}
		}
		fmt.Fprintln(out, "")
		fmt.Fprintln(out, "GitHub runner group:")
		fmt.Fprintln(out, "  Choose which repositories can use these runners.")
		fmt.Fprintln(out, "  For better security, use a custom group for selected trusted repositories, with public access disabled.")
		fmt.Fprintln(out, "  See docs/runner-groups.md for details.")
		if showGroupDetails {
			fmt.Fprintln(out, "")
			fmt.Fprintln(out, "  Repository access meanings:")
			fmt.Fprintln(out, "    Selected repositories: Only repositories added to the group can use its runners.")
			fmt.Fprintln(out, "    All private repositories: All current and future private repositories can use its runners.")
			fmt.Fprintln(out, "    All repositories: All repositories allowed by the public-repository setting can use its runners.")
		}
		visibleGroups := filterRunnerGroupsForWizard(groups, repositories, showBlockedGroups)
		if len(visibleGroups) == 0 {
			fmt.Fprintln(out, "")
			fmt.Fprintln(out, "  No selectable runner groups found. Show blocked groups to review them.")
		}
		for i, group := range visibleGroups {
			printRunnerGroupChoice(out, i+1, group, repositories[group.ID], showGroupDetails)
		}
		fmt.Fprintln(out, "  R. Refresh runner groups")
		if showBlockedGroups {
			fmt.Fprintln(out, "  B. Hide blocked runner groups")
		} else {
			fmt.Fprintln(out, "  B. Show blocked runner groups")
		}
		if showGroupDetails {
			fmt.Fprintln(out, "  D. Hide runner group details")
		} else {
			fmt.Fprintln(out, "  D. Show runner group details")
		}
		if allowBack {
			fmt.Fprintln(out, "  0. Back")
		}
		fmt.Fprintln(out, "  Q. Quit without writing a config")

		choice, err := promptRequired(out, reader, "Runner group choice")
		if err != nil {
			return initWizardResult[initRunnerGroupSelection]{}, err
		}
		switch strings.ToLower(choice) {
		case "0", "back":
			if allowBack {
				return initWizardResult[initRunnerGroupSelection]{Action: initWizardBack}, nil
			}
		case "r", "refresh":
			groups = nil
			repositories = nil
			continue
		case "b", "blocked":
			showBlockedGroups = !showBlockedGroups
			continue
		case "d", "details":
			showGroupDetails = !showGroupDetails
			continue
		case "q", "quit":
			return initWizardResult[initRunnerGroupSelection]{Action: initWizardQuit}, nil
		}
		index, parseErr := strconv.Atoi(choice)
		if parseErr != nil || index < 1 || index > len(visibleGroups) {
			fmt.Fprintln(out, "Choose a runner group number, R to refresh, B for blocked groups, D for details, 0 to go back when shown, or Q to quit.")
			continue
		}
		group := visibleGroups[index-1]
		selectedRepositories := repositories[group.ID]
		_, publicCount := repositoryPrivacyCounts(selectedRepositories)
		if runnerGroupVisibilityRank(group.Visibility) == 3 {
			fmt.Fprintln(out, "")
			fmt.Fprintln(out, "*** SECURITY BLOCK: GITHUB RETURNED AN UNKNOWN REPOSITORY-ACCESS POLICY ***")
			fmt.Fprintf(out, "Runner group %q uses repository access %q, which this EPAR version cannot evaluate safely.\n", group.Name, group.Visibility)
			fmt.Fprintln(out, "RECOMMENDED ACTION: Do not select this group. Review its policy in GitHub, update EPAR if support is available, and choose Refresh; otherwise choose another documented group.")
			refresh, err := promptBackRefreshQuit(out, reader)
			if err != nil {
				return initWizardResult[initRunnerGroupSelection]{}, err
			}
			if refresh {
				groups = nil
				repositories = nil
			}
			continue
		}
		if group.AllowsPublicRepositories || publicCount > 0 {
			fmt.Fprintln(out, "")
			fmt.Fprintln(out, "*** SECURITY BLOCK: THIS RUNNER GROUP IS NOT ALLOWED BY EPAR'S SAFE DEFAULTS ***")
			fmt.Fprintf(out, "Runner group %q permits public repository access. A public repository or fork-triggered workflow may run untrusted code on a self-hosted runner and reach the runner host or connected services.\n", group.Name)
			fmt.Fprintln(out, "RECOMMENDED ACTION: Do not use this group for a normal EPAR deployment. Follow docs/runner-groups.md to create a dedicated non-default group, allow only explicitly selected trusted repositories, and disable public repository access. Then return here and choose that group.")
			fmt.Fprintln(out, "If you intentionally operate a separately reviewed public-project deployment, finish initialization with a secure group first and document any manual policy override afterward.")
			refresh, err := promptBackRefreshQuit(out, reader)
			if err != nil {
				return initWizardResult[initRunnerGroupSelection]{}, err
			}
			if refresh {
				groups = nil
				repositories = nil
			}
			continue
		}

		if group.Default {
			fmt.Fprintln(out, "")
			fmt.Fprintln(out, "*** SECURITY REMINDER: DEFAULT RUNNER GROUP ***")
			fmt.Fprintln(out, "The Default runner group is fine for trying EPAR.")
			fmt.Fprintln(out, "For regular use, a custom group limited to selected trusted repositories offers better security.")
			continueSelection, err := promptContinueOrBack(out, reader, "Continue with Default runner group")
			if err != nil {
				return initWizardResult[initRunnerGroupSelection]{}, err
			}
			if !continueSelection {
				continue
			}
		} else {
			warnings := runnerGroupSelectionWarnings(group)
			if len(warnings) > 0 {
				fmt.Fprintln(out, "")
				notRecommended := runnerGroupDoesNotMeetRecommendedPolicy(group)
				if notRecommended {
					fmt.Fprintln(out, "*** SECURITY WARNING: THIS RUNNER GROUP IS NOT RECOMMENDED ***")
				} else {
					fmt.Fprintln(out, "*** SECURITY ADVISORY: ENTERPRISE-MANAGED RUNNER GROUP ***")
				}
				fmt.Fprintf(out, "Runner group %q requires explicit review:\n", group.Name)
				for _, warning := range warnings {
					fmt.Fprintf(out, "  - %s\n", warning)
				}
				continueLabel := "Continue after confirming the enterprise-managed policy"
				if notRecommended {
					fmt.Fprintln(out, "RECOMMENDED ACTION: Choose Back. Follow docs/runner-groups.md to create a dedicated non-default group with Selected repositories and public repository access disabled, then select that safer group.")
					fmt.Fprintln(out, "Continuing will deliberately relax the generated policy to match this broader group. Future repositories may gain access without another EPAR configuration change.")
					continueLabel = "Continue anyway and generate a relaxed policy"
				}
				continueSelection, err := promptContinueOrBack(out, reader, continueLabel)
				if err != nil {
					return initWizardResult[initRunnerGroupSelection]{}, err
				}
				if !continueSelection {
					continue
				}
			}
		}
		return initWizardResult[initRunnerGroupSelection]{Action: initWizardNext, Value: initRunnerGroupSelection{
			Group: group,
			Policy: config.RunnerGroupSecurityConfig{
				Enforcement:                       config.RunnerGroupEnforcementEnforce,
				RequireExplicitGroup:              true,
				RequireNonDefaultGroup:            !group.Default,
				RequiredRepositoryAccess:          group.Visibility,
				RequirePublicRepositoriesDisabled: true,
			},
		}}, nil
	}
}

func loadInitRunnerGroups(ctx context.Context, client initRunnerGroupClient) ([]gh.RunnerGroup, map[int64][]gh.RunnerGroupRepository, error) {
	groups, err := client.ListRunnerGroups(ctx)
	if err != nil {
		return nil, nil, err
	}
	if len(groups) == 0 {
		return nil, nil, fmt.Errorf("GitHub returned no runner groups for the organization")
	}
	groups = sortRunnerGroupsForWizard(groups)
	repositories := make(map[int64][]gh.RunnerGroupRepository)
	for _, group := range groups {
		if group.Visibility != config.RunnerGroupRepositoryAccessSelected {
			continue
		}
		selected, err := client.ListRunnerGroupRepositories(ctx, group.ID)
		if err != nil {
			return nil, nil, err
		}
		repositories[group.ID] = selected
	}
	return groups, repositories, nil
}

func sortRunnerGroupsForWizard(groups []gh.RunnerGroup) []gh.RunnerGroup {
	ordered := append([]gh.RunnerGroup(nil), groups...)
	sort.SliceStable(ordered, func(i, j int) bool {
		left, right := ordered[i], ordered[j]
		if left.Default != right.Default {
			return left.Default
		}
		if left.AllowsPublicRepositories != right.AllowsPublicRepositories {
			return !left.AllowsPublicRepositories
		}
		if leftRank, rightRank := runnerGroupVisibilityRank(left.Visibility), runnerGroupVisibilityRank(right.Visibility); leftRank != rightRank {
			return leftRank < rightRank
		}
		if left.Inherited != right.Inherited {
			return !left.Inherited
		}
		return strings.ToLower(left.Name) < strings.ToLower(right.Name)
	})
	return ordered
}

func filterRunnerGroupsForWizard(groups []gh.RunnerGroup, repositories map[int64][]gh.RunnerGroupRepository, showBlocked bool) []gh.RunnerGroup {
	if showBlocked {
		return groups
	}
	visible := make([]gh.RunnerGroup, 0, len(groups))
	for _, group := range groups {
		if runnerGroupBlockedByWizard(group, repositories[group.ID]) {
			continue
		}
		visible = append(visible, group)
	}
	return visible
}

func runnerGroupBlockedByWizard(group gh.RunnerGroup, repositories []gh.RunnerGroupRepository) bool {
	_, publicCount := repositoryPrivacyCounts(repositories)
	return runnerGroupVisibilityRank(group.Visibility) == 3 || group.AllowsPublicRepositories || publicCount > 0
}

func runnerGroupVisibilityRank(visibility string) int {
	switch visibility {
	case config.RunnerGroupRepositoryAccessSelected:
		return 0
	case config.RunnerGroupRepositoryAccessPrivate:
		return 1
	case config.RunnerGroupRepositoryAccessAll:
		return 2
	default:
		return 3
	}
}

func printRunnerGroupChoice(out io.Writer, number int, group gh.RunnerGroup, repositories []gh.RunnerGroupRepository, showDetails bool) {
	privateCount, publicCount := repositoryPrivacyCounts(repositories)
	fmt.Fprintf(out, "\n  %d. %s\n", number, group.Name)
	if showDetails {
		switch group.Visibility {
		case config.RunnerGroupRepositoryAccessSelected:
			fmt.Fprintf(out, "     Repository access: Selected repositories — only the %d private and %d public repositories explicitly selected in GitHub can use this group.\n", privateCount, publicCount)
		case config.RunnerGroupRepositoryAccessPrivate:
			fmt.Fprintln(out, "     Repository access: All private repositories — every current and future private repository in the organization can use this group.")
		case config.RunnerGroupRepositoryAccessAll:
			fmt.Fprintln(out, "     Repository access: All repositories — every current and future repository permitted by the public-repository setting can use this group.")
		default:
			fmt.Fprintf(out, "     Repository access: Unknown GitHub value %q — do not select this group until its policy can be understood.\n", group.Visibility)
		}
		if group.AllowsPublicRepositories || publicCount > 0 {
			fmt.Fprintln(out, "     Public repositories: ALLOWED — public or fork-triggered workflows may reach self-hosted runners.")
		} else {
			fmt.Fprintln(out, "     Public repositories: Disabled — public repositories cannot use this group.")
		}
		groupTypes := []string{"organization-managed", "non-default"}
		if group.Default {
			groupTypes = []string{"GitHub default group"}
		}
		if group.Inherited {
			groupTypes = append(groupTypes, "inherited from the enterprise")
		}
		fmt.Fprintf(out, "     Group type: %s.\n", strings.Join(groupTypes, ", "))
	}
	switch {
	case runnerGroupVisibilityRank(group.Visibility) == 3:
		fmt.Fprintln(out, "     Assessment: BLOCKED BY WIZARD — repository access cannot be evaluated safely.")
	case group.AllowsPublicRepositories || publicCount > 0:
		fmt.Fprintln(out, "     Assessment: BLOCKED BY WIZARD — does not satisfy the public-repository safety requirement.")
	case group.Default:
		fmt.Fprintln(out, "     Assessment: It is fine for first-time tasting of EPAR, but generally recommend to create and use custom runner group for better security.")
	case runnerGroupDoesNotMeetRecommendedPolicy(group):
		fmt.Fprintln(out, "     Assessment: NOT RECOMMENDED — requires an explicit warning and a relaxed generated policy.")
	case group.Inherited:
		fmt.Fprintln(out, "     Assessment: REVIEW REQUIRED — access is restrictive, but policy changes are controlled at enterprise level.")
	default:
		fmt.Fprintln(out, "     Assessment: RECOMMENDED — matches EPAR's strict generated policy.")
	}
}

func repositoryPrivacyCounts(repositories []gh.RunnerGroupRepository) (privateCount, publicCount int) {
	for _, repository := range repositories {
		if repository.Private {
			privateCount++
		} else {
			publicCount++
		}
	}
	return privateCount, publicCount
}

func runnerGroupSelectionWarnings(group gh.RunnerGroup) []string {
	var warnings []string
	if group.Default {
		warnings = append(warnings, "This is GitHub's default runner group. New or unintended repositories may gain access as organization policy changes.")
	}
	switch group.Visibility {
	case config.RunnerGroupRepositoryAccessPrivate:
		warnings = append(warnings, "This group is available to every private repository in the organization, including repositories created later.")
	case config.RunnerGroupRepositoryAccessAll:
		warnings = append(warnings, "This group is available to every repository allowed by its public-repository setting, including repositories created later.")
	}
	if group.Inherited {
		warnings = append(warnings, "This group is inherited from the enterprise. Its policy must be reviewed and changed at enterprise level.")
	}
	return warnings
}

func runnerGroupDoesNotMeetRecommendedPolicy(group gh.RunnerGroup) bool {
	return group.Default || group.Visibility != config.RunnerGroupRepositoryAccessSelected
}

func promptContinueOrBack(out io.Writer, reader *bufio.Reader, continueLabel string) (bool, error) {
	fmt.Fprintf(out, "  1. %s\n", continueLabel)
	fmt.Fprintln(out, "  0. Back to group selection")
	for {
		choice, err := promptRequired(out, reader, "Choice")
		if err != nil {
			return false, err
		}
		switch strings.ToLower(choice) {
		case "1", "continue":
			return true, nil
		case "0", "2", "back":
			return false, nil
		default:
			fmt.Fprintln(out, "Choose 1 to continue or 0 to go back.")
		}
	}
}

func promptBackRefreshQuit(out io.Writer, reader *bufio.Reader) (bool, error) {
	fmt.Fprintln(out, "  0. Back to group selection")
	fmt.Fprintln(out, "  R. Refresh runner groups")
	fmt.Fprintln(out, "  Q. Quit without writing a config")
	for {
		choice, err := promptRequired(out, reader, "Choice")
		if err != nil {
			return false, err
		}
		switch strings.ToLower(choice) {
		case "0", "1", "back":
			return false, nil
		case "2", "r", "refresh":
			return true, nil
		case "3", "q", "quit":
			return false, fmt.Errorf("runner-group selection cancelled; no config was written")
		default:
			fmt.Fprintln(out, "Choose 0 to go back, R to refresh, or Q to quit.")
		}
	}
}

func resolveInitPrivateKeyPath(projectRoot, path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			if path == "~" {
				return home
			}
			return filepath.Join(home, path[2:])
		}
	}
	return config.ProjectPath(projectRoot, path)
}

func promptRequired(out io.Writer, reader *bufio.Reader, label string) (string, error) {
	for {
		fmt.Fprintf(out, "%s: ", label)
		value, err := reader.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", err
		}
		value = strings.TrimSpace(value)
		if strings.ContainsAny(value, "\r\n") {
			return "", fmt.Errorf("%s must be one line", label)
		}
		if value != "" {
			return value, nil
		}
		if errors.Is(err, io.EOF) {
			return "", fmt.Errorf("%s is required", label)
		}
		fmt.Fprintf(out, "%s is required.\n", label)
	}
}

func promptRequiredInt64(out io.Writer, reader *bufio.Reader, label string) (int64, error) {
	for {
		value, err := promptRequired(out, reader, label)
		if err != nil {
			return 0, err
		}
		parsed, parseErr := strconv.ParseInt(value, 10, 64)
		if parseErr == nil && parsed > 0 {
			return parsed, nil
		}
		fmt.Fprintf(out, "%s must be a positive number.\n", label)
	}
}

func promptPoolNamePrefix(out io.Writer, reader *bufio.Reader, defaultValue string) (string, error) {
	for {
		value, hitEOF, err := promptDefault(out, reader, "Pool name prefix", defaultValue)
		if err != nil {
			return "", err
		}
		if err := config.ValidatePrefix(value); err != nil {
			fmt.Fprintf(out, "Pool name prefix is invalid: %v\n", err)
			if hitEOF {
				return "", err
			}
			continue
		}
		return value, nil
	}
}

type initProviderOption struct {
	Number    string
	Type      string
	Label     string
	Available bool
	Status    string
	Default   bool
	Aliases   []string
}

type initProviderPrerequisites struct {
	DockerAvailable          bool
	DockerStatus             string
	DockerSandboxesAvailable bool
	DockerSandboxesStatus    string
	WSLAvailable             bool
	WSLStatus                string
	TartAvailable            bool
	TartStatus               string
}

func promptInitProvider(ctx context.Context, projectRoot string, out io.Writer, reader *bufio.Reader, skipDockerCheck bool) (string, sandboxpromotion.Record, *initDockerSandboxesProfile, error) {
	hostPlatform := initSandboxPromotionPlatform()
	record, promoted := initSandboxPromotionLookup(hostPlatform)
	for {
		providerType, _, refresh, err := promptInitProviderChoice(ctx, projectRoot, hostPlatform, record, promoted, out, reader, skipDockerCheck)
		if err != nil {
			return "", sandboxpromotion.Record{}, nil, err
		}
		if refresh {
			fmt.Fprintln(out, "Refreshing provider prerequisites...")
			continue
		}
		descriptor, found := providerregistry.DescriptorFor(providerType)
		if !found {
			return "", sandboxpromotion.Record{}, nil, fmt.Errorf("provider %q has no registry contribution", providerType)
		}
		switch descriptor.WizardOnboarding {
		case provider.WizardOnboardingNone:
			return providerType, sandboxpromotion.Record{}, nil, nil
		case provider.WizardOnboardingCatthehackerDocker:
			profile, accepted, profileErr := promptDockerImageProfile(ctx, projectRoot, providerType, hostPlatform, out, reader)
			if profileErr != nil {
				return "", sandboxpromotion.Record{}, nil, profileErr
			}
			if !accepted {
				return "", sandboxpromotion.Record{}, nil, fmt.Errorf("%s image setup did not complete; no config was written", providerType)
			}
			return providerType, sandboxpromotion.Record{}, profile, nil
		default:
			return "", sandboxpromotion.Record{}, nil, fmt.Errorf("provider %q has unsupported image onboarding strategy %q", providerType, descriptor.WizardOnboarding)
		}
	}
}

func promptInitProviderChoice(ctx context.Context, projectRoot string, hostPlatform sandboxpromotion.Platform, record sandboxpromotion.Record, promoted bool, out io.Writer, reader *bufio.Reader, skipDockerCheck bool) (string, bool, bool, error) {
	result, promotionPassed, err := promptInitProviderChoiceWizard(ctx, projectRoot, hostPlatform, record, promoted, out, reader, skipDockerCheck, false, "")
	if err != nil {
		return "", false, false, err
	}
	return result.Value, promotionPassed, result.Action == initWizardRefresh, nil
}

func promptInitProviderChoiceWizard(ctx context.Context, projectRoot string, hostPlatform sandboxpromotion.Platform, record sandboxpromotion.Record, promoted bool, out io.Writer, reader *bufio.Reader, skipDockerCheck, allowBack bool, preferredProvider string) (initWizardResult[string], bool, error) {
	prerequisites := detectInitProviderPrerequisites(ctx, hostPlatform, skipDockerCheck)
	operationalDefault := !promoted && prerequisites.DockerSandboxesAvailable
	promotionPassed := false
	var promotionFailures []sandboxpromotion.Failure
	if promoted {
		var preflight sandboxpromotion.PreflightResult
		if os.Getenv(sandboxpromotion.DisableEnvironment) == "1" {
			preflight.Failures = append(preflight.Failures, sandboxpromotion.Failure{
				Gate:       "operator kill switch",
				Detail:     sandboxpromotion.DisableEnvironment + "=1 disables Docker Sandboxes admission and automatic selection",
				Resolution: "Unset the kill switch only after the Docker Sandboxes issue is resolved, or explicitly choose another provider.",
			})
		} else if err := sandboxpromotion.Validate(record); err != nil {
			preflight.Failures = append(preflight.Failures, sandboxpromotion.Failure{
				Gate:       "promotion record",
				Detail:     err.Error(),
				Resolution: "Explicitly choose Docker Container or another provider and report the invalid embedded promotion record.",
			})
		} else if prerequisites.DockerSandboxesAvailable {
			preflightContext, cancel := context.WithTimeout(ctx, 45*time.Second)
			preflight = initDockerSandboxesPreflight(preflightContext, record, projectRoot)
			cancel()
		}
		promotionPassed = preflight.Passed() && prerequisites.DockerSandboxesAvailable
		promotionFailures = preflight.Failures
		fmt.Fprintln(out, "")
		fmt.Fprintln(out, "Docker Sandboxes automatic-default preflight:")
		if promotionPassed {
			fmt.Fprintln(out, "  PASS: the exact promoted platform, host, sbx, template, policy, and resource gates passed.")
		} else {
			for _, failure := range promotionFailures {
				fmt.Fprintf(out, "  FAIL [%s]: %s\n", failure.Gate, failure.Detail)
				fmt.Fprintf(out, "    Action: %s\n", failure.Resolution)
			}
			if len(promotionFailures) == 0 {
				fmt.Fprintf(out, "  FAIL [prerequisites]: %s\n", prerequisites.DockerSandboxesStatus)
			}
			prerequisites.DockerSandboxesAvailable = false
			prerequisites.DockerSandboxesStatus = "UNAVAILABLE — promoted admission did not pass; review the failures above"
		}
	}

	defaultProvider := "docker-container"
	if promotionPassed || operationalDefault {
		defaultProvider = "docker-sandboxes"
	} else if promoted || !prerequisites.DockerAvailable {
		defaultProvider = ""
	}
	result, err := promptProviderOptionsWizard(out, reader, prerequisites, promoted, promotionPassed, operationalDefault, defaultProvider, allowBack, preferredProvider)
	if err != nil {
		return initWizardResult[string]{}, false, err
	}
	return result, promotionPassed, nil
}

func promptDockerSandboxesProfile(ctx context.Context, projectRoot string, hostPlatform sandboxpromotion.Platform, out io.Writer, reader *bufio.Reader) (*initDockerSandboxesProfile, bool, error) {
	return promptDockerImageProfile(ctx, projectRoot, "docker-sandboxes", hostPlatform, out, reader)
}

func promptDockerImageProfileWizard(ctx context.Context, projectRoot, providerType string, hostPlatform sandboxpromotion.Platform, out io.Writer, reader *bufio.Reader) (initWizardResult[*initDockerSandboxesProfile], *initArtifactEstimate, error) {
	guestPlatform, err := initDockerGuestPlatform(providerType, hostPlatform)
	if err != nil {
		return initWizardResult[*initDockerSandboxesProfile]{}, nil, fmt.Errorf("%s image setup is unavailable: %w", providerType, err)
	}
	descriptor, found := providerregistry.DescriptorFor(providerType)
	if !found || !descriptor.GuidedArtifacts || len(descriptor.WizardImageProfiles) == 0 {
		return initWizardResult[*initDockerSandboxesProfile]{}, nil, fmt.Errorf("%s has no registered guided image onboarding contribution", providerType)
	}
	fmt.Fprintln(out, "")
	fmt.Fprintf(out, "%s image setup:\n", descriptor.DisplayName)
	fmt.Fprintln(out, "  Choose the Catthehacker Ubuntu image for this runner. EPAR will provision or update the reusable runner artifact during startup.")
	if !descriptor.WizardCustomImageTags {
		fmt.Fprintln(out, "  Docker Sandboxes profiles must include a private Docker daemon; specialized and custom tags are not admitted.")
	}
	fmt.Fprintln(out, "  Image catalog: https://github.com/catthehacker/docker_images#images-available")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Runner base image:")
	for index, profile := range descriptor.WizardImageProfiles {
		defaultLabel := ""
		if index == 0 {
			defaultLabel = " (default)"
		}
		fmt.Fprintf(out, "  %d. %s — %s%s\n", index+1, profile.Name, profile.Tag, defaultLabel)
	}
	customChoice := ""
	if descriptor.WizardCustomImageTags {
		customChoice = strconv.Itoa(len(descriptor.WizardImageProfiles) + 1)
		fmt.Fprintf(out, "  %s. Another catthehacker/ubuntu tag, such as go-24.04\n", customChoice)
	}
	fmt.Fprintln(out, "  0. Back")

	var source imageartifact.ResolvedDockerSource
	for {
		choiceResult, hitEOF, promptErr := promptWizardDefault(out, reader, "Runner base image", "1")
		if promptErr != nil {
			return initWizardResult[*initDockerSandboxesProfile]{}, nil, promptErr
		}
		if choiceResult.Action == initWizardBack {
			return initWizardResult[*initDockerSandboxesProfile]{Action: initWizardBack}, nil, nil
		}
		choice := choiceResult.Value
		if choice == "0" {
			return initWizardResult[*initDockerSandboxesProfile]{Action: initWizardBack}, nil, nil
		}
		normalizedChoice := strings.ToLower(choice)
		input := ""
		for index, profile := range descriptor.WizardImageProfiles {
			if normalizedChoice == strconv.Itoa(index+1) || normalizedChoice == profile.Name {
				input = profile.Name
				break
			}
		}
		if customChoice != "" && normalizedChoice == customChoice {
			var requiredResult initWizardResult[string]
			requiredResult, promptErr = promptWizardRequired(out, reader, "catthehacker/ubuntu tag")
			if promptErr != nil {
				return initWizardResult[*initDockerSandboxesProfile]{}, nil, promptErr
			}
			if requiredResult.Action == initWizardBack {
				return initWizardResult[*initDockerSandboxesProfile]{Action: initWizardBack}, nil, nil
			}
			input = requiredResult.Value
		}
		if input == "" {
			if customChoice != "" {
				fmt.Fprintf(out, "  Choose a built-in image from 1 to %d, %s for another catthehacker/ubuntu tag, or 0 to go back.\n", len(descriptor.WizardImageProfiles), customChoice)
			} else {
				fmt.Fprintf(out, "  Choose a supported image from 1 to %d or 0 to go back.\n", len(descriptor.WizardImageProfiles))
			}
			if hitEOF {
				return initWizardResult[*initDockerSandboxesProfile]{}, nil, fmt.Errorf("invalid runner base image %q", choice)
			}
			continue
		}
		resolveContext, cancel := context.WithTimeout(ctx, 90*time.Second)
		source, err = initResolveDockerSandboxesSource(resolveContext, input, guestPlatform)
		cancel()
		if err == nil {
			break
		}
		fmt.Fprintf(out, "  That image cannot be used for %s: %v\n", guestPlatform, err)
		fmt.Fprintln(out, "  Choose an existing ghcr.io/catthehacker/ubuntu tag that publishes this platform.")
		if hitEOF {
			return initWizardResult[*initDockerSandboxesProfile]{}, nil, fmt.Errorf("resolve runner source image: %w", err)
		}
	}

	policyFingerprint := ""
	if providerType == "docker-sandboxes" {
		policyFingerprint, err = initDockerSandboxesPolicyFingerprint(ctx)
		if err != nil {
			return initWizardResult[*initDockerSandboxesProfile]{}, nil, err
		}
	}
	sourceEstimate, err := imageartifact.EstimateSourceSize(source.CompressedLayerBytes, 0)
	if err != nil {
		return initWizardResult[*initDockerSandboxesProfile]{}, nil, err
	}
	const dockerDisk = config.DockerSandboxesDefaultDockerDisk
	dockerDiskBytes, _ := config.ParseByteSize(dockerDisk)
	artifactPlan, err := imageartifact.PlanArtifactStorage(providerType, sourceEstimate, false, uint64(dockerDiskBytes))
	if err != nil {
		return initWizardResult[*initDockerSandboxesProfile]{}, nil, err
	}
	var availableText = "unknown"
	if capacity, probeErr := storage.ProbeFilesystemCapacity(projectRoot, time.Now()); probeErr == nil && capacity.Known {
		availableText = formatInitUintByteCount(capacity.AvailableBytes)
	}
	estimate := &initArtifactEstimate{
		Source:                 source.Reference,
		Platform:               source.Platform,
		DownloadBytes:          source.CompressedLayerBytes,
		ExpandedBytes:          sourceEstimate.ExpandedBytes,
		IncrementalPeakBytes:   artifactPlan.EstimatedIncrementalPeak,
		AvailablePhysicalSpace: availableText,
	}
	if providerType == "docker-sandboxes" {
		estimate.LogicalRootMaximumBytes = artifactPlan.LogicalRootMaximumBytes
		estimate.LogicalDockerMaximumBytes = artifactPlan.LogicalDockerMaximumBytes
	}
	profile := &initDockerSandboxesProfile{
		Provider:          providerType,
		HostPlatform:      hostPlatform,
		GuestPlatform:     guestPlatform,
		SourceImage:       source.Reference,
		PolicyFingerprint: policyFingerprint,
		RootDisk:          config.DockerSandboxesAutomaticRootDisk,
		DockerDisk:        dockerDisk,
	}
	return initWizardResult[*initDockerSandboxesProfile]{Action: initWizardNext, Value: profile}, estimate, nil
}

func promptDockerImageProfile(ctx context.Context, projectRoot, providerType string, hostPlatform sandboxpromotion.Platform, out io.Writer, reader *bufio.Reader) (*initDockerSandboxesProfile, bool, error) {
	result, estimate, err := promptDockerImageProfileWizard(ctx, projectRoot, providerType, hostPlatform, out, reader)
	if err != nil {
		return nil, false, err
	}
	if result.Action == initWizardBack {
		return nil, false, nil
	}
	renderInitArtifactEstimate(out, estimate)
	return result.Value, true, nil
}

func initDockerGuestPlatform(providerType string, hostPlatform sandboxpromotion.Platform) (string, error) {
	if providerType == "docker-sandboxes" {
		platform, _, err := dockerSandboxesPlatform(hostPlatform)
		return platform, err
	}
	_, architecture, found := strings.Cut(string(hostPlatform), "/")
	if !found {
		return "", fmt.Errorf("unsupported controller platform %q", hostPlatform)
	}
	switch architecture {
	case "amd64", "arm64":
		return "linux/" + architecture, nil
	default:
		return "", fmt.Errorf("unsupported Docker image architecture %q", architecture)
	}
}

func checkInitDockerSandboxesCapacity(rootDisk, dockerDisk, minHostFreeSpace uint64) (initDockerSandboxesCapacityResult, error) {
	_ = rootDisk
	_ = dockerDisk
	storageRoot, err := sandboxcapacity.DockerSandboxesStorageRoot()
	if err != nil {
		return initDockerSandboxesCapacityResult{}, err
	}
	probePath := storageRoot
	for {
		if _, statErr := os.Lstat(probePath); statErr == nil {
			break
		} else if !os.IsNotExist(statErr) {
			return initDockerSandboxesCapacityResult{}, fmt.Errorf("inspect %s: %w", probePath, statErr)
		}
		parent := filepath.Dir(probePath)
		if parent == probePath {
			return initDockerSandboxesCapacityResult{}, fmt.Errorf("find existing filesystem ancestor for %s", storageRoot)
		}
		probePath = parent
	}
	capacity, err := storage.ProbeFilesystemCapacity(probePath, time.Now())
	if err != nil {
		return initDockerSandboxesCapacityResult{}, fmt.Errorf("probe %s for %s: %w", probePath, storageRoot, err)
	}
	reservation := uint64(0)
	hostWatermark, err := sandboxcapacity.HostWatermark(minHostFreeSpace, capacity.TotalBytes)
	if err != nil {
		return initDockerSandboxesCapacityResult{}, err
	}
	check, err := storage.EvaluateCapacity(storage.Surface{
		ID:       "docker-sandboxes-backing",
		Provider: "docker-sandboxes",
		Kind:     storage.SurfaceSandboxCache,
		Location: storageRoot,
		Capacity: capacity,
	}, storage.Requirement{
		ID:               "docker-sandboxes-instance-create",
		Provider:         "docker-sandboxes",
		SurfaceID:        "docker-sandboxes-backing",
		PeakBytes:        reservation,
		MinimumFreeBytes: hostWatermark,
	})
	if err != nil {
		return initDockerSandboxesCapacityResult{}, err
	}
	return initDockerSandboxesCapacityResult{
		StorageRoot:    storageRoot,
		AvailableBytes: capacity.AvailableBytes,
		TotalBytes:     capacity.TotalBytes,
		Reservation:    reservation,
		HostWatermark:  hostWatermark,
		RequiredBytes:  check.RequiredAvailableBytes,
		DeficitBytes:   check.DeficitBytes,
		CapacityStatus: check.Status,
	}, nil
}

func promptInitByteSize(out io.Writer, reader *bufio.Reader, label, defaultValue string, minimum int64) (string, error) {
	for {
		value, hitEOF, err := promptDefault(out, reader, label, defaultValue)
		if err != nil {
			return "", err
		}
		parsed, parseErr := config.ParseByteSize(value)
		if parseErr == nil && parsed >= minimum {
			return value, nil
		}
		if parseErr != nil {
			fmt.Fprintf(out, "%s is invalid: %v\n", label, parseErr)
		} else {
			fmt.Fprintf(out, "%s must be at least %s.\n", label, formatInitByteCount(minimum))
		}
		if hitEOF {
			return "", fmt.Errorf("invalid %s %q", strings.ToLower(label), value)
		}
	}
}

func formatInitByteCount(value int64) string {
	const (
		gib = int64(1 << 30)
		mib = int64(1 << 20)
	)
	if value >= gib {
		if value%gib == 0 {
			return strconv.FormatInt(value/gib, 10) + "GiB"
		}
		return strconv.FormatFloat(float64(value)/float64(gib), 'f', 2, 64) + "GiB"
	}
	if value >= mib {
		if value%mib == 0 {
			return strconv.FormatInt(value/mib, 10) + "MiB"
		}
		return strconv.FormatFloat(float64(value)/float64(mib), 'f', 2, 64) + "MiB"
	}
	return strconv.FormatInt(value, 10) + "B"
}

func formatInitUintByteCount(value uint64) string {
	if value <= math.MaxInt64 {
		return formatInitByteCount(int64(value))
	}
	return strconv.FormatFloat(float64(value)/float64(uint64(1)<<30), 'f', 2, 64) + "GiB"
}

func dockerSandboxesRootMeasurement(hostPlatform sandboxpromotion.Platform, template initDockerSandboxesTemplate) (initDockerSandboxesRootMeasurement, bool) {
	const fullTemplateDigest = "sha256:00303a3e249a1baf8b0585d20273af408c27182dcfc827a98aa25ffe66b1f67f"
	if hostPlatform != sandboxpromotion.WindowsAMD64 || template.Digest != fullTemplateDigest || template.Platform != "linux/amd64" {
		return initDockerSandboxesRootMeasurement{}, false
	}
	return initDockerSandboxesRootMeasurement{
		PeakBytes: 324_780_032,
		Evidence:  "exact full-template Buildx and Compose validation probe on Docker Sandboxes",
	}, true
}

func discoverDockerSandboxes(ctx context.Context, projectRoot, guestPlatform string) (initDockerSandboxesDiscovery, error) {
	profiles, err := readDockerSandboxesActiveProfiles(projectRoot, guestPlatform)
	if err != nil {
		return initDockerSandboxesDiscovery{}, err
	}
	adapter := dockersandboxes.New("")
	if err := adapter.VerifyAdmission(ctx); err != nil {
		return initDockerSandboxesDiscovery{}, fmt.Errorf("Docker Sandboxes admission check failed: %w", err)
	}
	cached, err := adapter.CachedTemplates(ctx)
	if err != nil {
		return initDockerSandboxesDiscovery{}, fmt.Errorf("read Docker Sandboxes template inventory: %w", err)
	}
	cachedByReference := make(map[string]dockersandboxes.CachedTemplate, len(cached))
	for _, template := range cached {
		reference, canonicalErr := canonicalDockerSandboxesTemplateReference(template.Reference)
		if canonicalErr != nil {
			continue
		}
		cachedByReference[reference] = template
	}
	var templates []initDockerSandboxesTemplate
	for _, profile := range profiles {
		template, found := cachedByReference[profile.TemplateReference]
		if !found {
			continue
		}
		templates = append(templates, initDockerSandboxesTemplate{
			Reference:     template.Reference,
			Digest:        "",
			CacheID:       template.CacheID,
			Platform:      guestPlatform,
			Size:          template.SizeBytes,
			Label:         profile.DisplayLabel,
			SourceChannel: profile.ObservedTag,
		})
	}
	rules, err := adapter.ReadGlobalNetworkPolicy(ctx)
	if err != nil {
		return initDockerSandboxesDiscovery{}, fmt.Errorf("read Docker Sandboxes global policy: %w", err)
	}
	fingerprint, err := sandboxpolicy.Fingerprint(rules)
	if err != nil {
		return initDockerSandboxesDiscovery{}, fmt.Errorf("fingerprint Docker Sandboxes global policy: %w", err)
	}
	if err := sandboxpolicy.VerifyBaseline(fingerprint, "epar-preview-policy-probe", rules); err != nil {
		return initDockerSandboxesDiscovery{}, fmt.Errorf("verify Docker Sandboxes global policy: %w", err)
	}
	return initDockerSandboxesDiscovery{Templates: templates, PolicyFingerprint: fingerprint}, nil
}

func readDockerSandboxesActiveProfiles(projectRoot, guestPlatform string) ([]dockerSandboxesActiveProfile, error) {
	if projectRoot == "" {
		return nil, errors.New("read Docker Sandboxes source lock: project root is empty")
	}
	lockPath := filepath.Join(projectRoot, "templates", "docker-sandboxes", "sources.lock.json")
	contents, err := os.ReadFile(lockPath)
	if err != nil {
		return nil, fmt.Errorf("read Docker Sandboxes source lock %q: %w", lockPath, err)
	}
	var lock dockerSandboxesSourceLock
	if err := json.Unmarshal(contents, &lock); err != nil {
		return nil, fmt.Errorf("parse Docker Sandboxes source lock %q: %w", lockPath, err)
	}
	if lock.SchemaVersion != 2 {
		return nil, fmt.Errorf("read Docker Sandboxes source lock %q: unsupported schemaVersion %d", lockPath, lock.SchemaVersion)
	}
	if len(lock.Profiles) == 0 {
		return nil, fmt.Errorf("read Docker Sandboxes source lock %q: no active profiles", lockPath)
	}
	profileOrder := []string{"full", "act-22.04"}
	expectedSourceChannels := map[string]string{
		"full":      "ghcr.io/catthehacker/ubuntu:full-latest",
		"act-22.04": "ghcr.io/catthehacker/ubuntu:act-22.04",
	}
	profiles := make([]dockerSandboxesActiveProfile, 0, len(profileOrder))
	for _, name := range profileOrder {
		profile, found := lock.Profiles[name]
		if !found {
			return nil, fmt.Errorf("read Docker Sandboxes source lock %q: active profile %q is missing", lockPath, name)
		}
		platform, found := profile.Platforms[guestPlatform]
		if !found || platform.TemplateTag == "" {
			return nil, fmt.Errorf("read Docker Sandboxes source lock %q: active profile %q has no templateTag for %s", lockPath, name, guestPlatform)
		}
		reference, err := canonicalDockerSandboxesTemplateReference(platform.TemplateTag)
		if err != nil {
			return nil, fmt.Errorf("read Docker Sandboxes source lock %q: active profile %q has invalid templateTag: %w", lockPath, name, err)
		}
		if profile.ObservedTagReference != expectedSourceChannels[name] {
			return nil, fmt.Errorf("read Docker Sandboxes source lock %q: active profile %q has unexpected observedTagReference %q", lockPath, name, profile.ObservedTagReference)
		}
		label := "Catthehacker Ubuntu Act 22.04"
		if name == "full" {
			label = "Catthehacker Ubuntu Full"
		}
		profiles = append(profiles, dockerSandboxesActiveProfile{
			Name:              name,
			ObservedTag:       profile.ObservedTagReference,
			TemplateReference: reference,
			DisplayLabel:      label,
		})
	}
	if len(lock.Profiles) != len(profiles) {
		return nil, fmt.Errorf("read Docker Sandboxes source lock %q: active profiles must be exactly full and act-22.04", lockPath)
	}
	profiles[0].DisplayLabel += " (recommended)"
	profiles[1].DisplayLabel += " (current lean profile)"
	return profiles, nil
}

func canonicalDockerSandboxesTemplateReference(reference string) (string, error) {
	if strings.HasPrefix(reference, "docker.io/library/") {
		reference = strings.TrimPrefix(reference, "docker.io/library/")
	}
	if strings.ContainsAny(reference, "@/ \t\r\n") || !strings.HasPrefix(reference, "epar-docker-sandboxes-catthehacker-") || strings.Count(reference, ":") != 1 {
		return "", fmt.Errorf("must be an EPAR repository:tag reference, got %q", reference)
	}
	parts := strings.SplitN(reference, ":", 2)
	if parts[0] == "" || parts[1] == "" {
		return "", fmt.Errorf("must include a repository and tag, got %q", reference)
	}
	return "docker.io/library/" + reference, nil
}

func detectInitProviderPrerequisites(ctx context.Context, hostPlatform sandboxpromotion.Platform, skipDockerCheck bool) initProviderPrerequisites {
	result := initProviderPrerequisites{}
	if skipDockerCheck {
		result.DockerAvailable = true
		result.DockerStatus = "AVAILABLE — Docker check skipped by --skip-docker-check"
	} else {
		dockerContext, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := dockerAvailable(dockerContext)
		cancel()
		if err == nil {
			result.DockerAvailable = true
			result.DockerStatus = "READY — Docker CLI and daemon are available"
		} else {
			result.DockerStatus = fmt.Sprintf("UNAVAILABLE — Docker CLI or daemon check failed: %v", err)
		}
	}

	if _, _, err := dockerSandboxesPlatform(hostPlatform); err != nil {
		result.DockerSandboxesStatus = fmt.Sprintf("UNAVAILABLE — %v", err)
	} else if os.Getenv(sandboxpromotion.DisableEnvironment) == "1" {
		result.DockerSandboxesStatus = "UNAVAILABLE — " + sandboxpromotion.DisableEnvironment + "=1 disables admission"
	} else if !result.DockerAvailable {
		result.DockerSandboxesStatus = "UNAVAILABLE — Docker CLI and daemon are required"
	} else {
		readinessContext, cancel := context.WithTimeout(ctx, 30*time.Second)
		readiness, err := initDockerSandboxesReadiness(readinessContext)
		cancel()
		if err == nil {
			result.DockerSandboxesAvailable = true
			result.DockerSandboxesStatus = fmt.Sprintf("READY — Docker and sbx diagnostics passed (%d pass, %d warn, %d fail, %d skip)", readiness.ChecksPassed, readiness.ChecksWarned, readiness.ChecksFailed, readiness.ChecksSkipped)
		} else {
			result.DockerSandboxesStatus = fmt.Sprintf("UNAVAILABLE — sbx readiness failed: %v", err)
			if !strings.Contains(err.Error(), "sbx diagnose --output json") {
				result.DockerSandboxesStatus += ". Run 'sbx diagnose --output json' and review the hints for each failed check."
			}
		}
	}

	switch {
	case initGOOS != "windows":
		result.WSLStatus = "UNAVAILABLE — native Windows, WSL2, and Docker are required"
	case !result.DockerAvailable:
		result.WSLStatus = "UNAVAILABLE — Docker CLI and daemon are required"
	case !wsl2Available():
		result.WSLStatus = "UNAVAILABLE — wsl.exe must report Default Version: 2"
	default:
		result.WSLAvailable = true
		result.WSLStatus = "READY — Docker and WSL2 are available"
	}

	switch {
	case initGOOS != "darwin":
		result.TartStatus = "UNAVAILABLE — native macOS and tart are required"
	case !tartAvailable():
		result.TartStatus = "UNAVAILABLE — tart --version failed"
	default:
		result.TartAvailable = true
		result.TartStatus = "READY — tart is available"
	}
	return result
}

func promptProviderOptions(out io.Writer, reader *bufio.Reader, prerequisites initProviderPrerequisites, promoted, promotionPassed, operationalDefault bool, defaultProvider string) (string, bool, error) {
	result, err := promptProviderOptionsWizard(out, reader, prerequisites, promoted, promotionPassed, operationalDefault, defaultProvider, false, "")
	if err != nil {
		return "", false, err
	}
	return result.Value, result.Action == initWizardRefresh, nil
}

func promptProviderOptionsWizard(out io.Writer, reader *bufio.Reader, prerequisites initProviderPrerequisites, promoted, promotionPassed, operationalDefault bool, defaultProvider string, allowBack bool, preferredProvider string) (initWizardResult[string], error) {
	options := make([]initProviderOption, 0, len(providerregistry.Descriptors()))
	for _, descriptor := range providerregistry.Descriptors() {
		option := initProviderOption{
			Number:  descriptor.WizardNumber,
			Type:    descriptor.Type,
			Label:   descriptor.WizardLabel,
			Aliases: append([]string(nil), descriptor.WizardAliases...),
		}
		switch descriptor.WizardPrerequisite {
		case provider.WizardPrerequisiteDocker:
			option.Available = prerequisites.DockerAvailable
			option.Status = prerequisites.DockerStatus
		case provider.WizardPrerequisiteDockerSandboxes:
			option.Available = prerequisites.DockerSandboxesAvailable && (!promoted || promotionPassed)
			option.Status = prerequisites.DockerSandboxesStatus
			if operationalDefault {
				option.Label = "Docker Sandboxes — recommended"
			} else if promoted {
				option.Label = "Docker Sandboxes (independently certified for this exact platform)"
			}
		case provider.WizardPrerequisiteWSL2:
			option.Available = prerequisites.WSLAvailable
			option.Status = prerequisites.WSLStatus
		case provider.WizardPrerequisiteTart:
			option.Available = prerequisites.TartAvailable
			option.Status = prerequisites.TartStatus
		default:
			return initWizardResult[string]{}, fmt.Errorf("registered provider %q has no prerequisite contribution", descriptor.Type)
		}
		options = append(options, option)
	}
	if err := validateWizardProviderOptions(options); err != nil {
		return initWizardResult[string]{}, err
	}
	if preferredProvider != "" {
		defaultProvider = preferredProvider
	}
	defaultNumber := prioritizeDefaultProviderOption(options, defaultProvider)

	fmt.Fprintln(out, "")
	if defaultNumber == "" {
		fmt.Fprintln(out, "Runner provider (explicit choice required):")
	} else {
		fmt.Fprintln(out, "Runner provider:")
	}
	for _, option := range options {
		defaultLabel := ""
		if option.Default {
			defaultLabel = " (default)"
		}
		fmt.Fprintf(out, "  %s. %s%s\n", option.Number, option.Label, defaultLabel)
		fmt.Fprintf(out, "     Prerequisites: %s\n", option.Status)
	}
	fmt.Fprintln(out, "  R. Refresh provider prerequisites")
	if allowBack {
		fmt.Fprintln(out, "  0. Back")
	}
	for {
		var value string
		var hitEOF bool
		var err error
		if defaultNumber == "" {
			fmt.Fprint(out, "Runner provider: ")
			value, err = reader.ReadString('\n')
			if err != nil && !errors.Is(err, io.EOF) {
				return initWizardResult[string]{}, err
			}
			hitEOF = errors.Is(err, io.EOF)
			if hitEOF {
				err = nil
			}
			value = strings.TrimSpace(value)
		} else {
			value, hitEOF, err = promptDefault(out, reader, "Runner provider", defaultNumber)
		}
		if err != nil {
			return initWizardResult[string]{}, err
		}
		normalized := strings.ToLower(value)
		if normalized == "r" || normalized == "refresh" {
			return initWizardResult[string]{Action: initWizardRefresh}, nil
		}
		if allowBack && (normalized == "0" || normalized == "back") {
			return initWizardResult[string]{Action: initWizardBack}, nil
		}
		var selected *initProviderOption
		for index := range options {
			option := &options[index]
			if normalized == option.Number || normalized == option.Type {
				selected = option
				break
			}
			for _, alias := range option.Aliases {
				if normalized == alias {
					selected = option
					break
				}
			}
			if selected != nil {
				break
			}
		}
		if selected != nil && selected.Available {
			return initWizardResult[string]{Action: initWizardNext, Value: selected.Type}, nil
		}
		if selected != nil {
			fmt.Fprintf(out, "%s is unavailable: %s\n", selected.Label, selected.Status)
		} else {
			fmt.Fprintln(out, "Choose an available provider number or name shown above, R to refresh, or 0 to go back when shown.")
		}
		if hitEOF {
			if selected != nil {
				return initWizardResult[string]{}, fmt.Errorf("runner provider %q is unavailable: %s", value, selected.Status)
			}
			return initWizardResult[string]{}, fmt.Errorf("invalid runner provider %q", value)
		}
	}
}

func prioritizeDefaultProviderOption(options []initProviderOption, defaultProvider string) string {
	defaultIndex := -1
	for index := range options {
		options[index].Default = false
		if options[index].Type == defaultProvider && options[index].Available {
			defaultIndex = index
		}
	}
	if defaultIndex > 0 {
		selected := options[defaultIndex]
		copy(options[1:defaultIndex+1], options[:defaultIndex])
		options[0] = selected
		defaultIndex = 0
	}
	for index := range options {
		options[index].Number = strconv.Itoa(index + 1)
	}
	if defaultIndex < 0 {
		return ""
	}
	options[defaultIndex].Default = true
	return options[defaultIndex].Number
}

func validateWizardProviderOptions(options []initProviderOption) error {
	registered := make(map[string]struct{}, len(options))
	for _, option := range options {
		descriptor, found := providerregistry.DescriptorFor(option.Type)
		if !found {
			return fmt.Errorf("wizard provider %q has no registry entry", option.Type)
		}
		if !descriptor.WizardSupported {
			return fmt.Errorf("wizard provider %q is not registered for onboarding", option.Type)
		}
		if option.Number != descriptor.WizardNumber || option.Label == "" || len(option.Aliases) == 0 {
			return fmt.Errorf("wizard provider %q does not use its complete registry contribution", option.Type)
		}
		if err := provider.ValidateWizardContributions(descriptor); err != nil {
			return fmt.Errorf("wizard provider %q has incomplete contributions: %w", option.Type, err)
		}
		if _, duplicate := registered[option.Type]; duplicate {
			return fmt.Errorf("wizard provider %q is duplicated", option.Type)
		}
		registered[option.Type] = struct{}{}
	}
	for _, descriptor := range providerregistry.Descriptors() {
		if descriptor.WizardSupported {
			if _, found := registered[descriptor.Type]; !found {
				return fmt.Errorf("registered provider %q has no ./start wizard option", descriptor.Type)
			}
		}
	}
	return nil
}

func providerDisplayName(providerType string) string {
	if descriptor, found := providerregistry.DescriptorFor(providerType); found {
		return descriptor.DisplayName
	}
	return providerType
}

func promptDefault(out io.Writer, reader *bufio.Reader, label string, defaultValue string) (string, bool, error) {
	fmt.Fprintf(out, "%s (press Enter to use %s): ", label, defaultValue)
	value, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", false, err
	}
	hitEOF := errors.Is(err, io.EOF)
	value = strings.TrimSpace(value)
	if strings.ContainsAny(value, "\r\n") {
		return "", hitEOF, fmt.Errorf("%s must be one line", label)
	}
	if value == "" {
		return defaultValue, hitEOF, nil
	}
	return value, hitEOF, nil
}

func promptOptional(out io.Writer, reader *bufio.Reader, label string) (string, error) {
	fmt.Fprintf(out, "%s (press Enter for none): ", label)
	value, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	value = strings.TrimSpace(value)
	if strings.ContainsAny(value, "\r\n") {
		return "", fmt.Errorf("%s must be one line", label)
	}
	return value, nil
}

func promptYesNo(out io.Writer, reader *bufio.Reader, label string, defaultYes bool) (bool, error) {
	defaultValue := "Y"
	if !defaultYes {
		defaultValue = "N"
	}
	for {
		value, hitEOF, err := promptDefault(out, reader, label+" [Y/n]", defaultValue)
		if err != nil {
			return false, err
		}
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		default:
			fmt.Fprintln(out, "Please answer yes or no.")
			if hitEOF {
				return false, fmt.Errorf("invalid yes/no response %q", value)
			}
		}
	}
}

func hostTrustScopesForOS(goos string) []string {
	if goos == "windows" || goos == "darwin" {
		return []string{config.HostTrustScopeSystem, config.HostTrustScopeUser}
	}
	return []string{config.HostTrustScopeSystem}
}

func generatedPoolNamePrefix() (string, error) {
	const (
		maxPrefixLength = 40
		randomHexLength = 6
		fallbackHost    = "runner"
	)
	randomPart, err := randomHex(randomHexLength)
	if err != nil {
		return "", fmt.Errorf("generate pool name prefix: %w", err)
	}
	hostPart := ""
	if hostname, err := initHostname(); err == nil {
		hostPart = config.SanitizeNamePart(hostname)
	}
	if hostPart == "" {
		hostPart = fallbackHost
	}
	maxHostPartLength := maxPrefixLength - 1 - randomHexLength
	if len(hostPart) > maxHostPartLength {
		hostPart = strings.TrimRight(hostPart[:maxHostPartLength], ".-_")
	}
	if hostPart == "" {
		hostPart = fallbackHost
	}
	return hostPart + "-" + randomPart, nil
}

func randomHex(length int) (string, error) {
	if length <= 0 || length%2 != 0 {
		return "", fmt.Errorf("random hex length must be a positive even number")
	}
	data := make([]byte, length/2)
	if n, err := initRandomRead(data); err != nil {
		return "", err
	} else if n != len(data) {
		return "", io.ErrUnexpectedEOF
	}
	return hex.EncodeToString(data), nil
}

func wsl2Available() bool {
	if initGOOS != "windows" {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	status, err := initWSLStatus(ctx)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(cleanWSLStatus(status), "\n") {
		if strings.EqualFold(strings.TrimSpace(line), "default version: 2") {
			return true
		}
	}
	return false
}

func cleanWSLStatus(data []byte) string {
	if len(data) == 0 {
		return ""
	}
	if looksUTF16LE(data) {
		if len(data)%2 != 0 {
			return ""
		}
		units := make([]uint16, 0, len(data)/2)
		for i := 0; i+1 < len(data); i += 2 {
			units = append(units, uint16(data[i])|uint16(data[i+1])<<8)
		}
		text := string(utf16.Decode(units))
		text = strings.TrimPrefix(text, "\ufeff")
		return strings.ReplaceAll(text, "\r\n", "\n")
	}
	text := strings.ReplaceAll(string(data), "\x00", "")
	return strings.ReplaceAll(text, "\r\n", "\n")
}

func looksUTF16LE(data []byte) bool {
	if len(data) >= 2 && data[0] == 0xff && data[1] == 0xfe {
		return true
	}
	if len(data) < 4 {
		return false
	}
	zeros := 0
	pairs := 0
	for i := 1; i < len(data); i += 2 {
		pairs++
		if data[i] == 0 {
			zeros++
		}
	}
	return pairs > 0 && zeros*2 >= pairs
}

func wslStatus(ctx context.Context) ([]byte, error) {
	const maxOutputBytes = 8 * 1024

	var output boundedBuffer
	output.limit = maxOutputBytes
	cmd := exec.CommandContext(ctx, "wsl.exe", "--status")
	cmd.Stdout = &output
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	if output.overflow {
		return nil, fmt.Errorf("wsl.exe --status output exceeds %d bytes", maxOutputBytes)
	}
	return output.Bytes(), nil
}

func tartAvailable() bool {
	if initGOOS != "darwin" {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return initTartVersion(ctx) == nil
}

func tartVersion(ctx context.Context) error {
	return exec.CommandContext(ctx, "tart", "--version").Run()
}

type boundedBuffer struct {
	bytes.Buffer
	limit    int
	overflow bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	remaining := b.limit - b.Len()
	if remaining <= 0 {
		b.overflow = true
		return len(p), nil
	}
	if len(p) > remaining {
		_, _ = b.Buffer.Write(p[:remaining])
		b.overflow = true
		return len(p), nil
	}
	return b.Buffer.Write(p)
}

func defaultDockerContainerConfig(appID int64, organization, privateKeyPath string, poolNamePrefix, hostTrustMode string, hostTrustScopes []string, runnerGroup initRunnerGroupSelection, profile initDockerSandboxesProfile, updatePolicy initImageUpdatePolicy) string {
	return fmt.Sprintf(`github:
  appId: %d
  organization: %s
  privateKeyPath: %s
  apiBaseUrl: https://api.github.com
  webBaseUrl: https://github.com

image:
  sourceType: docker-image
  sourceImage: %s
  sourcePlatform: %s
  outputImage: epar-docker-container-catthehacker-ubuntu
  upstreamDir: third_party/runner-images
  upstreamLock: third_party/runner-images.lock
  runnerVersion: latest
  updateFrequency: %s
  updateTime: "%s"
  hostTrustMode: %s
  hostTrustScopes: [%s]
  customInstallScripts:
%s

pool:
  instances: 1
  namePrefix: %s
  replacementRetryInitialSeconds: 15
  replacementRetryMaxSeconds: 1800
  replacementRetryMultiplier: 2
  replacementRetryJitterPercent: 20

storage:
  minimumFree: 1GiB
  gracePeriod: 168h
  keepPrevious: 0
  automaticHousekeeping: conservative
  buildCacheLimit: 20GiB
  goCacheLimit: 10GiB

logging:
  directory: work/logs
  managerSinks: [console]
  managerConsoleFormat: text
  managerConsoleTextFormat: "{time} [{level}] {message}"
  managerFileFormat: json
  transcriptSinks: [file]
  transcriptConsoleFormat: text
  maxFileSizeMiB: 100
  maxBackups: 3
  compressBackups: true
  retentionEnabled: true
  retentionMaxTotalMiB: 1024
  managerMaxAgeDays: 14
  instanceMaxAgeDays: 14
  buildMaxAgeDays: 14
  errorMaxAgeDays: 30
  benchmarkMaxAgeDays: 90
  retentionIntervalMinutes: 60

runner:
  group: %s
  labels: [self-hosted, linux, epar-docker-container-catthehacker-ubuntu]
  includeHostLabel: true
  ephemeral: true

security:
  runnerGroup:
    enforcement: %s
    requireExplicitGroup: %t
    requireNonDefaultGroup: %t
    requiredRepositoryAccess: %s
    requirePublicRepositoriesDisabled: %t

provider:
  type: docker-container
  sourceImage: epar-docker-container-catthehacker-ubuntu
  network: default

docker:
  registryMirrors:
    # - http://host.docker.internal:5050

timeouts:
  bootSeconds: 180
  githubOnlineSeconds: 180
  commandSeconds: 900
`, appID, organization, privateKeyPath, profile.SourceImage, profile.GuestPlatform, updatePolicy.Frequency, updatePolicy.Time, hostTrustMode, strings.Join(hostTrustScopes, ", "), renderInitCustomInstallScripts(profile.CustomScripts), poolNamePrefix, strconv.Quote(runnerGroup.Group.Name), runnerGroup.Policy.Enforcement, runnerGroup.Policy.RequireExplicitGroup, runnerGroup.Policy.RequireNonDefaultGroup, runnerGroup.Policy.RequiredRepositoryAccess, runnerGroup.Policy.RequirePublicRepositoriesDisabled)
}

func promotedDockerSandboxesPlatform(record sandboxpromotion.Record) (string, string, error) {
	return dockerSandboxesPlatform(record.Platform)
}

func dockerSandboxesPlatform(platform sandboxpromotion.Platform) (string, string, error) {
	hostOS, hostArch, found := strings.Cut(string(platform), "/")
	if !found || hostOS == "" || hostArch == "" || strings.Contains(hostArch, "/") {
		return "", "", fmt.Errorf("unsupported Docker Sandboxes controller platform %q", platform)
	}
	guestPlatform, err := config.DockerSandboxesGuestPlatform(hostOS, hostArch)
	if err != nil {
		return "", "", err
	}
	switch guestPlatform {
	case "linux/amd64":
		return guestPlatform, "X64", nil
	case "linux/arm64":
		return guestPlatform, "ARM64", nil
	default:
		return "", "", fmt.Errorf("Docker Sandboxes guest platform %q has no GitHub runner architecture label", guestPlatform)
	}
}

func defaultDockerSandboxesConfig(appID int64, organization, privateKeyPath string, poolNamePrefix, hostTrustMode string, hostTrustScopes []string, runnerGroup initRunnerGroupSelection, profile initDockerSandboxesProfile, updatePolicy initImageUpdatePolicy, guestPlatform, runnerArchitectureLabel string) string {
	return fmt.Sprintf(`github:
  appId: %d
  organization: %s
  privateKeyPath: %s
  apiBaseUrl: https://api.github.com
  webBaseUrl: https://github.com

image:
  sourceType: docker-image
  sourceImage: %s
  sourcePlatform: %s
  runnerVersion: latest
  updateFrequency: %s
  updateTime: "%s"
  customInstallScripts:
%s
  hostTrustMode: %s
  hostTrustScopes: [%s]

pool:
  instances: 1
  namePrefix: %s
  replacementRetryInitialSeconds: 15
  replacementRetryMaxSeconds: 1800
  replacementRetryMultiplier: 2
  replacementRetryJitterPercent: 20

storage:
  minimumFree: 1GiB
  gracePeriod: 168h
  keepPrevious: 0
  automaticHousekeeping: conservative
  buildCacheLimit: 20GiB
  goCacheLimit: 10GiB

logging:
  directory: work/logs
  managerSinks: [console]
  managerConsoleFormat: text
  managerConsoleTextFormat: "{time} [{level}] {message}"
  managerFileFormat: json
  transcriptSinks: [file]
  transcriptConsoleFormat: text
  maxFileSizeMiB: 100
  maxBackups: 3
  compressBackups: true
  retentionEnabled: true
  retentionMaxTotalMiB: 1024
  managerMaxAgeDays: 14
  instanceMaxAgeDays: 14
  buildMaxAgeDays: 14
  errorMaxAgeDays: 30
  benchmarkMaxAgeDays: 90
  retentionIntervalMinutes: 60

runner:
  group: %s
  labels: [self-hosted, linux, %s, epar-docker-sandboxes]
  includeHostLabel: true
  ephemeral: true

security:
  runnerGroup:
    enforcement: %s
    requireExplicitGroup: %t
    requireNonDefaultGroup: %t
    requiredRepositoryAccess: %s
    requirePublicRepositoriesDisabled: %t

provider:
  type: docker-sandboxes
  platform: %s

dockerSandboxes:
  policyGeneration: %s
  networkBaseline: open
  stagingRoot: .local/docker-sandboxes-staging
  cpus: 4
  memory: 8GiB
  rootDisk: %s
  dockerDisk: %s
  maxConcurrentCreates: 2

timeouts:
  bootSeconds: 180
  githubOnlineSeconds: 180
  commandSeconds: 900
`, appID, organization, privateKeyPath, profile.SourceImage, guestPlatform, updatePolicy.Frequency, updatePolicy.Time, renderInitCustomInstallScripts(profile.CustomScripts), hostTrustMode, strings.Join(hostTrustScopes, ", "), poolNamePrefix, strconv.Quote(runnerGroup.Group.Name), runnerArchitectureLabel, runnerGroup.Policy.Enforcement, runnerGroup.Policy.RequireExplicitGroup, runnerGroup.Policy.RequireNonDefaultGroup, runnerGroup.Policy.RequiredRepositoryAccess, runnerGroup.Policy.RequirePublicRepositoriesDisabled, guestPlatform, profile.PolicyFingerprint, profile.RootDisk, profile.DockerDisk)
}

func renderInitCustomInstallScripts(paths []string) string {
	if len(paths) == 0 {
		return "    # - examples/custom-install/install-extra-apt-tools.sh"
	}
	var lines []string
	for _, path := range paths {
		lines = append(lines, "    - "+strconv.Quote(path))
	}
	return strings.Join(lines, "\n")
}

func defaultWSLConfig(appID int64, organization, privateKeyPath string, poolNamePrefix string, runnerGroup initRunnerGroupSelection, profile initDockerSandboxesProfile, updatePolicy initImageUpdatePolicy) string {
	return fmt.Sprintf(`github:
  appId: %d
  organization: %s
  privateKeyPath: %s
  apiBaseUrl: https://api.github.com
  webBaseUrl: https://github.com

image:
  sourceType: docker-image
  sourceImage: %s
  sourcePlatform: %s
  outputImage: work/images/epar-wsl-catthehacker-ubuntu.tar
  upstreamDir: third_party/runner-images
  upstreamLock: third_party/runner-images.lock
  runnerVersion: latest
  updateFrequency: %s
  updateTime: "%s"
  customInstallScripts:
%s

pool:
  instances: 1
  # Must be unique for this machine/config within the GitHub organization.
  namePrefix: %s
  replacementRetryInitialSeconds: 15
  replacementRetryMaxSeconds: 1800
  replacementRetryMultiplier: 2
  replacementRetryJitterPercent: 20

storage:
  minimumFree: 1GiB
  gracePeriod: 168h
  keepPrevious: 0
  automaticHousekeeping: conservative
  buildCacheLimit: 20GiB
  goCacheLimit: 10GiB

logging:
  directory: work/logs
  managerSinks: [console]
  managerConsoleFormat: text
  managerConsoleTextFormat: "{time} [{level}] {message}"
  managerFileFormat: json
  transcriptSinks: [file]
  transcriptConsoleFormat: text
  maxFileSizeMiB: 100
  maxBackups: 3
  compressBackups: true
  retentionEnabled: true
  retentionMaxTotalMiB: 1024
  managerMaxAgeDays: 14
  instanceMaxAgeDays: 14
  buildMaxAgeDays: 14
  errorMaxAgeDays: 30
  benchmarkMaxAgeDays: 90
  retentionIntervalMinutes: 60

runner:
  group: %s
  labels: [self-hosted, linux, X64, epar-wsl-catthehacker-ubuntu]
  includeHostLabel: true
  ephemeral: true

security:
  runnerGroup:
    enforcement: %s
    requireExplicitGroup: %t
    requireNonDefaultGroup: %t
    requiredRepositoryAccess: %s
    requirePublicRepositoriesDisabled: %t

provider:
  type: wsl
  sourceImage: work/images/epar-wsl-catthehacker-ubuntu.tar
  network: default
  installRoot: work/wsl

docker:
  registryMirrors:
    # - https://mirror.example.test

timeouts:
  bootSeconds: 180
  githubOnlineSeconds: 180
  commandSeconds: 900
`, appID, organization, privateKeyPath, profile.SourceImage, profile.GuestPlatform, updatePolicy.Frequency, updatePolicy.Time, renderInitCustomInstallScripts(profile.CustomScripts), poolNamePrefix, strconv.Quote(runnerGroup.Group.Name), runnerGroup.Policy.Enforcement, runnerGroup.Policy.RequireExplicitGroup, runnerGroup.Policy.RequireNonDefaultGroup, runnerGroup.Policy.RequiredRepositoryAccess, runnerGroup.Policy.RequirePublicRepositoriesDisabled)
}

func defaultTartConfig(appID int64, organization, privateKeyPath string, poolNamePrefix string, runnerGroup initRunnerGroupSelection, updatePolicy initImageUpdatePolicy) string {
	return fmt.Sprintf(`# Experimental: this default is a basic Ubuntu ARM64 Tart VM, not a GitHub-hosted runner image.
# It does not include the broad dependency set from https://github.com/actions/runner-images.
github:
  appId: %d
  organization: %s
  privateKeyPath: %s
  apiBaseUrl: https://api.github.com
  webBaseUrl: https://github.com

image:
  sourceImage: ghcr.io/cirruslabs/ubuntu:latest
  outputImage: epar-ubuntu-24-arm64
  upstreamDir: third_party/runner-images
  upstreamLock: third_party/runner-images.lock
  runnerVersion: latest
  updateFrequency: %s
  updateTime: "%s"
  customInstallScripts:
    # - examples/custom-install/install-extra-apt-tools.sh

pool:
  instances: 1
  # Must be unique for this machine/config within the GitHub organization.
  namePrefix: %s
  replacementRetryInitialSeconds: 15
  replacementRetryMaxSeconds: 1800
  replacementRetryMultiplier: 2
  replacementRetryJitterPercent: 20

storage:
  minimumFree: 1GiB
  gracePeriod: 168h
  keepPrevious: 0
  automaticHousekeeping: conservative
  buildCacheLimit: 20GiB
  goCacheLimit: 10GiB

logging:
  directory: work/logs
  managerSinks: [console]
  managerConsoleFormat: text
  managerConsoleTextFormat: "{time} [{level}] {message}"
  managerFileFormat: json
  transcriptSinks: [file]
  transcriptConsoleFormat: text
  maxFileSizeMiB: 100
  maxBackups: 3
  compressBackups: true
  retentionEnabled: true
  retentionMaxTotalMiB: 1024
  managerMaxAgeDays: 14
  instanceMaxAgeDays: 14
  buildMaxAgeDays: 14
  errorMaxAgeDays: 30
  benchmarkMaxAgeDays: 90
  retentionIntervalMinutes: 60

runner:
  group: %s
  labels: [self-hosted, linux, ARM64, epar-tart-ubuntu-24.04-base]
  includeHostLabel: true
  ephemeral: true

security:
  runnerGroup:
    enforcement: %s
    requireExplicitGroup: %t
    requireNonDefaultGroup: %t
    requiredRepositoryAccess: %s
    requirePublicRepositoriesDisabled: %t

provider:
  type: tart
  sourceImage: epar-ubuntu-24-arm64
  network: default

docker:
  registryMirrors:
    # - https://mirror.example.test

timeouts:
  bootSeconds: 180
  githubOnlineSeconds: 180
  commandSeconds: 900
`, appID, organization, privateKeyPath, updatePolicy.Frequency, updatePolicy.Time, poolNamePrefix, strconv.Quote(runnerGroup.Group.Name), runnerGroup.Policy.Enforcement, runnerGroup.Policy.RequireExplicitGroup, runnerGroup.Policy.RequireNonDefaultGroup, runnerGroup.Policy.RequiredRepositoryAccess, runnerGroup.Policy.RequirePublicRepositoriesDisabled)
}

var stdinIsInteractive = func() bool {
	info, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}
