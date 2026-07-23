package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
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
	In                 io.Reader
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
	fmt.Fprintln(opts.Out, "This creates .local/config.yml for an EPAR runner.")
	fmt.Fprintln(opts.Out, "Before continuing, create a GitHub App with organization self-hosted runner read/write access.")
	fmt.Fprintln(opts.Out, "See README.md and docs/github-app.md for the GitHub App steps.")
	fmt.Fprintln(opts.Out, "")

	reader := bufio.NewReader(opts.In)
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
	runnerGroup, err := promptRunnerGroup(opts.Context, opts.Out, reader, newInitRunnerGroupClient(githubConfig))
	if err != nil {
		return err
	}
	providerType := "docker-container"
	if wsl2Available() {
		providerType, err = promptProviderType(opts.Out, reader, "wsl")
		if err != nil {
			return err
		}
	} else if tartAvailable() {
		providerType, err = promptProviderType(opts.Out, reader, "tart")
		if err != nil {
			return err
		}
	}
	if !opts.SkipDockerCheck && providerType != "tart" {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		dockerErr := dockerAvailable(ctx)
		cancel()
		if dockerErr != nil {
			return fmt.Errorf("Docker is required for the selected %s setup. Install Docker Desktop, Docker Engine, or a compatible Docker host, then rerun %s init", providerDisplayName(providerType), binaryName)
		}
	}
	defaultPrefix, err := generatedPoolNamePrefix()
	if err != nil {
		return err
	}
	fmt.Fprintln(opts.Out, "")
	fmt.Fprintln(opts.Out, "Pool name prefix must be unique for this machine/config within the GitHub organization.")
	fmt.Fprintln(opts.Out, "EPAR cleanup deletes GitHub runner records matching this prefix.")
	poolNamePrefix, err := promptPoolNamePrefix(opts.Out, reader, defaultPrefix)
	if err != nil {
		return err
	}

	hostTrustMode := config.HostTrustModeDisabled
	hostTrustScopes := []string{config.HostTrustScopeSystem}
	if providerType == "docker-container" {
		enabled, promptErr := promptYesNo(opts.Out, reader, "Inherit this host's trusted TLS roots into disposable runners?", true)
		if promptErr != nil {
			return promptErr
		}
		if enabled {
			hostTrustMode = config.HostTrustModeOverlay
			hostTrustScopes = hostTrustScopesForOS(initHostTrustOS)
			deferred := os.Getenv("EPAR_HOST_TRUST_INIT_DEFERRED") == "1"
			if !opts.SkipHostTrustCheck && !deferred {
				preflightCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				_, collectErr := initResolveHostTrust(preflightCtx, hosttrust.Options{
					Mode:             hostTrustMode,
					Scopes:           hostTrustScopes,
					ControllerHostOS: initHostTrustOS,
				})
				cancel()
				if collectErr != nil {
					return fmt.Errorf("collect host trusted TLS roots before writing config: %w", collectErr)
				}
			}
		}
	}

	content := defaultDockerContainerConfig(appID, organization, privateKeyPath, poolNamePrefix, hostTrustMode, hostTrustScopes, runnerGroup)
	switch providerType {
	case "wsl":
		content = defaultWSLConfig(appID, organization, privateKeyPath, poolNamePrefix, runnerGroup)
	case "tart":
		content = defaultTartConfig(appID, organization, privateKeyPath, poolNamePrefix, runnerGroup)
	}
	if err := os.MkdirAll(filepath.Dir(opts.ConfigPath), 0755); err != nil {
		return err
	}
	if err := os.WriteFile(opts.ConfigPath, []byte(content), 0600); err != nil {
		return err
	}

	fmt.Fprintf(opts.Out, `
Created %s

Next:
  %s start

Manual/advanced:
  %s image build --replace
  %s pool verify --instances 2 --register-only --cleanup
  %s pool up --instances 2
`, opts.ConfigPath, binaryName, binaryName, binaryName, binaryName)
	return nil
}

type initRunnerGroupSelection struct {
	Group  gh.RunnerGroup
	Policy config.RunnerGroupSecurityConfig
}

func promptRunnerGroup(ctx context.Context, out io.Writer, reader *bufio.Reader, client initRunnerGroupClient) (initRunnerGroupSelection, error) {
	var groups []gh.RunnerGroup
	var repositories map[int64][]gh.RunnerGroupRepository
	for {
		if groups == nil {
			var err error
			groups, repositories, err = loadInitRunnerGroups(ctx, client)
			if err != nil {
				return initRunnerGroupSelection{}, fmt.Errorf("load GitHub runner groups: %w", err)
			}
		}
		fmt.Fprintln(out, "")
		fmt.Fprintln(out, "GitHub runner group:")
		fmt.Fprintln(out, "  Choose which repositories GitHub may route to these self-hosted runners.")
		fmt.Fprintln(out, "  Groups are ordered from the most restrictive policy to the least restrictive policy; the first item is not an automatic default.")
		fmt.Fprintln(out, "")
		fmt.Fprintln(out, "  Repository access meanings:")
		fmt.Fprintln(out, "    Selected repositories: Only repositories explicitly added to the group can use its runners. This is recommended.")
		fmt.Fprintln(out, "    All private repositories: Every current and future private repository in the organization can use its runners.")
		fmt.Fprintln(out, "    All repositories: Every current and future repository allowed by the public-repository setting can use its runners.")
		fmt.Fprintln(out, "")
		fmt.Fprintln(out, "  Recommended policy: a non-default group, Selected repositories, trusted repositories only, and public repository access disabled.")
		fmt.Fprintln(out, "  See docs/runner-groups.md for details.")
		for i, group := range groups {
			printRunnerGroupChoice(out, i+1, group, repositories[group.ID])
		}
		fmt.Fprintln(out, "  R. Refresh runner groups")
		fmt.Fprintln(out, "  Q. Quit without writing a config")

		choice, err := promptRequired(out, reader, "Runner group choice")
		if err != nil {
			return initRunnerGroupSelection{}, err
		}
		switch strings.ToLower(choice) {
		case "r", "refresh":
			groups = nil
			repositories = nil
			continue
		case "q", "quit":
			return initRunnerGroupSelection{}, fmt.Errorf("runner-group selection cancelled; no config was written")
		}
		index, parseErr := strconv.Atoi(choice)
		if parseErr != nil || index < 1 || index > len(groups) {
			fmt.Fprintf(out, "Choose a runner group number, R to refresh, or Q to quit.\n")
			continue
		}
		group := groups[index-1]
		selectedRepositories := repositories[group.ID]
		_, publicCount := repositoryPrivacyCounts(selectedRepositories)
		if runnerGroupVisibilityRank(group.Visibility) == 3 {
			fmt.Fprintln(out, "")
			fmt.Fprintln(out, "*** SECURITY BLOCK: GITHUB RETURNED AN UNKNOWN REPOSITORY-ACCESS POLICY ***")
			fmt.Fprintf(out, "Runner group %q uses repository access %q, which this EPAR version cannot evaluate safely.\n", group.Name, group.Visibility)
			fmt.Fprintln(out, "RECOMMENDED ACTION: Do not select this group. Review its policy in GitHub, update EPAR if support is available, and choose Refresh; otherwise choose another documented group.")
			refresh, err := promptBackRefreshQuit(out, reader)
			if err != nil {
				return initRunnerGroupSelection{}, err
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
				return initRunnerGroupSelection{}, err
			}
			if refresh {
				groups = nil
				repositories = nil
			}
			continue
		}

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
				return initRunnerGroupSelection{}, err
			}
			if !continueSelection {
				continue
			}
		}
		return initRunnerGroupSelection{
			Group: group,
			Policy: config.RunnerGroupSecurityConfig{
				Enforcement:                       config.RunnerGroupEnforcementEnforce,
				RequireExplicitGroup:              true,
				RequireNonDefaultGroup:            !group.Default,
				RequiredRepositoryAccess:          group.Visibility,
				RequirePublicRepositoriesDisabled: true,
			},
		}, nil
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
		if left.AllowsPublicRepositories != right.AllowsPublicRepositories {
			return !left.AllowsPublicRepositories
		}
		if leftRank, rightRank := runnerGroupVisibilityRank(left.Visibility), runnerGroupVisibilityRank(right.Visibility); leftRank != rightRank {
			return leftRank < rightRank
		}
		if left.Default != right.Default {
			return !left.Default
		}
		if left.Inherited != right.Inherited {
			return !left.Inherited
		}
		return strings.ToLower(left.Name) < strings.ToLower(right.Name)
	})
	return ordered
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

func printRunnerGroupChoice(out io.Writer, number int, group gh.RunnerGroup, repositories []gh.RunnerGroupRepository) {
	privateCount, publicCount := repositoryPrivacyCounts(repositories)
	fmt.Fprintf(out, "\n  %d. %s\n", number, group.Name)
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
	switch {
	case group.AllowsPublicRepositories || publicCount > 0:
		fmt.Fprintln(out, "     Assessment: BLOCKED BY WIZARD — does not satisfy the public-repository safety requirement.")
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
	fmt.Fprintln(out, "  2. Back to group selection")
	for {
		choice, err := promptRequired(out, reader, "Choice")
		if err != nil {
			return false, err
		}
		switch strings.ToLower(choice) {
		case "1", "continue":
			return true, nil
		case "2", "back":
			return false, nil
		default:
			fmt.Fprintln(out, "Choose 1 to continue or 2 to go back.")
		}
	}
}

func promptBackRefreshQuit(out io.Writer, reader *bufio.Reader) (bool, error) {
	fmt.Fprintln(out, "  1. Back to group selection")
	fmt.Fprintln(out, "  2. Refresh runner groups")
	fmt.Fprintln(out, "  3. Quit without writing a config")
	for {
		choice, err := promptRequired(out, reader, "Choice")
		if err != nil {
			return false, err
		}
		switch strings.ToLower(choice) {
		case "1", "back":
			return false, nil
		case "2", "refresh":
			return true, nil
		case "3", "quit":
			return false, fmt.Errorf("runner-group selection cancelled; no config was written")
		default:
			fmt.Fprintln(out, "Choose 1 to go back, 2 to refresh, or 3 to quit.")
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

func promptProviderType(out io.Writer, reader *bufio.Reader, alternative string) (string, error) {
	alternativeLabel := providerDisplayName(alternative)
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Runner provider:")
	fmt.Fprintln(out, "  1. Docker Container — private daemon (default)")
	fmt.Fprintf(out, "  2. %s\n", alternativeLabel)
	for {
		value, hitEOF, err := promptDefault(out, reader, "Runner provider", "1")
		if err != nil {
			return "", err
		}
		switch strings.ToLower(value) {
		case "1", "docker", "docker-container":
			return "docker-container", nil
		case "2", alternative:
			return alternative, nil
		case "wsl2":
			if alternative == "wsl" {
				return alternative, nil
			}
		default:
			// Continue below so aliases that belong to an unavailable provider are rejected.
		}
		fmt.Fprintf(out, "Runner provider must be 1 (Docker Container — private daemon) or 2 (%s).\n", alternativeLabel)
		if hitEOF {
			return "", fmt.Errorf("invalid runner provider %q", value)
		}
	}
}

func providerDisplayName(providerType string) string {
	switch providerType {
	case "wsl":
		return "WSL2"
	case "tart":
		return "Tart (experimental)"
	default:
		return "Docker Container — private daemon"
	}
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

func defaultDockerContainerConfig(appID int64, organization, privateKeyPath string, poolNamePrefix, hostTrustMode string, hostTrustScopes []string, runnerGroup initRunnerGroupSelection) string {
	return fmt.Sprintf(`github:
  appId: %d
  organization: %s
  privateKeyPath: %s
  apiBaseUrl: https://api.github.com
  webBaseUrl: https://github.com

image:
  sourceType: docker-image
  sourceImage: ghcr.io/catthehacker/ubuntu:full-latest
  outputImage: epar-docker-container-catthehacker-ubuntu
  upstreamDir: third_party/runner-images
  upstreamLock: third_party/runner-images.lock
  runnerVersion: latest
  hostTrustMode: %s
  hostTrustScopes: [%s]
  customInstallScripts:

pool:
  instances: 1
  namePrefix: %s
  replacementRetryInitialSeconds: 15
  replacementRetryMaxSeconds: 1800
  replacementRetryMultiplier: 2
  replacementRetryJitterPercent: 20
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
`, appID, organization, privateKeyPath, hostTrustMode, strings.Join(hostTrustScopes, ", "), poolNamePrefix, strconv.Quote(runnerGroup.Group.Name), runnerGroup.Policy.Enforcement, runnerGroup.Policy.RequireExplicitGroup, runnerGroup.Policy.RequireNonDefaultGroup, runnerGroup.Policy.RequiredRepositoryAccess, runnerGroup.Policy.RequirePublicRepositoriesDisabled)
}

func defaultWSLConfig(appID int64, organization, privateKeyPath string, poolNamePrefix string, runnerGroup initRunnerGroupSelection) string {
	return fmt.Sprintf(`github:
  appId: %d
  organization: %s
  privateKeyPath: %s
  apiBaseUrl: https://api.github.com
  webBaseUrl: https://github.com

image:
  sourceType: docker-image
  sourceImage: ghcr.io/catthehacker/ubuntu:full-latest
  sourcePlatform: linux/amd64
  outputImage: work/images/epar-wsl-catthehacker-ubuntu.tar
  upstreamDir: third_party/runner-images
  upstreamLock: third_party/runner-images.lock
  runnerVersion: latest
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
`, appID, organization, privateKeyPath, poolNamePrefix, strconv.Quote(runnerGroup.Group.Name), runnerGroup.Policy.Enforcement, runnerGroup.Policy.RequireExplicitGroup, runnerGroup.Policy.RequireNonDefaultGroup, runnerGroup.Policy.RequiredRepositoryAccess, runnerGroup.Policy.RequirePublicRepositoriesDisabled)
}

func defaultTartConfig(appID int64, organization, privateKeyPath string, poolNamePrefix string, runnerGroup initRunnerGroupSelection) string {
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
`, appID, organization, privateKeyPath, poolNamePrefix, strconv.Quote(runnerGroup.Group.Name), runnerGroup.Policy.Enforcement, runnerGroup.Policy.RequireExplicitGroup, runnerGroup.Policy.RequireNonDefaultGroup, runnerGroup.Policy.RequiredRepositoryAccess, runnerGroup.Policy.RequirePublicRepositoriesDisabled)
}

var stdinIsInteractive = func() bool {
	info, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}
