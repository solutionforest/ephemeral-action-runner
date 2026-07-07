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
	dir := t.TempDir()
	path := filepath.Join(dir, ".local", "config.yml")
	var out bytes.Buffer

	if err := runInitWithOptions(initOptions{
		ProjectRoot:     dir,
		ConfigPath:      path,
		SkipDockerCheck: true,
		In:              strings.NewReader("123456\nsolutionforest\n.local/github-app.pem\n"),
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
	if got := strings.Join(cfg.Runner.Labels, ","); !strings.Contains(got, "epar-docker-dind-gitea-ubuntu") {
		t.Fatalf("runner labels = %q", got)
	}
	if !strings.Contains(out.String(), "start") || !strings.Contains(out.String(), "pool up --instances 2") {
		t.Fatalf("init output did not include next steps:\n%s", out.String())
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
