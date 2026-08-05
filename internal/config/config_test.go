package config

import (
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestLoadYAMLSubset(t *testing.T) {
	oldHostname := osHostname
	osHostname = func() (string, error) { return "CI Box 01", nil }
	t.Cleanup(func() { osHostname = oldHostname })
	t.Setenv(HostNameEnv, "")

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(path, []byte(`
github:
  appId: 123
  organization: example
  privateKeyPath: /tmp/key.pem
pool:
  instances: 3
  namePrefix: epar-test
runner:
  labels:
    - self-hosted
    - linux
    - ARM64
    - custom
  includeHostLabel: true
  ephemeral: true
  group: epar-ci-canary
  noDefaultLabels: true
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
  trustedCaCertificatePaths:
    - .local/corporate-root.pem
    - /opt/epar/enterprise-root.crt
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
	if got, want := cfg.Runner.Labels[4], "epar-host-ci-box-01"; got != want {
		t.Fatalf("host label = %q, want %q", got, want)
	}
	if got, want := cfg.Runner.Group, "epar-ci-canary"; got != want {
		t.Fatalf("runner.group = %q, want %q", got, want)
	}
	if !cfg.Runner.NoDefaultLabels {
		t.Fatal("runner.noDefaultLabels = false, want true")
	}
	if got, want := cfg.Provider.InstallRoot, "work/custom-wsl"; got != want {
		t.Fatalf("provider.installRoot = %q, want %q", got, want)
	}
	if got, want := cfg.Provider.RosettaTag, "rosetta"; got != want {
		t.Fatalf("provider.rosettaTag = %q, want %q", got, want)
	}
	if got, want := cfg.Pool.Instances, 3; got != want {
		t.Fatalf("pool.instances = %d, want %d", got, want)
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
	if got, want := len(cfg.Image.CustomInstallScripts), 2; got != want {
		t.Fatalf("custom install scripts = %d, want %d", got, want)
	}
	if got, want := len(cfg.Image.TrustedCACertificatePaths), 2; got != want {
		t.Fatalf("trusted CA certificate paths = %d, want %d", got, want)
	}
	if err := Validate(cfg); err != nil {
		t.Fatal(err)
	}
}

func TestImageUpdatePolicyDefaults(t *testing.T) {
	cfg := Default()
	if got, want := cfg.Image.UpdateFrequency, ImageUpdateFrequencyWeekly; got != want {
		t.Fatalf("Image.UpdateFrequency = %q, want %q", got, want)
	}
	if got, want := cfg.Image.UpdateTime, DefaultImageUpdateTime; got != want {
		t.Fatalf("Image.UpdateTime = %q, want %q", got, want)
	}
	if err := ValidateImageUpdatePolicy(cfg.Image); err != nil {
		t.Fatal(err)
	}
}

func TestLoadImageUpdatePolicy(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	content := "image:\n  updateFrequency: biweekly\n  updateTime: \"06:30\"\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := cfg.Image.UpdateFrequency, ImageUpdateFrequencyBiweekly; got != want {
		t.Fatalf("Image.UpdateFrequency = %q, want %q", got, want)
	}
	if got, want := cfg.Image.UpdateTime, "06:30"; got != want {
		t.Fatalf("Image.UpdateTime = %q, want %q", got, want)
	}
	if slices.ContainsFunc(cfg.Warnings(), func(warning string) bool {
		return strings.Contains(warning, "image update policy is not configured")
	}) {
		t.Fatalf("Warnings() = %#v, want no image-policy default notice", cfg.Warnings())
	}
}

func TestLoadOmittedImageUpdatePolicyAddsNotice(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(path, []byte("image:\n  runnerVersion: latest\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(cfg.Warnings(), func(warning string) bool {
		return strings.Contains(warning, "using weekly checks at 07:00 local time")
	}) {
		t.Fatalf("Warnings() = %#v, want image-policy default notice", cfg.Warnings())
	}
}

func TestValidateImageUpdatePolicy(t *testing.T) {
	for _, frequency := range []string{
		ImageUpdateFrequencyDaily,
		ImageUpdateFrequencyWeekly,
		ImageUpdateFrequencyBiweekly,
		ImageUpdateFrequencyMonthly,
		ImageUpdateFrequencyManual,
	} {
		image := Default().Image
		image.UpdateFrequency = frequency
		if err := ValidateImageUpdatePolicy(image); err != nil {
			t.Fatalf("ValidateImageUpdatePolicy(%q): %v", frequency, err)
		}
	}
	image := Default().Image
	image.UpdateFrequency = "hourly"
	if err := ValidateImageUpdatePolicy(image); err == nil || !strings.Contains(err.Error(), "unsupported image.updateFrequency") {
		t.Fatalf("invalid frequency error = %v", err)
	}
	image = Default().Image
	image.UpdateTime = "7am"
	if err := ValidateImageUpdatePolicy(image); err == nil || !strings.Contains(err.Error(), "24-hour HH:MM") {
		t.Fatalf("invalid time error = %v", err)
	}
	image.UpdateFrequency = ImageUpdateFrequencyManual
	if err := ValidateImageUpdatePolicy(image); err != nil {
		t.Fatalf("manual mode should ignore image.updateTime: %v", err)
	}
}

func TestRunnerRegistrationControlsDefaultToDisabled(t *testing.T) {
	cfg := Default()
	if cfg.Runner.Group != "" {
		t.Fatalf("runner.group = %q, want empty", cfg.Runner.Group)
	}
	if cfg.Runner.NoDefaultLabels {
		t.Fatal("runner.noDefaultLabels = true, want false")
	}
}

func TestStorageDefaultsAreBoundedAndConservative(t *testing.T) {
	storage := Default().Storage
	if storage.MinimumFree != "1GiB" ||
		storage.GracePeriod != "168h" ||
		storage.KeepPrevious != 0 ||
		storage.AutomaticHousekeeping != StorageHousekeepingConservative ||
		storage.BuildCacheLimit != "20GiB" ||
		storage.GoCacheLimit != "10GiB" {
		t.Fatalf("unexpected storage defaults: %+v", storage)
	}
	if err := ValidateStorage(storage); err != nil {
		t.Fatal(err)
	}
}

func TestLoadStoragePolicy(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	content := "storage:\n  minimumFree: 30GiB\n  gracePeriod: 72h\n  keepPrevious: 1\n  automaticHousekeeping: disabled\n  buildCacheLimit: 80GiB\n  goCacheLimit: 12GiB\n"
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := cfg.Storage.MinimumFree, "30GiB"; got != want {
		t.Fatalf("storage.minimumFree = %q, want %q", got, want)
	}
	if got, want := cfg.Storage.GracePeriod, "72h"; got != want {
		t.Fatalf("storage.gracePeriod = %q, want %q", got, want)
	}
	if cfg.Storage.KeepPrevious != 1 || cfg.Storage.AutomaticHousekeeping != StorageHousekeepingDisabled {
		t.Fatalf("unexpected loaded storage policy: %+v", cfg.Storage)
	}
	if err := ValidateStorage(cfg.Storage); err != nil {
		t.Fatal(err)
	}
}

func TestValidateStorageRejectsUnboundedOrUnknownPolicy(t *testing.T) {
	tests := []func(*StorageConfig){
		func(storage *StorageConfig) { storage.MinimumFree = "0GiB" },
		func(storage *StorageConfig) { storage.GracePeriod = "0h" },
		func(storage *StorageConfig) { storage.KeepPrevious = -1 },
		func(storage *StorageConfig) { storage.AutomaticHousekeeping = "aggressive" },
		func(storage *StorageConfig) { storage.BuildCacheLimit = "64GB" },
		func(storage *StorageConfig) { storage.GoCacheLimit = "" },
	}
	for _, mutate := range tests {
		storage := Default().Storage
		mutate(&storage)
		if err := ValidateStorage(storage); err == nil {
			t.Fatalf("ValidateStorage accepted invalid policy: %+v", storage)
		}
	}
}

func TestRunnerGroupSecurityDefaultsToStrictWarnings(t *testing.T) {
	policy := Default().Security.RunnerGroup
	if policy.Enforcement != RunnerGroupEnforcementWarn ||
		!policy.RequireExplicitGroup ||
		!policy.RequireNonDefaultGroup ||
		policy.RequiredRepositoryAccess != RunnerGroupRepositoryAccessSelected ||
		!policy.RequirePublicRepositoriesDisabled {
		t.Fatalf("unexpected runner-group security defaults: %+v", policy)
	}
}

func TestLoadNestedRunnerGroupSecurity(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	content := `security:
  runnerGroup:
    enforcement: enforce
    requireExplicitGroup: false
    requireNonDefaultGroup: false
    requiredRepositoryAccess: all
    requirePublicRepositoriesDisabled: false
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	policy := cfg.Security.RunnerGroup
	if policy.Enforcement != RunnerGroupEnforcementEnforce || policy.RequireExplicitGroup || policy.RequireNonDefaultGroup || policy.RequiredRepositoryAccess != RunnerGroupRepositoryAccessAll || policy.RequirePublicRepositoriesDisabled {
		t.Fatalf("unexpected parsed runner-group policy: %+v", policy)
	}
	for _, warning := range cfg.Warnings() {
		if strings.Contains(warning, "runner-group security policy is not configured") {
			t.Fatalf("unexpected migration warning for configured policy: %q", warning)
		}
	}
}

func TestLoadLegacyConfigWarnsAboutRunnerGroupSecurity(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(path, []byte("runner:\n  group: existing-group\n"), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(cfg.Warnings(), func(warning string) bool {
		return strings.Contains(warning, "runner-group security policy is not configured") && strings.Contains(warning, "warn mode")
	}) {
		t.Fatalf("Warnings() = %#v, want runner-group migration warning", cfg.Warnings())
	}
}

func TestLoadRejectsInvalidRunnerGroupSecurityNesting(t *testing.T) {
	for _, content := range []string{
		"security:\n  unknown:\n    enforcement: enforce\n",
		"security:\n  enforcement: enforce\n",
		"security:\n  runnerGroup:\n  enforcement: enforce\n",
		"security:\n  runnerGroup:\n    unknown: true\n",
	} {
		dir := t.TempDir()
		path := filepath.Join(dir, "config.yml")
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Fatalf("Load(%q) succeeded, want nesting/key error", content)
		}
	}
}

func TestValidateRunnerGroupSecurityRejectsInvalidValues(t *testing.T) {
	for _, mutate := range []func(*RunnerGroupSecurityConfig){
		func(policy *RunnerGroupSecurityConfig) { policy.Enforcement = "off" },
		func(policy *RunnerGroupSecurityConfig) { policy.RequiredRepositoryAccess = "repositories" },
	} {
		cfg := Default()
		mutate(&cfg.Security.RunnerGroup)
		if err := Validate(cfg); err == nil || !strings.Contains(err.Error(), "security.runnerGroup") {
			t.Fatalf("Validate() error = %v, want runner-group policy error", err)
		}
	}
}

func TestLoggingDefaults(t *testing.T) {
	got := Default().Logging
	if got.Directory != "work/logs" || !slices.Equal(got.ManagerSinks, []string{"console"}) || got.ManagerConsoleFormat != "text" || got.ManagerFileFormat != "json" || !slices.Equal(got.TranscriptSinks, []string{"file"}) || got.TranscriptConsoleFormat != "text" {
		t.Fatalf("unexpected logging destination defaults: %+v", got)
	}
	if got.MaxFileSizeMiB != 100 || got.MaxBackups != 3 || !got.CompressBackups || !got.RetentionEnabled || got.RetentionMaxTotalMiB != 1024 {
		t.Fatalf("unexpected logging retention defaults: %+v", got)
	}
	if got.ManagerMaxAgeDays != 14 || got.InstanceMaxAgeDays != 14 || got.BuildMaxAgeDays != 14 || got.ErrorMaxAgeDays != 30 || got.BenchmarkMaxAgeDays != 90 || got.RetentionIntervalMinutes != 60 {
		t.Fatalf("unexpected logging age defaults: %+v", got)
	}
}

func TestCheckedInExampleConfigurationsValidate(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("..", "..", "configs", "*.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatal("no configuration examples found")
	}
	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			cfg, err := Load(path)
			if err != nil {
				t.Fatalf("Load(%s): %v", path, err)
			}
			if err := Validate(cfg); err != nil {
				t.Fatalf("Validate(%s): %v", path, err)
			}
		})
	}
}

func TestObservabilityExamplesRequireAnExplicitProviderType(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("..", "..", "examples", "observability", "*.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatal("no observability examples found")
	}
	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			cfg, err := Load(path)
			if err != nil {
				t.Fatalf("Load(%s): %v", path, err)
			}
			if err := Validate(cfg); err == nil || err.Error() != "provider.type is required" {
				t.Fatalf("Validate(%s) error = %v, want provider.type is required", path, err)
			}
		})
	}
}

func TestLoadLoggingConfiguration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(path, []byte(`
logging:
  directory: custom/logs
  managerSinks:
    - console
    - file
  managerConsoleFormat: json
  managerFileFormat: text
  transcriptSinks: [file, console]
  transcriptConsoleFormat: json
  maxFileSizeMiB: 64
  maxBackups: 5
  compressBackups: false
  retentionEnabled: false
  retentionMaxTotalMiB: 2048
  managerMaxAgeDays: 7
  instanceMaxAgeDays: 8
  buildMaxAgeDays: 9
  errorMaxAgeDays: 10
  benchmarkMaxAgeDays: 11
  retentionIntervalMinutes: 12
provider:
  type: tart
`), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.Logging
	if got.Directory != "custom/logs" || !slices.Equal(got.ManagerSinks, []string{"console", "file"}) || !slices.Equal(got.TranscriptSinks, []string{"file", "console"}) || got.ManagerConsoleFormat != "json" || got.ManagerFileFormat != "text" || got.TranscriptConsoleFormat != "json" || got.MaxFileSizeMiB != 64 || got.MaxBackups != 5 || got.CompressBackups || got.RetentionEnabled || got.RetentionMaxTotalMiB != 2048 || got.ManagerMaxAgeDays != 7 || got.InstanceMaxAgeDays != 8 || got.BuildMaxAgeDays != 9 || got.ErrorMaxAgeDays != 10 || got.BenchmarkMaxAgeDays != 11 || got.RetentionIntervalMinutes != 12 {
		t.Fatalf("unexpected logging config: %+v", got)
	}
	if err := Validate(cfg); err != nil {
		t.Fatal(err)
	}
}

func TestLoadCustomConsoleTextFormats(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	content := "logging:\n  managerConsoleFormat: text\n  managerConsoleTextFormat: '[{level}] {message}{attributes}'\n  transcriptConsoleFormat: text\n  transcriptConsoleTextFormat: '{stream} {instance}: {message}'\nprovider:\n  type: tart\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := cfg.Logging.ManagerConsoleTextFormat, "[{level}] {message}{attributes}"; got != want {
		t.Fatalf("managerConsoleTextFormat = %q, want %q", got, want)
	}
	if got, want := cfg.Logging.TranscriptConsoleTextFormat, "{stream} {instance}: {message}"; got != want {
		t.Fatalf("transcriptConsoleTextFormat = %q, want %q", got, want)
	}
	if err := Validate(cfg); err != nil {
		t.Fatal(err)
	}
}

func TestLoadMigratesPoolLogDirInMemoryWithWarning(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(path, []byte("pool:\n  logDir: custom/logs\nprovider:\n  type: tart\n"), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := cfg.Logging.Directory, "custom/logs"; got != want {
		t.Fatalf("logging.directory = %q, want legacy value %q", got, want)
	}
	warnings := cfg.Warnings()
	if !slices.ContainsFunc(warnings, func(warning string) bool {
		return strings.Contains(warning, "pool.logDir is deprecated") && strings.Contains(warning, "logging.directory")
	}) {
		t.Fatalf("Warnings() = %#v, want migration warning", warnings)
	}
	if err := Validate(cfg); err != nil {
		t.Fatal(err)
	}
}

func TestLoadRejectsAmbiguousLegacyAndNewLogDirectories(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	content := "logging:\n  directory: new/logs\npool:\n  logDir: old/logs\n"
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "pool.logDir cannot be used with logging.directory") {
		t.Fatalf("Load() error = %v, want ambiguous-directory error", err)
	}
}

func TestLoadRejectsUnknownSectionsAndKeys(t *testing.T) {
	for _, text := range []string{
		"unknown:\n  value: true\n",
		"github:\n  unknown: value\n",
		"image:\n  unknown: value\n",
		"pool:\n  unknown: value\n",
		"storage:\n  unknown: value\n",
		"logging:\n  unknown: value\n",
		"runner:\n  unknown: value\n",
		"security:\n  runnerGroup:\n    unknown: value\n",
		"provider:\n  unknown: value\n",
		"docker:\n  unknown: value\n",
		"timeouts:\n  unknown: 1\n",
	} {
		dir := t.TempDir()
		path := filepath.Join(dir, "config.yml")
		if err := os.WriteFile(path, []byte(text), 0644); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "unknown") {
			t.Fatalf("Load(%q) error = %v, want unknown-key error", text, err)
		}
	}
}

func TestValidateLoggingRejectsInvalidValues(t *testing.T) {
	for _, mutate := range []func(*LoggingConfig){
		func(logging *LoggingConfig) { logging.Directory = " " },
		func(logging *LoggingConfig) { logging.ManagerSinks = nil },
		func(logging *LoggingConfig) { logging.TranscriptSinks = []string{"syslog"} },
		func(logging *LoggingConfig) { logging.ManagerSinks = []string{"Console"} },
		func(logging *LoggingConfig) { logging.ManagerFileFormat = "yaml" },
		func(logging *LoggingConfig) { logging.ManagerConsoleFormat = "JSON" },
		func(logging *LoggingConfig) {
			logging.ManagerConsoleFormat = "json"
			logging.ManagerConsoleTextFormat = "{level} {message}"
		},
		func(logging *LoggingConfig) { logging.ManagerConsoleTextFormat = "{unknown} {message}" },
		func(logging *LoggingConfig) { logging.ManagerConsoleTextFormat = "{level}" },
		func(logging *LoggingConfig) { logging.TranscriptConsoleTextFormat = "{level} {message}" },
		func(logging *LoggingConfig) { logging.MaxFileSizeMiB = 0 },
		func(logging *LoggingConfig) { logging.MaxBackups = -1 },
		func(logging *LoggingConfig) { logging.MaxBackups = 0 },
		func(logging *LoggingConfig) { logging.RetentionMaxTotalMiB = 0 },
		func(logging *LoggingConfig) { logging.ErrorMaxAgeDays = 0 },
		func(logging *LoggingConfig) { logging.RetentionIntervalMinutes = 0 },
	} {
		cfg := Default()
		mutate(&cfg.Logging)
		if err := ValidateLogging(cfg.Logging); err == nil {
			t.Fatal("ValidateLogging accepted invalid config")
		}
	}
}

func TestTrustedCACertificatePathsDefaultToEmpty(t *testing.T) {
	cfg := Default()
	if len(cfg.Image.TrustedCACertificatePaths) != 0 {
		t.Fatalf("image.trustedCaCertificatePaths = %#v, want empty", cfg.Image.TrustedCACertificatePaths)
	}
}

func TestValidateRejectsEmptyTrustedCACertificatePath(t *testing.T) {
	cfg := defaultTartConfig()
	cfg.Image.TrustedCACertificatePaths = []string{" "}
	if err := Validate(cfg); err == nil || !strings.Contains(err.Error(), "image.trustedCaCertificatePaths") {
		t.Fatalf("Validate() error = %v, want trusted CA path error", err)
	}
}

func TestHostTrustDefaultsToDisabledWithSystemScope(t *testing.T) {
	cfg := Default()
	if cfg.Image.HostTrustMode != HostTrustModeDisabled {
		t.Fatalf("image.hostTrustMode = %q, want %q", cfg.Image.HostTrustMode, HostTrustModeDisabled)
	}
	if got, want := cfg.Image.HostTrustScopes, []string{HostTrustScopeSystem}; !slices.Equal(got, want) {
		t.Fatalf("image.hostTrustScopes = %#v, want %#v", got, want)
	}
}

func TestLoadAndValidateHostTrustOverlay(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(path, []byte(`
image:
  hostTrustMode: overlay
  hostTrustScopes: [system, user]
provider:
  type: docker-container
`), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := cfg.Image.HostTrustMode, HostTrustModeOverlay; got != want {
		t.Fatalf("image.hostTrustMode = %q, want %q", got, want)
	}
	if got, want := cfg.Image.HostTrustScopes, []string{HostTrustScopeSystem, HostTrustScopeUser}; !slices.Equal(got, want) {
		t.Fatalf("image.hostTrustScopes = %#v, want %#v", got, want)
	}
	if err := Validate(cfg); err != nil {
		t.Fatal(err)
	}
}

func TestValidateRejectsInvalidHostTrustConfigurations(t *testing.T) {
	for _, test := range []struct {
		name      string
		mode      string
		scopes    []string
		provider  string
		ephemeral bool
	}{
		{name: "unknown mode", mode: "mirror", scopes: []string{HostTrustScopeSystem}, provider: "docker-container", ephemeral: true},
		{name: "non-ephemeral", mode: HostTrustModeOverlay, scopes: []string{HostTrustScopeSystem}, provider: "docker-container"},
		{name: "empty scopes", mode: HostTrustModeOverlay, provider: "docker-container", ephemeral: true},
		{name: "unknown scope", mode: HostTrustModeOverlay, scopes: []string{"global"}, provider: "docker-container", ephemeral: true},
		{name: "duplicate scope", mode: HostTrustModeOverlay, scopes: []string{HostTrustScopeSystem, HostTrustScopeSystem}, provider: "docker-container", ephemeral: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := Default()
			cfg.Provider.Type = test.provider
			cfg.Provider.SourceImage = "runner-image"
			cfg.Runner.Ephemeral = test.ephemeral
			cfg.Image.HostTrustMode = test.mode
			cfg.Image.HostTrustScopes = test.scopes
			if err := Validate(cfg); err == nil {
				t.Fatal("Validate accepted invalid host trust configuration")
			}
		})
	}
}

func TestValidateHostTrustAllowsDockerSandboxesOverlay(t *testing.T) {
	cfg := validDockerSandboxesConfig()
	cfg.Image.HostTrustMode = HostTrustModeOverlay
	cfg.Image.HostTrustScopes = []string{HostTrustScopeSystem}
	if err := Validate(cfg); err != nil {
		t.Fatalf("Docker Sandboxes host trust overlay rejected: %v", err)
	}
}

func TestValidateHostTrustAllowsWSLOverlay(t *testing.T) {
	cfg := defaultWSLConfig()
	cfg.Provider.SourceImage = "runner-image.tar"
	cfg.Runner.Ephemeral = true
	cfg.Image.HostTrustMode = HostTrustModeOverlay
	cfg.Image.HostTrustScopes = []string{HostTrustScopeSystem}
	if err := Validate(cfg); err != nil {
		t.Fatalf("WSL host trust overlay rejected: %v", err)
	}
}

func TestRunnerHostLabelDefaultsToEnabled(t *testing.T) {
	oldHostname := osHostname
	osHostname = func() (string, error) { return "Build Box_01.example", nil }
	t.Cleanup(func() { osHostname = oldHostname })
	t.Setenv(HostNameEnv, "")

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(path, []byte(`
provider:
  type: docker-container
`), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := cfg.Runner.Labels[len(cfg.Runner.Labels)-1], "epar-host-build-box_01.example"; got != want {
		t.Fatalf("host label = %q, want %q", got, want)
	}
}

func TestRunnerHostLabelPrefersHostNameEnv(t *testing.T) {
	oldHostname := osHostname
	osHostname = func() (string, error) { return "container-id", nil }
	t.Cleanup(func() { osHostname = oldHostname })
	t.Setenv(HostNameEnv, "Real Windows Host")

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(path, []byte(`
provider:
  type: docker-container
`), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := cfg.Runner.Labels[len(cfg.Runner.Labels)-1], "epar-host-real-windows-host"; got != want {
		t.Fatalf("host label = %q, want %q", got, want)
	}
}

func TestRunnerHostLabelCanBeDisabled(t *testing.T) {
	oldHostname := osHostname
	osHostname = func() (string, error) { return "build-box", nil }
	t.Cleanup(func() { osHostname = oldHostname })
	t.Setenv(HostNameEnv, "")

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(path, []byte(`
runner:
  includeHostLabel: false
provider:
  type: docker-container
`), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, label := range cfg.Runner.Labels {
		if strings.HasPrefix(label, "epar-host-") {
			t.Fatalf("host label should be disabled, got labels %v", cfg.Runner.Labels)
		}
	}
}

func TestRunnerHostLabelDoesNotDuplicateExistingLabel(t *testing.T) {
	oldHostname := osHostname
	osHostname = func() (string, error) { return "Build Box", nil }
	t.Cleanup(func() { osHostname = oldHostname })
	t.Setenv(HostNameEnv, "")

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(path, []byte(`
runner:
  labels: [self-hosted, linux, epar-host-build-box]
provider:
  type: docker-container
`), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, label := range cfg.Runner.Labels {
		if label == "epar-host-build-box" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("host label count = %d, want 1 in labels %v", count, cfg.Runner.Labels)
	}
}

func TestHostLabelSanitizesMachineName(t *testing.T) {
	if got, want := HostLabel("JJ ORION/Dev@Box"), "epar-host-jj-orion-dev-box"; got != want {
		t.Fatalf("HostLabel = %q, want %q", got, want)
	}
}

func TestHostLabelDoesNotExceedGitHubLimit(t *testing.T) {
	got := HostLabel(strings.Repeat("a", MaxRunnerLabelLength+100))
	if len(got) != MaxRunnerLabelLength {
		t.Fatalf("host label length = %d, want %d", len(got), MaxRunnerLabelLength)
	}
	if !strings.HasPrefix(got, "epar-host-") {
		t.Fatalf("host label = %q, want epar-host prefix", got)
	}
}

func TestSanitizeNamePart(t *testing.T) {
	tests := map[string]string{
		"JJ ORION/Dev@Box":      "jj-orion-dev-box",
		"...Build_Box--":        "build_box",
		"---name---":            "name",
		"!!!":                   "",
		strings.Repeat("A", 60): strings.Repeat("a", 60),
	}
	for input, want := range tests {
		if got := SanitizeNamePart(input); got != want {
			t.Fatalf("SanitizeNamePart(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestLoadDockerContainerPlatform(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(path, []byte(`
pool:
  instances: 1
  namePrefix: epar-docker-container
runner:
  labels: [self-hosted, linux, ARM64, epar-docker-container]
provider:
  type: docker-container
  sourceImage: epar-docker-container-ubuntu-24
  platform: linux/arm64
docker:
  registryMirrors:
    - http://host.docker.internal:5000
  httpProxy: http://host.docker.internal:3128
  httpsProxy: http://host.docker.internal:3128
  noProxy: localhost,127.0.0.1,.example.test
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
	if got, want := cfg.Docker.HTTPProxy, "http://host.docker.internal:3128"; got != want {
		t.Fatalf("docker.httpProxy = %q, want %q", got, want)
	}
	if got, want := cfg.Docker.HTTPSProxy, "http://host.docker.internal:3128"; got != want {
		t.Fatalf("docker.httpsProxy = %q, want %q", got, want)
	}
	if got, want := cfg.Docker.NoProxy, "localhost,127.0.0.1,.example.test"; got != want {
		t.Fatalf("docker.noProxy = %q, want %q", got, want)
	}
	if err := Validate(cfg); err != nil {
		t.Fatal(err)
	}
	if !DockerRegistryMirrorsNeedHostGateway(cfg.Docker.RegistryMirrors) {
		t.Fatal("host.docker.internal mirror should request docker-container host gateway")
	}
	if !DockerConfigNeedsHostGateway(cfg.Docker) {
		t.Fatal("host.docker.internal Docker config should request docker-container host gateway")
	}
}

func TestLoadAllowsEmptyBlockList(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(path, []byte(`
image:
  customInstallScripts:
pool:
  instances: 1
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

func TestProviderTypeIsRequiredAndDoesNotApplyProviderDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(path, []byte("runner:\n  includeHostLabel: false\npool:\n  instances: 1\n"), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := cfg.Provider, (ProviderConfig{}); got != want {
		t.Fatalf("provider defaults = %+v, want %+v", got, want)
	}
	if got, want := cfg.Image.SourceImage, ""; got != want {
		t.Fatalf("image.sourceImage = %q, want empty without provider.type", got)
	}
	if got, want := cfg.Image.OutputImage, ""; got != want {
		t.Fatalf("image.outputImage = %q, want empty without provider.type", got)
	}
	if got := cfg.Runner.Labels; len(got) != 0 {
		t.Fatalf("runner.labels = %#v, want no provider-specific defaults", got)
	}
	if err := Validate(cfg); err == nil || err.Error() != "provider.type is required" {
		t.Fatalf("Validate() error = %v, want provider.type is required", err)
	}
}

func TestProviderDefaultsForMinimalTartConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(path, []byte("runner:\n  includeHostLabel: false\nprovider:\n  type: tart\n"), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := cfg.Image.SourceImage, "ghcr.io/cirruslabs/ubuntu:latest"; got != want {
		t.Fatalf("image.sourceImage = %q, want %q", got, want)
	}
	if got, want := cfg.Image.OutputImage, "epar-ubuntu-24-arm64"; got != want {
		t.Fatalf("image.outputImage = %q, want %q", got, want)
	}
	if got, want := cfg.Provider.SourceImage, cfg.Image.OutputImage; got != want {
		t.Fatalf("provider.sourceImage = %q, want %q", got, want)
	}
	if got, want := cfg.Provider.Network, "default"; got != want {
		t.Fatalf("provider.network = %q, want %q", got, want)
	}
	if got, want := cfg.Runner.Labels, []string{"self-hosted", "linux", "ARM64", "epar-tart-ubuntu-24.04-base"}; !slices.Equal(got, want) {
		t.Fatalf("runner.labels = %#v, want %#v", got, want)
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
	if got, want := cfg.Image.SourceImage, "ghcr.io/catthehacker/ubuntu:full-latest"; got != want {
		t.Fatalf("image.sourceImage = %q, want %q", got, want)
	}
	if got, want := cfg.Image.SourcePlatform, "linux/amd64"; got != want {
		t.Fatalf("image.sourcePlatform = %q, want %q", got, want)
	}
	if got, want := cfg.Image.OutputImage, "work/images/epar-wsl-catthehacker-ubuntu.tar"; got != want {
		t.Fatalf("image.outputImage = %q, want %q", got, want)
	}
	if got, want := cfg.Provider.SourceImage, cfg.Image.OutputImage; got != want {
		t.Fatalf("provider.sourceImage = %q, want %q", got, want)
	}
	if got, want := cfg.Provider.InstallRoot, "work/wsl"; got != want {
		t.Fatalf("provider.installRoot = %q, want %q", got, want)
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

func TestProviderDefaultsForMinimalDockerContainerConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(path, []byte(`
provider:
  type: docker-container
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
	if got, want := cfg.Image.SourceImage, "ghcr.io/catthehacker/ubuntu:full-latest"; got != want {
		t.Fatalf("image.sourceImage = %q, want %q", got, want)
	}
	if got, want := cfg.Image.OutputImage, "epar-docker-container-catthehacker-ubuntu"; got != want {
		t.Fatalf("image.outputImage = %q, want %q", got, want)
	}
	if got, want := cfg.Provider.SourceImage, cfg.Image.OutputImage; got != want {
		t.Fatalf("provider.sourceImage = %q, want %q", got, want)
	}
	if got, want := cfg.Pool.NamePrefix, "epar-docker-container"; got != want {
		t.Fatalf("pool.namePrefix = %q, want %q", got, want)
	}
	if got, want := cfg.Runner.Labels[2], "epar-docker-container-catthehacker-ubuntu"; got != want {
		t.Fatalf("runner label = %q, want %q", got, want)
	}
	if err := Validate(cfg); err != nil {
		t.Fatal(err)
	}
}

func TestExampleConfigsLoadAndValidate(t *testing.T) {
	oldHostname := osHostname
	osHostname = func() (string, error) { return "Example Host", nil }
	t.Cleanup(func() { osHostname = oldHostname })
	t.Setenv(HostNameEnv, "")

	entries, err := os.ReadDir(filepath.Join("..", "..", "configs"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".yml" {
			continue
		}
		t.Run(entry.Name(), func(t *testing.T) {
			cfg, err := Load(filepath.Join("..", "..", "configs", entry.Name()))
			if err != nil {
				t.Fatal(err)
			}
			if err := Validate(cfg); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestValidateRosettaTag(t *testing.T) {
	cfg := defaultTartConfig()
	cfg.Provider.RosettaTag = "rosetta"
	if err := Validate(cfg); err != nil {
		t.Fatalf("valid rosetta tag rejected: %v", err)
	}

	for _, tag := range []string{"bad tag", "bad/tag", "../rosetta", "-bad"} {
		cfg := defaultTartConfig()
		cfg.Provider.RosettaTag = tag
		if err := Validate(cfg); err == nil {
			t.Fatalf("provider.rosettaTag %q accepted", tag)
		}
	}

	cfg = defaultWSLConfig()
	cfg.Provider.SourceImage = "image.tar"
	cfg.Provider.RosettaTag = "rosetta"
	if err := Validate(cfg); err == nil {
		t.Fatal("provider.rosettaTag accepted for WSL")
	}
}

func TestValidateDockerPlatform(t *testing.T) {
	cfg := defaultDockerContainerConfig()
	cfg.Provider.SourceImage = "runner-image"
	cfg.Provider.Platform = "linux/amd64"
	if err := Validate(cfg); err != nil {
		t.Fatalf("valid docker platform rejected: %v", err)
	}

	for _, platform := range []string{"bad platform", "-linux/amd64", "linux/$bad"} {
		cfg := defaultDockerContainerConfig()
		cfg.Provider.SourceImage = "runner-image"
		cfg.Provider.Platform = platform
		if err := Validate(cfg); err == nil {
			t.Fatalf("provider.platform %q accepted", platform)
		}
	}

	cfg = defaultTartConfig()
	cfg.Provider.Platform = "linux/arm64"
	if err := Validate(cfg); err == nil {
		t.Fatal("provider.platform accepted for Tart")
	}
}

func TestValidateDockerRegistryMirror(t *testing.T) {
	cfg := defaultTartConfig()
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
		cfg := defaultTartConfig()
		cfg.Docker.RegistryMirrors = []string{mirror}
		if err := Validate(cfg); err == nil {
			t.Fatalf("docker.registryMirrors %q accepted", mirror)
		}
	}
}

func TestValidateDockerDaemonProxy(t *testing.T) {
	cfg := defaultTartConfig()
	cfg.Docker.HTTPProxy = "http://proxy.example.test:3128"
	cfg.Docker.HTTPSProxy = "https://proxy.example.test:8443"
	cfg.Docker.NoProxy = "localhost,127.0.0.1,.example.test,10.0.0.0/8,*"
	if err := Validate(cfg); err != nil {
		t.Fatalf("valid Docker daemon proxy rejected: %v", err)
	}

	for _, proxyURL := range []string{
		"proxy.example.test:3128",
		"ftp://proxy.example.test",
		"http://user:password@proxy.example.test",
		"http://proxy.example.test/path",
		"http://proxy.example.test?x=1",
		" http://proxy.example.test",
	} {
		cfg := defaultTartConfig()
		cfg.Docker.HTTPProxy = proxyURL
		if err := Validate(cfg); err == nil {
			t.Fatalf("docker.httpProxy %q accepted", proxyURL)
		}
	}

	for _, noProxy := range []string{"localhost,,example.test", " localhost", "http://example.test", "user@example.test", "10.0.0.0/not-a-prefix"} {
		cfg := defaultTartConfig()
		cfg.Docker.NoProxy = noProxy
		if err := Validate(cfg); err == nil {
			t.Fatalf("docker.noProxy %q accepted", noProxy)
		}
	}
}

func TestDockerConfigNeedsHostGatewayForProxy(t *testing.T) {
	if !DockerConfigNeedsHostGateway(DockerConfig{HTTPSProxy: "http://host.docker.internal:3128"}) {
		t.Fatal("host.docker.internal proxy should request docker-container host gateway")
	}
	if DockerConfigNeedsHostGateway(DockerConfig{HTTPSProxy: "http://http.docker.internal:3128"}) {
		t.Fatal("http.docker.internal proxy should not request host.docker.internal mapping")
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
	if got := err.Error(); got != "provider.type docker-socket is intentionally unsupported; use provider.type=docker-container for a private Docker daemon" {
		t.Fatalf("error = %q", got)
	}
}

func TestValidateDoesNotRequireGitHubForImageCommands(t *testing.T) {
	cfg := defaultTartConfig()
	if err := Validate(cfg); err != nil {
		t.Fatal(err)
	}
	if err := ValidateGitHub(cfg); err == nil {
		t.Fatal("ValidateGitHub accepted empty GitHub settings")
	}
}

func TestValidateRejectsInvalidPoolInstances(t *testing.T) {
	cfg := defaultTartConfig()
	cfg.Pool.Instances = 0
	if err := Validate(cfg); err == nil {
		t.Fatal("pool.instances=0 accepted")
	}
}

func TestLoadPoolReplacementRetryConfiguration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(path, []byte(`
pool:
  replacementRetryInitialSeconds: 12
  replacementRetryMaxSeconds: 720
  replacementRetryMultiplier: 1.5
  replacementRetryJitterPercent: 0
provider:
  type: tart
`), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := cfg.Pool.ReplacementRetryInitialSeconds, 12; got != want {
		t.Fatalf("pool.replacementRetryInitialSeconds = %d, want %d", got, want)
	}
	if got, want := cfg.Pool.ReplacementRetryMaxSeconds, 720; got != want {
		t.Fatalf("pool.replacementRetryMaxSeconds = %d, want %d", got, want)
	}
	if got, want := cfg.Pool.ReplacementRetryMultiplier, 1.5; got != want {
		t.Fatalf("pool.replacementRetryMultiplier = %v, want %v", got, want)
	}
	if got, want := cfg.Pool.ReplacementRetryJitterPercent, 0; got != want {
		t.Fatalf("pool.replacementRetryJitterPercent = %d, want %d", got, want)
	}
	if err := Validate(cfg); err != nil {
		t.Fatal(err)
	}
}

func TestValidateRejectsInvalidPoolReplacementRetryConfiguration(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{
			name:   "initial retry delay is not positive",
			mutate: func(cfg *Config) { cfg.Pool.ReplacementRetryInitialSeconds = 0 },
		},
		{
			name:   "maximum retry delay is below initial delay",
			mutate: func(cfg *Config) { cfg.Pool.ReplacementRetryMaxSeconds = cfg.Pool.ReplacementRetryInitialSeconds - 1 },
		},
		{
			name:   "retry multiplier is below one",
			mutate: func(cfg *Config) { cfg.Pool.ReplacementRetryMultiplier = 0.5 },
		},
		{
			name:   "retry multiplier is not finite",
			mutate: func(cfg *Config) { cfg.Pool.ReplacementRetryMultiplier = math.NaN() },
		},
		{
			name:   "retry jitter is below zero",
			mutate: func(cfg *Config) { cfg.Pool.ReplacementRetryJitterPercent = -1 },
		},
		{
			name:   "retry jitter exceeds one hundred percent",
			mutate: func(cfg *Config) { cfg.Pool.ReplacementRetryJitterPercent = 101 },
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := defaultTartConfig()
			test.mutate(&cfg)
			if err := Validate(cfg); err == nil {
				t.Fatal("Validate accepted invalid replacement retry configuration")
			}
		})
	}
}

func TestLoadRejectsInvalidPoolReplacementRetryValues(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		value string
	}{
		{name: "initial retry delay", key: "replacementRetryInitialSeconds", value: "invalid"},
		{name: "maximum retry delay", key: "replacementRetryMaxSeconds", value: "invalid"},
		{name: "retry multiplier", key: "replacementRetryMultiplier", value: "invalid"},
		{name: "retry jitter", key: "replacementRetryJitterPercent", value: "invalid"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.yml")
			content := "pool:\n  " + test.key + ": " + test.value + "\n"
			if err := os.WriteFile(path, []byte(content), 0644); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil {
				t.Fatal("Load accepted invalid replacement retry value")
			}
		})
	}
}

func TestValidateRejectsOverlongRunnerLabel(t *testing.T) {
	cfg := Default()
	cfg.Runner.IncludeHostLabel = false
	cfg.Runner.Labels = []string{"self-hosted", strings.Repeat("a", MaxRunnerLabelLength+1)}
	if err := Validate(cfg); err == nil {
		t.Fatal("overlong runner label accepted")
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

func TestDockerSandboxesRejectsNamePrefixOutsideProviderGrammar(t *testing.T) {
	cfg := Default()
	cfg.Provider.Type = "docker-sandboxes"
	cfg.Provider.Platform = "linux/amd64"
	cfg.Provider.SourceImage = ""
	cfg.Pool.NamePrefix = "EPAR_sandbox"
	cfg.Runner.Ephemeral = true
	cfg.Security.RunnerGroup.Enforcement = RunnerGroupEnforcementEnforce
	cfg.DockerSandboxes.PolicyGeneration = "sha256:" + strings.Repeat("b", 64)
	cfg.DockerSandboxes.RootDisk = "120GiB"
	cfg.DockerSandboxes.DockerDisk = "50GiB"
	if err := Validate(cfg); err == nil || !strings.Contains(err.Error(), "lowercase") {
		t.Fatalf("Validate() error = %v, want Docker Sandboxes prefix grammar rejection", err)
	}
}

func TestLoadDockerSandboxesConfig(t *testing.T) {
	t.Setenv(HostNameEnv, "sandbox-preview-host")
	dir := t.TempDir()
	path := filepath.Join(dir, "docker-sandboxes.yml")
	if err := os.WriteFile(path, []byte(`
provider:
  type: docker-sandboxes
image:
  sourceType: docker-image
  sourceImage: ghcr.io/catthehacker/ubuntu:full-latest
  sourcePlatform: linux/amd64
runner:
  ephemeral: true
security:
  runnerGroup:
    enforcement: enforce
dockerSandboxes:
  policyGeneration: sha256:abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789
  networkBaseline: balanced
  additionalAllow: [api.github.com, '*.githubusercontent.com:443']
  additionalDeny:
    - telemetry.example.invalid
  stagingRoot: .local/docker-sandboxes
  cpus: 2
  memory: 4GiB
  rootDisk: auto
  dockerDisk: 50GiB
  maxConcurrentCreates: 1
`), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := cfg.Provider.SourceImage, ""; got != want {
		t.Fatalf("provider.sourceImage = %q, want empty for docker-sandboxes", got)
	}
	if got, want := cfg.Provider.Platform, "linux/amd64"; got != want {
		t.Fatalf("provider.platform = %q, want %q", got, want)
	}
	if got, want := cfg.Pool.NamePrefix, "epar-docker-sandboxes"; got != want {
		t.Fatalf("pool.namePrefix = %q, want %q", got, want)
	}
	if got, want := cfg.Runner.Labels, []string{"self-hosted", "linux", "X64", "epar-docker-sandboxes", "epar-host-sandbox-preview-host"}; !slices.Equal(got, want) {
		t.Fatalf("runner.labels = %#v, want %#v", got, want)
	}
	if got, want := cfg.DockerSandboxes.Memory, "4GiB"; got != want {
		t.Fatalf("dockerSandboxes.memory = %q, want %q", got, want)
	}
	if got, want := cfg.DockerSandboxes.AdditionalAllow, []string{"api.github.com", "*.githubusercontent.com:443"}; !slices.Equal(got, want) {
		t.Fatalf("dockerSandboxes.additionalAllow = %#v, want %#v", got, want)
	}
	if err := Validate(cfg); err != nil {
		t.Fatal(err)
	}
}

func TestLoadDockerSandboxesRejectsRemovedHostReserve(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "docker-sandboxes.yml")
	if err := os.WriteFile(path, []byte("provider:\n  type: docker-sandboxes\ndockerSandboxes:\n  minHostFreeSpace: 50GiB\n"), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "remove it and configure the provider-neutral storage.minimumFree value instead") {
		t.Fatalf("Load() error = %v, want exact regeneration guidance", err)
	}
}

func TestLoadDockerSandboxesRejectsArchitectureEmulationConfiguration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "docker-sandboxes.yml")
	if err := os.WriteFile(path, []byte("provider:\n  type: docker-sandboxes\ndockerSandboxes:\n  architectureEmulation: disabled\n"), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "unknown key dockerSandboxes.architectureEmulation") {
		t.Fatalf("Load() error = %v, want removed emulation-setting rejection", err)
	}
}

func TestValidateDockerSandboxesRejectsInvalidPreviewConfiguration(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{
			name:   "source image is unsupported",
			mutate: func(cfg *Config) { cfg.Provider.SourceImage = "runner-image" },
		},
		{
			name: "specialized source image lacks the private daemon contract",
			mutate: func(cfg *Config) {
				cfg.Image.SourceType = ImageSourceDockerImage
				cfg.Image.SourceImage = "ghcr.io/catthehacker/ubuntu:js-latest"
			},
		},
		{
			name:   "platform is not a supported sandbox guest",
			mutate: func(cfg *Config) { cfg.Provider.Platform = "linux/s390x" },
		},
		{
			name:   "runner is persistent",
			mutate: func(cfg *Config) { cfg.Runner.Ephemeral = false },
		},
		{
			name:   "runner group enforcement is not fail closed",
			mutate: func(cfg *Config) { cfg.Security.RunnerGroup.Enforcement = RunnerGroupEnforcementWarn },
		},
		{
			name:   "policy generation is not content addressed",
			mutate: func(cfg *Config) { cfg.DockerSandboxes.PolicyGeneration = "verified-balanced-policy-fingerprint" },
		},
		{
			name:   "network baseline is unsupported",
			mutate: func(cfg *Config) { cfg.DockerSandboxes.NetworkBaseline = "locked-down" },
		},
		{
			name:   "allowlist wildcard is unsafe",
			mutate: func(cfg *Config) { cfg.DockerSandboxes.AdditionalAllow = []string{"**.example.test"} },
		},
		{
			name:   "allowlist contains a URL",
			mutate: func(cfg *Config) { cfg.DockerSandboxes.AdditionalAllow = []string{"https://example.test"} },
		},
		{
			name:   "allowlist overlaps denylist",
			mutate: func(cfg *Config) { cfg.DockerSandboxes.AdditionalDeny = []string{"api.github.com"} },
		},
		{
			name:   "open allowlist overrides host boundary",
			mutate: func(cfg *Config) { cfg.DockerSandboxes.AdditionalAllow = []string{"host.docker.internal"} },
		},
		{
			name:   "cpus are not positive",
			mutate: func(cfg *Config) { cfg.DockerSandboxes.CPUs = 0 },
		},
		{
			name:   "memory is not a positive byte size",
			mutate: func(cfg *Config) { cfg.DockerSandboxes.Memory = "0GiB" },
		},
		{
			name:   "root disk is below the hard minimum",
			mutate: func(cfg *Config) { cfg.DockerSandboxes.RootDisk = "19GiB" },
		},
		{
			name:   "docker disk is below the hard minimum",
			mutate: func(cfg *Config) { cfg.DockerSandboxes.DockerDisk = "512MiB" },
		},
		{
			name:   "concurrency is not positive",
			mutate: func(cfg *Config) { cfg.DockerSandboxes.MaxConcurrentCreates = 0 },
		},
		{
			name:   "staging root is an absolute host path",
			mutate: func(cfg *Config) { cfg.DockerSandboxes.StagingRoot = `C:\epar-staging` },
		},
		{
			name:   "staging root is an absolute Unix host path",
			mutate: func(cfg *Config) { cfg.DockerSandboxes.StagingRoot = "/tmp/epar-staging" },
		},
		{
			name:   "staging root escapes project local state",
			mutate: func(cfg *Config) { cfg.DockerSandboxes.StagingRoot = "../epar-staging" },
		},
		{
			name:   "staging root overlaps ledger",
			mutate: func(cfg *Config) { cfg.DockerSandboxes.StagingRoot = ".local/state/docker-sandboxes" },
		},
		{
			name:   "staging root overlaps native binary cache",
			mutate: func(cfg *Config) { cfg.DockerSandboxes.StagingRoot = ".local/bin/staging" },
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := validDockerSandboxesConfig()
			test.mutate(&cfg)
			if err := Validate(cfg); err == nil {
				t.Fatal("Validate accepted invalid docker-sandboxes configuration")
			}
		})
	}
}

func TestValidateDockerSandboxesAcceptsARM64Configuration(t *testing.T) {
	cfg := validDockerSandboxesConfig()
	cfg.Provider.Platform = "linux/arm64"
	cfg.Image.SourcePlatform = cfg.Provider.Platform
	if err := Validate(cfg); err != nil {
		t.Fatalf("Validate() rejected linux/arm64 Docker Sandboxes configuration: %v", err)
	}
}

func TestDockerSandboxesGuestPlatform(t *testing.T) {
	tests := []struct {
		hostOS   string
		hostArch string
		want     string
		wantErr  bool
	}{
		{hostOS: "windows", hostArch: "amd64", want: "linux/amd64"},
		{hostOS: "linux", hostArch: "amd64", want: "linux/amd64"},
		{hostOS: "darwin", hostArch: "arm64", want: "linux/arm64"},
		{hostOS: "windows", hostArch: "arm64", want: "linux/arm64"},
		{hostOS: "linux", hostArch: "arm64", want: "linux/arm64"},
		{hostOS: "darwin", hostArch: "amd64", want: "linux/amd64"},
		{hostOS: "futureos", hostArch: "amd64", want: "linux/amd64"},
		{hostOS: "futureos", hostArch: "arm64", want: "linux/arm64"},
		{hostOS: "futureos", hostArch: "386", wantErr: true},
		{hostOS: "", hostArch: "amd64", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.hostOS+"_"+test.hostArch, func(t *testing.T) {
			got, err := DockerSandboxesGuestPlatform(test.hostOS, test.hostArch)
			if test.wantErr {
				if err == nil {
					t.Fatalf("DockerSandboxesGuestPlatform(%q, %q) = %q, nil; want error", test.hostOS, test.hostArch, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("DockerSandboxesGuestPlatform(%q, %q) error = %v", test.hostOS, test.hostArch, err)
			}
			if got != test.want {
				t.Fatalf("DockerSandboxesGuestPlatform(%q, %q) = %q, want %q", test.hostOS, test.hostArch, got, test.want)
			}
		})
	}
}

func TestValidateDockerSandboxHostname(t *testing.T) {
	for _, hostname := range []string{"api.github.com", "api.github.com:443", "*.githubusercontent.com", "*.githubusercontent.com:443"} {
		if err := ValidateDockerSandboxHostname(hostname); err != nil {
			t.Fatalf("hostname %q rejected: %v", hostname, err)
		}
	}
	for _, hostname := range []string{"**.githubusercontent.com", "api.*.github.com", "https://api.github.com", "api.github.com/path", "api.github.com:0", "api.github.com:65536", "127.0.0.1", "[::1]:443"} {
		if err := ValidateDockerSandboxHostname(hostname); err == nil {
			t.Fatalf("hostname %q accepted", hostname)
		}
	}
}

func TestParseByteSize(t *testing.T) {
	if got, err := ParseByteSize("4GiB"); err != nil || got != 4*(1<<30) {
		t.Fatalf("ParseByteSize(4GiB) = %d, %v", got, err)
	}
	for _, value := range []string{"", " 4GiB", "4GB", "0GiB", "-1GiB", "999999999999999999999TiB"} {
		if _, err := ParseByteSize(value); err == nil {
			t.Fatalf("ParseByteSize(%q) accepted invalid value", value)
		}
	}
}

func TestEffectiveMinimumFreeBytesUsesCommonReserve(t *testing.T) {
	cfg := Default()
	cfg.Provider.Type = "docker-sandboxes"
	cfg.Storage.MinimumFree = "20GiB"

	got, err := EffectiveMinimumFreeBytes(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if want := uint64(20 << 30); got != want {
		t.Fatalf("EffectiveMinimumFreeBytes() = %d, want %d", got, want)
	}
}

func TestEffectiveMinimumFreeBytesKeepsStricterCommonReserve(t *testing.T) {
	cfg := Default()
	cfg.Provider.Type = "docker-sandboxes"
	cfg.Storage.MinimumFree = "60GiB"

	got, err := EffectiveMinimumFreeBytes(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if want := uint64(60 << 30); got != want {
		t.Fatalf("EffectiveMinimumFreeBytes() = %d, want %d", got, want)
	}
}

func validDockerSandboxesConfig() Config {
	cfg := Default()
	cfg.Provider.Type = "docker-sandboxes"
	cfg.Provider.Platform = "linux/amd64"
	applyProviderDefaults(&cfg, map[string]bool{"provider.type": true, "provider.platform": true})
	cfg.Provider.SourceImage = ""
	cfg.Runner.Ephemeral = true
	cfg.Security.RunnerGroup.Enforcement = RunnerGroupEnforcementEnforce
	cfg.DockerSandboxes.PolicyGeneration = "sha256:abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	cfg.DockerSandboxes.AdditionalAllow = []string{"api.github.com"}
	cfg.DockerSandboxes.RootDisk = "120GiB"
	cfg.DockerSandboxes.DockerDisk = "50GiB"
	return cfg
}

func defaultTartConfig() Config {
	cfg := Default()
	cfg.Provider.Type = "tart"
	applyProviderDefaults(&cfg, map[string]bool{"provider.type": true})
	return cfg
}

func defaultWSLConfig() Config {
	cfg := Default()
	cfg.Provider.Type = "wsl"
	applyProviderDefaults(&cfg, map[string]bool{"provider.type": true})
	return cfg
}

func defaultDockerContainerConfig() Config {
	cfg := Default()
	cfg.Provider.Type = "docker-container"
	applyProviderDefaults(&cfg, map[string]bool{"provider.type": true})
	return cfg
}
