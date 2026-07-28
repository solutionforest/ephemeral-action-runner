package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
	gh "github.com/solutionforest/ephemeral-action-runner/internal/github"
	"github.com/solutionforest/ephemeral-action-runner/internal/hosttrust"
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
	if !strings.Contains(string(configText), "storage:\n  minimumFree: 20GiB\n  gracePeriod: 168h\n  keepPrevious: 0\n  automaticHousekeeping: conservative\n  buildCacheLimit: 64GiB\n  goCacheLimit: 10GiB\n") {
		t.Fatalf("generated config did not include bounded storage settings:\n%s", configText)
	}
	if got := strings.Join(cfg.Runner.Labels, ","); !strings.Contains(got, "epar-docker-container-catthehacker-ubuntu") {
		t.Fatalf("runner labels = %q", got)
	}
	if !strings.Contains(out.String(), "start") || !strings.Contains(out.String(), "pool up --instances 2") {
		t.Fatalf("init output did not include next steps:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "Pool name prefix (press Enter to use build-box-01-a4f9c2):") {
		t.Fatalf("init output did not explain default prefix acceptance:\n%s", out.String())
	}
	for _, want := range []string{"1. Docker Container", "private daemon (default)", "2. Docker Sandboxes — recommended when ready"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("init output did not preserve explicit Docker Container default and capability-driven Docker Sandboxes labeling %q:\n%s", want, out.String())
		}
	}
	for _, want := range []string{"Repository access meanings:", "Selected repositories: Only repositories explicitly added", "Assessment: RECOMMENDED"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("init output did not explain runner-group policy term %q:\n%s", want, out.String())
		}
	}
}

func TestInitAllowsWarnedDefaultGroupAndWritesMatchingPolicy(t *testing.T) {
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
		In:                 strings.NewReader("123\nexample\nkey.pem\n1\n1\n\nn\n"),
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
	if !strings.Contains(out.String(), "*** SECURITY WARNING: THIS RUNNER GROUP IS NOT RECOMMENDED ***") || !strings.Contains(out.String(), "New or unintended repositories") || !strings.Contains(out.String(), "RECOMMENDED ACTION: Choose Back") || !strings.Contains(out.String(), "docs/runner-groups.md") || !strings.Contains(out.String(), "Continue anyway and generate a relaxed policy") || strings.Count(out.String(), "requires explicit review") != 1 {
		t.Fatalf("default-group warning/back option missing:\n%s", out.String())
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
	if err := runInitWithOptions(initOptions{
		ProjectRoot:        dir,
		ConfigPath:         path,
		SkipDockerCheck:    true,
		SkipHostTrustCheck: true,
		In:                 strings.NewReader("123\nexample\nkey.pem\n2\n2\n1\n\nn\n"),
		Out:                &bytes.Buffer{},
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
		In:                 strings.NewReader("123\nexample\nkey.pem\n2\n1\n1\n\nn\n"),
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
		In:                 strings.NewReader("123\nexample\nkey.pem\n1\n3\n"),
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

func TestSortRunnerGroupsForWizardPutsRestrictiveGroupsFirst(t *testing.T) {
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
	want := []string{"recommended", "inherited-recommended", "all-private-repositories", "Default", "public-selected"}
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
		In:                 strings.NewReader("123\nexample\nkey.pem\nr\n1\n\n"),
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
		In:                 strings.NewReader("123\nexample\nkey.pem\n1\n1\n\n"),
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

func TestInitCanDisableHostTrustOverlay(t *testing.T) {
	stubInitHostAndRandom(t, "Build Box 01", []byte{0xa4, 0xf9, 0xc2})
	stubNoWSL2(t)
	dir := t.TempDir()
	path := filepath.Join(dir, ".local", "config.yml")
	if err := runInitWithOptions(initOptions{
		ProjectRoot:        dir,
		ConfigPath:         path,
		SkipDockerCheck:    true,
		SkipHostTrustCheck: true,
		In:                 strings.NewReader("123456\nsolutionforest\n.local/github-app.pem\n1\n\n\nn\n"),
		Out:                &bytes.Buffer{},
	}); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Image.HostTrustMode != config.HostTrustModeDisabled {
		t.Fatalf("image.hostTrustMode = %q, want disabled", cfg.Image.HostTrustMode)
	}
}

func TestInitDoesNotWriteEnabledConfigWhenHostTrustPreflightFails(t *testing.T) {
	stubInitHostAndRandom(t, "Build Box 01", []byte{0xa4, 0xf9, 0xc2})
	stubNoWSL2(t)
	oldResolve := initResolveHostTrust
	initResolveHostTrust = func(context.Context, hosttrust.Options) (hosttrust.Snapshot, error) {
		return hosttrust.Snapshot{}, errors.New("collector unavailable")
	}
	t.Cleanup(func() { initResolveHostTrust = oldResolve })
	dir := t.TempDir()
	path := filepath.Join(dir, ".local", "config.yml")
	err := runInitWithOptions(initOptions{
		ProjectRoot:     dir,
		ConfigPath:      path,
		SkipDockerCheck: true,
		In:              strings.NewReader("123456\nsolutionforest\n.local/github-app.pem\n1\n\n\n"),
		Out:             &bytes.Buffer{},
	})
	if err == nil || !strings.Contains(err.Error(), "collector unavailable") {
		t.Fatalf("init error = %v, want collector failure", err)
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
		In:                 strings.NewReader("123456\nsolutionforest\n.local/github-app.pem\n1\n\ncustom-prefix\n\n"),
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
		In:                 strings.NewReader("123456\nsolutionforest\n.local/github-app.pem\n1\n\n-bad\nfixed-prefix\n\n"),
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
		In:                 strings.NewReader("123456\nsolutionforest\n.local/github-app.pem\n1\n3\n\n"),
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
	if !strings.Contains(out.String(), "Choose an available provider number or name shown above, or R to refresh.") {
		t.Fatalf("init output did not explain invalid provider input:\n%s", out.String())
	}
	if strings.Contains(out.String(), "Start runners now?") {
		t.Fatalf("standalone init unexpectedly asked whether to start runners:\n%s", out.String())
	}
}

func TestInitDockerSandboxesGeneratesConfigFromDiscoveredTemplate(t *testing.T) {
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
		In:                 strings.NewReader("123456\nsolutionforest\n.local/github-app.pem\n1\n1\n2\nnot-a-size\n30GiB\n\nn\n"),
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
	if got, want := cfg.DockerSandboxes.Template, "docker.io/library/epar-docker-sandboxes-catthehacker-act-22.04:20260723-r4-amd64"; got != want {
		t.Fatalf("dockerSandboxes.template = %q, want %q", got, want)
	}
	if got, want := cfg.DockerSandboxes.TemplateDigest, "sha256:"+strings.Repeat("c", 64); got != want {
		t.Fatalf("dockerSandboxes.templateDigest = %q, want %q", got, want)
	}
	if got, want := cfg.DockerSandboxes.PolicyGeneration, policyFingerprint; got != want {
		t.Fatalf("dockerSandboxes.policyGeneration = %q, want %q", got, want)
	}
	if got, want := cfg.DockerSandboxes.NetworkBaseline, config.DockerSandboxesNetworkBaselineOpen; got != want {
		t.Fatalf("dockerSandboxes.networkBaseline = %q, want %q", got, want)
	}
	for key, values := range map[string]struct{ got, want string }{
		"rootDisk":         {cfg.DockerSandboxes.RootDisk, "30GiB"},
		"dockerDisk":       {cfg.DockerSandboxes.DockerDisk, "100GiB"},
		"minHostFreeSpace": {cfg.DockerSandboxes.MinHostFreeSpace, "50GiB"},
	} {
		if values.got != values.want {
			t.Fatalf("dockerSandboxes.%s = %q, want %q", key, values.got, values.want)
		}
	}
	for _, want := range []string{"Docker Sandboxes — recommended", "provides EPAR's strongest current host boundary", "current host-global Balanced policy", "default to open public HTTP/HTTPS egress with owned deny-wins host-alias guardrails", "Verified local Docker Sandboxes source profiles:", "Catthehacker Ubuntu Full (recommended) — ghcr.io/catthehacker/ubuntu:full-latest", "Catthehacker Ubuntu Act 22.04 (current lean profile) — ghcr.io/catthehacker/ubuntu:act-22.04", "Automatic Docker Sandboxes source refresh is not implemented yet", "Resolved exact EPAR template tag: docker.io/library/epar-docker-sandboxes-catthehacker-act-22.04:20260723-r4-amd64", "Resolved full local template digest: sha256:" + strings.Repeat("c", 64), "8GiB on host", "default selection; capacity unmeasured", "Shared host template cache: 4GiB (already present; do not add this byte count arithmetically to each sandbox root disk).", "Measured guest root peak: unavailable", "Sandbox root filesystem total capacity is invalid", "Automatically selected sandbox Docker disk: 100GiB.", "Automatically selected minimum host free space: 50GiB.", "Verified policy fingerprint: " + policyFingerprint} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("init output omitted %q:\n%s", want, out.String())
		}
	}
}

func TestDockerSandboxesPrerequisitesRejectInsufficientMinimumCapacity(t *testing.T) {
	stubNoWSL2(t)
	oldReadiness := initDockerSandboxesReadiness
	oldCapacityCheck := initDockerSandboxesCapacityCheck
	initDockerSandboxesReadiness = func(context.Context) (dockersandboxes.HostReadiness, error) {
		return dockersandboxes.HostReadiness{ChecksPassed: 8, ChecksWarned: 1}, nil
	}
	initDockerSandboxesCapacityCheck = func(rootDisk, dockerDisk, minHostFreeSpace uint64) (initDockerSandboxesCapacityResult, error) {
		if rootDisk != 20<<30 || dockerDisk != 100<<30 || minHostFreeSpace != 50<<30 {
			t.Fatalf("minimum capacity check = root %d, Docker %d, reserve %d", rootDisk, dockerDisk, minHostFreeSpace)
		}
		return initDockerSandboxesCapacityResult{
			StorageRoot:    `C:\Users\runner\AppData\Local\DockerSandboxes`,
			AvailableBytes: 140 << 30,
			TotalBytes:     1 << 40,
			Reservation:    120 << 30,
			HostWatermark:  50 << 30,
			RequiredBytes:  170 << 30,
			DeficitBytes:   30 << 30,
			CapacityStatus: storage.CapacityInsufficient,
		}, nil
	}
	t.Cleanup(func() {
		initDockerSandboxesReadiness = oldReadiness
		initDockerSandboxesCapacityCheck = oldCapacityCheck
	})

	got := detectInitProviderPrerequisites(context.Background(), sandboxpromotion.WindowsAMD64, true)
	if got.DockerSandboxesAvailable {
		t.Fatal("Docker Sandboxes was available despite insufficient minimum capacity")
	}
	for _, want := range []string{`C:\Users\runner\AppData\Local\DockerSandboxes`, "140GiB available", "require 170GiB", "shortfall 30GiB"} {
		if !strings.Contains(got.DockerSandboxesStatus, want) {
			t.Fatalf("Docker Sandboxes status omitted %q: %s", want, got.DockerSandboxesStatus)
		}
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
	if providerType != "docker-sandboxes" || selectedRecord.Template != record.Template || profile != nil {
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

func TestDockerSandboxesProfileRejectsSelectedCapacityWithoutWritingConfig(t *testing.T) {
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
		if rootDisk != 30<<30 || dockerDisk != 100<<30 || minHostFreeSpace != 50<<30 {
			t.Fatalf("selected capacity check = root %d, Docker %d, reserve %d", rootDisk, dockerDisk, minHostFreeSpace)
		}
		return initDockerSandboxesCapacityResult{
			StorageRoot:    `C:\Users\runner\AppData\Local\DockerSandboxes`,
			AvailableBytes: 140 << 30,
			TotalBytes:     1 << 40,
			Reservation:    130 << 30,
			HostWatermark:  50 << 30,
			RequiredBytes:  180 << 30,
			DeficitBytes:   40 << 30,
			CapacityStatus: storage.CapacityInsufficient,
		}, nil
	}
	t.Cleanup(func() { initDockerSandboxesCapacityCheck = oldCapacityCheck })

	var out bytes.Buffer
	profile, accepted, err := promptDockerSandboxesProfile(context.Background(), t.TempDir(), sandboxpromotion.WindowsAMD64, &out, bufio.NewReader(strings.NewReader("1\n30GiB\n")))
	if err == nil || !strings.Contains(err.Error(), "no config was written") {
		t.Fatalf("capacity rejection error = %v", err)
	}
	if profile != nil || accepted {
		t.Fatalf("capacity rejection returned profile=%+v accepted=%t", profile, accepted)
	}
	for _, want := range []string{"Docker Sandboxes capacity admission failed:", "Available: 140GiB", "Required before creation: 180GiB", "Shortfall: 40GiB", "storage prune --provider docker-sandboxes"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("capacity rejection output omitted %q:\n%s", want, out.String())
		}
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
				In:                 strings.NewReader("123456\nsolutionforest\n.local/github-app.pem\n1\n\n\n\n\nn\n"),
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
				"Docker Sandboxes setup:",
				"recommended default because Docker and sbx diagnostics passed on this machine",
				"Default resource reservations are recommended starting values",
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

func TestInitDockerSandboxesDerivesRootDiskFromExactMeasurement(t *testing.T) {
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
		In:                 strings.NewReader("123456\nsolutionforest\n.local/github-app.pem\n1\n1\n2\n99GiB\n100GiB\n\nn\n"),
		Out:                &out,
	}); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := cfg.DockerSandboxes.RootDisk, "30GiB"; got != want {
		t.Fatalf("dockerSandboxes.rootDisk = %q, want %q", got, want)
	}
	if got, want := cfg.DockerSandboxes.Template, template.Reference; got != want {
		t.Fatalf("dockerSandboxes.template = %q, want measured default %q", got, want)
	}
	for _, want := range []string{
		"17.44GiB on host",
		"default selection; capacity unmeasured",
		"Shared host template cache: 17.44GiB (already present; do not add this byte count arithmetically to each sandbox root disk).",
		"Measured guest root peak: 309.73MiB (test workload).",
		"Automatically selected sandbox root filesystem total capacity: 30GiB.",
		"Sandbox Docker disk must be at least 100GiB.",
		"Automatically selected minimum host free space: 50GiB.",
		"EPAR rechecks current Docker Sandboxes storage free space, existing and uncertain reservations",
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
		In:                 strings.NewReader("123456\nsolutionforest\n.local/github-app.pem\n1\n1\ny\n\n\n\nn\n"),
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
	if got := strings.Count(out.String(), "Retry Docker Sandboxes setup checks?"); got != 1 {
		t.Fatalf("setup retry prompts = %d, want 1:\n%s", got, out.String())
	}
	if discoveryCalls != 2 {
		t.Fatalf("Docker Sandboxes discovery calls = %d, want 2", discoveryCalls)
	}
	if !strings.Contains(out.String(), "Docker Sandboxes setup preparation failed: template cache unavailable") || !strings.Contains(out.String(), "build and load a Candidate A template") {
		t.Fatalf("init output did not explain Docker Sandboxes discovery recovery:\n%s", out.String())
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
		In:                 strings.NewReader("123456\nsolutionforest\n.local/github-app.pem\n1\n1\nn\n"),
		Out:                &out,
	})
	if err == nil || !strings.Contains(err.Error(), "no config was written") {
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
		In:                 strings.NewReader("123456\nsolutionforest\n.local/github-app.pem\n1\n\n\nn\n"),
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
	if got, want := cfg.DockerSandboxes.Template, record.Template; got != want {
		t.Fatalf("dockerSandboxes.template = %q, want %q", got, want)
	}
	if got, want := cfg.DockerSandboxes.TemplateDigest, record.TemplateDigest; got != want {
		t.Fatalf("dockerSandboxes.templateDigest = %q, want %q", got, want)
	}
	if got, want := cfg.DockerSandboxes.PolicyGeneration, record.PolicyFingerprint; got != want {
		t.Fatalf("dockerSandboxes.policyGeneration = %q, want %q", got, want)
	}
	for key, values := range map[string]struct {
		got  string
		want string
	}{
		"rootDisk":         {cfg.DockerSandboxes.RootDisk, "120GiB"},
		"dockerDisk":       {cfg.DockerSandboxes.DockerDisk, "100GiB"},
		"minHostFreeSpace": {cfg.DockerSandboxes.MinHostFreeSpace, "50GiB"},
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
				In:                 strings.NewReader("123456\nsolutionforest\n.local/github-app.pem\n1\ndocker-sandboxes\n1\n\nn\n"),
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
		In:                 strings.NewReader("123456\nsolutionforest\n.local/github-app.pem\n1\n3\n\n"),
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
	initHostname = func() (string, error) { return hostname, nil }
	initRandomRead = fixedRandomRead(random)
	stubInitRunnerGroupClient(t)
	t.Cleanup(func() {
		initHostname = oldHostname
		initRandomRead = oldRandomRead
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
	initGOOS = "windows"
	initWSLStatus = func(context.Context) ([]byte, error) {
		return []byte("Default Distribution: Ubuntu\nDefault Version: 2\n"), nil
	}
	t.Cleanup(func() {
		initGOOS = oldGOOS
		initWSLStatus = oldWSLStatus
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
	})
}

func stubInitSandboxPromotion(t *testing.T, record sandboxpromotion.Record, result sandboxpromotion.PreflightResult) {
	t.Helper()
	oldPlatform := initSandboxPromotionPlatform
	oldLookup := initSandboxPromotionLookup
	oldPreflight := initDockerSandboxesPreflight
	oldReadiness := initDockerSandboxesReadiness
	oldCapacityCheck := initDockerSandboxesCapacityCheck
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
