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
	if got, want := len(cfg.Image.CustomInstallScripts), 2; got != want {
		t.Fatalf("custom install scripts = %d, want %d", got, want)
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
