package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
	"github.com/solutionforest/ephemeral-action-runner/internal/hosttrust"
	"github.com/solutionforest/ephemeral-action-runner/internal/pool"
	sandboxpromotion "github.com/solutionforest/ephemeral-action-runner/internal/provider/dockersandboxes/promotion"
)

func TestNoArgsRoutesToStart(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(configPath, []byte("config"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("EPAR_CONFIG", configPath)

	fake := &fakeStarterManager{}
	oldFactory := newStarterManager
	t.Cleanup(func() {
		newStarterManager = oldFactory
	})
	newStarterManager = func(path, _ string, _ bool, githubEnabled bool) (starterManager, error) {
		if path != configPath {
			t.Fatalf("config path = %q, want %q", path, configPath)
		}
		if !githubEnabled {
			t.Fatal("githubEnabled = false, want true")
		}
		return fake, nil
	}

	if err := run(nil); err != nil {
		t.Fatal(err)
	}
	if fake.ensureCalls != 1 || fake.runCalls != 1 {
		t.Fatalf("ensure/run calls = %d/%d, want 1/1", fake.ensureCalls, fake.runCalls)
	}
	if fake.runOptions.Instances != 0 {
		t.Fatalf("instances = %d, want 0 to use config", fake.runOptions.Instances)
	}
}

func TestStartPropagatesConfigAndInstances(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "custom.yml")
	if err := os.WriteFile(configPath, []byte("config"), 0600); err != nil {
		t.Fatal(err)
	}
	fake := &fakeStarterManager{}
	var gotPath string
	var out bytes.Buffer
	err := runStartWithOptions(startOptions{
		Context:     context.Background(),
		ProjectRoot: dir,
		ConfigPath:  "custom.yml",
		Instances:   3,
		Out:         &out,
		ManagerFactory: func(path, _ string, _ bool, _ bool) (starterManager, error) {
			gotPath = path
			return fake, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != configPath {
		t.Fatalf("config path = %q, want %q", gotPath, configPath)
	}
	if fake.runOptions.Instances != 3 {
		t.Fatalf("instances = %d, want 3", fake.runOptions.Instances)
	}
	if !strings.Contains(out.String(), "Press Ctrl-C once to stop; wait for cleanup confirmation before closing this window.") {
		t.Fatalf("start guidance = %q", out.String())
	}
	if strings.Contains(out.String(), "Start runners now?") {
		t.Fatalf("existing config unexpectedly triggered the new-config start prompt:\n%s", out.String())
	}
}

func TestStartConfiguresOneInvocationStorageOverride(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(configPath, []byte("config"), 0600); err != nil {
		t.Fatal(err)
	}
	fake := &fakeStarterManager{}
	err := runStartWithOptions(startOptions{
		Context:                  context.Background(),
		ProjectRoot:              dir,
		ConfigPath:               configPath,
		AllowInsufficientStorage: true,
		StorageOverrideCommand:   "./start --allow-insufficient-storage",
		Out:                      &bytes.Buffer{},
		ManagerFactory: func(string, string, bool, bool) (starterManager, error) {
			return fake, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !fake.allowStorage || fake.overrideHint != "./start --allow-insufficient-storage" {
		t.Fatalf("storage override = allow %t hint %q", fake.allowStorage, fake.overrideHint)
	}
}

func TestMatchingStartCommandPreservesWrapperEntryPoint(t *testing.T) {
	t.Setenv("EPAR_INVOCATION", "start")
	if got, want := matchingStartCommand([]string{"--allow-insufficient-storage"}), "./start --allow-insufficient-storage"; got != want {
		t.Fatalf("matchingStartCommand() = %q, want %q", got, want)
	}
}

func TestStartInteractiveMissingConfigRunsInitAndContinues(t *testing.T) {
	dir := t.TempDir()
	stubNoWSL2(t)
	stubInitHostAndRandom(t, "Build Box 01", []byte{0xa4, 0xf9, 0xc2})
	oldInteractive := stdinIsInteractive
	oldDocker := dockerAvailable
	oldResolveHostTrust := initResolveHostTrust
	t.Cleanup(func() {
		stdinIsInteractive = oldInteractive
		dockerAvailable = oldDocker
		initResolveHostTrust = oldResolveHostTrust
	})
	stdinIsInteractive = func() bool { return true }
	dockerAvailable = func(context.Context) error { return nil }
	initResolveHostTrust = func(context.Context, hosttrust.Options) (hosttrust.Snapshot, error) {
		return hosttrust.Snapshot{}, nil
	}

	fake := &fakeStarterManager{}
	var out bytes.Buffer
	err := runStartWithOptions(startOptions{
		Context:     context.Background(),
		ProjectRoot: dir,
		In:          strings.NewReader("123456\nsolutionforest\n.local/github-app.pem\n1\n\n\nn\n\n\n\n\n"),
		Out:         &out,
		ManagerFactory: func(path, _ string, _ bool, _ bool) (starterManager, error) {
			if path != filepath.Join(dir, ".local", "config.yml") {
				t.Fatalf("config path = %q", path)
			}
			return fake, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".local", "config.yml")); err != nil {
		t.Fatalf("config was not created: %v", err)
	}
	for _, want := range []string{"Start runners now?", "Continuing with"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output missing %q:\n%s", want, out.String())
		}
	}
	if fake.ensureCalls != 1 || fake.runCalls != 1 {
		t.Fatalf("ensure/run calls = %d/%d, want 1/1", fake.ensureCalls, fake.runCalls)
	}
}

func TestStartInteractiveMissingConfigCanExitToReview(t *testing.T) {
	dir := t.TempDir()
	stubNoWSL2(t)
	stubInitHostAndRandom(t, "Build Box 01", []byte{0xa4, 0xf9, 0xc2})
	oldInteractive := stdinIsInteractive
	oldDocker := dockerAvailable
	oldResolveHostTrust := initResolveHostTrust
	t.Cleanup(func() {
		stdinIsInteractive = oldInteractive
		dockerAvailable = oldDocker
		initResolveHostTrust = oldResolveHostTrust
	})
	stdinIsInteractive = func() bool { return true }
	dockerAvailable = func(context.Context) error { return nil }
	initResolveHostTrust = func(context.Context, hosttrust.Options) (hosttrust.Snapshot, error) {
		return hosttrust.Snapshot{}, nil
	}

	var out bytes.Buffer
	err := runStartWithOptions(startOptions{
		Context:     context.Background(),
		ProjectRoot: dir,
		In:          strings.NewReader("123456\nsolutionforest\n.local/github-app.pem\n1\n1\n\nn\n\n\nn\nn\n"),
		Out:         &out,
		ManagerFactory: func(string, string, bool, bool) (starterManager, error) {
			t.Fatal("manager factory should not run after choosing to review the new config")
			return nil, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, ".local", "config.yml")
	if _, err := os.Stat(configPath); err != nil {
		t.Fatalf("config was not created: %v", err)
	}
	for _, want := range []string{"Start runners now?", "Config saved at " + configPath, "Exiting before runner startup", "Review the config"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("review exit output omitted %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "Continuing with") {
		t.Fatalf("review exit unexpectedly continued startup:\n%s", out.String())
	}
}

func TestStartInteractiveMissingConfigCanSelectWSL2(t *testing.T) {
	dir := t.TempDir()
	stubInitHostAndRandom(t, "Build Box 01", []byte{0xa4, 0xf9, 0xc2})
	stubWSL2Available(t)
	oldInteractive := stdinIsInteractive
	oldDocker := dockerAvailable
	t.Cleanup(func() {
		stdinIsInteractive = oldInteractive
		dockerAvailable = oldDocker
	})
	stdinIsInteractive = func() bool { return true }
	dockerAvailable = func(context.Context) error { return nil }

	fake := &fakeStarterManager{}
	var out bytes.Buffer
	err := runStartWithOptions(startOptions{
		Context:     context.Background(),
		ProjectRoot: dir,
		In:          strings.NewReader("123456\nsolutionforest\n.local/github-app.pem\n1\n3\n\n"),
		Out:         &out,
		ManagerFactory: func(path, _ string, _ bool, _ bool) (starterManager, error) {
			if path != filepath.Join(dir, ".local", "config.yml") {
				t.Fatalf("config path = %q", path)
			}
			return fake, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(filepath.Join(dir, ".local", "config.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := cfg.Provider.Type, "wsl"; got != want {
		t.Fatalf("provider.type = %q, want %q", got, want)
	}
	if !strings.Contains(out.String(), "Continuing with") {
		t.Fatalf("output missing continuation message:\n%s", out.String())
	}
	if fake.ensureCalls != 1 || fake.runCalls != 1 {
		t.Fatalf("ensure/run calls = %d/%d, want 1/1", fake.ensureCalls, fake.runCalls)
	}
}

func TestStartInteractiveMissingConfigCanSelectDockerSandboxes(t *testing.T) {
	dir := t.TempDir()
	stubInitHostAndRandom(t, "Build Box 01", []byte{0xa4, 0xf9, 0xc2})
	stubNoWSL2(t)
	policyFingerprint := "sha256:" + strings.Repeat("b", 64)
	stubInitDockerSandboxesSetup(t, sandboxpromotion.WindowsAMD64, initDockerSandboxesDiscovery{
		Templates: []initDockerSandboxesTemplate{{
			Reference: "docker.io/library/epar-docker-sandboxes-catthehacker-full:preview",
			Digest:    "sha256:" + strings.Repeat("a", 64),
			CacheID:   strings.Repeat("a", 12),
			Platform:  "linux/amd64",
			Size:      8 << 30,
		}},
		PolicyFingerprint: policyFingerprint,
	}, nil)
	oldInteractive := stdinIsInteractive
	oldDocker := dockerAvailable
	oldResolveHostTrust := initResolveHostTrust
	t.Cleanup(func() {
		stdinIsInteractive = oldInteractive
		dockerAvailable = oldDocker
		initResolveHostTrust = oldResolveHostTrust
	})
	stdinIsInteractive = func() bool { return true }
	dockerAvailable = func(context.Context) error { return nil }
	initResolveHostTrust = func(context.Context, hosttrust.Options) (hosttrust.Snapshot, error) {
		return hosttrust.Snapshot{}, nil
	}

	fake := &fakeStarterManager{}
	var out bytes.Buffer
	err := runStartWithOptions(startOptions{
		Context:     context.Background(),
		ProjectRoot: dir,
		In:          strings.NewReader("123456\nsolutionforest\n.local/github-app.pem\n1\n1\n\nn\n\n\n\n\n\n"),
		Out:         &out,
		ManagerFactory: func(path, _ string, _ bool, _ bool) (starterManager, error) {
			if path != filepath.Join(dir, ".local", "config.yml") {
				t.Fatalf("config path = %q", path)
			}
			return fake, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(filepath.Join(dir, ".local", "config.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := cfg.Provider.Type, "docker-sandboxes"; got != want {
		t.Fatalf("provider.type = %q, want %q", got, want)
	}
	if got, want := cfg.DockerSandboxes.PolicyGeneration, policyFingerprint; got != want {
		t.Fatalf("dockerSandboxes.policyGeneration = %q, want %q", got, want)
	}
	if !strings.Contains(out.String(), "Docker Sandboxes — recommended") || !strings.Contains(out.String(), "Continuing with") {
		t.Fatalf("output did not include capability-ready Docker Sandboxes selection and start continuation:\n%s", out.String())
	}
	if fake.ensureCalls != 1 || fake.runCalls != 1 {
		t.Fatalf("ensure/run calls = %d/%d, want 1/1", fake.ensureCalls, fake.runCalls)
	}
}

func TestStartInteractiveMissingConfigCanSelectTartWithoutDocker(t *testing.T) {
	dir := t.TempDir()
	stubInitHostAndRandom(t, "Build Box 01", []byte{0xa4, 0xf9, 0xc2})
	stubTartAvailable(t)
	oldInteractive := stdinIsInteractive
	oldDocker := dockerAvailable
	t.Cleanup(func() {
		stdinIsInteractive = oldInteractive
		dockerAvailable = oldDocker
	})
	stdinIsInteractive = func() bool { return true }
	dockerAvailable = func(context.Context) error {
		return errors.New("Docker is unavailable on this Mac")
	}

	fake := &fakeStarterManager{}
	var out bytes.Buffer
	err := runStartWithOptions(startOptions{
		Context:     context.Background(),
		ProjectRoot: dir,
		In:          strings.NewReader("123456\nsolutionforest\n.local/github-app.pem\n1\n4\n\n"),
		Out:         &out,
		ManagerFactory: func(path, _ string, _ bool, _ bool) (starterManager, error) {
			if path != filepath.Join(dir, ".local", "config.yml") {
				t.Fatalf("config path = %q", path)
			}
			return fake, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(filepath.Join(dir, ".local", "config.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := cfg.Provider.Type, "tart"; got != want {
		t.Fatalf("provider.type = %q, want %q", got, want)
	}
	if !strings.Contains(out.String(), "Continuing with") {
		t.Fatalf("output missing continuation message:\n%s", out.String())
	}
	if fake.ensureCalls != 1 || fake.runCalls != 1 {
		t.Fatalf("ensure/run calls = %d/%d, want 1/1", fake.ensureCalls, fake.runCalls)
	}
}

func TestStartNonInteractiveMissingConfigFails(t *testing.T) {
	dir := t.TempDir()
	oldInteractive := stdinIsInteractive
	t.Cleanup(func() {
		stdinIsInteractive = oldInteractive
	})
	stdinIsInteractive = func() bool { return false }

	err := runStartWithOptions(startOptions{
		Context:     context.Background(),
		ProjectRoot: dir,
		Out:         &bytes.Buffer{},
		ManagerFactory: func(string, string, bool, bool) (starterManager, error) {
			t.Fatal("manager factory should not run without config")
			return nil, nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "no EPAR config found") {
		t.Fatalf("error = %v, want missing config error", err)
	}
}

func TestStartRejectsNonPositiveInstancesOverride(t *testing.T) {
	err := runStart([]string{"--instances", "0"})
	if err == nil || !strings.Contains(err.Error(), "--instances must be 1 or greater") {
		t.Fatalf("error = %v, want invalid instances error", err)
	}
}

func TestStartPreflightsBeforeImageAndPool(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(configPath, []byte("config"), 0600); err != nil {
		t.Fatal(err)
	}
	fake := &fakeStarterManager{}
	err := runStartWithOptions(startOptions{
		Context:     context.Background(),
		ProjectRoot: dir,
		ConfigPath:  configPath,
		Register:    true,
		Out:         &bytes.Buffer{},
		ManagerFactory: func(string, string, bool, bool) (starterManager, error) {
			return fake, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(fake.calls, ","); got != "preflight,image,pool" {
		t.Fatalf("call order = %q, want preflight,image,pool", got)
	}

	fake = &fakeStarterManager{preflightErr: errors.New("unsafe group")}
	err = runStartWithOptions(startOptions{
		Context:                  context.Background(),
		ProjectRoot:              dir,
		ConfigPath:               configPath,
		Register:                 true,
		AllowInsufficientStorage: true,
		Out:                      &bytes.Buffer{},
		ManagerFactory: func(string, string, bool, bool) (starterManager, error) {
			return fake, nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "unsafe group") {
		t.Fatalf("start error = %v, want preflight failure", err)
	}
	if got := strings.Join(fake.calls, ","); got != "preflight" {
		t.Fatalf("call order after rejection = %q, want preflight only", got)
	}
	if !fake.allowStorage {
		t.Fatal("storage override was not configured before the non-storage safety check")
	}
}

type fakeStarterManager struct {
	preflightErr error
	ensureCalls  int
	runCalls     int
	runOptions   pool.RunOptions
	calls        []string
	allowStorage bool
	overrideHint string
}

func (m *fakeStarterManager) ConfigureStorageAdmissionOverride(allow bool, command string) {
	m.allowStorage = allow
	m.overrideHint = command
}

func (m *fakeStarterManager) PreflightRunnerGroup(context.Context) error {
	m.calls = append(m.calls, "preflight")
	return m.preflightErr
}

func (m *fakeStarterManager) EnsureImage(context.Context) error {
	m.calls = append(m.calls, "image")
	m.ensureCalls++
	return nil
}

func (m *fakeStarterManager) RunPool(_ context.Context, opts pool.RunOptions) error {
	m.calls = append(m.calls, "pool")
	m.runCalls++
	m.runOptions = opts
	return nil
}
