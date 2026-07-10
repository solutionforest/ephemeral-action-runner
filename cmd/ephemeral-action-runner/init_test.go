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
)

func TestInitCreatesDefaultDockerDindConfig(t *testing.T) {
	stubInitHostAndRandom(t, "Build Box 01", []byte{0xa4, 0xf9, 0xc2})

	dir := t.TempDir()
	path := filepath.Join(dir, ".local", "config.yml")
	var out bytes.Buffer

	if err := runInitWithOptions(initOptions{
		ProjectRoot:     dir,
		ConfigPath:      path,
		SkipDockerCheck: true,
		In:              strings.NewReader("123456\nsolutionforest\n.local/github-app.pem\n\n"),
		Out:             &out,
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
	if got, want := cfg.Provider.Type, "docker-dind"; got != want {
		t.Fatalf("provider.type = %q, want %q", got, want)
	}
	if got, want := cfg.Image.SourceImage, "gitea/runner-images:ubuntu-latest-full"; got != want {
		t.Fatalf("image.sourceImage = %q, want %q", got, want)
	}
	if got, want := cfg.Image.OutputImage, "epar-docker-dind-gitea-ubuntu"; got != want {
		t.Fatalf("image.outputImage = %q, want %q", got, want)
	}
	if got, want := cfg.Pool.Instances, 1; got != want {
		t.Fatalf("pool.instances = %d, want %d", got, want)
	}
	if got, want := cfg.Pool.NamePrefix, "build-box-01-a4f9c2"; got != want {
		t.Fatalf("pool.namePrefix = %q, want %q", got, want)
	}
	if got := strings.Join(cfg.Runner.Labels, ","); !strings.Contains(got, "epar-docker-dind-gitea-ubuntu") {
		t.Fatalf("runner labels = %q", got)
	}
	if !strings.Contains(out.String(), "start") || !strings.Contains(out.String(), "pool up --instances 2") {
		t.Fatalf("init output did not include next steps:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "Pool name prefix (press Enter to use build-box-01-a4f9c2):") {
		t.Fatalf("init output did not explain default prefix acceptance:\n%s", out.String())
	}
}

func TestInitAcceptsCustomPoolNamePrefix(t *testing.T) {
	stubInitHostAndRandom(t, "Build Box 01", []byte{0xa4, 0xf9, 0xc2})

	dir := t.TempDir()
	path := filepath.Join(dir, ".local", "config.yml")
	if err := runInitWithOptions(initOptions{
		ProjectRoot:     dir,
		ConfigPath:      path,
		SkipDockerCheck: true,
		In:              strings.NewReader("123456\nsolutionforest\n.local/github-app.pem\ncustom-prefix\n"),
		Out:             &bytes.Buffer{},
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

	dir := t.TempDir()
	path := filepath.Join(dir, ".local", "config.yml")
	var out bytes.Buffer
	if err := runInitWithOptions(initOptions{
		ProjectRoot:     dir,
		ConfigPath:      path,
		SkipDockerCheck: true,
		In:              strings.NewReader("123456\nsolutionforest\n.local/github-app.pem\n-bad\nfixed-prefix\n"),
		Out:             &out,
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
		ProjectRoot:     dir,
		ConfigPath:      path,
		SkipDockerCheck: true,
		In:              strings.NewReader("123\norg\nkey.pem\n"),
		Out:             &bytes.Buffer{},
	})
	if err == nil || !strings.Contains(err.Error(), "config already exists") {
		t.Fatalf("error = %v, want existing config refusal", err)
	}
}

func TestInitChecksDockerByDefault(t *testing.T) {
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
		In:          strings.NewReader("123\norg\nkey.pem\n"),
		Out:         &bytes.Buffer{},
	})
	if err == nil || !strings.Contains(err.Error(), "Docker is required") {
		t.Fatalf("error = %v, want Docker requirement", err)
	}
}

func stubInitHostAndRandom(t *testing.T, hostname string, random []byte) {
	t.Helper()
	oldHostname := initHostname
	oldRandomRead := initRandomRead
	initHostname = func() (string, error) { return hostname, nil }
	initRandomRead = fixedRandomRead(random)
	t.Cleanup(func() {
		initHostname = oldHostname
		initRandomRead = oldRandomRead
	})
}

func fixedRandomRead(random []byte) func([]byte) (int, error) {
	return func(data []byte) (int, error) {
		copy(data, random)
		return len(data), nil
	}
}
