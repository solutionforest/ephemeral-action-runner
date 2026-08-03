package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
	gh "github.com/solutionforest/ephemeral-action-runner/internal/github"
	"github.com/solutionforest/ephemeral-action-runner/internal/hosttrust"
	imageartifact "github.com/solutionforest/ephemeral-action-runner/internal/image"
	"github.com/solutionforest/ephemeral-action-runner/internal/provider/dockersandboxes"
	sandboxpromotion "github.com/solutionforest/ephemeral-action-runner/internal/provider/dockersandboxes/promotion"
	providerregistry "github.com/solutionforest/ephemeral-action-runner/internal/provider/registry"
	"github.com/solutionforest/ephemeral-action-runner/internal/storage"
)

func TestWizardCoversEveryRegisteredProvider(t *testing.T) {
	options := make([]initProviderOption, 0, len(providerregistry.Descriptors()))
	for _, descriptor := range providerregistry.Descriptors() {
		options = append(options, initProviderOption{
			Number:  descriptor.WizardNumber,
			Type:    descriptor.Type,
			Label:   descriptor.WizardLabel,
			Aliases: descriptor.WizardAliases,
		})
	}
	if err := validateWizardProviderOptions(options); err != nil {
		t.Fatal(err)
	}
}

func TestProviderWizardPutsAvailableDefaultFirst(t *testing.T) {
	options := []initProviderOption{
		{Number: "1", Type: "docker-container", Available: true},
		{Number: "2", Type: "docker-sandboxes", Available: true},
		{Number: "3", Type: "wsl", Available: true},
	}

	if got := prioritizeDefaultProviderOption(options, "docker-sandboxes"); got != "1" {
		t.Fatalf("default number = %q, want 1", got)
	}
	if options[0].Type != "docker-sandboxes" || !options[0].Default {
		t.Fatalf("first option = %+v, want Docker Sandboxes default", options[0])
	}
	if options[1].Type != "docker-container" || options[1].Number != "2" || options[1].Default {
		t.Fatalf("second option = %+v, want non-default Docker Container", options[1])
	}
	if options[2].Type != "wsl" || options[2].Number != "3" {
		t.Fatalf("third option = %+v, want WSL", options[2])
	}
}

func TestProviderWizardDoesNotPromoteUnavailableDefault(t *testing.T) {
	options := []initProviderOption{
		{Number: "1", Type: "docker-container", Available: true},
		{Number: "2", Type: "docker-sandboxes", Available: false},
	}

	if got := prioritizeDefaultProviderOption(options, "docker-sandboxes"); got != "" {
		t.Fatalf("default number = %q, want none", got)
	}
	if options[0].Type != "docker-container" || options[0].Number != "1" || options[0].Default {
		t.Fatalf("first option = %+v, want unchanged non-default Docker Container", options[0])
	}
}

func TestInitCreatesDefaultDockerContainerConfig(t *testing.T) {
	stubInitHostAndRandom(t, "Build Box 01", []byte{0xa4, 0xf9, 0xc2})
	stubNoWSL2(t)
	oldPreflight := initDockerSandboxesPreflight
	initDockerSandboxesPreflight = func(context.Context, sandboxpromotion.Record, string) sandboxpromotion.PreflightResult {
		t.Fatal("empty promotion table must not run Docker Sandboxes preflight")
		return sandboxpromotion.PreflightResult{}
	}
	t.Cleanup(func() { initDockerSandboxesPreflight = oldPreflight })

	dir := t.TempDir()
	path := filepath.Join(dir, ".local", "config.yml")
	var out bytes.Buffer

	if err := runInitWithOptions(initOptions{
		ProjectRoot:        dir,
		ConfigPath:         path,
		SkipDockerCheck:    true,
		SkipHostTrustCheck: true,
		In:                 strings.NewReader("123456\nsolutionforest\n.local/github-app.pem\n1\n\n"),
		Out:                &out,
	}); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GitHub.AppID != 123456 || cfg.GitHub.Organization != "solutionforest" || cfg.GitHub.PrivateKeyPath != ".local/github-app.pem" {
		t.Fatalf("unexpected GitHub config: %+v", cfg.GitHub)
	}
	if cfg.Runner.Group != "restricted group" {
		t.Fatalf("runner.group = %q, want selected group", cfg.Runner.Group)
	}
	policy := cfg.Security.RunnerGroup
	if policy.Enforcement != config.RunnerGroupEnforcementEnforce || !policy.RequireExplicitGroup || !policy.RequireNonDefaultGroup || policy.RequiredRepositoryAccess != config.RunnerGroupRepositoryAccessSelected || !policy.RequirePublicRepositoriesDisabled {
		t.Fatalf("unexpected generated runner-group policy: %+v", policy)
	}
	if got, want := cfg.Provider.Type, "docker-container"; got != want {
		t.Fatalf("provider.type = %q, want %q", got, want)
	}
	if got, want := cfg.Image.SourceImage, "ghcr.io/catthehacker/ubuntu:full-latest"; got != want {
		t.Fatalf("image.sourceImage = %q, want %q", got, want)
	}
	if got, want := cfg.Image.OutputImage, "epar-docker-container-catthehacker-ubuntu"; got != want {
		t.Fatalf("image.outputImage = %q, want %q", got, want)
	}
	if got, want := cfg.Image.HostTrustMode, config.HostTrustModeOverlay; got != want {
		t.Fatalf("image.hostTrustMode = %q, want %q", got, want)
	}
	wantScopes := hostTrustScopesForOS(runtime.GOOS)
	if got := cfg.Image.HostTrustScopes; !slices.Equal(got, wantScopes) {
		t.Fatalf("image.hostTrustScopes = %#v, want %#v", got, wantScopes)
	}
	if got, want := cfg.Pool.Instances, 1; got != want {
		t.Fatalf("pool.instances = %d, want %d", got, want)
	}
	if got, want := cfg.Pool.NamePrefix, "build-box-01-a4f9c2"; got != want {
		t.Fatalf("pool.namePrefix = %q, want %q", got, want)
	}
	if got, want := cfg.Pool.ReplacementRetryInitialSeconds, 15; got != want {
		t.Fatalf("pool.replacementRetryInitialSeconds = %d, want %d", got, want)
	}
	if got, want := cfg.Pool.ReplacementRetryMaxSeconds, 1800; got != want {
		t.Fatalf("pool.replacementRetryMaxSeconds = %d, want %d", got, want)
	}
	if got, want := cfg.Pool.ReplacementRetryMultiplier, 2.0; got != want {
		t.Fatalf("pool.replacementRetryMultiplier = %v, want %v", got, want)
	}
	if got, want := cfg.Pool.ReplacementRetryJitterPercent, 20; got != want {
		t.Fatalf("pool.replacementRetryJitterPercent = %d, want %d", got, want)
	}
	if got, want := cfg.Logging.Directory, "work/logs"; got != want {
		t.Fatalf("logging.directory = %q, want %q", got, want)
	}
	configText, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(configText), "logging:\n  directory: work/logs\n  managerSinks: [console]\n") {
		t.Fatalf("generated config did not include logging schema:\n%s", configText)
	}
	if !strings.Contains(string(configText), "replacementRetryInitialSeconds: 15\n  replacementRetryMaxSeconds: 1800\n  replacementRetryMultiplier: 2\n  replacementRetryJitterPercent: 20\n") {
		t.Fatalf("generated config did not include replacement retry settings:\n%s", configText)
	}
	if !strings.Contains(string(configText), "storage:\n  minimumFree: 1GiB\n  gracePeriod: 168h\n  keepPrevious: 0\n  automaticHousekeeping: conservative\n  buildCacheLimit: 20GiB\n  goCacheLimit: 10GiB\n") {
		t.Fatalf("generated config did not include bounded storage settings:\n%s", configText)
	}
	for _, want := range []string{"updateFrequency: weekly", "updateTime: \"07:00\"", "hostTrustMode: overlay", "hostTrustScopes:", "customInstallScripts:"} {
		if !strings.Contains(string(configText), want) {
			t.Fatalf("generated config omitted default advanced setting %q:\n%s", want, configText)
		}
	}
	if got := strings.Join(cfg.Runner.Labels, ","); !strings.Contains(got, "epar-docker-container-catthehacker-ubuntu") {
		t.Fatalf("runner labels = %q", got)
	}
	if !strings.Contains(out.String(), "start") || !strings.Contains(out.String(), "pool up --instances 2") {
		t.Fatalf("init output did not include next steps:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "Pool name prefix (press Enter to use build-box-01-a4f9c2; /back to return):") {
		t.Fatalf("init output did not explain default prefix acceptance:\n%s", out.String())
	}
	if got, want := cfg.Image.UpdateFrequency, config.ImageUpdateFrequencyWeekly; got != want {
		t.Fatalf("image.updateFrequency = %q, want %q", got, want)
	}
	if got, want := cfg.Image.UpdateTime, config.DefaultImageUpdateTime; got != want {
		t.Fatalf("image.updateTime = %q, want %q", got, want)
	}
	if len(cfg.Image.CustomInstallScripts) != 0 {
		t.Fatalf("image.customInstallScripts = %#v, want default empty list", cfg.Image.CustomInstallScripts)
	}
	for _, hidden := range []string{"Run custom install scripts", "Custom install scripts:", "Inherit this host's trusted TLS roots", "Host trusted TLS roots:", "Automatic image and Actions runner updates:", "Update frequency", "Local update time", "Updates:", "Change host trust", "Change update frequency", "Change runner image or install scripts"} {
		if strings.Contains(out.String(), hidden) {
			t.Fatalf("init output retained removed wizard text %q:\n%s", hidden, out.String())
		}
	}
	if !strings.Contains(out.String(), "4. Change runner image") {
		t.Fatalf("review did not retain the simplified runner-image edit action:\n%s", out.String())
	}
	for _, want := range []string{"1. Docker Container", "private daemon (default)", "2. Docker Sandboxes — recommended when ready"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("init output did not preserve explicit Docker Container default and capability-driven Docker Sandboxes labeling %q:\n%s", want, out.String())
		}
	}
	for _, want := range []string{"D. Show runner group details", "B. Show blocked runner groups", "Assessment: RECOMMENDED"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("init output did not include concise runner-group choice %q:\n%s", want, out.String())
		}
	}
	for _, hidden := range []string{"Repository access meanings:", "Repository access:", "Public repositories:", "Group type:"} {
		if strings.Contains(out.String(), hidden) {
			t.Fatalf("init output included runner-group details before D was selected %q:\n%s", hidden, out.String())
		}
	}
}

func TestInitWizardCanBackAcrossSectionsAndPreservesCompletedAnswers(t *testing.T) {
	stubInitHostAndRandom(t, "Build Box 01", []byte{0xa4, 0xf9, 0xc2})
	stubNoWSL2(t)
	dir := t.TempDir()
	path := filepath.Join(dir, ".local", "config.yml")
	input := strings.Join([]string{
		"123456", "solutionforest", ".local/github-app.pem", "1",
		"", "2", "custom-prefix",
		"0", "/back", "0", "0", "1",
		"", "1", "", "",
	}, "\n") + "\n"
	var out bytes.Buffer
	if err := runInitWithOptions(initOptions{ProjectRoot: dir, ConfigPath: path, SkipDockerCheck: true, SkipHostTrustCheck: true, In: strings.NewReader(input), Out: &out}); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Image.SourceImage != "ghcr.io/catthehacker/ubuntu:act-latest" || cfg.Pool.NamePrefix != "custom-prefix" || cfg.Image.UpdateFrequency != config.ImageUpdateFrequencyWeekly || cfg.Image.UpdateTime != config.DefaultImageUpdateTime {
		t.Fatalf("answers and generated defaults were not preserved after repeated Back: image=%q prefix=%q updates=%s@%s", cfg.Image.SourceImage, cfg.Pool.NamePrefix, cfg.Image.UpdateFrequency, cfg.Image.UpdateTime)
	}
	for _, want := range []string{"0. Back", "/back to return", "Current runner artifact setup:", "Configuration review:"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("wizard transcript omitted %q:\n%s", want, out.String())
		}
	}
	if got := strings.Count(out.String(), "Configuration review:"); got != 2 {
		t.Fatalf("review count = %d, want 2 after returning from the first review", got)
	}
}

func TestInitWizardQuitAtReviewWritesNoConfig(t *testing.T) {
	stubInitHostAndRandom(t, "Build Box 01", []byte{0xa4, 0xf9, 0xc2})
	stubNoWSL2(t)
	dir := t.TempDir()
	path := filepath.Join(dir, ".local", "config.yml")
	input := strings.Join([]string{"123456", "solutionforest", ".local/github-app.pem", "1", "", "", "", "q"}, "\n") + "\n"
	var out bytes.Buffer
	if err := runInitWithOptions(initOptions{ProjectRoot: dir, ConfigPath: path, SkipDockerCheck: true, SkipHostTrustCheck: true, In: strings.NewReader(input), Out: &out}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("config exists after review quit: %v", err)
	}
	if !strings.Contains(out.String(), "Q. Quit without writing a config") || !strings.Contains(out.String(), "Setup cancelled. No config was written.") {
		t.Fatalf("review quit was not explained:\n%s", out.String())
	}
}

func TestInitWizardReviewCanEditPoolDirectly(t *testing.T) {
	stubInitHostAndRandom(t, "Build Box 01", []byte{0xa4, 0xf9, 0xc2})
	stubNoWSL2(t)
	dir := t.TempDir()
	path := filepath.Join(dir, ".local", "config.yml")
	input := strings.Join([]string{
		"123456", "solutionforest", ".local/github-app.pem", "1",
		"", "", "",
		"5", "edited-prefix", "",
	}, "\n") + "\n"
	var out bytes.Buffer
	if err := runInitWithOptions(initOptions{ProjectRoot: dir, ConfigPath: path, SkipDockerCheck: true, SkipHostTrustCheck: true, In: strings.NewReader(input), Out: &out}); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Pool.NamePrefix != "edited-prefix" {
		t.Fatalf("pool.namePrefix = %q, want direct review edit", cfg.Pool.NamePrefix)
	}
	if got := strings.Count(out.String(), "Configuration review:"); got != 2 {
		t.Fatalf("review count = %d, want initial and edited reviews", got)
	}
}

func TestInitWizardBackAfterDirectEditUsesNaturalHistory(t *testing.T) {
	stubInitHostAndRandom(t, "Build Box 01", []byte{0xa4, 0xf9, 0xc2})
	stubNoWSL2(t)
	dir := t.TempDir()
	path := filepath.Join(dir, ".local", "config.yml")
	input := strings.Join([]string{
		"123456", "solutionforest", ".local/github-app.pem", "1",
		"", "", "",
		"5", "edited-prefix",
		"0", "/back", "0", "0", "q",
	}, "\n") + "\n"
	var out bytes.Buffer
	if err := runInitWithOptions(initOptions{ProjectRoot: dir, ConfigPath: path, SkipDockerCheck: true, SkipHostTrustCheck: true, In: strings.NewReader(input), Out: &out}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("config exists after review quit: %v", err)
	}
	transcript := out.String()
	firstReview := strings.Index(transcript, "Configuration review:")
	secondRelative := strings.Index(transcript[firstReview+1:], "Configuration review:")
	if firstReview < 0 || secondRelative < 0 {
		t.Fatalf("expected two reviews after direct edit:\n%s", transcript)
	}
	secondReview := firstReview + 1 + secondRelative
	afterSecondReview := transcript[secondReview:]
	pool := strings.Index(afterSecondReview, "Pool name prefix must be unique")
	if pool < 0 {
		t.Fatalf("Back after direct edit did not resume the natural history at the pool section:\n%s", afterSecondReview)
	}
	if strings.Contains(afterSecondReview, "Automatic image and Actions runner updates:") {
		t.Fatalf("Back after direct edit retained the removed update section:\n%s", afterSecondReview)
	}
}

func TestInitWizardProviderEditInvalidatesProviderSpecificDraft(t *testing.T) {
	stubInitHostAndRandom(t, "Build Box 01", []byte{0xa4, 0xf9, 0xc2})
	stubNoWSL2(t)
	stubTartAvailable(t)
	dir := t.TempDir()
	path := filepath.Join(dir, ".local", "config.yml")
	input := strings.Join([]string{
		"123456", "solutionforest", ".local/github-app.pem", "1",
		"", "2", "custom-prefix",
		"3", "4", "",
	}, "\n") + "\n"
	var out bytes.Buffer
	if err := runInitWithOptions(initOptions{ProjectRoot: dir, ConfigPath: path, SkipDockerCheck: true, SkipHostTrustCheck: true, In: strings.NewReader(input), Out: &out}); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider.Type != "tart" || cfg.Pool.NamePrefix != "custom-prefix" {
		t.Fatalf("provider edit produced provider=%q prefix=%q, want Tart with preserved pool", cfg.Provider.Type, cfg.Pool.NamePrefix)
	}
	reviews := strings.Split(out.String(), "Configuration review:")
	if len(reviews) != 3 {
		t.Fatalf("review count = %d, want 2", len(reviews)-1)
	}
	if !strings.Contains(reviews[1], "Runner artifact estimate:") || strings.Contains(reviews[2], "Runner artifact estimate:") {
		t.Fatalf("provider-specific estimate was not invalidated after switching to Tart:\n%s", out.String())
	}
}

func TestInitWizardFreeTextZeroIsNotBack(t *testing.T) {
	var out bytes.Buffer
	result, err := promptPoolNamePrefixWizard(&out, bufio.NewReader(strings.NewReader("0\nvalid-prefix\n")), "default-prefix")
	if err != nil {
		t.Fatal(err)
	}
	if result.Action != initWizardNext || result.Value != "valid-prefix" {
		t.Fatalf("pool result = %+v, want valid-prefix after rejecting literal zero", result)
	}
	if !strings.Contains(out.String(), "Pool name prefix is invalid") {
		t.Fatalf("literal zero was treated as navigation instead of text validation:\n%s", out.String())
	}
}

func TestInitRunnerGroupDetailsAreOptIn(t *testing.T) {
	client := &fakeInitRunnerGroupClient{
		groups: []gh.RunnerGroup{
			{ID: 1, Name: "restricted", Visibility: config.RunnerGroupRepositoryAccessSelected},
			{ID: 2, Name: "blocked-public", Visibility: config.RunnerGroupRepositoryAccessSelected, AllowsPublicRepositories: true},
		},
		repositories: map[int64][]gh.RunnerGroupRepository{
			1: {{FullName: "example/private", Private: true}},
			2: {{FullName: "example/public", Private: false}},
		},
	}
	var out bytes.Buffer
	selection, err := promptRunnerGroup(context.Background(), &out, bufio.NewReader(strings.NewReader("d\n1\n")), client)
	if err != nil {
		t.Fatal(err)
	}
	if selection.Group.Name != "restricted" {
		t.Fatalf("selected group = %q, want restricted", selection.Group.Name)
	}
	for _, want := range []string{"Repository access meanings:", "Repository access: Selected repositories", "Public repositories: Disabled", "Group type: organization-managed, non-default", "D. Hide runner group details"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("runner-group details did not include %q after D was selected:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "blocked-public") {
		t.Fatalf("blocked group became visible when only details were requested:\n%s", out.String())
	}
}

func TestInitRunnerGroupBlockedChoicesAreHiddenByDefault(t *testing.T) {
	client := &fakeInitRunnerGroupClient{
		groups: []gh.RunnerGroup{
			{ID: 1, Name: "restricted", Visibility: config.RunnerGroupRepositoryAccessSelected},
			{ID: 2, Name: "blocked-public", Visibility: config.RunnerGroupRepositoryAccessSelected, AllowsPublicRepositories: true},
			{ID: 3, Name: "blocked-unknown", Visibility: "future-value"},
		},
		repositories: map[int64][]gh.RunnerGroupRepository{
			1: {{FullName: "example/private", Private: true}},
			2: {{FullName: "example/public", Private: false}},
		},
	}
	var out bytes.Buffer
	selection, err := promptRunnerGroup(context.Background(), &out, bufio.NewReader(strings.NewReader("1\n")), client)
	if err != nil {
		t.Fatal(err)
	}
	if selection.Group.Name != "restricted" {
		t.Fatalf("selected group = %q, want first visible restricted group", selection.Group.Name)
	}
	for _, hidden := range []string{"blocked-public", "blocked-unknown"} {
		if strings.Contains(out.String(), hidden) {
			t.Fatalf("default runner-group list included blocked group %q:\n%s", hidden, out.String())
		}
	}
	if !strings.Contains(out.String(), "B. Show blocked runner groups") {
		t.Fatalf("default runner-group list did not offer blocked groups on request:\n%s", out.String())
	}
}

func TestInitAllowsDefaultGroupWithReminderAndWritesMatchingPolicy(t *testing.T) {
	stubInitHostAndRandom(t, "Build Box 01", []byte{0xa4, 0xf9, 0xc2})
	stubNoWSL2(t)
	setInitRunnerGroupClient(t, &fakeInitRunnerGroupClient{groups: []gh.RunnerGroup{{ID: 1, Name: "Default", Visibility: config.RunnerGroupRepositoryAccessAll, Default: true}}})
	dir := t.TempDir()
	path := filepath.Join(dir, ".local", "config.yml")
	var out bytes.Buffer
	if err := runInitWithOptions(initOptions{
		ProjectRoot:        dir,
		ConfigPath:         path,
		SkipDockerCheck:    true,
		SkipHostTrustCheck: true,
		In:                 strings.NewReader("123\nexample\nkey.pem\n1\n1\n\n\n\n\n"),
		Out:                &out,
	}); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	policy := cfg.Security.RunnerGroup
	if cfg.Runner.Group != "Default" || policy.RequireNonDefaultGroup || policy.RequiredRepositoryAccess != config.RunnerGroupRepositoryAccessAll || !policy.RequirePublicRepositoriesDisabled {
		t.Fatalf("unexpected default-group config: group=%q policy=%+v", cfg.Runner.Group, policy)
	}
	for _, want := range []string{"*** SECURITY REMINDER: DEFAULT RUNNER GROUP ***", "The Default runner group is fine for trying EPAR.", "For regular use, a custom group limited to selected trusted repositories offers better security.", "Continue with Default runner group"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("default-group reminder did not include %q:\n%s", want, out.String())
		}
	}
	for _, unwanted := range []string{"SECURITY WARNING", "NOT RECOMMENDED", "requires explicit review", "RECOMMENDED ACTION", "Continue anyway", "relaxed policy"} {
		if strings.Contains(out.String(), unwanted) {
			t.Fatalf("default-group reminder included alarming wording %q:\n%s", unwanted, out.String())
		}
	}
	if !strings.Contains(out.String(), "Assessment: It is fine for first-time tasting of EPAR, but generally recommend to create and use custom runner group for better security.") {
		t.Fatalf("default-group first-use assessment missing:\n%s", out.String())
	}
}

func TestInitCanBackFromBroadGroupAndChooseRestrictedGroup(t *testing.T) {
	stubInitHostAndRandom(t, "Build Box 01", []byte{0xa4, 0xf9, 0xc2})
	stubNoWSL2(t)
	setInitRunnerGroupClient(t, &fakeInitRunnerGroupClient{
		groups: []gh.RunnerGroup{
			{ID: 1, Name: "broad", Visibility: config.RunnerGroupRepositoryAccessPrivate},
			{ID: 2, Name: "restricted", Visibility: config.RunnerGroupRepositoryAccessSelected},
		},
		repositories: map[int64][]gh.RunnerGroupRepository{2: {{FullName: "example/private", Private: true}}},
	})
	dir := t.TempDir()
	path := filepath.Join(dir, ".local", "config.yml")
	var out bytes.Buffer
	if err := runInitWithOptions(initOptions{
		ProjectRoot:        dir,
		ConfigPath:         path,
		SkipDockerCheck:    true,
		SkipHostTrustCheck: true,
		In:                 strings.NewReader("123\nexample\nkey.pem\n2\n2\n1\n\n\n\n\n"),
		Out:                &out,
	}); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Runner.Group != "restricted" || cfg.Security.RunnerGroup.RequiredRepositoryAccess != config.RunnerGroupRepositoryAccessSelected {
		t.Fatalf("unexpected selection after back: group=%q policy=%+v", cfg.Runner.Group, cfg.Security.RunnerGroup)
	}
	if !strings.Contains(out.String(), "*** SECURITY WARNING: THIS RUNNER GROUP IS NOT RECOMMENDED ***") || !strings.Contains(out.String(), "Continue anyway and generate a relaxed policy") {
		t.Fatalf("non-default broad group no longer showed its security warning:\n%s", out.String())
	}
}

func TestInitRejectsPublicGroupAndAllowsAnotherSelection(t *testing.T) {
	stubInitHostAndRandom(t, "Build Box 01", []byte{0xa4, 0xf9, 0xc2})
	stubNoWSL2(t)
	client := &fakeInitRunnerGroupClient{
		groups: []gh.RunnerGroup{
			{ID: 1, Name: "public", Visibility: config.RunnerGroupRepositoryAccessSelected, AllowsPublicRepositories: true},
			{ID: 2, Name: "restricted", Visibility: config.RunnerGroupRepositoryAccessSelected},
		},
		repositories: map[int64][]gh.RunnerGroupRepository{
			1: {{FullName: "example/public", Private: false}},
			2: {{FullName: "example/private", Private: true}},
		},
	}
	setInitRunnerGroupClient(t, client)
	dir := t.TempDir()
	path := filepath.Join(dir, ".local", "config.yml")
	var out bytes.Buffer
	if err := runInitWithOptions(initOptions{
		ProjectRoot:        dir,
		ConfigPath:         path,
		SkipDockerCheck:    true,
		SkipHostTrustCheck: true,
		In:                 strings.NewReader("123\nexample\nkey.pem\nb\n2\n1\n1\n\n\n\n\n"),
		Out:                &out,
	}); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Runner.Group != "restricted" {
		t.Fatalf("runner.group = %q, want restricted", cfg.Runner.Group)
	}
	if !strings.Contains(out.String(), "*** SECURITY BLOCK: THIS RUNNER GROUP IS NOT ALLOWED BY EPAR'S SAFE DEFAULTS ***") || !strings.Contains(out.String(), "public repository or fork-triggered workflow") || !strings.Contains(out.String(), "RECOMMENDED ACTION: Do not use this group") || !strings.Contains(out.String(), "docs/runner-groups.md") {
		t.Fatalf("public-repository warning missing:\n%s", out.String())
	}
	if client.groupCalls != 1 {
		t.Fatalf("ListRunnerGroups calls = %d, want 1 when choosing Back", client.groupCalls)
	}
}

func TestInitBlocksUnknownRunnerGroupRepositoryAccess(t *testing.T) {
	stubInitHostAndRandom(t, "Build Box 01", []byte{0xa4, 0xf9, 0xc2})
	stubNoWSL2(t)
	setInitRunnerGroupClient(t, &fakeInitRunnerGroupClient{groups: []gh.RunnerGroup{{ID: 1, Name: "future-policy", Visibility: "future-value"}}})
	dir := t.TempDir()
	path := filepath.Join(dir, ".local", "config.yml")
	var out bytes.Buffer
	err := runInitWithOptions(initOptions{
		ProjectRoot:        dir,
		ConfigPath:         path,
		SkipDockerCheck:    true,
		SkipHostTrustCheck: true,
		In:                 strings.NewReader("123\nexample\nkey.pem\nb\n1\n3\n"),
		Out:                &out,
	})
	if err == nil || !strings.Contains(err.Error(), "selection cancelled") {
		t.Fatalf("init error = %v, want cancellation after unknown policy block", err)
	}
	if !strings.Contains(out.String(), "*** SECURITY BLOCK: GITHUB RETURNED AN UNKNOWN REPOSITORY-ACCESS POLICY ***") {
		t.Fatalf("unknown access policy was not blocked clearly:\n%s", out.String())
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("config exists after unknown runner-group policy: %v", statErr)
	}
}

func TestSortRunnerGroupsForWizardPutsDefaultFirst(t *testing.T) {
	groups := []gh.RunnerGroup{
		{ID: 1, Name: "Default", Visibility: config.RunnerGroupRepositoryAccessAll, Default: true},
		{ID: 2, Name: "public-selected", Visibility: config.RunnerGroupRepositoryAccessSelected, AllowsPublicRepositories: true},
		{ID: 3, Name: "all-private-repositories", Visibility: config.RunnerGroupRepositoryAccessPrivate},
		{ID: 4, Name: "recommended", Visibility: config.RunnerGroupRepositoryAccessSelected},
		{ID: 5, Name: "inherited-recommended", Visibility: config.RunnerGroupRepositoryAccessSelected, Inherited: true},
	}
	ordered := sortRunnerGroupsForWizard(groups)
	got := make([]string, len(ordered))
	for i, group := range ordered {
		got[i] = group.Name
	}
	want := []string{"Default", "recommended", "inherited-recommended", "all-private-repositories", "public-selected"}
	if !slices.Equal(got, want) {
		t.Fatalf("runner-group order = %#v, want %#v", got, want)
	}
	if groups[0].Name != "Default" {
		t.Fatalf("sort mutated API response order: %#v", groups)
	}
}

func TestInitRefreshesRunnerGroupsOnlyWhenRequested(t *testing.T) {
	stubInitHostAndRandom(t, "Build Box 01", []byte{0xa4, 0xf9, 0xc2})
	stubNoWSL2(t)
	client := &fakeInitRunnerGroupClient{
		groupResponses: [][]gh.RunnerGroup{
			{{ID: 1, Name: "before-refresh", Visibility: config.RunnerGroupRepositoryAccessSelected}},
			{{ID: 2, Name: "after-refresh", Visibility: config.RunnerGroupRepositoryAccessSelected}},
		},
		repositories: map[int64][]gh.RunnerGroupRepository{
			1: {{FullName: "example/private", Private: true}},
			2: {{FullName: "example/private", Private: true}},
		},
	}
	setInitRunnerGroupClient(t, client)
	dir := t.TempDir()
	path := filepath.Join(dir, ".local", "config.yml")
	if err := runInitWithOptions(initOptions{
		ProjectRoot:        dir,
		ConfigPath:         path,
		SkipDockerCheck:    true,
		SkipHostTrustCheck: true,
		In:                 strings.NewReader("123\nexample\nkey.pem\nr\n1\n\n\n\n\n"),
		Out:                &bytes.Buffer{},
	}); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Runner.Group != "after-refresh" || client.groupCalls != 2 {
		t.Fatalf("refresh result: group=%q ListRunnerGroups calls=%d", cfg.Runner.Group, client.groupCalls)
	}
}

func TestInitAllowsInheritedGroupWithAdvisory(t *testing.T) {
	stubInitHostAndRandom(t, "Build Box 01", []byte{0xa4, 0xf9, 0xc2})
	stubNoWSL2(t)
	setInitRunnerGroupClient(t, &fakeInitRunnerGroupClient{
		groups:       []gh.RunnerGroup{{ID: 1, Name: "enterprise-restricted", Visibility: config.RunnerGroupRepositoryAccessSelected, Inherited: true}},
		repositories: map[int64][]gh.RunnerGroupRepository{1: {{FullName: "example/private", Private: true}}},
	})
	dir := t.TempDir()
	path := filepath.Join(dir, ".local", "config.yml")
	var out bytes.Buffer
	if err := runInitWithOptions(initOptions{
		ProjectRoot:        dir,
		ConfigPath:         path,
		SkipDockerCheck:    true,
		SkipHostTrustCheck: true,
		In:                 strings.NewReader("123\nexample\nkey.pem\n1\n1\n\n\n\n\n"),
		Out:                &out,
	}); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Runner.Group != "enterprise-restricted" || !cfg.Security.RunnerGroup.RequireNonDefaultGroup || !strings.Contains(out.String(), "enterprise level") {
		t.Fatalf("inherited selection missing advisory or strict policy: group=%q policy=%+v output=%s", cfg.Runner.Group, cfg.Security.RunnerGroup, out.String())
	}
}

func TestInitRunnerGroupAPIFailureDoesNotWriteConfig(t *testing.T) {
	stubInitHostAndRandom(t, "Build Box 01", []byte{0xa4, 0xf9, 0xc2})
	stubNoWSL2(t)
	setInitRunnerGroupClient(t, &fakeInitRunnerGroupClient{err: errors.New("permission denied")})
	dir := t.TempDir()
	path := filepath.Join(dir, ".local", "config.yml")
	err := runInitWithOptions(initOptions{
		ProjectRoot:        dir,
		ConfigPath:         path,
		SkipDockerCheck:    true,
		SkipHostTrustCheck: true,
		In:                 strings.NewReader("123\nexample\nkey.pem\n"),
		Out:                &bytes.Buffer{},
	})
	if err == nil || !strings.Contains(err.Error(), "load GitHub runner groups") || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("init error = %v, want runner-group API failure", err)
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("config exists after runner-group API failure: %v", statErr)
	}
}

func TestInitRunnerGroupAPIFailureDoesNotOverwriteExistingConfig(t *testing.T) {
	stubInitHostAndRandom(t, "Build Box 01", []byte{0xa4, 0xf9, 0xc2})
	stubNoWSL2(t)
	setInitRunnerGroupClient(t, &fakeInitRunnerGroupClient{err: errors.New("permission denied")})
	dir := t.TempDir()
	path := filepath.Join(dir, ".local", "config.yml")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	const original = "existing config must remain\n"
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	err := runInitWithOptions(initOptions{
		ProjectRoot:        dir,
		ConfigPath:         path,
		Force:              true,
		SkipDockerCheck:    true,
		SkipHostTrustCheck: true,
		In:                 strings.NewReader("123\nexample\nkey.pem\n"),
		Out:                &bytes.Buffer{},
	})
	if err == nil || !strings.Contains(err.Error(), "load GitHub runner groups") {
		t.Fatalf("init error = %v, want runner-group API failure", err)
	}
	contents, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(contents) != original {
		t.Fatalf("existing config changed after API failure: %q", contents)
	}
}

func TestDetectedInitHostTrustOSUsesWrapperHost(t *testing.T) {
	t.Setenv("EPAR_CONTROLLER_HOST_OS", " windows ")
	if got, want := detectedInitHostTrustOS(), "windows"; got != want {
		t.Fatalf("detectedInitHostTrustOS() = %q, want %q", got, want)
	}
}

func TestInitDefaultsHostTrustOverlayWithoutPrompt(t *testing.T) {
	stubInitHostAndRandom(t, "Build Box 01", []byte{0xa4, 0xf9, 0xc2})
	stubNoWSL2(t)
	dir := t.TempDir()
	path := filepath.Join(dir, ".local", "config.yml")
	var out bytes.Buffer
	if err := runInitWithOptions(initOptions{
		ProjectRoot:        dir,
		ConfigPath:         path,
		SkipDockerCheck:    true,
		SkipHostTrustCheck: true,
		In:                 strings.NewReader("123456\nsolutionforest\n.local/github-app.pem\n1\n\n\n\n\n"),
		Out:                &out,
	}); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Image.HostTrustMode != config.HostTrustModeOverlay {
		t.Fatalf("image.hostTrustMode = %q, want overlay", cfg.Image.HostTrustMode)
	}
	if got, want := cfg.Image.HostTrustScopes, hostTrustScopesForOS(runtime.GOOS); !slices.Equal(got, want) {
		t.Fatalf("image.hostTrustScopes = %#v, want %#v", got, want)
	}
	for _, hidden := range []string{"Runners need this host's trusted TLS roots", "Inherit this host's trusted TLS roots", "Host trusted TLS roots:"} {
		if strings.Contains(out.String(), hidden) {
			t.Fatalf("wizard retained host-trust prompt or review text %q:\n%s", hidden, out.String())
		}
	}
}

func TestInitDoesNotWriteEnabledConfigWhenHostTrustPreflightFails(t *testing.T) {
	stubInitHostAndRandom(t, "Build Box 01", []byte{0xa4, 0xf9, 0xc2})
	stubNoWSL2(t)
	oldResolve := initResolveHostTrust
	var resolvedOptions hosttrust.Options
	initResolveHostTrust = func(_ context.Context, options hosttrust.Options) (hosttrust.Snapshot, error) {
		resolvedOptions = options
		return hosttrust.Snapshot{}, errors.New("collector unavailable")
	}
	t.Cleanup(func() { initResolveHostTrust = oldResolve })
	dir := t.TempDir()
	path := filepath.Join(dir, ".local", "config.yml")
	err := runInitWithOptions(initOptions{
		ProjectRoot:     dir,
		ConfigPath:      path,
		SkipDockerCheck: true,
		In:              strings.NewReader("123456\nsolutionforest\n.local/github-app.pem\n1\n\n\n\n\n"),
		Out:             &bytes.Buffer{},
	})
	if err == nil || !strings.Contains(err.Error(), "collector unavailable") {
		t.Fatalf("init error = %v, want collector failure", err)
	}
	if resolvedOptions.Mode != config.HostTrustModeOverlay {
		t.Fatalf("host-trust preflight mode = %q, want overlay", resolvedOptions.Mode)
	}
	if got, want := resolvedOptions.Scopes, hostTrustScopesForOS(runtime.GOOS); !slices.Equal(got, want) {
		t.Fatalf("host-trust preflight scopes = %#v, want %#v", got, want)
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("config exists after failed preflight: %v", statErr)
	}
}

func TestInitAcceptsCustomPoolNamePrefix(t *testing.T) {
	stubInitHostAndRandom(t, "Build Box 01", []byte{0xa4, 0xf9, 0xc2})
	stubNoWSL2(t)

	dir := t.TempDir()
	path := filepath.Join(dir, ".local", "config.yml")
	if err := runInitWithOptions(initOptions{
		ProjectRoot:        dir,
		ConfigPath:         path,
		SkipDockerCheck:    true,
		SkipHostTrustCheck: true,
		In:                 strings.NewReader("123456\nsolutionforest\n.local/github-app.pem\n1\n\n\ncustom-prefix\n\n"),
		Out:                &bytes.Buffer{},
	}); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := cfg.Pool.NamePrefix, "custom-prefix"; got != want {
		t.Fatalf("pool.namePrefix = %q, want %q", got, want)
	}
}

func TestInitRepromptsInvalidPoolNamePrefix(t *testing.T) {
	stubInitHostAndRandom(t, "Build Box 01", []byte{0xa4, 0xf9, 0xc2})
	stubNoWSL2(t)

	dir := t.TempDir()
	path := filepath.Join(dir, ".local", "config.yml")
	var out bytes.Buffer
	if err := runInitWithOptions(initOptions{
		ProjectRoot:        dir,
		ConfigPath:         path,
		SkipDockerCheck:    true,
		SkipHostTrustCheck: true,
		In:                 strings.NewReader("123456\nsolutionforest\n.local/github-app.pem\n1\n\n\n-bad\nfixed-prefix\n\n"),
		Out:                &out,
	}); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := cfg.Pool.NamePrefix, "fixed-prefix"; got != want {
		t.Fatalf("pool.namePrefix = %q, want %q", got, want)
	}
	if !strings.Contains(out.String(), "Pool name prefix is invalid") {
		t.Fatalf("init output did not include validation warning:\n%s", out.String())
	}
}

func TestGeneratedPoolNamePrefixTruncatesLongHostname(t *testing.T) {
	stubInitHostAndRandom(t, strings.Repeat("a", 80), []byte{0xa4, 0xf9, 0xc2})

	prefix, err := generatedPoolNamePrefix()
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Repeat("a", 33) + "-a4f9c2"
	if prefix != want {
		t.Fatalf("generatedPoolNamePrefix() = %q, want %q", prefix, want)
	}
	if len(prefix) != 40 {
		t.Fatalf("prefix length = %d, want 40", len(prefix))
	}
}

func TestGeneratedPoolNamePrefixPrefersHostNameEnv(t *testing.T) {
	oldHostname := initHostname
	oldRandomRead := initRandomRead
	initHostname = config.HostName
	initRandomRead = fixedRandomRead([]byte{0xa4, 0xf9, 0xc2})
	t.Cleanup(func() {
		initHostname = oldHostname
		initRandomRead = oldRandomRead
	})
	t.Setenv(config.HostNameEnv, "Real Windows Host")

	prefix, err := generatedPoolNamePrefix()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := prefix, "real-windows-host-a4f9c2"; got != want {
		t.Fatalf("generatedPoolNamePrefix() = %q, want %q", got, want)
	}
}

func TestGeneratedPoolNamePrefixFallsBackWhenHostnameIsUnavailable(t *testing.T) {
	oldHostname := initHostname
	oldRandomRead := initRandomRead
	initHostname = func() (string, error) { return "", errors.New("hostname unavailable") }
	initRandomRead = fixedRandomRead([]byte{0xa4, 0xf9, 0xc2})
	t.Cleanup(func() {
		initHostname = oldHostname
		initRandomRead = oldRandomRead
	})

	prefix, err := generatedPoolNamePrefix()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := prefix, "runner-a4f9c2"; got != want {
		t.Fatalf("generatedPoolNamePrefix() = %q, want %q", got, want)
	}
}

func TestGeneratedPoolNamePrefixFallsBackWhenHostnameSanitizesEmpty(t *testing.T) {
	stubInitHostAndRandom(t, "!!!", []byte{0xa4, 0xf9, 0xc2})

	prefix, err := generatedPoolNamePrefix()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := prefix, "runner-a4f9c2"; got != want {
		t.Fatalf("generatedPoolNamePrefix() = %q, want %q", got, want)
	}
}

func TestInitRefusesExistingConfig(t *testing.T) {
	oldPlatform := initSandboxPromotionPlatform
	oldLookup := initSandboxPromotionLookup
	initSandboxPromotionPlatform = func() sandboxpromotion.Platform {
		t.Fatal("existing config must be rejected before promotion lookup")
		return ""
	}
	initSandboxPromotionLookup = func(sandboxpromotion.Platform) (sandboxpromotion.Record, bool) {
		t.Fatal("existing config must be rejected before promotion lookup")
		return sandboxpromotion.Record{}, false
	}
	t.Cleanup(func() {
		initSandboxPromotionPlatform = oldPlatform
		initSandboxPromotionLookup = oldLookup
	})
	dir := t.TempDir()
	path := filepath.Join(dir, ".local", "config.yml")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("existing"), 0600); err != nil {
		t.Fatal(err)
	}

	err := runInitWithOptions(initOptions{
		ProjectRoot:        dir,
		ConfigPath:         path,
		SkipDockerCheck:    true,
		SkipHostTrustCheck: true,
		In:                 strings.NewReader("123\norg\nkey.pem\n"),
		Out:                &bytes.Buffer{},
	})
	if err == nil || !strings.Contains(err.Error(), "config already exists") {
		t.Fatalf("error = %v, want existing config refusal", err)
	}
}

func TestInitChecksDockerByDefault(t *testing.T) {
	stubInitRunnerGroupClient(t)
	oldDockerAvailable := dockerAvailable
	t.Cleanup(func() {
		dockerAvailable = oldDockerAvailable
	})
	dockerAvailable = func(ctx context.Context) error {
		return errors.New("docker unavailable")
	}

	var out bytes.Buffer
	err := runInitWithOptions(initOptions{
		ProjectRoot: t.TempDir(),
		ConfigPath:  filepath.Join(t.TempDir(), ".local", "config.yml"),
		In:          strings.NewReader("123\norg\nkey.pem\n1\n1"),
		Out:         &out,
	})
	if err == nil || !strings.Contains(err.Error(), `runner provider "1" is unavailable`) {
		t.Fatalf("error = %v, want unavailable Docker Container selection", err)
	}
	if !strings.Contains(out.String(), "Docker CLI or daemon check failed: docker unavailable") {
		t.Fatalf("output did not report Docker prerequisite status:\n%s", out.String())
	}
}

func TestInitOffersWSL2ConfigWhenAvailable(t *testing.T) {
	stubInitHostAndRandom(t, "Build Box 01", []byte{0xa4, 0xf9, 0xc2})
	stubWSL2Available(t)

	dir := t.TempDir()
	path := filepath.Join(dir, ".local", "config.yml")
	var out bytes.Buffer
	if err := runInitWithOptions(initOptions{
		ProjectRoot:        dir,
		ConfigPath:         path,
		SkipDockerCheck:    true,
		SkipHostTrustCheck: true,
		In:                 strings.NewReader("123456\nsolutionforest\n.local/github-app.pem\n1\n3\n\n\n\n"),
		Out:                &out,
	}); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(filepath.Join("..", "..", "configs", "wsl.example.yml"))
	if err != nil {
		t.Fatal(err)
	}
	wantText := strings.NewReplacer(
		"appId: 123456", "appId: 123456",
		"organization: your-org", "organization: solutionforest",
		"privateKeyPath: ~/.config/ephemeral-action-runner/github-app.pem", "privateKeyPath: .local/github-app.pem",
		"namePrefix: CHANGE-ME-unique-machine-prefix", "namePrefix: build-box-01-a4f9c2",
		"group: your-runner-group", "group: \"restricted group\"",
	).Replace(string(want))
	wantText = strings.ReplaceAll(wantText, "\r\n", "\n")
	if string(got) != wantText {
		t.Fatalf("WSL config did not match configs/wsl.example.yml:\nwant:\n%s\ngot:\n%s", wantText, got)
	}
	if !strings.Contains(out.String(), "2. Docker Sandboxes — recommended when ready") || !strings.Contains(out.String(), "3. WSL2") {
		t.Fatalf("init output did not offer WSL2:\n%s", out.String())
	}
	providerPrompt := strings.Index(out.String(), "Runner provider:")
	prefixPrompt := strings.Index(out.String(), "Pool name prefix must be unique")
	if providerPrompt < 0 || prefixPrompt < 0 || providerPrompt > prefixPrompt {
		t.Fatalf("provider prompt did not appear before pool name prefix:\n%s", out.String())
	}
}

func TestInitWSL2ChoiceDefaultsToDockerContainerAndRepromptsInvalidValues(t *testing.T) {
	stubInitHostAndRandom(t, "Build Box 01", []byte{0xa4, 0xf9, 0xc2})
	stubWSL2Available(t)

	dir := t.TempDir()
	path := filepath.Join(dir, ".local", "config.yml")
	var out bytes.Buffer
	if err := runInitWithOptions(initOptions{
		ProjectRoot:        dir,
		ConfigPath:         path,
		SkipDockerCheck:    true,
		SkipHostTrustCheck: true,
		In:                 strings.NewReader("123456\nsolutionforest\n.local/github-app.pem\n1\ninvalid\n\n\n"),
		Out:                &out,
	}); err != nil {
		t.Fatal(err)
	}
	configBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(configBytes), "type: docker-container") {
		t.Fatalf("config did not use the default Docker Container provider:\n%s", configBytes)
	}
	if !strings.Contains(out.String(), "Choose an available provider number or name shown above, R to refresh, or 0 to go back when shown.") {
		t.Fatalf("init output did not explain invalid provider input:\n%s", out.String())
	}
	if strings.Contains(out.String(), "Start runners now?") {
		t.Fatalf("standalone init unexpectedly asked whether to start runners:\n%s", out.String())
	}
}

func TestInitDockerSandboxesGeneratesDesiredImageConfigAndProvisionsTemplate(t *testing.T) {
	stubInitHostAndRandom(t, "Build Box 01", []byte{0xa4, 0xf9, 0xc2})
	stubNoWSL2(t)
	policyFingerprint := "sha256:" + strings.Repeat("b", 64)
	stubInitDockerSandboxesSetup(t, sandboxpromotion.WindowsAMD64, initDockerSandboxesDiscovery{
		Templates: []initDockerSandboxesTemplate{
			{Reference: "docker.io/library/epar-docker-sandboxes-catthehacker-full:20260723-r2-amd64", Digest: "sha256:" + strings.Repeat("a", 64), CacheID: strings.Repeat("a", 12), Platform: "linux/amd64", Size: 8 << 30, Label: "Catthehacker Ubuntu Full (recommended)", SourceChannel: "ghcr.io/catthehacker/ubuntu:full-latest"},
			{Reference: "docker.io/library/epar-docker-sandboxes-catthehacker-act-22.04:20260723-r4-amd64", Digest: "sha256:" + strings.Repeat("c", 64), CacheID: strings.Repeat("c", 12), Platform: "linux/amd64", Size: 4 << 30, Label: "Catthehacker Ubuntu Act 22.04 (current lean profile)", SourceChannel: "ghcr.io/catthehacker/ubuntu:act-22.04"},
		},
		PolicyFingerprint: policyFingerprint,
	}, nil)

	dir := t.TempDir()
	path := filepath.Join(dir, ".local", "config.yml")
	var out bytes.Buffer
	if err := runInitWithOptions(initOptions{
		ProjectRoot:        dir,
		ConfigPath:         path,
		SkipDockerCheck:    true,
		SkipHostTrustCheck: true,
		In:                 strings.NewReader("123456\nsolutionforest\n.local/github-app.pem\n1\n1\n2\n\n\n"),
		Out:                &out,
	}); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := cfg.Provider.Type, "docker-sandboxes"; got != want {
		t.Fatalf("provider.type = %q, want %q", got, want)
	}
	if got, want := cfg.Provider.Platform, "linux/amd64"; got != want {
		t.Fatalf("provider.platform = %q, want %q", got, want)
	}
	if got, want := cfg.Image.SourceImage, "ghcr.io/catthehacker/ubuntu:act-latest"; got != want {
		t.Fatalf("image.sourceImage = %q, want %q", got, want)
	}
	configContent, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(configContent), "templateDigest:") || strings.Contains(string(configContent), "\n  template:") {
		t.Fatalf("generated config retained local template identities:\n%s", configContent)
	}
	if got, want := cfg.DockerSandboxes.PolicyGeneration, policyFingerprint; got != want {
		t.Fatalf("dockerSandboxes.policyGeneration = %q, want %q", got, want)
	}
	if got, want := cfg.DockerSandboxes.NetworkBaseline, config.DockerSandboxesNetworkBaselineOpen; got != want {
		t.Fatalf("dockerSandboxes.networkBaseline = %q, want %q", got, want)
	}
	for key, values := range map[string]struct{ got, want string }{
		"rootDisk":   {cfg.DockerSandboxes.RootDisk, "auto"},
		"dockerDisk": {cfg.DockerSandboxes.DockerDisk, "50GiB"},
	} {
		if values.got != values.want {
			t.Fatalf("dockerSandboxes.%s = %q, want %q", key, values.got, values.want)
		}
	}
	for _, want := range []string{"Docker Sandboxes image setup:", "Runner base image:", "1. full — full-latest (default)", "2. act — act-latest", "Image catalog:", "Runner artifact estimate:", "Source: ghcr.io/catthehacker/ubuntu:act-latest", "Automatic sandbox root limit:"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("init output omitted %q:\n%s", want, out.String())
		}
	}
	setupHints := "  Choose the Catthehacker Ubuntu image for this runner. EPAR will provision or update the reusable runner artifact during startup.\n  Docker Sandboxes profiles must include a private Docker daemon; specialized and custom tags are not admitted.\n  Image catalog: https://github.com/catthehacker/docker_images#images-available\n\nRunner base image:"
	if !strings.Contains(out.String(), setupHints) {
		t.Fatalf("Docker Sandboxes setup hints were not grouped before the option list:\n%s", out.String())
	}
	cleanOptions := "Runner base image:\n  1. full — full-latest (default)\n  2. act — act-latest\n  0. Back"
	if !strings.Contains(out.String(), cleanOptions) {
		t.Fatalf("Docker Sandboxes option list contains non-option text:\n%s", out.String())
	}
	for _, removed := range []string{"informational; configuration creation is not blocked", "Estimate confidence:", "Expected duration:"} {
		if strings.Contains(out.String(), removed) {
			t.Fatalf("init output retained removed estimate text %q:\n%s", removed, out.String())
		}
	}
}

func TestInitDockerSandboxesWritesConfigBeforeOrdinaryProvisioning(t *testing.T) {
	stubInitHostAndRandom(t, "Build Box 01", []byte{0xa4, 0xf9, 0xc2})
	stubNoWSL2(t)
	stubInitDockerSandboxesSetup(t, sandboxpromotion.WindowsAMD64, initDockerSandboxesDiscovery{
		Templates:         []initDockerSandboxesTemplate{{Platform: "linux/amd64", Size: 4 << 30}},
		PolicyFingerprint: "sha256:" + strings.Repeat("b", 64),
	}, nil)
	initEnsureDockerSandboxesTemplate = func(context.Context, string, string) error {
		return errors.New("simulated import readback failure")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, ".local", "config.yml")
	err := runInitWithOptions(initOptions{
		ProjectRoot:        dir,
		ConfigPath:         path,
		SkipDockerCheck:    true,
		SkipHostTrustCheck: true,
		In:                 strings.NewReader("123456\nsolutionforest\n.local/github-app.pem\n1\n1\n\n\n\n"),
		Out:                io.Discard,
	})
	if err != nil {
		t.Fatalf("runInitWithOptions() error = %v", err)
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatalf("configuration was not created before ordinary provisioning: %v", statErr)
	}
}

func TestDockerSandboxesWizardResolvesEveryBuiltInImageChoice(t *testing.T) {
	for _, test := range []struct {
		choice string
		want   string
	}{
		{choice: "", want: "ghcr.io/catthehacker/ubuntu:full-latest"},
		{choice: "1", want: "ghcr.io/catthehacker/ubuntu:full-latest"},
		{choice: "2", want: "ghcr.io/catthehacker/ubuntu:act-latest"},
	} {
		t.Run(test.want, func(t *testing.T) {
			stubInitDockerSandboxesSetup(t, sandboxpromotion.WindowsAMD64, initDockerSandboxesDiscovery{
				PolicyFingerprint: "sha256:" + strings.Repeat("b", 64),
			}, nil)
			input := test.choice + "\nn\n\n\n"
			profile, accepted, err := promptDockerSandboxesProfile(context.Background(), t.TempDir(), sandboxpromotion.WindowsAMD64, io.Discard, bufio.NewReader(strings.NewReader(input)))
			if err != nil {
				t.Fatal(err)
			}
			if !accepted || profile == nil || profile.SourceImage != test.want {
				t.Fatalf("wizard profile = %+v, accepted=%t, want source %q", profile, accepted, test.want)
			}
		})
	}
}

func TestSharedDockerImageWizardCoversDockerContainerSandboxesAndWSL(t *testing.T) {
	for _, providerType := range []string{"docker-container", "docker-sandboxes", "wsl"} {
		t.Run(providerType, func(t *testing.T) {
			stubInitDockerSandboxesSetup(t, sandboxpromotion.WindowsAMD64, initDockerSandboxesDiscovery{
				PolicyFingerprint: "sha256:" + strings.Repeat("b", 64),
			}, nil)
			var out bytes.Buffer
			profile, accepted, err := promptDockerImageProfile(context.Background(), t.TempDir(), providerType, sandboxpromotion.WindowsAMD64, &out, bufio.NewReader(strings.NewReader("\nn\n\n")))
			if err != nil {
				t.Fatal(err)
			}
			if !accepted || profile == nil || profile.Provider != providerType || profile.SourceImage != "ghcr.io/catthehacker/ubuntu:full-latest" {
				t.Fatalf("shared wizard profile = %+v, accepted=%t", profile, accepted)
			}
			wants := []string{"1. full — full-latest (default)", "2. act — act-latest", "Runner artifact estimate:"}
			if providerType != "docker-sandboxes" {
				wants = append(wants, "3. dotnet — dotnet-latest", "4. js — js-latest", "5. Another catthehacker/ubuntu tag")
			} else {
				wants = append(wants, "specialized and custom tags are not admitted")
			}
			for _, want := range wants {
				if !strings.Contains(out.String(), want) {
					t.Fatalf("%s wizard output omitted %q:\n%s", providerType, want, out.String())
				}
			}
		})
	}
}

func TestDockerContainerWizardCollectsCustomTagWithoutInstallScriptPrompt(t *testing.T) {
	stubInitDockerSandboxesSetup(t, sandboxpromotion.DarwinARM64, initDockerSandboxesDiscovery{
		PolicyFingerprint: "sha256:" + strings.Repeat("b", 64),
	}, nil)
	projectRoot := t.TempDir()
	var out bytes.Buffer
	profile, accepted, err := promptDockerImageProfile(context.Background(), projectRoot, "docker-container", sandboxpromotion.DarwinARM64, &out, bufio.NewReader(strings.NewReader("5\ngo-24.04\n")))
	if err != nil {
		t.Fatal(err)
	}
	if !accepted || profile == nil || profile.SourceImage != "ghcr.io/catthehacker/ubuntu:go-24.04" || profile.HostPlatform != sandboxpromotion.DarwinARM64 {
		t.Fatalf("wizard profile = %+v, accepted=%t", profile, accepted)
	}
	if len(profile.CustomScripts) != 0 {
		t.Fatalf("custom scripts = %#v, want wizard default", profile.CustomScripts)
	}
	for _, want := range []string{"Platform: linux/arm64", "Runner artifact estimate:"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("wizard output omitted %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "custom install") || strings.Contains(out.String(), "Custom install") {
		t.Fatalf("wizard output retained custom-install interaction or review text:\n%s", out.String())
	}
}

func TestDockerSandboxesWizardUsesNoCustomInstallScriptsByDefault(t *testing.T) {
	stubInitDockerSandboxesSetup(t, sandboxpromotion.WindowsAMD64, initDockerSandboxesDiscovery{
		PolicyFingerprint: "sha256:" + strings.Repeat("b", 64),
	}, nil)
	var out bytes.Buffer
	profile, accepted, err := promptDockerSandboxesProfile(context.Background(), t.TempDir(), sandboxpromotion.WindowsAMD64, &out, bufio.NewReader(strings.NewReader("1\n")))
	if err != nil {
		t.Fatal(err)
	}
	if !accepted || profile == nil {
		t.Fatalf("wizard profile = %+v, accepted=%t", profile, accepted)
	}
	if len(profile.CustomScripts) != 0 {
		t.Fatalf("custom scripts = %#v, want none", profile.CustomScripts)
	}
	if strings.Contains(out.String(), "custom install") || strings.Contains(out.String(), "Custom install") {
		t.Fatalf("wizard output retained custom-install interaction or review text:\n%s", out.String())
	}
}

func TestDockerContainerWizardRepromptsAfterUnresolvableTag(t *testing.T) {
	stubInitDockerSandboxesSetup(t, sandboxpromotion.WindowsAMD64, initDockerSandboxesDiscovery{
		PolicyFingerprint: "sha256:" + strings.Repeat("b", 64),
	}, nil)
	resolver := initResolveDockerSandboxesSource
	initResolveDockerSandboxesSource = func(ctx context.Context, input, platform string) (imageartifact.ResolvedDockerSource, error) {
		if input == "missing" {
			return imageartifact.ResolvedDockerSource{}, errors.New("tag does not exist")
		}
		return resolver(ctx, input, platform)
	}
	var out bytes.Buffer
	profile, accepted, err := promptDockerImageProfile(context.Background(), t.TempDir(), "docker-container", sandboxpromotion.WindowsAMD64, &out, bufio.NewReader(strings.NewReader("5\nmissing\n4\n")))
	if err != nil {
		t.Fatal(err)
	}
	if !accepted || profile == nil || profile.SourceImage != "ghcr.io/catthehacker/ubuntu:js-latest" {
		t.Fatalf("wizard profile = %+v, accepted=%t", profile, accepted)
	}
	if !strings.Contains(out.String(), "That image cannot be used for linux/amd64: tag does not exist") {
		t.Fatalf("wizard did not explain the failed tag resolution:\n%s", out.String())
	}
}

func TestDockerSandboxesWizardBackReturnsNoProfile(t *testing.T) {
	stubInitDockerSandboxesSetup(t, sandboxpromotion.WindowsAMD64, initDockerSandboxesDiscovery{
		PolicyFingerprint: "sha256:" + strings.Repeat("b", 64),
	}, nil)
	var out bytes.Buffer
	profile, accepted, err := promptDockerSandboxesProfile(context.Background(), t.TempDir(), sandboxpromotion.WindowsAMD64, &out, bufio.NewReader(strings.NewReader("/back\n")))
	if err != nil {
		t.Fatal(err)
	}
	if profile != nil || accepted {
		t.Fatalf("back returned profile=%+v accepted=%t", profile, accepted)
	}
	if strings.Contains(out.String(), "Create this configuration?") {
		t.Fatalf("provider setup retained an early create confirmation:\n%s", out.String())
	}
}

func TestPrepareDockerSandboxesReadinessStartsInstalledDaemonBeforeDiagnostics(t *testing.T) {
	oldLookPath := initDockerSandboxesLookPath
	oldStartDaemon := initDockerSandboxesStartDaemon
	oldDiagnose := initDockerSandboxesDiagnose
	t.Cleanup(func() {
		initDockerSandboxesLookPath = oldLookPath
		initDockerSandboxesStartDaemon = oldStartDaemon
		initDockerSandboxesDiagnose = oldDiagnose
	})

	const binary = `C:\Program Files\Docker\Docker\resources\bin\sbx.exe`
	var operations []string
	initDockerSandboxesLookPath = func(name string) (string, error) {
		if name != "sbx" {
			t.Fatalf("looked up %q, want sbx", name)
		}
		return binary, nil
	}
	initDockerSandboxesStartDaemon = func(_ context.Context, actual string) error {
		if actual != binary {
			t.Fatalf("daemon binary = %q, want %q", actual, binary)
		}
		operations = append(operations, "start")
		return nil
	}
	initDockerSandboxesDiagnose = func(_ context.Context, actual string) (dockersandboxes.HostReadiness, error) {
		if actual != binary {
			t.Fatalf("diagnostic binary = %q, want %q", actual, binary)
		}
		operations = append(operations, "diagnose")
		return dockersandboxes.HostReadiness{ChecksPassed: 8}, nil
	}

	readiness, err := prepareInitDockerSandboxesReadiness(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if readiness.ChecksPassed != 8 || !reflect.DeepEqual(operations, []string{"start", "diagnose"}) {
		t.Fatalf("readiness = %+v, operations = %v", readiness, operations)
	}
}

func TestPrepareDockerSandboxesReadinessSkipsDaemonStartWhenSBXIsMissing(t *testing.T) {
	oldLookPath := initDockerSandboxesLookPath
	oldStartDaemon := initDockerSandboxesStartDaemon
	oldDiagnose := initDockerSandboxesDiagnose
	t.Cleanup(func() {
		initDockerSandboxesLookPath = oldLookPath
		initDockerSandboxesStartDaemon = oldStartDaemon
		initDockerSandboxesDiagnose = oldDiagnose
	})

	initDockerSandboxesLookPath = func(string) (string, error) {
		return "", exec.ErrNotFound
	}
	initDockerSandboxesStartDaemon = func(context.Context, string) error {
		t.Fatal("daemon start was attempted without an installed sbx executable")
		return nil
	}
	initDockerSandboxesDiagnose = func(_ context.Context, binary string) (dockersandboxes.HostReadiness, error) {
		if binary != "sbx" {
			t.Fatalf("diagnostic binary = %q, want sbx", binary)
		}
		return dockersandboxes.HostReadiness{}, exec.ErrNotFound
	}

	if _, err := prepareInitDockerSandboxesReadiness(context.Background()); !errors.Is(err, exec.ErrNotFound) {
		t.Fatalf("readiness error = %v, want executable-not-found error", err)
	}
}

func TestPrepareDockerSandboxesReadinessUsesSuccessfulDiagnosticsAfterStartWarning(t *testing.T) {
	oldLookPath := initDockerSandboxesLookPath
	oldStartDaemon := initDockerSandboxesStartDaemon
	oldDiagnose := initDockerSandboxesDiagnose
	t.Cleanup(func() {
		initDockerSandboxesLookPath = oldLookPath
		initDockerSandboxesStartDaemon = oldStartDaemon
		initDockerSandboxesDiagnose = oldDiagnose
	})

	initDockerSandboxesLookPath = func(string) (string, error) { return "sbx-test", nil }
	initDockerSandboxesStartDaemon = func(context.Context, string) error { return errors.New("daemon already running") }
	initDockerSandboxesDiagnose = func(context.Context, string) (dockersandboxes.HostReadiness, error) {
		return dockersandboxes.HostReadiness{ChecksPassed: 8, ChecksWarned: 1}, nil
	}

	readiness, err := prepareInitDockerSandboxesReadiness(context.Background())
	if err != nil {
		t.Fatalf("healthy diagnostics were rejected after a daemon-start warning: %v", err)
	}
	if readiness.ChecksPassed != 8 || readiness.ChecksWarned != 1 {
		t.Fatalf("readiness = %+v", readiness)
	}
}

func TestPrepareDockerSandboxesReadinessReportsStartAndDiagnosticFailures(t *testing.T) {
	oldLookPath := initDockerSandboxesLookPath
	oldStartDaemon := initDockerSandboxesStartDaemon
	oldDiagnose := initDockerSandboxesDiagnose
	t.Cleanup(func() {
		initDockerSandboxesLookPath = oldLookPath
		initDockerSandboxesStartDaemon = oldStartDaemon
		initDockerSandboxesDiagnose = oldDiagnose
	})

	initDockerSandboxesLookPath = func(string) (string, error) { return "sbx-test", nil }
	initDockerSandboxesStartDaemon = func(context.Context, string) error { return errors.New("daemon startup failed") }
	initDockerSandboxesDiagnose = func(context.Context, string) (dockersandboxes.HostReadiness, error) {
		return dockersandboxes.HostReadiness{}, errors.New("daemon diagnostic failed")
	}

	_, err := prepareInitDockerSandboxesReadiness(context.Background())
	if err == nil || !strings.Contains(err.Error(), "sbx daemon start --detach") || !strings.Contains(err.Error(), "daemon startup failed") || !strings.Contains(err.Error(), "daemon diagnostic failed") {
		t.Fatalf("combined readiness error = %v", err)
	}
}

func TestDockerSandboxesPrerequisitesIgnoreStorageCapacity(t *testing.T) {
	stubNoWSL2(t)
	oldReadiness := initDockerSandboxesReadiness
	oldCapacityCheck := initDockerSandboxesCapacityCheck
	initDockerSandboxesReadiness = func(context.Context) (dockersandboxes.HostReadiness, error) {
		return dockersandboxes.HostReadiness{ChecksPassed: 8, ChecksWarned: 1}, nil
	}
	initDockerSandboxesCapacityCheck = func(rootDisk, dockerDisk, minHostFreeSpace uint64) (initDockerSandboxesCapacityResult, error) {
		t.Fatal("provider prerequisite detection must not perform storage admission")
		return initDockerSandboxesCapacityResult{}, nil
	}
	t.Cleanup(func() {
		initDockerSandboxesReadiness = oldReadiness
		initDockerSandboxesCapacityCheck = oldCapacityCheck
	})

	got := detectInitProviderPrerequisites(context.Background(), sandboxpromotion.WindowsAMD64, true)
	if !got.DockerSandboxesAvailable {
		t.Fatalf("Docker Sandboxes was unavailable despite passing tooling diagnostics: %s", got.DockerSandboxesStatus)
	}
}

func TestDockerSandboxesPrerequisitesExplainHowToInspectFailedDiagnosticHints(t *testing.T) {
	stubNoWSL2(t)
	oldReadiness := initDockerSandboxesReadiness
	oldCapacityCheck := initDockerSandboxesCapacityCheck
	initDockerSandboxesReadiness = func(context.Context) (dockersandboxes.HostReadiness, error) {
		return dockersandboxes.HostReadiness{}, errors.New("docker sandboxes diagnostics reported 1 failed check")
	}
	initDockerSandboxesCapacityCheck = func(uint64, uint64, uint64) (initDockerSandboxesCapacityResult, error) {
		t.Fatal("capacity check must not run after failed diagnostics")
		return initDockerSandboxesCapacityResult{}, nil
	}
	t.Cleanup(func() {
		initDockerSandboxesReadiness = oldReadiness
		initDockerSandboxesCapacityCheck = oldCapacityCheck
	})

	got := detectInitProviderPrerequisites(context.Background(), sandboxpromotion.WindowsAMD64, true)
	if got.DockerSandboxesAvailable {
		t.Fatal("Docker Sandboxes was available despite failed diagnostics")
	}
	for _, want := range []string{"1 failed check", "sbx diagnose --output json", "hints for each failed check"} {
		if !strings.Contains(got.DockerSandboxesStatus, want) {
			t.Fatalf("Docker Sandboxes status omitted %q: %s", want, got.DockerSandboxesStatus)
		}
	}
}

func TestInitProviderRefreshRechecksAvailabilityAndRedrawsMenu(t *testing.T) {
	record := validInitPromotionRecord()
	stubInitSandboxPromotion(t, record, sandboxpromotion.PreflightResult{})
	oldReadiness := initDockerSandboxesReadiness
	readinessCalls := 0
	initDockerSandboxesReadiness = func(context.Context) (dockersandboxes.HostReadiness, error) {
		readinessCalls++
		if readinessCalls == 1 {
			return dockersandboxes.HostReadiness{}, errors.New("docker sandboxes diagnostics reported 1 failed check")
		}
		return dockersandboxes.HostReadiness{ChecksPassed: 8, ChecksWarned: 1}, nil
	}
	t.Cleanup(func() {
		initDockerSandboxesReadiness = oldReadiness
	})

	var out bytes.Buffer
	providerType, selectedRecord, profile, err := promptInitProvider(context.Background(), t.TempDir(), &out, bufio.NewReader(strings.NewReader("2\nr\n1\n")), true)
	if err != nil {
		t.Fatal(err)
	}
	if providerType != "docker-sandboxes" || selectedRecord.Template != "" || profile == nil || profile.SourceImage != "ghcr.io/catthehacker/ubuntu:full-latest" {
		t.Fatalf("refreshed selection = provider %q, record template %q, profile %+v", providerType, selectedRecord.Template, profile)
	}
	if readinessCalls != 2 {
		t.Fatalf("Docker Sandboxes readiness calls = %d, want 2", readinessCalls)
	}
	for _, want := range []string{
		"R. Refresh provider prerequisites",
		"Docker Sandboxes (independently certified for this exact platform) is unavailable",
		"Refreshing provider prerequisites...",
		"PASS: the exact promoted platform, host, sbx, template, policy, and resource gates passed.",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("provider refresh output omitted %q:\n%s", want, out.String())
		}
	}
	if got := strings.Count(out.String(), "R. Refresh provider prerequisites"); got != 2 {
		t.Fatalf("provider menu render count = %d, want 2:\n%s", got, out.String())
	}
}

func TestDockerSandboxesProfileShowsEstimateWithoutCapacityAdmission(t *testing.T) {
	policyFingerprint := "sha256:" + strings.Repeat("b", 64)
	stubInitDockerSandboxesSetup(t, sandboxpromotion.WindowsAMD64, initDockerSandboxesDiscovery{
		Templates: []initDockerSandboxesTemplate{{
			Reference:     "docker.io/library/epar-docker-sandboxes-catthehacker-act-22.04:test-amd64",
			Digest:        "sha256:" + strings.Repeat("c", 64),
			CacheID:       strings.Repeat("c", 12),
			Platform:      "linux/amd64",
			Size:          4 << 30,
			Label:         "Catthehacker Ubuntu Act 22.04",
			SourceChannel: "ghcr.io/catthehacker/ubuntu:act-22.04",
		}},
		PolicyFingerprint: policyFingerprint,
	}, nil)
	oldCapacityCheck := initDockerSandboxesCapacityCheck
	initDockerSandboxesCapacityCheck = func(rootDisk, dockerDisk, minHostFreeSpace uint64) (initDockerSandboxesCapacityResult, error) {
		t.Fatal("image onboarding must not perform storage admission")
		return initDockerSandboxesCapacityResult{}, nil
	}
	t.Cleanup(func() { initDockerSandboxesCapacityCheck = oldCapacityCheck })

	var out bytes.Buffer
	profile, accepted, err := promptDockerSandboxesProfile(context.Background(), t.TempDir(), sandboxpromotion.WindowsAMD64, &out, bufio.NewReader(strings.NewReader("1\nn\n\n")))
	if err != nil {
		t.Fatalf("informational estimate error = %v", err)
	}
	if profile == nil || !accepted {
		t.Fatalf("informational estimate returned profile=%+v accepted=%t", profile, accepted)
	}
	for _, want := range []string{"Runner artifact estimate:", "Capacity domains (non-blocking estimate; startup admission remains authoritative):", "sparse logical maximum"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("informational estimate output omitted %q:\n%s", want, out.String())
		}
	}
	if !strings.Contains(out.String(), "role=") && !strings.Contains(out.String(), "STORAGE DISCOVERY WARNING") {
		t.Fatalf("informational estimate reported neither capacity domains nor a discovery warning:\n%s", out.String())
	}
}

func TestInitCapabilityReadyDockerSandboxesIsDefaultWithoutPreviewAcknowledgement(t *testing.T) {
	for _, test := range []struct {
		name          string
		hostPlatform  sandboxpromotion.Platform
		guestPlatform string
	}{
		{name: "windows amd64", hostPlatform: sandboxpromotion.WindowsAMD64, guestPlatform: "linux/amd64"},
		{name: "linux amd64", hostPlatform: sandboxpromotion.LinuxAMD64, guestPlatform: "linux/amd64"},
		{name: "darwin arm64", hostPlatform: sandboxpromotion.DarwinARM64, guestPlatform: "linux/arm64"},
		{name: "future os amd64", hostPlatform: sandboxpromotion.Platform("futureos/amd64"), guestPlatform: "linux/amd64"},
	} {
		t.Run(test.name, func(t *testing.T) {
			stubInitHostAndRandom(t, "Build Box 01", []byte{0xa4, 0xf9, 0xc2})
			stubNoWSL2(t)
			policyFingerprint := "sha256:" + strings.Repeat("b", 64)
			stubInitDockerSandboxesSetup(t, test.hostPlatform, initDockerSandboxesDiscovery{
				Templates: []initDockerSandboxesTemplate{{
					Reference:     "docker.io/library/epar-docker-sandboxes-catthehacker-full:capability-default",
					Digest:        "sha256:" + strings.Repeat("a", 64),
					CacheID:       strings.Repeat("a", 12),
					Platform:      test.guestPlatform,
					Size:          18_730_706_190,
					Label:         "Catthehacker Ubuntu Full (recommended)",
					SourceChannel: "ghcr.io/catthehacker/ubuntu:full-latest",
				}},
				PolicyFingerprint: policyFingerprint,
			}, nil)
			dir := t.TempDir()
			path := filepath.Join(dir, ".local", "config.yml")
			var out bytes.Buffer
			if err := runInitWithOptions(initOptions{
				ProjectRoot:        dir,
				ConfigPath:         path,
				SkipDockerCheck:    true,
				SkipHostTrustCheck: true,
				In:                 strings.NewReader("123456\nsolutionforest\n.local/github-app.pem\n1\n\n\n\n\n"),
				Out:                &out,
			}); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.Load(path)
			if err != nil {
				t.Fatal(err)
			}
			if got, want := cfg.Provider.Type, "docker-sandboxes"; got != want {
				t.Fatalf("provider.type = %q, want %q", got, want)
			}
			if got, want := cfg.Provider.Platform, test.guestPlatform; got != want {
				t.Fatalf("provider.platform = %q, want %q", got, want)
			}
			for _, want := range []string{
				"1. Docker Sandboxes — recommended (default)",
				"2. Docker Container — private daemon",
				"Docker Sandboxes — recommended (default)",
				"Runner provider (press Enter to use 1):",
				"Docker Sandboxes image setup:",
				"Runner base image:",
				"full — full-latest (default)",
				"Runner artifact estimate:",
			} {
				if !strings.Contains(out.String(), want) {
					t.Fatalf("capability-default output omitted %q:\n%s", want, out.String())
				}
			}
			for _, forbidden := range []string{"Continue with explicit preview setup?", "Docker Sandboxes preview:"} {
				if strings.Contains(out.String(), forbidden) {
					t.Fatalf("capability-default output retained preview interaction %q:\n%s", forbidden, out.String())
				}
			}
		})
	}
}

func TestReadDockerSandboxesActiveProfilesUsesOnlyCurrentLockedTemplates(t *testing.T) {
	projectRoot := t.TempDir()
	lockDirectory := filepath.Join(projectRoot, "templates", "docker-sandboxes")
	if err := os.MkdirAll(lockDirectory, 0755); err != nil {
		t.Fatal(err)
	}
	const lock = `{
  "schemaVersion": 2,
  "profiles": {
    "full": {
      "observedTagReference": "ghcr.io/catthehacker/ubuntu:full-latest",
      "platforms": {"linux/amd64": {"templateTag": "epar-docker-sandboxes-catthehacker-full:20260723-r2-amd64"}}
    },
    "act-22.04": {
      "observedTagReference": "ghcr.io/catthehacker/ubuntu:act-22.04",
      "platforms": {"linux/amd64": {"templateTag": "epar-docker-sandboxes-catthehacker-act-22.04:20260723-r4-amd64"}}
    }
  },
  "supersededRecords": {
    "linux/amd64": {
      "full": {"templateTag": "epar-docker-sandboxes-catthehacker-full:20260723-r1-amd64"},
      "act-22.04": {"templateTag": "epar-docker-sandboxes-catthehacker-act-22.04:20260723-r3-amd64"}
    }
  }
}`
	if err := os.WriteFile(filepath.Join(lockDirectory, "sources.lock.json"), []byte(lock), 0644); err != nil {
		t.Fatal(err)
	}

	profiles, err := readDockerSandboxesActiveProfiles(projectRoot, "linux/amd64")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(profiles), 2; got != want {
		t.Fatalf("active profile count = %d, want %d", got, want)
	}
	for index, want := range []struct {
		name, channel, reference, label string
	}{
		{"full", "ghcr.io/catthehacker/ubuntu:full-latest", "docker.io/library/epar-docker-sandboxes-catthehacker-full:20260723-r2-amd64", "Catthehacker Ubuntu Full (recommended)"},
		{"act-22.04", "ghcr.io/catthehacker/ubuntu:act-22.04", "docker.io/library/epar-docker-sandboxes-catthehacker-act-22.04:20260723-r4-amd64", "Catthehacker Ubuntu Act 22.04 (current lean profile)"},
	} {
		got := profiles[index]
		if got.Name != want.name || got.ObservedTag != want.channel || got.TemplateReference != want.reference || got.DisplayLabel != want.label {
			t.Fatalf("active profile %d = %#v, want name=%q channel=%q reference=%q label=%q", index, got, want.name, want.channel, want.reference, want.label)
		}
		if strings.Contains(got.TemplateReference, "-r1-") || strings.Contains(got.TemplateReference, "-r3-") {
			t.Fatalf("historical template leaked into active profiles: %#v", got)
		}
	}
}

func TestReadDockerSandboxesActiveProfilesRejectsUnexpectedSourceChannel(t *testing.T) {
	projectRoot := t.TempDir()
	lockDirectory := filepath.Join(projectRoot, "templates", "docker-sandboxes")
	if err := os.MkdirAll(lockDirectory, 0755); err != nil {
		t.Fatal(err)
	}
	const lock = `{"schemaVersion":2,"profiles":{"full":{"observedTagReference":"ghcr.io/catthehacker/ubuntu:full-latest\nnot-a-channel","platforms":{"linux/amd64":{"templateTag":"epar-docker-sandboxes-catthehacker-full:20260723-r2-amd64"}}},"act-22.04":{"observedTagReference":"ghcr.io/catthehacker/ubuntu:act-22.04","platforms":{"linux/amd64":{"templateTag":"epar-docker-sandboxes-catthehacker-act-22.04:20260723-r4-amd64"}}}}}`
	if err := os.WriteFile(filepath.Join(lockDirectory, "sources.lock.json"), []byte(lock), 0644); err != nil {
		t.Fatal(err)
	}

	if _, err := readDockerSandboxesActiveProfiles(projectRoot, "linux/amd64"); err == nil || !strings.Contains(err.Error(), "unexpected observedTagReference") {
		t.Fatalf("unexpected source channel was accepted: %v", err)
	}
}

func TestInitDockerSandboxesUsesGuidedRootDiskDefault(t *testing.T) {
	stubInitHostAndRandom(t, "Build Box 01", []byte{0xa4, 0xf9, 0xc2})
	stubNoWSL2(t)
	policyFingerprint := "sha256:" + strings.Repeat("b", 64)
	template := initDockerSandboxesTemplate{
		Reference: "docker.io/library/epar-docker-sandboxes-catthehacker-full:measured",
		Digest:    "sha256:" + strings.Repeat("a", 64),
		CacheID:   strings.Repeat("a", 12),
		Platform:  "linux/amd64",
		Size:      18_730_706_190,
	}
	stubInitDockerSandboxesSetup(t, sandboxpromotion.WindowsAMD64, initDockerSandboxesDiscovery{
		Templates: []initDockerSandboxesTemplate{
			{Reference: "docker.io/library/epar-docker-sandboxes-catthehacker-full:unmeasured-newer", Digest: "sha256:" + strings.Repeat("f", 64), CacheID: strings.Repeat("f", 12), Platform: "linux/amd64", Size: 19 << 30},
			template,
		},
		PolicyFingerprint: policyFingerprint,
	}, nil)
	oldMeasurement := initDockerSandboxesRootMeasurementFor
	initDockerSandboxesRootMeasurementFor = func(host sandboxpromotion.Platform, actual initDockerSandboxesTemplate) (initDockerSandboxesRootMeasurement, bool) {
		if host != sandboxpromotion.WindowsAMD64 {
			t.Fatalf("measurement host = %q, want %q", host, sandboxpromotion.WindowsAMD64)
		}
		if actual.Digest != template.Digest {
			return initDockerSandboxesRootMeasurement{}, false
		}
		return initDockerSandboxesRootMeasurement{
			PeakBytes: 324_780_032,
			Evidence:  "test workload",
		}, true
	}
	t.Cleanup(func() {
		initDockerSandboxesRootMeasurementFor = oldMeasurement
	})

	dir := t.TempDir()
	path := filepath.Join(dir, ".local", "config.yml")
	var out bytes.Buffer
	if err := runInitWithOptions(initOptions{
		ProjectRoot:        dir,
		ConfigPath:         path,
		SkipDockerCheck:    true,
		SkipHostTrustCheck: true,
		In:                 strings.NewReader("123456\nsolutionforest\n.local/github-app.pem\n1\n1\n\n\n\n"),
		Out:                &out,
	}); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := cfg.DockerSandboxes.RootDisk, "auto"; got != want {
		t.Fatalf("dockerSandboxes.rootDisk = %q, want %q", got, want)
	}
	if got, want := cfg.Image.SourceImage, "ghcr.io/catthehacker/ubuntu:full-latest"; got != want {
		t.Fatalf("image.sourceImage = %q, want guided default %q", got, want)
	}
	for _, want := range []string{
		"1. full — full-latest (default)",
		"Automatic sandbox root limit:",
		"Estimated download:",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("init output omitted %q:\n%s", want, out.String())
		}
	}
}

func TestFormatInitByteCountUsesReadableBinaryUnits(t *testing.T) {
	for _, test := range []struct {
		value int64
		want  string
	}{
		{value: 18_730_706_190, want: "17.44GiB"},
		{value: 324_780_032, want: "309.73MiB"},
		{value: 20 << 30, want: "20GiB"},
		{value: 512, want: "512B"},
	} {
		if got := formatInitByteCount(test.value); got != test.want {
			t.Fatalf("formatInitByteCount(%d) = %q, want %q", test.value, got, test.want)
		}
	}
}

func TestDockerSandboxesRootMeasurementIsBoundToExactTemplateIdentity(t *testing.T) {
	const digest = "sha256:00303a3e249a1baf8b0585d20273af408c27182dcfc827a98aa25ffe66b1f67f"
	measurement, ok := dockerSandboxesRootMeasurement(sandboxpromotion.WindowsAMD64, initDockerSandboxesTemplate{Digest: digest, Platform: "linux/amd64"})
	if !ok {
		t.Fatal("exact measured template identity did not return capacity evidence")
	}
	if got, want := measurement.PeakBytes, int64(324_780_032); got != want {
		t.Fatalf("measurement peak = %d, want %d", got, want)
	}
	for _, test := range []struct {
		host     sandboxpromotion.Platform
		template initDockerSandboxesTemplate
	}{
		{host: sandboxpromotion.WindowsAMD64, template: initDockerSandboxesTemplate{Digest: "sha256:" + strings.Repeat("f", 64), Platform: "linux/amd64"}},
		{host: sandboxpromotion.WindowsAMD64, template: initDockerSandboxesTemplate{Digest: digest, Platform: "linux/arm64"}},
		{host: sandboxpromotion.LinuxAMD64, template: initDockerSandboxesTemplate{Digest: digest, Platform: "linux/amd64"}},
	} {
		if _, ok := dockerSandboxesRootMeasurement(test.host, test.template); ok {
			t.Fatalf("unexpected measurement for host %q digest %q platform %q", test.host, test.template.Digest, test.template.Platform)
		}
	}
}

func TestInitDockerSandboxesDiscoveryRetryKeepsProviderSelectionAndWritesVerifiedConfig(t *testing.T) {
	stubInitHostAndRandom(t, "Build Box 01", []byte{0xa4, 0xf9, 0xc2})
	stubNoWSL2(t)
	policyFingerprint := "sha256:" + strings.Repeat("b", 64)
	discovery := initDockerSandboxesDiscovery{
		Templates: []initDockerSandboxesTemplate{{
			Reference:     "docker.io/library/epar-docker-sandboxes-catthehacker-act-22.04:20260723-r4-amd64",
			Digest:        "sha256:" + strings.Repeat("c", 64),
			CacheID:       strings.Repeat("c", 12),
			Platform:      "linux/amd64",
			Size:          4 << 30,
			Label:         "Catthehacker Ubuntu Act 22.04 (current lean profile)",
			SourceChannel: "ghcr.io/catthehacker/ubuntu:act-22.04",
		}},
		PolicyFingerprint: policyFingerprint,
	}
	stubInitDockerSandboxesSetup(t, sandboxpromotion.WindowsAMD64, discovery, nil)
	originalDiscovery := initDiscoverDockerSandboxes
	discoveryCalls := 0
	initDiscoverDockerSandboxes = func(ctx context.Context, projectRoot, guestPlatform string) (initDockerSandboxesDiscovery, error) {
		discoveryCalls++
		if discoveryCalls == 1 {
			return initDockerSandboxesDiscovery{}, errors.New("template cache unavailable")
		}
		return originalDiscovery(ctx, projectRoot, guestPlatform)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, ".local", "config.yml")
	var out bytes.Buffer
	if err := runInitWithOptions(initOptions{
		ProjectRoot:        dir,
		ConfigPath:         path,
		SkipDockerCheck:    true,
		SkipHostTrustCheck: true,
		In:                 strings.NewReader("123456\nsolutionforest\n.local/github-app.pem\n1\n1\n\n\n\n\n"),
		Out:                &out,
	}); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := cfg.Provider.Type, "docker-sandboxes"; got != want {
		t.Fatalf("provider.type = %q, want %q after retained-selection retry", got, want)
	}
	if got := strings.Count(out.String(), "Runner provider:"); got != 1 {
		t.Fatalf("Runner provider prompts = %d, want 1 after Docker Sandboxes discovery retry:\n%s", got, out.String())
	}
	if got := strings.Count(out.String(), "Continue with explicit preview setup?"); got != 0 {
		t.Fatalf("preview acknowledgements = %d, want 0 after capability-driven default:\n%s", got, out.String())
	}
	if discoveryCalls != 3 {
		t.Fatalf("Docker Sandboxes discovery calls = %d, want 3 including policy readback", discoveryCalls)
	}
	if !strings.Contains(out.String(), "That image cannot be used for linux/amd64: template cache unavailable") || !strings.Contains(out.String(), "Choose an existing ghcr.io/catthehacker/ubuntu tag") {
		t.Fatalf("init output did not explain Docker Sandboxes source recovery:\n%s", out.String())
	}
}

func TestInitDockerSandboxesDiscoveryRetryDeclinedExitsWithoutRepeatingProviderOrWritingConfig(t *testing.T) {
	stubInitHostAndRandom(t, "Build Box 01", []byte{0xa4, 0xf9, 0xc2})
	stubNoWSL2(t)
	stubInitDockerSandboxesSetup(t, sandboxpromotion.WindowsAMD64, initDockerSandboxesDiscovery{}, errors.New("template cache unavailable"))

	dir := t.TempDir()
	path := filepath.Join(dir, ".local", "config.yml")
	var out bytes.Buffer
	err := runInitWithOptions(initOptions{
		ProjectRoot:        dir,
		ConfigPath:         path,
		SkipDockerCheck:    true,
		SkipHostTrustCheck: true,
		In:                 strings.NewReader("123456\nsolutionforest\n.local/github-app.pem\n1\n1\n"),
		Out:                &out,
	})
	if err == nil || !strings.Contains(err.Error(), "resolve runner source image") {
		t.Fatalf("retry-declined error = %v", err)
	}
	if got := strings.Count(out.String(), "Runner provider:"); got != 1 {
		t.Fatalf("Runner provider prompts = %d, want 1:\n%s", got, out.String())
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("retry-declined setup wrote a config: %v", statErr)
	}
}

func TestInitDockerSandboxesKillSwitchDoesNotInvokeDiscovery(t *testing.T) {
	stubInitHostAndRandom(t, "Build Box 01", []byte{0xa4, 0xf9, 0xc2})
	stubNoWSL2(t)
	oldPlatform := initSandboxPromotionPlatform
	oldLookup := initSandboxPromotionLookup
	oldDiscovery := initDiscoverDockerSandboxes
	initSandboxPromotionPlatform = func() sandboxpromotion.Platform { return sandboxpromotion.WindowsAMD64 }
	initSandboxPromotionLookup = func(sandboxpromotion.Platform) (sandboxpromotion.Record, bool) {
		return sandboxpromotion.Record{}, false
	}
	initDiscoverDockerSandboxes = func(context.Context, string, string) (initDockerSandboxesDiscovery, error) {
		t.Fatal("kill switch must prevent Docker Sandboxes discovery")
		return initDockerSandboxesDiscovery{}, nil
	}
	t.Cleanup(func() {
		initSandboxPromotionPlatform = oldPlatform
		initSandboxPromotionLookup = oldLookup
		initDiscoverDockerSandboxes = oldDiscovery
	})
	t.Setenv(sandboxpromotion.DisableEnvironment, "1")

	dir := t.TempDir()
	path := filepath.Join(dir, ".local", "config.yml")
	var out bytes.Buffer
	err := runInitWithOptions(initOptions{
		ProjectRoot:        dir,
		ConfigPath:         path,
		SkipDockerCheck:    true,
		SkipHostTrustCheck: true,
		In:                 strings.NewReader("123456\nsolutionforest\n.local/github-app.pem\n1\n2"),
		Out:                &out,
	})
	if err == nil || !strings.Contains(err.Error(), "EPAR_DISABLE_DOCKER_SANDBOXES") {
		t.Fatalf("kill-switch setup error = %v", err)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("kill-switch setup wrote a config: %v", statErr)
	}
	if !strings.Contains(out.String(), "EPAR_DISABLE_DOCKER_SANDBOXES=1 disables admission") {
		t.Fatalf("init output did not explain preview kill switch:\n%s", out.String())
	}
}

func TestInitPromotedDockerSandboxesDefaultsOnlyAfterPassingPreflight(t *testing.T) {
	stubInitHostAndRandom(t, "Build Box 01", []byte{0xa4, 0xf9, 0xc2})
	stubNoWSL2(t)
	record := validInitPromotionRecord()
	stubInitSandboxPromotion(t, record, sandboxpromotion.PreflightResult{})
	dir := t.TempDir()
	path := filepath.Join(dir, ".local", "config.yml")
	var out bytes.Buffer
	if err := runInitWithOptions(initOptions{
		ProjectRoot:        dir,
		ConfigPath:         path,
		SkipDockerCheck:    true,
		SkipHostTrustCheck: true,
		In:                 strings.NewReader("123456\nsolutionforest\n.local/github-app.pem\n1\n\n\n\n\n"),
		Out:                &out,
	}); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := cfg.Provider.Type, "docker-sandboxes"; got != want {
		t.Fatalf("provider.type = %q, want %q", got, want)
	}
	if got, want := cfg.Provider.Platform, "linux/amd64"; got != want {
		t.Fatalf("provider.platform = %q, want %q", got, want)
	}
	if !slices.Contains(cfg.Runner.Labels, "X64") {
		t.Fatalf("runner.labels = %q, want the mapped X64 guest architecture", cfg.Runner.Labels)
	}
	if got, want := cfg.Image.SourceImage, "ghcr.io/catthehacker/ubuntu:full-latest"; got != want {
		t.Fatalf("image.sourceImage = %q, want %q", got, want)
	}
	if got, want := cfg.DockerSandboxes.PolicyGeneration, record.PolicyFingerprint; got != want {
		t.Fatalf("dockerSandboxes.policyGeneration = %q, want %q", got, want)
	}
	for key, values := range map[string]struct {
		got  string
		want string
	}{
		"rootDisk":   {cfg.DockerSandboxes.RootDisk, "auto"},
		"dockerDisk": {cfg.DockerSandboxes.DockerDisk, "50GiB"},
	} {
		if values.got != values.want {
			t.Fatalf("dockerSandboxes.%s = %q, want %q", key, values.got, values.want)
		}
	}
	configText, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"type: docker-sandboxes", "dockerSandboxes:", "epar-docker-sandboxes", "policyGeneration: " + record.PolicyFingerprint} {
		if !strings.Contains(string(configText), required) {
			t.Fatalf("generated Docker Sandboxes config omitted %q:\n%s", required, configText)
		}
	}
	for _, forbidden := range []string{"dockerSandbox:", "\n  type: docker-sandbox\n", "epar-docker-sandbox]"} {
		if strings.Contains(string(configText), forbidden) {
			t.Fatalf("generated config used singular Docker Sandbox key %q:\n%s", forbidden, configText)
		}
	}
	if !strings.Contains(out.String(), "PASS: the exact promoted platform") || !strings.Contains(out.String(), "Docker Sandboxes (independently certified for this exact platform) (default)") {
		t.Fatalf("init output did not explain the promoted default:\n%s", out.String())
	}
}

func TestPromotedDockerSandboxesPlatformUsesSharedHostGuestMapping(t *testing.T) {
	tests := []struct {
		host            sandboxpromotion.Platform
		wantGuest       string
		wantRunnerLabel string
		wantUnsupported bool
	}{
		{host: sandboxpromotion.WindowsAMD64, wantGuest: "linux/amd64", wantRunnerLabel: "X64"},
		{host: sandboxpromotion.LinuxAMD64, wantGuest: "linux/amd64", wantRunnerLabel: "X64"},
		{host: sandboxpromotion.DarwinARM64, wantGuest: "linux/arm64", wantRunnerLabel: "ARM64"},
		{host: sandboxpromotion.Platform("linux/arm64"), wantGuest: "linux/arm64", wantRunnerLabel: "ARM64"},
		{host: sandboxpromotion.Platform("windows/arm64"), wantGuest: "linux/arm64", wantRunnerLabel: "ARM64"},
		{host: sandboxpromotion.Platform("darwin/amd64"), wantGuest: "linux/amd64", wantRunnerLabel: "X64"},
		{host: sandboxpromotion.Platform("futureos/amd64"), wantGuest: "linux/amd64", wantRunnerLabel: "X64"},
		{host: sandboxpromotion.Platform("futureos/386"), wantUnsupported: true},
	}
	for _, test := range tests {
		t.Run(string(test.host), func(t *testing.T) {
			guest, runnerLabel, err := promotedDockerSandboxesPlatform(sandboxpromotion.Record{Platform: test.host})
			if test.wantUnsupported {
				if err == nil {
					t.Fatalf("promotedDockerSandboxesPlatform(%q) unexpectedly succeeded", test.host)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if guest != test.wantGuest || runnerLabel != test.wantRunnerLabel {
				t.Fatalf("promotedDockerSandboxesPlatform(%q) = (%q, %q), want (%q, %q)", test.host, guest, runnerLabel, test.wantGuest, test.wantRunnerLabel)
			}
		})
	}
}

func TestInitPromotionGateFailuresRequireExplicitProviderAndExplainAction(t *testing.T) {
	record := validInitPromotionRecord()
	tests := []struct {
		name    string
		gate    string
		detail  string
		disable bool
	}{
		{name: "kill switch", gate: "operator kill switch", detail: sandboxpromotion.DisableEnvironment, disable: true},
		{name: "native controller", gate: "native controller", detail: "native controller is unavailable"},
		{name: "daemon", gate: "daemon health", detail: "daemon is not running"},
		{name: "virtualization", gate: "virtualization", detail: "virtualization is unavailable"},
		{name: "template", gate: "promoted template", detail: "template identity differs"},
		{name: "policy", gate: "promoted policy", detail: "policy fingerprint differs"},
		{name: "resource", gate: "resource availability", detail: "insufficient free space"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stubInitHostAndRandom(t, "Build Box 01", []byte{0xa4, 0xf9, 0xc2})
			stubNoWSL2(t)
			preflight := sandboxpromotion.PreflightResult{Failures: []sandboxpromotion.Failure{{
				Gate:       test.gate,
				Detail:     test.detail,
				Resolution: "take the exact corrective action and rerun setup",
			}}}
			stubInitSandboxPromotion(t, record, preflight)
			if test.disable {
				t.Setenv(sandboxpromotion.DisableEnvironment, "1")
				initDockerSandboxesPreflight = func(context.Context, sandboxpromotion.Record, string) sandboxpromotion.PreflightResult {
					t.Fatal("kill switch must stop automatic-default preflight before admission checks")
					return sandboxpromotion.PreflightResult{}
				}
			} else {
				t.Setenv(sandboxpromotion.DisableEnvironment, "")
			}
			dir := t.TempDir()
			path := filepath.Join(dir, ".local", "config.yml")
			var out bytes.Buffer
			if err := runInitWithOptions(initOptions{
				ProjectRoot:        dir,
				ConfigPath:         path,
				SkipDockerCheck:    true,
				SkipHostTrustCheck: true,
				In:                 strings.NewReader("123456\nsolutionforest\n.local/github-app.pem\n1\ndocker-sandboxes\n1\n\n\n\n"),
				Out:                &out,
			}); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.Load(path)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Provider.Type != "docker-container" {
				t.Fatalf("provider.type = %q, want explicit Docker Container selection", cfg.Provider.Type)
			}
			if !strings.Contains(out.String(), "FAIL ["+test.gate+"]") || !strings.Contains(out.String(), test.detail) || !strings.Contains(out.String(), "Action:") {
				t.Fatalf("init output omitted actionable %s failure:\n%s", test.gate, out.String())
			}
			if !strings.Contains(out.String(), "explicit choice required") || !strings.Contains(out.String(), "Docker Sandboxes (independently certified for this exact platform) is unavailable") {
				t.Fatalf("init output did not reject explicit unavailable Docker Sandboxes selection:\n%s", out.String())
			}
		})
	}
}

func TestInitFailedPromotionAllowsExplicitWSLSelection(t *testing.T) {
	stubInitHostAndRandom(t, "Build Box 01", []byte{0xa4, 0xf9, 0xc2})
	stubWSL2Available(t)
	record := validInitPromotionRecord()
	stubInitSandboxPromotion(t, record, sandboxpromotion.PreflightResult{Failures: []sandboxpromotion.Failure{{
		Gate: "authentication", Detail: "authentication is not valid", Resolution: "run sbx login",
	}}})
	dir := t.TempDir()
	path := filepath.Join(dir, ".local", "config.yml")
	if err := runInitWithOptions(initOptions{
		ProjectRoot:        dir,
		ConfigPath:         path,
		SkipDockerCheck:    true,
		SkipHostTrustCheck: true,
		In:                 strings.NewReader("123456\nsolutionforest\n.local/github-app.pem\n1\n3\n\n\n\n"),
		Out:                &bytes.Buffer{},
	}); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider.Type != "wsl" {
		t.Fatalf("provider.type = %q, want explicit WSL selection", cfg.Provider.Type)
	}
}

func TestInitFailedPromotionDoesNotSilentlyFallBackOnEOF(t *testing.T) {
	stubInitHostAndRandom(t, "Build Box 01", []byte{0xa4, 0xf9, 0xc2})
	stubNoWSL2(t)
	record := validInitPromotionRecord()
	stubInitSandboxPromotion(t, record, sandboxpromotion.PreflightResult{Failures: []sandboxpromotion.Failure{{
		Gate: "authentication", Detail: "authentication is not valid", Resolution: "run sbx login",
	}}})
	dir := t.TempDir()
	path := filepath.Join(dir, ".local", "config.yml")
	err := runInitWithOptions(initOptions{
		ProjectRoot:        dir,
		ConfigPath:         path,
		SkipDockerCheck:    true,
		SkipHostTrustCheck: true,
		In:                 strings.NewReader("123456\nsolutionforest\n.local/github-app.pem\n1\n"),
		Out:                &bytes.Buffer{},
	})
	if err == nil || !strings.Contains(err.Error(), "invalid runner provider") {
		t.Fatalf("error = %v, want explicit provider requirement", err)
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("config was written after failed explicit selection: %v", statErr)
	}
}

func TestInitOffersTartConfigWhenAvailable(t *testing.T) {
	stubInitHostAndRandom(t, "Build Box 01", []byte{0xa4, 0xf9, 0xc2})
	stubTartAvailable(t)
	oldDockerAvailable := dockerAvailable
	dockerAvailable = func(context.Context) error {
		return errors.New("Docker is unavailable on this Mac")
	}
	t.Cleanup(func() { dockerAvailable = oldDockerAvailable })

	dir := t.TempDir()
	path := filepath.Join(dir, ".local", "config.yml")
	var out bytes.Buffer
	if err := runInitWithOptions(initOptions{
		ProjectRoot:        dir,
		ConfigPath:         path,
		SkipHostTrustCheck: true,
		In:                 strings.NewReader("654321\nexample\n.local/github-app.pem\n1\n4\n\n"),
		Out:                &out,
	}); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(filepath.Join("..", "..", "configs", "tart.example.yml"))
	if err != nil {
		t.Fatal(err)
	}
	wantText := strings.NewReplacer(
		"appId: 123456", "appId: 654321",
		"organization: your-org", "organization: example",
		"privateKeyPath: ~/.config/ephemeral-action-runner/github-app.pem", "privateKeyPath: .local/github-app.pem",
		"namePrefix: CHANGE-ME-unique-machine-prefix", "namePrefix: build-box-01-a4f9c2",
		"group: your-runner-group", "group: \"restricted group\"",
	).Replace(string(want))
	wantText = strings.ReplaceAll(wantText, "\r\n", "\n")
	if string(got) != wantText {
		t.Fatalf("Tart config did not match configs/tart.example.yml:\nwant:\n%s\ngot:\n%s", wantText, got)
	}
	if !strings.Contains(out.String(), "2. Docker Sandboxes — recommended when ready") || !strings.Contains(out.String(), "3. WSL2") || !strings.Contains(out.String(), "4. Tart (experimental)") || !strings.Contains(out.String(), "Docker CLI or daemon check failed: Docker is unavailable on this Mac") {
		t.Fatalf("init output did not offer Tart:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "Configuration review:") || !strings.Contains(out.String(), "Runner image: ghcr.io/cirruslabs/ubuntu:latest") || !strings.Contains(out.String(), "Reusable artifact: epar-ubuntu-24-arm64") || strings.Contains(out.String(), "Runner artifact estimate:") || strings.Contains(out.String(), "Create this configuration?") {
		t.Fatalf("Tart review was missing or contained Docker-specific setup output:\n%s", out.String())
	}
}

func TestTartAvailabilityRequiresNativeMacOSAndSuccessfulVersion(t *testing.T) {
	oldGOOS := initGOOS
	oldTartVersion := initTartVersion
	t.Cleanup(func() {
		initGOOS = oldGOOS
		initTartVersion = oldTartVersion
	})

	initGOOS = "darwin"
	initTartVersion = func(context.Context) error { return nil }
	if !tartAvailable() {
		t.Fatal("tartAvailable() = false when tart --version succeeds on macOS")
	}
	initTartVersion = func(context.Context) error { return errors.New("tart unavailable") }
	if tartAvailable() {
		t.Fatal("tartAvailable() = true when tart --version fails")
	}

	initGOOS = "linux"
	initTartVersion = func(context.Context) error {
		t.Fatal("tart --version should not run outside native macOS")
		return nil
	}
	if tartAvailable() {
		t.Fatal("tartAvailable() = true outside native macOS")
	}
}

func TestWSL2AvailabilityRequiresNativeWindowsSuccessfulVersion2Status(t *testing.T) {
	stubWSL2Available(t)
	for _, test := range []struct {
		name   string
		status []byte
		want   bool
	}{
		{
			name:   "UTF-8",
			status: []byte("Default Distribution: Ubuntu\r\nDefault Version: 2\r\n"),
			want:   true,
		},
		{
			name:   "UTF-16LE without BOM",
			status: utf16LE("Default Distribution: Ubuntu\r\nDefault Version: 2\r\n", false),
			want:   true,
		},
		{
			name:   "UTF-16LE with BOM",
			status: utf16LE("Default Version: 2\r\n", true),
			want:   true,
		},
		{
			name:   "wrong version",
			status: []byte("Default Version: 1\n"),
			want:   false,
		},
		{
			name:   "malformed UTF-16LE",
			status: append(utf16LE("Default Version: 2\r\n", false), 0xff),
			want:   false,
		},
		{
			name:   "unrecognized output",
			status: []byte("WSL status unavailable\n"),
			want:   false,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			initWSLStatus = func(context.Context) ([]byte, error) {
				return test.status, nil
			}
			if got := wsl2Available(); got != test.want {
				t.Fatalf("wsl2Available() = %t, want %t", got, test.want)
			}
		})
	}

	initWSLStatus = func(context.Context) ([]byte, error) {
		return nil, errors.New("wsl unavailable")
	}
	if wsl2Available() {
		t.Fatal("wsl2Available() = true when wsl.exe --status fails")
	}
}

func stubInitHostAndRandom(t *testing.T, hostname string, random []byte) {
	t.Helper()
	oldHostname := initHostname
	oldRandomRead := initRandomRead
	oldResolver := initResolveDockerSandboxesSource
	initHostname = func() (string, error) { return hostname, nil }
	initRandomRead = fixedRandomRead(random)
	initResolveDockerSandboxesSource = func(_ context.Context, input, platform string) (imageartifact.ResolvedDockerSource, error) {
		reference, err := imageartifact.NormalizeCatthehackerSource(input)
		if err != nil {
			return imageartifact.ResolvedDockerSource{}, err
		}
		return imageartifact.ResolvedDockerSource{
			Reference:            reference,
			ImmutableReference:   "ghcr.io/catthehacker/ubuntu@sha256:" + strings.Repeat("a", 64),
			IndexDigest:          "sha256:" + strings.Repeat("a", 64),
			PlatformDigest:       "sha256:" + strings.Repeat("b", 64),
			Platform:             platform,
			CompressedLayerBytes: 8 << 30,
		}, nil
	}
	stubInitRunnerGroupClient(t)
	t.Cleanup(func() {
		initHostname = oldHostname
		initRandomRead = oldRandomRead
		initResolveDockerSandboxesSource = oldResolver
	})
}

type fakeInitRunnerGroupClient struct {
	groups         []gh.RunnerGroup
	groupResponses [][]gh.RunnerGroup
	repositories   map[int64][]gh.RunnerGroupRepository
	err            error
	groupCalls     int
}

func (f *fakeInitRunnerGroupClient) ListRunnerGroups(context.Context) ([]gh.RunnerGroup, error) {
	f.groupCalls++
	if len(f.groupResponses) > 0 {
		index := f.groupCalls - 1
		if index >= len(f.groupResponses) {
			index = len(f.groupResponses) - 1
		}
		return append([]gh.RunnerGroup(nil), f.groupResponses[index]...), f.err
	}
	return append([]gh.RunnerGroup(nil), f.groups...), f.err
}

func (f *fakeInitRunnerGroupClient) ListRunnerGroupRepositories(_ context.Context, groupID int64) ([]gh.RunnerGroupRepository, error) {
	return append([]gh.RunnerGroupRepository(nil), f.repositories[groupID]...), f.err
}

func stubInitRunnerGroupClient(t *testing.T) {
	t.Helper()
	oldFactory := newInitRunnerGroupClient
	newInitRunnerGroupClient = func(config.GitHubConfig) initRunnerGroupClient {
		return &fakeInitRunnerGroupClient{
			groups: []gh.RunnerGroup{{ID: 1, Name: "restricted group", Visibility: config.RunnerGroupRepositoryAccessSelected}},
			repositories: map[int64][]gh.RunnerGroupRepository{
				1: {{ID: 1, FullName: "example/private", Private: true}},
			},
		}
	}
	t.Cleanup(func() { newInitRunnerGroupClient = oldFactory })
}

func setInitRunnerGroupClient(t *testing.T, client initRunnerGroupClient) {
	t.Helper()
	oldFactory := newInitRunnerGroupClient
	newInitRunnerGroupClient = func(config.GitHubConfig) initRunnerGroupClient { return client }
	t.Cleanup(func() { newInitRunnerGroupClient = oldFactory })
}

func fixedRandomRead(random []byte) func([]byte) (int, error) {
	return func(data []byte) (int, error) {
		copy(data, random)
		return len(data), nil
	}
}

func utf16LE(text string, includeBOM bool) []byte {
	units := utf16.Encode([]rune(text))
	data := make([]byte, 0, len(units)*2+2)
	if includeBOM {
		data = append(data, 0xff, 0xfe)
	}
	for _, unit := range units {
		data = append(data, byte(unit), byte(unit>>8))
	}
	return data
}

func stubNoWSL2(t *testing.T) {
	t.Helper()
	stubDockerSandboxesUnavailable(t)
	oldGOOS := initGOOS
	oldWSLStatus := initWSLStatus
	initGOOS = "linux"
	initWSLStatus = func(context.Context) ([]byte, error) {
		t.Fatal("wsl.exe --status should not run outside native Windows")
		return nil, nil
	}
	t.Cleanup(func() {
		initGOOS = oldGOOS
		initWSLStatus = oldWSLStatus
	})
}

func stubWSL2Available(t *testing.T) {
	t.Helper()
	stubDockerSandboxesUnavailable(t)
	oldGOOS := initGOOS
	oldWSLStatus := initWSLStatus
	oldPlatform := initSandboxPromotionPlatform
	initGOOS = "windows"
	initWSLStatus = func(context.Context) ([]byte, error) {
		return []byte("Default Distribution: Ubuntu\nDefault Version: 2\n"), nil
	}
	initSandboxPromotionPlatform = func() sandboxpromotion.Platform {
		return sandboxpromotion.WindowsAMD64
	}
	t.Cleanup(func() {
		initGOOS = oldGOOS
		initWSLStatus = oldWSLStatus
		initSandboxPromotionPlatform = oldPlatform
	})
}

func stubDockerSandboxesUnavailable(t *testing.T) {
	t.Helper()
	oldReadiness := initDockerSandboxesReadiness
	initDockerSandboxesReadiness = func(context.Context) (dockersandboxes.HostReadiness, error) {
		return dockersandboxes.HostReadiness{}, errors.New("Docker Sandboxes unavailable in provider-neutral wizard test")
	}
	t.Cleanup(func() {
		initDockerSandboxesReadiness = oldReadiness
	})
}

func stubTartAvailable(t *testing.T) {
	t.Helper()
	oldGOOS := initGOOS
	oldTartVersion := initTartVersion
	initGOOS = "darwin"
	initTartVersion = func(context.Context) error { return nil }
	t.Cleanup(func() {
		initGOOS = oldGOOS
		initTartVersion = oldTartVersion
	})
}

func stubInitDockerSandboxesSetup(t *testing.T, platform sandboxpromotion.Platform, discovery initDockerSandboxesDiscovery, discoveryErr error) {
	t.Helper()
	oldPlatform := initSandboxPromotionPlatform
	oldLookup := initSandboxPromotionLookup
	oldDiscovery := initDiscoverDockerSandboxes
	oldReadiness := initDockerSandboxesReadiness
	oldCapacityCheck := initDockerSandboxesCapacityCheck
	oldResolver := initResolveDockerSandboxesSource
	oldPolicyFingerprint := initDockerSandboxesPolicyFingerprint
	oldEnsureTemplate := initEnsureDockerSandboxesTemplate
	initSandboxPromotionPlatform = func() sandboxpromotion.Platform { return platform }
	initSandboxPromotionLookup = func(actual sandboxpromotion.Platform) (sandboxpromotion.Record, bool) {
		if actual != platform {
			t.Fatalf("promotion lookup platform = %q, want %q", actual, platform)
		}
		return sandboxpromotion.Record{}, false
	}
	initDiscoverDockerSandboxes = func(_ context.Context, projectRoot, guestPlatform string) (initDockerSandboxesDiscovery, error) {
		if projectRoot == "" {
			t.Fatal("Docker Sandboxes discovery project root is empty")
		}
		expectedGuestPlatform, _, err := dockerSandboxesPlatform(platform)
		if err != nil {
			t.Fatalf("derive Docker Sandboxes discovery guest platform: %v", err)
		}
		if guestPlatform != expectedGuestPlatform {
			t.Fatalf("Docker Sandboxes discovery guest platform = %q, want %q", guestPlatform, expectedGuestPlatform)
		}
		return discovery, discoveryErr
	}
	initDockerSandboxesReadiness = func(context.Context) (dockersandboxes.HostReadiness, error) {
		return dockersandboxes.HostReadiness{ChecksPassed: 8, ChecksWarned: 1}, nil
	}
	initResolveDockerSandboxesSource = func(ctx context.Context, input, guestPlatform string) (imageartifact.ResolvedDockerSource, error) {
		current, err := initDiscoverDockerSandboxes(ctx, "test-project", guestPlatform)
		if err != nil {
			return imageartifact.ResolvedDockerSource{}, err
		}
		reference, err := imageartifact.NormalizeCatthehackerSource(input)
		if err != nil {
			return imageartifact.ResolvedDockerSource{}, err
		}
		compressed := uint64(8 << 30)
		if len(current.Templates) > 0 && current.Templates[0].Size > 0 {
			compressed = uint64(current.Templates[0].Size)
		}
		return imageartifact.ResolvedDockerSource{
			Reference:            reference,
			ImmutableReference:   "ghcr.io/catthehacker/ubuntu@sha256:" + strings.Repeat("a", 64),
			IndexDigest:          "sha256:" + strings.Repeat("a", 64),
			PlatformDigest:       "sha256:" + strings.Repeat("b", 64),
			Platform:             guestPlatform,
			CompressedLayerBytes: compressed,
		}, nil
	}
	initDockerSandboxesPolicyFingerprint = func(ctx context.Context) (string, error) {
		current, err := initDiscoverDockerSandboxes(ctx, "test-project", expectedDockerSandboxesGuestPlatform(t, platform))
		return current.PolicyFingerprint, err
	}
	initEnsureDockerSandboxesTemplate = func(context.Context, string, string) error { return nil }
	initDockerSandboxesCapacityCheck = func(rootDisk, dockerDisk, minHostFreeSpace uint64) (initDockerSandboxesCapacityResult, error) {
		return initDockerSandboxesCapacityResult{
			StorageRoot:    `C:\stub\DockerSandboxes`,
			AvailableBytes: 1 << 40,
			TotalBytes:     2 << 40,
			Reservation:    rootDisk + dockerDisk,
			HostWatermark:  minHostFreeSpace,
			RequiredBytes:  rootDisk + dockerDisk + minHostFreeSpace,
			CapacityStatus: storage.CapacityReady,
		}, nil
	}
	t.Cleanup(func() {
		initSandboxPromotionPlatform = oldPlatform
		initSandboxPromotionLookup = oldLookup
		initDiscoverDockerSandboxes = oldDiscovery
		initDockerSandboxesReadiness = oldReadiness
		initDockerSandboxesCapacityCheck = oldCapacityCheck
		initResolveDockerSandboxesSource = oldResolver
		initDockerSandboxesPolicyFingerprint = oldPolicyFingerprint
		initEnsureDockerSandboxesTemplate = oldEnsureTemplate
	})
}

func expectedDockerSandboxesGuestPlatform(t *testing.T, platform sandboxpromotion.Platform) string {
	t.Helper()
	guestPlatform, _, err := dockerSandboxesPlatform(platform)
	if err != nil {
		t.Fatal(err)
	}
	return guestPlatform
}

func stubInitSandboxPromotion(t *testing.T, record sandboxpromotion.Record, result sandboxpromotion.PreflightResult) {
	t.Helper()
	oldPlatform := initSandboxPromotionPlatform
	oldLookup := initSandboxPromotionLookup
	oldPreflight := initDockerSandboxesPreflight
	oldReadiness := initDockerSandboxesReadiness
	oldCapacityCheck := initDockerSandboxesCapacityCheck
	oldResolver := initResolveDockerSandboxesSource
	oldPolicyFingerprint := initDockerSandboxesPolicyFingerprint
	oldEnsureTemplate := initEnsureDockerSandboxesTemplate
	initSandboxPromotionPlatform = func() sandboxpromotion.Platform { return record.Platform }
	initSandboxPromotionLookup = func(platform sandboxpromotion.Platform) (sandboxpromotion.Record, bool) {
		if platform != record.Platform {
			t.Fatalf("promotion lookup platform = %q, want %q", platform, record.Platform)
		}
		return record, true
	}
	initDockerSandboxesPreflight = func(context.Context, sandboxpromotion.Record, string) sandboxpromotion.PreflightResult {
		return result
	}
	initDockerSandboxesReadiness = func(context.Context) (dockersandboxes.HostReadiness, error) {
		return dockersandboxes.HostReadiness{ChecksPassed: 8}, nil
	}
	initResolveDockerSandboxesSource = func(_ context.Context, input, guestPlatform string) (imageartifact.ResolvedDockerSource, error) {
		reference, err := imageartifact.NormalizeCatthehackerSource(input)
		if err != nil {
			return imageartifact.ResolvedDockerSource{}, err
		}
		return imageartifact.ResolvedDockerSource{
			Reference:            reference,
			ImmutableReference:   "ghcr.io/catthehacker/ubuntu@" + record.TemplateDigest,
			IndexDigest:          record.TemplateDigest,
			PlatformDigest:       record.TemplateDigest,
			Platform:             guestPlatform,
			CompressedLayerBytes: 8 << 30,
		}, nil
	}
	initDockerSandboxesPolicyFingerprint = func(context.Context) (string, error) { return record.PolicyFingerprint, nil }
	initEnsureDockerSandboxesTemplate = func(context.Context, string, string) error { return nil }
	initDockerSandboxesCapacityCheck = func(rootDisk, dockerDisk, minHostFreeSpace uint64) (initDockerSandboxesCapacityResult, error) {
		return initDockerSandboxesCapacityResult{
			StorageRoot:    `C:\stub\DockerSandboxes`,
			AvailableBytes: 1 << 40,
			TotalBytes:     2 << 40,
			Reservation:    rootDisk + dockerDisk,
			HostWatermark:  minHostFreeSpace,
			RequiredBytes:  rootDisk + dockerDisk + minHostFreeSpace,
			CapacityStatus: storage.CapacityReady,
		}, nil
	}
	t.Cleanup(func() {
		initSandboxPromotionPlatform = oldPlatform
		initSandboxPromotionLookup = oldLookup
		initDockerSandboxesPreflight = oldPreflight
		initDockerSandboxesReadiness = oldReadiness
		initDockerSandboxesCapacityCheck = oldCapacityCheck
		initResolveDockerSandboxesSource = oldResolver
		initDockerSandboxesPolicyFingerprint = oldPolicyFingerprint
		initEnsureDockerSandboxesTemplate = oldEnsureTemplate
	})
}

func validInitPromotionRecord() sandboxpromotion.Record {
	digest := func(character string) string { return "sha256:" + strings.Repeat(character, 64) }
	return sandboxpromotion.Record{
		Platform:                 sandboxpromotion.WindowsAMD64,
		EPARRevision:             digest("1"),
		Template:                 "epar-docker-sandboxes-catthehacker-full:promoted",
		TemplateDigest:           digest("a"),
		TemplateCacheID:          strings.Repeat("a", 12),
		TemplateMetadataDigest:   digest("b"),
		TemplateArchiveDigest:    digest("b"),
		PolicyFingerprint:        digest("b"),
		EvidenceDigest:           digest("c"),
		SBOMDigest:               digest("d"),
		ProvenanceDigest:         digest("e"),
		SoftwareInventoryDigest:  digest("f"),
		VerifiedAt:               time.Date(2026, 7, 23, 0, 0, 0, 0, time.UTC),
		Verifier:                 "independent-test-verifier",
		Gates:                    sandboxpromotion.GateResults{Local: true, Functional: true, Recovery: true, Security: true, Policy: true, Cleanup: true, SecretScanning: true, ConcurrentProvisioning: true, IndependentSecurityReview: true},
		RootDiskBytes:            120 << 30,
		DockerDiskBytes:          100 << 30,
		MinHostFreeSpaceBytes:    50 << 30,
		ReliabilityJobs:          25,
		ReliabilityDuration:      2 * time.Hour,
		CachedCreateP95:          30 * time.Second,
		QueueToOnlineP95:         90 * time.Second,
		ForceRemoveP95:           60 * time.Second,
		BuildxComposeSlowdownPct: 10,
	}
}
