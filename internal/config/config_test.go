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
docker:
  registryMirrors:
    - http://host.docker.internal:5000
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
	if got, want := cfg.Docker.RegistryMirrors[0], "http://host.docker.internal:5000"; got != want {
		t.Fatalf("docker.registryMirrors[0] = %q, want %q", got, want)
	}
	if err := Validate(cfg); err != nil {
		t.Fatal(err)
	}
	if !DockerRegistryMirrorsNeedHostGateway(cfg.Docker.RegistryMirrors) {
		t.Fatal("host.docker.internal mirror should request docker-dind host gateway")
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

func TestProviderDefaultsForMinimalWSLConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(path, []byte(`
provider:
  type: wsl
`), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := cfg.Image.SourceType, ImageSourceDockerImage; got != want {
		t.Fatalf("image.sourceType = %q, want %q", got, want)
	}
	if got, want := cfg.Image.SourceImage, "gitea/runner-images:ubuntu-latest-full"; got != want {
		t.Fatalf("image.sourceImage = %q, want %q", got, want)
	}
	if got, want := cfg.Image.SourcePlatform, "linux/amd64"; got != want {
		t.Fatalf("image.sourcePlatform = %q, want %q", got, want)
	}
	if got, want := cfg.Image.OutputImage, "work/images/epar-wsl-gitea-ubuntu.tar"; got != want {
		t.Fatalf("image.outputImage = %q, want %q", got, want)
	}
	if got, want := cfg.Provider.SourceImage, cfg.Image.OutputImage; got != want {
		t.Fatalf("provider.sourceImage = %q, want %q", got, want)
	}
	if got, want := cfg.Pool.NamePrefix, "epar-wsl"; got != want {
		t.Fatalf("pool.namePrefix = %q, want %q", got, want)
	}
	if got, want := cfg.Runner.Labels[2], "X64"; got != want {
		t.Fatalf("runner.labels[2] = %q, want %q", got, want)
	}
	if err := Validate(cfg); err != nil {
		t.Fatal(err)
	}
}

func TestProviderDefaultsInferExistingWSLRootFSTar(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(path, []byte(`
image:
  sourceImage: work/images/ubuntu-24.04-clean.rootfs.tar
provider:
  type: wsl
`), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := cfg.Image.SourceType, ImageSourceRootFSTar; got != want {
		t.Fatalf("image.sourceType = %q, want %q", got, want)
	}
	if got, want := cfg.Image.OutputImage, "work/images/epar-ubuntu-24-wsl.tar"; got != want {
		t.Fatalf("image.outputImage = %q, want %q", got, want)
	}
	if got := cfg.Image.SourcePlatform; got != "" {
		t.Fatalf("image.sourcePlatform = %q, want empty", got)
	}
}

func TestProviderDefaultsRespectExplicitWSLOverrides(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(path, []byte(`
image:
  sourceImage: example/rootfs.tar
  sourceType: rootfs-tar
  outputImage: work/images/custom-wsl.tar
pool:
  namePrefix: custom-wsl
runner:
  labels: [self-hosted, linux, custom]
provider:
  type: wsl
  sourceImage: work/images/custom-provider.tar
`), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := cfg.Image.SourceImage, "example/rootfs.tar"; got != want {
		t.Fatalf("image.sourceImage = %q, want %q", got, want)
	}
	if got, want := cfg.Image.OutputImage, "work/images/custom-wsl.tar"; got != want {
		t.Fatalf("image.outputImage = %q, want %q", got, want)
	}
	if got, want := cfg.Provider.SourceImage, "work/images/custom-provider.tar"; got != want {
		t.Fatalf("provider.sourceImage = %q, want %q", got, want)
	}
	if got, want := cfg.Pool.NamePrefix, "custom-wsl"; got != want {
		t.Fatalf("pool.namePrefix = %q, want %q", got, want)
	}
	if got, want := cfg.Runner.Labels[2], "custom"; got != want {
		t.Fatalf("runner label = %q, want %q", got, want)
	}
}

func TestProviderDefaultsForMinimalDockerDindConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(path, []byte(`
provider:
  type: docker-dind
`), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := cfg.Image.SourceType, ImageSourceDockerImage; got != want {
		t.Fatalf("image.sourceType = %q, want %q", got, want)
	}
	if got, want := cfg.Image.SourceImage, "gitea/runner-images:ubuntu-latest-full"; got != want {
		t.Fatalf("image.sourceImage = %q, want %q", got, want)
	}
	if got, want := cfg.Image.OutputImage, "epar-docker-dind-gitea-ubuntu"; got != want {
		t.Fatalf("image.outputImage = %q, want %q", got, want)
	}
	if got, want := cfg.Provider.SourceImage, cfg.Image.OutputImage; got != want {
		t.Fatalf("provider.sourceImage = %q, want %q", got, want)
	}
	if got, want := cfg.Pool.NamePrefix, "epar-dind"; got != want {
		t.Fatalf("pool.namePrefix = %q, want %q", got, want)
	}
	if got, want := cfg.Runner.Labels[2], "epar-docker-dind-gitea-ubuntu"; got != want {
		t.Fatalf("runner label = %q, want %q", got, want)
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

func TestValidateDockerRegistryMirror(t *testing.T) {
	cfg := Default()
	cfg.Docker.RegistryMirrors = []string{"https://mirror.example.test", "http://host.docker.internal:5000/"}
	if err := Validate(cfg); err != nil {
		t.Fatalf("valid registry mirrors rejected: %v", err)
	}

	for _, mirror := range []string{
		"mirror.example.test",
		"ftp://mirror.example.test",
		"https://user:pass@mirror.example.test",
		"https://mirror.example.test/path",
		"https://mirror.example.test?x=1",
		"https://mirror example.test",
		" https://mirror.example.test",
	} {
		cfg := Default()
		cfg.Docker.RegistryMirrors = []string{mirror}
		if err := Validate(cfg); err == nil {
			t.Fatalf("docker.registryMirrors %q accepted", mirror)
		}
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
