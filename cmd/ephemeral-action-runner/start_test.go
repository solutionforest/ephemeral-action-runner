package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
	"github.com/solutionforest/ephemeral-action-runner/internal/hosttrust"
	"github.com/solutionforest/ephemeral-action-runner/internal/pool"
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
	err := runStartWithOptions(startOptions{
		Context:     context.Background(),
		ProjectRoot: dir,
		ConfigPath:  "custom.yml",
		Instances:   3,
		Out:         &bytes.Buffer{},
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
}

func TestStartInteractiveMissingConfigRunsInitAndContinues(t *testing.T) {
	dir := t.TempDir()
	stubNoWSL2(t)
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
		In:          strings.NewReader("123456\nsolutionforest\n.local/github-app.pem\n"),
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
	if !strings.Contains(out.String(), "Continuing with") {
		t.Fatalf("output missing continuation message:\n%s", out.String())
	}
	if fake.ensureCalls != 1 || fake.runCalls != 1 {
		t.Fatalf("ensure/run calls = %d/%d, want 1/1", fake.ensureCalls, fake.runCalls)
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
		In:          strings.NewReader("123456\nsolutionforest\n.local/github-app.pem\n2\n\n"),
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
		t.Fatal("Docker availability should not be checked for Tart")
		return nil
	}

	fake := &fakeStarterManager{}
	var out bytes.Buffer
	err := runStartWithOptions(startOptions{
		Context:     context.Background(),
		ProjectRoot: dir,
		In:          strings.NewReader("123456\nsolutionforest\n.local/github-app.pem\n2\n\n"),
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

type fakeStarterManager struct {
	ensureCalls int
	runCalls    int
	runOptions  pool.RunOptions
}

func (m *fakeStarterManager) EnsureImage(context.Context) error {
	m.ensureCalls++
	return nil
}

func (m *fakeStarterManager) RunPool(_ context.Context, opts pool.RunOptions) error {
	m.runCalls++
	m.runOptions = opts
	return nil
}
