package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
	gh "github.com/solutionforest/ephemeral-action-runner/internal/github"
	"github.com/solutionforest/ephemeral-action-runner/internal/hosttrust"
)

func TestInitCreatesDefaultDockerDindConfig(t *testing.T) {
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
	if got, want := cfg.Provider.Type, "docker-dind"; got != want {
		t.Fatalf("provider.type = %q, want %q", got, want)
	}
	if got, want := cfg.Image.SourceImage, "ghcr.io/catthehacker/ubuntu:full-latest"; got != want {
		t.Fatalf("image.sourceImage = %q, want %q", got, want)
	}
	if got, want := cfg.Image.OutputImage, "epar-docker-dind-catthehacker-ubuntu"; got != want {
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
	if got := strings.Join(cfg.Runner.Labels, ","); !strings.Contains(got, "epar-docker-dind-catthehacker-ubuntu") {
		t.Fatalf("runner labels = %q", got)
	}
	if !strings.Contains(out.String(), "start") || !strings.Contains(out.String(), "pool up --instances 2") {
		t.Fatalf("init output did not include next steps:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "Pool name prefix (press Enter to use build-box-01-a4f9c2):") {
		t.Fatalf("init output did not explain default prefix acceptance:\n%s", out.String())
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
		In:                 strings.NewReader("123456\nsolutionforest\n.local/github-app.pem\n1\n\nn\n"),
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
		In:                 strings.NewReader("123456\nsolutionforest\n.local/github-app.pem\n1\ncustom-prefix\n"),
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
		In:                 strings.NewReader("123456\nsolutionforest\n.local/github-app.pem\n1\n-bad\nfixed-prefix\n"),
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

	err := runInitWithOptions(initOptions{
		ProjectRoot: t.TempDir(),
		ConfigPath:  filepath.Join(t.TempDir(), ".local", "config.yml"),
		In:          strings.NewReader("123\norg\nkey.pem\n1\n"),
		Out:         &bytes.Buffer{},
	})
	if err == nil || !strings.Contains(err.Error(), "Docker is required") {
		t.Fatalf("error = %v, want Docker requirement", err)
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
		In:                 strings.NewReader("123456\nsolutionforest\n.local/github-app.pem\n1\n2\n\n"),
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
	if !strings.Contains(out.String(), "2. WSL2") {
		t.Fatalf("init output did not offer WSL2:\n%s", out.String())
	}
	providerPrompt := strings.Index(out.String(), "Runner provider:")
	prefixPrompt := strings.Index(out.String(), "Pool name prefix must be unique")
	if providerPrompt < 0 || prefixPrompt < 0 || providerPrompt > prefixPrompt {
		t.Fatalf("provider prompt did not appear before pool name prefix:\n%s", out.String())
	}
}

func TestInitWSL2ChoiceDefaultsToDockerDindAndRepromptsInvalidValues(t *testing.T) {
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
	if !strings.Contains(string(configBytes), "type: docker-dind") {
		t.Fatalf("config did not use the default Docker-DinD provider:\n%s", configBytes)
	}
	if !strings.Contains(out.String(), "Runner provider must be 1 (Docker-DinD) or 2 (WSL2).") {
		t.Fatalf("init output did not explain invalid provider input:\n%s", out.String())
	}
}

func TestInitOffersTartConfigWhenAvailable(t *testing.T) {
	stubInitHostAndRandom(t, "Build Box 01", []byte{0xa4, 0xf9, 0xc2})
	stubTartAvailable(t)
	oldDockerAvailable := dockerAvailable
	dockerAvailable = func(context.Context) error {
		t.Fatal("Docker availability should not be checked for Tart")
		return nil
	}
	t.Cleanup(func() { dockerAvailable = oldDockerAvailable })

	dir := t.TempDir()
	path := filepath.Join(dir, ".local", "config.yml")
	var out bytes.Buffer
	if err := runInitWithOptions(initOptions{
		ProjectRoot:        dir,
		ConfigPath:         path,
		SkipHostTrustCheck: true,
		In:                 strings.NewReader("654321\nexample\n.local/github-app.pem\n1\n2\n\n"),
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
	if !strings.Contains(out.String(), "2. Tart (experimental)") {
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
