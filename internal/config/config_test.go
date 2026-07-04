package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadYAMLSubset(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(path, []byte(`
github:
  appId: 123
  organization: example
  privateKeyPath: /tmp/key.pem
pool:
  minIdle: 2
  maxInstances: 3
  namePrefix: epar-test
runner:
  labels:
    - self-hosted
    - linux
    - ARM64
    - custom
  ephemeral: true
provider:
  type: tart
  sourceImage: runner-base
  network: softnet
  rosettaTag: rosetta
  installRoot: work/custom-wsl
image:
  customInstallScripts:
    - .local/web-e2e.sh
    - /opt/epar/install-extra.sh
`), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GitHub.AppID != 123 || cfg.GitHub.Organization != "example" {
		t.Fatalf("unexpected github config: %+v", cfg.GitHub)
	}
	if got, want := cfg.Runner.Labels[3], "custom"; got != want {
		t.Fatalf("label = %q, want %q", got, want)
	}
	if got, want := cfg.Provider.InstallRoot, "work/custom-wsl"; got != want {
		t.Fatalf("provider.installRoot = %q, want %q", got, want)
	}
	if got, want := cfg.Provider.RosettaTag, "rosetta"; got != want {
		t.Fatalf("provider.rosettaTag = %q, want %q", got, want)
	}
	if got, want := len(cfg.Image.CustomInstallScripts), 2; got != want {
		t.Fatalf("custom install scripts = %d, want %d", got, want)
	}
	if err := Validate(cfg); err != nil {
		t.Fatal(err)
	}
}

func TestLoadDockerDindPlatform(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(path, []byte(`
pool:
  minIdle: 1
  maxInstances: 1
  namePrefix: epar-dind
runner:
  labels: [self-hosted, linux, ARM64, epar-docker-dind]
provider:
  type: docker-dind
  sourceImage: epar-docker-dind-ubuntu-24
  platform: linux/arm64
`), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := cfg.Provider.Platform, "linux/arm64"; got != want {
		t.Fatalf("provider.platform = %q, want %q", got, want)
	}
	if err := Validate(cfg); err != nil {
		t.Fatal(err)
	}
}

func TestLoadAllowsEmptyBlockList(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(path, []byte(`
image:
  customInstallScripts:
pool:
  minIdle: 0
  maxInstances: 1
provider:
  type: wsl
  sourceImage: image.tar
runner:
  labels:
    - self-hosted
`), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(cfg.Image.CustomInstallScripts); got != 0 {
		t.Fatalf("custom install scripts = %d, want 0", got)
	}
	if err := Validate(cfg); err != nil {
		t.Fatal(err)
	}
}

func TestValidateRosettaTag(t *testing.T) {
	cfg := Default()
	cfg.Provider.RosettaTag = "rosetta"
	if err := Validate(cfg); err != nil {
		t.Fatalf("valid rosetta tag rejected: %v", err)
	}

	for _, tag := range []string{"bad tag", "bad/tag", "../rosetta", "-bad"} {
		cfg := Default()
		cfg.Provider.RosettaTag = tag
		if err := Validate(cfg); err == nil {
			t.Fatalf("provider.rosettaTag %q accepted", tag)
		}
	}

	cfg = Default()
	cfg.Provider.Type = "wsl"
	cfg.Provider.SourceImage = "image.tar"
	cfg.Provider.RosettaTag = "rosetta"
	if err := Validate(cfg); err == nil {
		t.Fatal("provider.rosettaTag accepted for WSL")
	}
}

func TestValidateDockerPlatform(t *testing.T) {
	cfg := Default()
	cfg.Provider.Type = "docker-dind"
	cfg.Provider.SourceImage = "runner-image"
	cfg.Provider.Platform = "linux/amd64"
	if err := Validate(cfg); err != nil {
		t.Fatalf("valid docker platform rejected: %v", err)
	}

	for _, platform := range []string{"bad platform", "-linux/amd64", "linux/$bad"} {
		cfg := Default()
		cfg.Provider.Type = "docker-dind"
		cfg.Provider.SourceImage = "runner-image"
		cfg.Provider.Platform = platform
		if err := Validate(cfg); err == nil {
			t.Fatalf("provider.platform %q accepted", platform)
		}
	}

	cfg = Default()
	cfg.Provider.Type = "tart"
	cfg.Provider.Platform = "linux/arm64"
	if err := Validate(cfg); err == nil {
		t.Fatal("provider.platform accepted for Tart")
	}
}

func TestValidateRejectsDockerSocketProvider(t *testing.T) {
	cfg := Default()
	cfg.Provider.Type = "docker-socket"
	cfg.Provider.SourceImage = "runner-image"
	err := Validate(cfg)
	if err == nil {
		t.Fatal("docker-socket provider accepted")
	}
	if got := err.Error(); got != "provider.type docker-socket is intentionally unsupported; use provider.type=docker-dind for a private Docker daemon" {
		t.Fatalf("error = %q", got)
	}
}

func TestValidateDoesNotRequireGitHubForImageCommands(t *testing.T) {
	cfg := Default()
	if err := Validate(cfg); err != nil {
		t.Fatal(err)
	}
	if err := ValidateGitHub(cfg); err == nil {
		t.Fatal("ValidateGitHub accepted empty GitHub settings")
	}
}

func TestValidatePrefix(t *testing.T) {
	for _, prefix := range []string{"epar-test", "a_1.test"} {
		if err := ValidatePrefix(prefix); err != nil {
			t.Fatalf("prefix %q rejected: %v", prefix, err)
		}
	}
	for _, prefix := range []string{"-bad", "bad*", "x"} {
		if err := ValidatePrefix(prefix); err == nil {
			t.Fatalf("prefix %q accepted", prefix)
		}
	}
}

func TestLoadRejectsImageProfile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(path, []byte(`
image:
  profile: web-e2e
`), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("Load accepted image.profile")
	}
}
