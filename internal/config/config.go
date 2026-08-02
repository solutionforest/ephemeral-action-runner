package config

import (
	"bufio"
	"fmt"
	"math"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	GitHub          GitHubConfig
	Image           ImageConfig
	Pool            PoolConfig
	Storage         StorageConfig
	Logging         LoggingConfig
	Runner          RunnerConfig
	Security        SecurityConfig
	Provider        ProviderConfig
	Docker          DockerConfig
	DockerSandboxes DockerSandboxesConfig
	Timeouts        TimeoutConfig
	warnings        []string
}

// Warnings returns non-fatal configuration migration notices discovered while loading.
func (cfg Config) Warnings() []string {
	return append([]string(nil), cfg.warnings...)
}

type GitHubConfig struct {
	AppID          int64
	Organization   string
	PrivateKeyPath string
	APIBaseURL     string
	WebBaseURL     string
}

type ImageConfig struct {
	SourceImage               string
	SourceType                string
	SourcePlatform            string
	OutputImage               string
	UpstreamDir               string
	UpstreamLock              string
	RunnerVersion             string
	UpdateFrequency           string
	UpdateTime                string
	CustomInstallScripts      []string
	TrustedCACertificatePaths []string
	HostTrustMode             string
	HostTrustScopes           []string
}

const (
	HostTrustModeDisabled = "disabled"
	HostTrustModeOverlay  = "overlay"

	HostTrustScopeSystem = "system"
	HostTrustScopeUser   = "user"

	ImageUpdateFrequencyDaily    = "daily"
	ImageUpdateFrequencyWeekly   = "weekly"
	ImageUpdateFrequencyBiweekly = "biweekly"
	ImageUpdateFrequencyMonthly  = "monthly"
	ImageUpdateFrequencyManual   = "manual"
	DefaultImageUpdateTime       = "07:00"
)

type PoolConfig struct {
	Instances                      int
	NamePrefix                     string
	ReplacementRetryInitialSeconds int
	ReplacementRetryMaxSeconds     int
	ReplacementRetryMultiplier     float64
	ReplacementRetryJitterPercent  int
}

// StorageConfig controls provider-neutral capacity admission and conservative
// retention. String byte sizes keep the YAML readable and are validated before
// any provider or cleanup operation is constructed.
type StorageConfig struct {
	MinimumFree           string
	GracePeriod           string
	KeepPrevious          int
	AutomaticHousekeeping string
	BuildCacheLimit       string
	GoCacheLimit          string
}

const (
	StorageHousekeepingConservative = "conservative"
	StorageHousekeepingDisabled     = "disabled"
)

type LoggingConfig struct {
	Directory                   string
	ManagerSinks                []string
	ManagerConsoleFormat        string
	ManagerConsoleTextFormat    string
	ManagerFileFormat           string
	TranscriptSinks             []string
	TranscriptConsoleFormat     string
	TranscriptConsoleTextFormat string
	MaxFileSizeMiB              int
	MaxBackups                  int
	CompressBackups             bool
	RetentionEnabled            bool
	RetentionMaxTotalMiB        int
	ManagerMaxAgeDays           int
	InstanceMaxAgeDays          int
	BuildMaxAgeDays             int
	ErrorMaxAgeDays             int
	BenchmarkMaxAgeDays         int
	RetentionIntervalMinutes    int
}

type RunnerConfig struct {
	Labels           []string
	IncludeHostLabel bool
	Ephemeral        bool
	Group            string
	NoDefaultLabels  bool
}

type SecurityConfig struct {
	RunnerGroup RunnerGroupSecurityConfig
}

type RunnerGroupSecurityConfig struct {
	Enforcement                       string
	RequireExplicitGroup              bool
	RequireNonDefaultGroup            bool
	RequiredRepositoryAccess          string
	RequirePublicRepositoriesDisabled bool
}

const (
	RunnerGroupEnforcementEnforce = "enforce"
	RunnerGroupEnforcementWarn    = "warn"

	RunnerGroupRepositoryAccessSelected = "selected"
	RunnerGroupRepositoryAccessPrivate  = "private"
	RunnerGroupRepositoryAccessAll      = "all"
)

type ProviderConfig struct {
	Type        string
	SourceImage string
	Network     string
	RosettaTag  string
	InstallRoot string
	Platform    string
}

type DockerConfig struct {
	RegistryMirrors []string
	HTTPProxy       string
	HTTPSProxy      string
	NoProxy         string
}

// DockerSandboxesConfig configures host/runtime behavior for the
// docker-sandboxes provider. The desired source belongs to ImageConfig, while
// the exact imported template identity is stored in EPAR's local artifact
// receipt rather than user configuration.
type DockerSandboxesConfig struct {
	PolicyGeneration     string
	NetworkBaseline      string
	AdditionalAllow      []string
	AdditionalDeny       []string
	StagingRoot          string
	CPUs                 int
	Memory               string
	RootDisk             string
	DockerDisk           string
	MaxConcurrentCreates int
}

const (
	DockerSandboxesNetworkBaselineOpen     = "open"
	DockerSandboxesNetworkBaselineBalanced = "balanced"
)

var dockerSandboxesOpenDefaultDenyResources = []string{
	"host.docker.internal",
	"gateway.docker.internal",
	"kubernetes.docker.internal",
	"host.containers.internal",
}

// DockerSandboxesOpenDefaultDenyResources returns host aliases that EPAR denies
// in every sandbox-scoped Open policy. Docker Sandboxes can proxy an allowed
// host.docker.internal request to a native-host loopback service, so
// public egress must not implicitly enable these host-service aliases.
func DockerSandboxesOpenDefaultDenyResources() []string {
	return append([]string(nil), dockerSandboxesOpenDefaultDenyResources...)
}

const (
	DockerSandboxesAutomaticRootDisk            = "auto"
	DockerSandboxesMinimumRootDiskBytes   int64 = 20 << 30
	DockerSandboxesMinimumDockerDiskBytes int64 = 1 << 30
	DockerSandboxesDefaultDockerDisk            = "50GiB"
)

type TimeoutConfig struct {
	BootSeconds         int
	GitHubOnlineSeconds int
	CommandSeconds      int
}

const (
	ImageSourceDockerImage = "docker-image"
	ImageSourceRootFSTar   = "rootfs-tar"
	MaxRunnerLabelLength   = 256
	HostNameEnv            = "EPAR_HOST_NAME"
)

func Default() Config {
	return Config{
		GitHub: GitHubConfig{
			APIBaseURL: "https://api.github.com",
			WebBaseURL: "https://github.com",
		},
		Image: ImageConfig{
			SourceImage:     "ghcr.io/cirruslabs/ubuntu:latest",
			OutputImage:     "epar-ubuntu-24-arm64",
			UpstreamDir:     "third_party/runner-images",
			UpstreamLock:    "third_party/runner-images.lock",
			RunnerVersion:   "latest",
			UpdateFrequency: ImageUpdateFrequencyWeekly,
			UpdateTime:      DefaultImageUpdateTime,
			HostTrustMode:   HostTrustModeDisabled,
			HostTrustScopes: []string{
				HostTrustScopeSystem,
			},
		},
		Pool: PoolConfig{
			Instances:                      1,
			NamePrefix:                     "epar",
			ReplacementRetryInitialSeconds: 15,
			ReplacementRetryMaxSeconds:     1800,
			ReplacementRetryMultiplier:     2,
			ReplacementRetryJitterPercent:  20,
		},
		Storage: StorageConfig{
			MinimumFree:           "1GiB",
			GracePeriod:           "168h",
			KeepPrevious:          0,
			AutomaticHousekeeping: StorageHousekeepingConservative,
			BuildCacheLimit:       "20GiB",
			GoCacheLimit:          "10GiB",
		},
		Logging: LoggingConfig{
			Directory:                "work/logs",
			ManagerSinks:             []string{"console"},
			ManagerConsoleFormat:     "text",
			ManagerFileFormat:        "json",
			TranscriptSinks:          []string{"file"},
			TranscriptConsoleFormat:  "text",
			MaxFileSizeMiB:           100,
			MaxBackups:               3,
			CompressBackups:          true,
			RetentionEnabled:         true,
			RetentionMaxTotalMiB:     1024,
			ManagerMaxAgeDays:        14,
			InstanceMaxAgeDays:       14,
			BuildMaxAgeDays:          14,
			ErrorMaxAgeDays:          30,
			BenchmarkMaxAgeDays:      90,
			RetentionIntervalMinutes: 60,
		},
		Runner: RunnerConfig{
			Labels:           []string{"self-hosted", "linux", "ARM64", "epar-tart-ubuntu-24.04-base"},
			IncludeHostLabel: true,
			Ephemeral:        true,
		},
		Security: SecurityConfig{
			RunnerGroup: RunnerGroupSecurityConfig{
				Enforcement:                       RunnerGroupEnforcementWarn,
				RequireExplicitGroup:              true,
				RequireNonDefaultGroup:            true,
				RequiredRepositoryAccess:          RunnerGroupRepositoryAccessSelected,
				RequirePublicRepositoriesDisabled: true,
			},
		},
		Provider: ProviderConfig{
			Type:        "tart",
			SourceImage: "epar-ubuntu-24-arm64",
			Network:     "default",
			InstallRoot: "work/wsl",
		},
		DockerSandboxes: DockerSandboxesConfig{
			NetworkBaseline:      DockerSandboxesNetworkBaselineOpen,
			StagingRoot:          ".local/docker-sandboxes-staging",
			CPUs:                 4,
			Memory:               "8GiB",
			RootDisk:             DockerSandboxesAutomaticRootDisk,
			DockerDisk:           DockerSandboxesDefaultDockerDisk,
			MaxConcurrentCreates: 2,
		},
		Timeouts: TimeoutConfig{
			BootSeconds:         180,
			GitHubOnlineSeconds: 180,
			CommandSeconds:      900,
		},
	}
}

func Load(path string) (Config, error) {
	cfg := Default()
	if path == "" {
		applyRunnerHostLabel(&cfg)
		return cfg, nil
	}
	file, err := os.Open(expandHome(path))
	if err != nil {
		return cfg, err
	}
	defer file.Close()

	section := ""
	subsection := ""
	subsectionIndent := 0
	scanner := bufio.NewScanner(file)
	lineNo := 0
	var pendingList *pendingListKey
	explicit := map[string]bool{}
	legacyLogDir := ""
	legacyLogDirLine := 0
	for scanner.Scan() {
		lineNo++
		raw := strings.TrimRight(scanner.Text(), " \t")
		line := strings.TrimSpace(stripComment(raw))
		if line == "" {
			continue
		}
		indent := leadingSpaces(raw)
		if pendingList != nil {
			if indent > pendingList.indent && strings.HasPrefix(line, "-") {
				item := strings.TrimSpace(strings.TrimPrefix(line, "-"))
				if item == "" {
					return cfg, fmt.Errorf("%s:%d: empty list item for %s.%s", path, lineNo, pendingList.section, pendingList.key)
				}
				if err := appendListValue(&cfg, pendingList.section, pendingList.key, item); err != nil {
					return cfg, fmt.Errorf("%s:%d: %w", path, lineNo, err)
				}
				continue
			}
			pendingList = nil
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			return cfg, fmt.Errorf("%s:%d: expected key: value", path, lineNo)
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if indent == 0 && value == "" {
			if !isKnownSection(key) {
				return cfg, fmt.Errorf("%s:%d: unknown section %q", path, lineNo, key)
			}
			section = key
			subsection = ""
			subsectionIndent = 0
			continue
		}
		if indent == 0 {
			return cfg, fmt.Errorf("%s:%d: section %q must not have a scalar value", path, lineNo, key)
		}
		if section == "" {
			return cfg, fmt.Errorf("%s:%d: key %q must be under a section", path, lineNo, key)
		}
		if section == "security" && value == "" {
			if key != "runnerGroup" {
				return cfg, fmt.Errorf("%s:%d: unknown subsection security.%s", path, lineNo, key)
			}
			subsection = key
			subsectionIndent = indent
			continue
		}
		effectiveSection := section
		if section == "security" {
			if subsection == "" {
				return cfg, fmt.Errorf("%s:%d: key %q must be under security.runnerGroup", path, lineNo, key)
			}
			if indent <= subsectionIndent {
				return cfg, fmt.Errorf("%s:%d: key %q must be nested under security.%s", path, lineNo, key, subsection)
			}
			effectiveSection = section + "." + subsection
		}
		if effectiveSection == "pool" && key == "logDir" {
			legacyLogDir = trimQuotes(value)
			legacyLogDirLine = lineNo
			explicit["pool.logDir"] = true
			continue
		}
		if value == "" && isListKey(effectiveSection, key) {
			if err := setListValue(&cfg, effectiveSection, key, nil); err != nil {
				return cfg, fmt.Errorf("%s:%d: %w", path, lineNo, err)
			}
			explicit[effectiveSection+"."+key] = true
			pendingList = &pendingListKey{section: effectiveSection, key: key, indent: indent}
			continue
		}
		if err := apply(&cfg, effectiveSection, key, value); err != nil {
			return cfg, fmt.Errorf("%s:%d: %w", path, lineNo, err)
		}
		explicit[effectiveSection+"."+key] = true
	}
	if err := scanner.Err(); err != nil {
		return cfg, err
	}
	if explicit["pool.logDir"] {
		if explicit["logging.directory"] {
			return cfg, fmt.Errorf("%s:%d: pool.logDir cannot be used with logging.directory; remove pool.logDir", path, legacyLogDirLine)
		}
		cfg.Logging.Directory = legacyLogDir
		cfg.warnings = append(cfg.warnings, fmt.Sprintf("%s:%d: pool.logDir is deprecated; using its value as logging.directory (move it to the top-level logging section)", path, legacyLogDirLine))
	}
	if !explicit["security.runnerGroup.enforcement"] &&
		!explicit["security.runnerGroup.requireExplicitGroup"] &&
		!explicit["security.runnerGroup.requireNonDefaultGroup"] &&
		!explicit["security.runnerGroup.requiredRepositoryAccess"] &&
		!explicit["security.runnerGroup.requirePublicRepositoriesDisabled"] {
		cfg.warnings = append(cfg.warnings, fmt.Sprintf("%s: runner-group security policy is not configured; using strict recommended checks in warn mode (add security.runnerGroup.enforcement: enforce after reviewing the policy)", path))
	}
	if !explicit["image.updateFrequency"] && !explicit["image.updateTime"] {
		cfg.warnings = append(cfg.warnings, fmt.Sprintf("%s: image update policy is not configured; using weekly checks at 07:00 local time", path))
	}
	applyProviderDefaults(&cfg, explicit)
	applyRunnerHostLabel(&cfg)
	cfg.GitHub.PrivateKeyPath = expandHome(cfg.GitHub.PrivateKeyPath)
	for i, path := range cfg.Image.TrustedCACertificatePaths {
		cfg.Image.TrustedCACertificatePaths[i] = expandHome(path)
	}
	return cfg, nil
}

type pendingListKey struct {
	section string
	key     string
	indent  int
}

func apply(cfg *Config, section, key, value string) error {
	value = trimQuotes(value)
	switch section {
	case "github":
		switch key {
		case "appId":
			v, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return fmt.Errorf("invalid github.appId: %w", err)
			}
			cfg.GitHub.AppID = v
		case "organization":
			cfg.GitHub.Organization = value
		case "privateKeyPath":
			cfg.GitHub.PrivateKeyPath = value
		case "apiBaseUrl":
			cfg.GitHub.APIBaseURL = strings.TrimRight(value, "/")
		case "webBaseUrl":
			cfg.GitHub.WebBaseURL = strings.TrimRight(value, "/")
		default:
			return unknownKey(section, key)
		}
	case "image":
		switch key {
		case "sourceImage":
			cfg.Image.SourceImage = value
		case "sourceType":
			cfg.Image.SourceType = value
		case "sourcePlatform":
			cfg.Image.SourcePlatform = value
		case "outputImage":
			cfg.Image.OutputImage = value
		case "upstreamDir":
			cfg.Image.UpstreamDir = value
		case "upstreamLock":
			cfg.Image.UpstreamLock = value
		case "runnerVersion":
			cfg.Image.RunnerVersion = value
		case "updateFrequency":
			cfg.Image.UpdateFrequency = strings.ToLower(value)
		case "updateTime":
			cfg.Image.UpdateTime = value
		case "profile":
			return fmt.Errorf("image.profile is not supported; use image.customInstallScripts")
		case "customInstallScripts":
			return setListValue(cfg, section, key, parseList(value))
		case "trustedCaCertificatePaths":
			return setListValue(cfg, section, key, parseList(value))
		case "hostTrustMode":
			cfg.Image.HostTrustMode = strings.ToLower(value)
		case "hostTrustScopes":
			return setListValue(cfg, section, key, parseList(value))
		default:
			return unknownKey(section, key)
		}
	case "pool":
		switch key {
		case "instances":
			v, err := strconv.Atoi(value)
			if err != nil {
				return fmt.Errorf("invalid pool.instances: %w", err)
			}
			cfg.Pool.Instances = v
		case "namePrefix", "vmPrefix":
			cfg.Pool.NamePrefix = value
		case "replacementRetryInitialSeconds":
			v, err := strconv.Atoi(value)
			if err != nil {
				return fmt.Errorf("invalid pool.replacementRetryInitialSeconds: %w", err)
			}
			cfg.Pool.ReplacementRetryInitialSeconds = v
		case "replacementRetryMaxSeconds":
			v, err := strconv.Atoi(value)
			if err != nil {
				return fmt.Errorf("invalid pool.replacementRetryMaxSeconds: %w", err)
			}
			cfg.Pool.ReplacementRetryMaxSeconds = v
		case "replacementRetryMultiplier":
			v, err := strconv.ParseFloat(value, 64)
			if err != nil {
				return fmt.Errorf("invalid pool.replacementRetryMultiplier: %w", err)
			}
			cfg.Pool.ReplacementRetryMultiplier = v
		case "replacementRetryJitterPercent":
			v, err := strconv.Atoi(value)
			if err != nil {
				return fmt.Errorf("invalid pool.replacementRetryJitterPercent: %w", err)
			}
			cfg.Pool.ReplacementRetryJitterPercent = v
		case "logDir":
			return fmt.Errorf("pool.logDir is deprecated; use logging.directory")
		default:
			return unknownKey(section, key)
		}
	case "logging":
		switch key {
		case "directory":
			cfg.Logging.Directory = value
		case "managerSinks", "transcriptSinks":
			return setListValue(cfg, section, key, parseList(value))
		case "managerConsoleFormat":
			cfg.Logging.ManagerConsoleFormat = strings.ToLower(value)
		case "managerConsoleTextFormat":
			cfg.Logging.ManagerConsoleTextFormat = value
		case "managerFileFormat":
			cfg.Logging.ManagerFileFormat = strings.ToLower(value)
		case "transcriptConsoleFormat":
			cfg.Logging.TranscriptConsoleFormat = strings.ToLower(value)
		case "transcriptConsoleTextFormat":
			cfg.Logging.TranscriptConsoleTextFormat = value
		case "maxFileSizeMiB":
			v, err := strconv.Atoi(value)
			if err != nil {
				return fmt.Errorf("invalid logging.maxFileSizeMiB: %w", err)
			}
			cfg.Logging.MaxFileSizeMiB = v
		case "maxBackups":
			v, err := strconv.Atoi(value)
			if err != nil {
				return fmt.Errorf("invalid logging.maxBackups: %w", err)
			}
			cfg.Logging.MaxBackups = v
		case "compressBackups":
			v, err := strconv.ParseBool(value)
			if err != nil {
				return fmt.Errorf("invalid logging.compressBackups: %w", err)
			}
			cfg.Logging.CompressBackups = v
		case "retentionEnabled":
			v, err := strconv.ParseBool(value)
			if err != nil {
				return fmt.Errorf("invalid logging.retentionEnabled: %w", err)
			}
			cfg.Logging.RetentionEnabled = v
		case "retentionMaxTotalMiB":
			v, err := strconv.Atoi(value)
			if err != nil {
				return fmt.Errorf("invalid logging.retentionMaxTotalMiB: %w", err)
			}
			cfg.Logging.RetentionMaxTotalMiB = v
		case "managerMaxAgeDays", "instanceMaxAgeDays", "buildMaxAgeDays", "errorMaxAgeDays", "benchmarkMaxAgeDays", "retentionIntervalMinutes":
			v, err := strconv.Atoi(value)
			if err != nil {
				return fmt.Errorf("invalid logging.%s: %w", key, err)
			}
			switch key {
			case "managerMaxAgeDays":
				cfg.Logging.ManagerMaxAgeDays = v
			case "instanceMaxAgeDays":
				cfg.Logging.InstanceMaxAgeDays = v
			case "buildMaxAgeDays":
				cfg.Logging.BuildMaxAgeDays = v
			case "errorMaxAgeDays":
				cfg.Logging.ErrorMaxAgeDays = v
			case "benchmarkMaxAgeDays":
				cfg.Logging.BenchmarkMaxAgeDays = v
			case "retentionIntervalMinutes":
				cfg.Logging.RetentionIntervalMinutes = v
			}
		default:
			return unknownKey(section, key)
		}
	case "storage":
		switch key {
		case "minimumFree":
			cfg.Storage.MinimumFree = value
		case "gracePeriod":
			cfg.Storage.GracePeriod = value
		case "keepPrevious":
			v, err := strconv.Atoi(value)
			if err != nil {
				return fmt.Errorf("invalid storage.keepPrevious: %w", err)
			}
			cfg.Storage.KeepPrevious = v
		case "automaticHousekeeping":
			cfg.Storage.AutomaticHousekeeping = strings.ToLower(value)
		case "buildCacheLimit":
			cfg.Storage.BuildCacheLimit = value
		case "goCacheLimit":
			cfg.Storage.GoCacheLimit = value
		default:
			return unknownKey(section, key)
		}
	case "runner":
		switch key {
		case "labels":
			return setListValue(cfg, section, key, parseList(value))
		case "group":
			cfg.Runner.Group = value
		case "includeHostLabel":
			v, err := strconv.ParseBool(value)
			if err != nil {
				return fmt.Errorf("invalid runner.includeHostLabel: %w", err)
			}
			cfg.Runner.IncludeHostLabel = v
		case "ephemeral":
			v, err := strconv.ParseBool(value)
			if err != nil {
				return fmt.Errorf("invalid runner.ephemeral: %w", err)
			}
			cfg.Runner.Ephemeral = v
		case "noDefaultLabels":
			v, err := strconv.ParseBool(value)
			if err != nil {
				return fmt.Errorf("invalid runner.noDefaultLabels: %w", err)
			}
			cfg.Runner.NoDefaultLabels = v
		default:
			return unknownKey(section, key)
		}
	case "security.runnerGroup":
		switch key {
		case "enforcement":
			cfg.Security.RunnerGroup.Enforcement = strings.ToLower(value)
		case "requireExplicitGroup":
			v, err := strconv.ParseBool(value)
			if err != nil {
				return fmt.Errorf("invalid security.runnerGroup.requireExplicitGroup: %w", err)
			}
			cfg.Security.RunnerGroup.RequireExplicitGroup = v
		case "requireNonDefaultGroup":
			v, err := strconv.ParseBool(value)
			if err != nil {
				return fmt.Errorf("invalid security.runnerGroup.requireNonDefaultGroup: %w", err)
			}
			cfg.Security.RunnerGroup.RequireNonDefaultGroup = v
		case "requiredRepositoryAccess":
			cfg.Security.RunnerGroup.RequiredRepositoryAccess = strings.ToLower(value)
		case "requirePublicRepositoriesDisabled":
			v, err := strconv.ParseBool(value)
			if err != nil {
				return fmt.Errorf("invalid security.runnerGroup.requirePublicRepositoriesDisabled: %w", err)
			}
			cfg.Security.RunnerGroup.RequirePublicRepositoriesDisabled = v
		default:
			return unknownKey(section, key)
		}
	case "provider":
		switch key {
		case "type":
			cfg.Provider.Type = value
		case "sourceImage":
			cfg.Provider.SourceImage = value
		case "network":
			cfg.Provider.Network = value
		case "rosettaTag":
			cfg.Provider.RosettaTag = value
		case "installRoot":
			cfg.Provider.InstallRoot = value
		case "platform":
			cfg.Provider.Platform = value
		default:
			return unknownKey(section, key)
		}
	case "docker":
		switch key {
		case "registryMirrors":
			return setListValue(cfg, section, key, parseList(value))
		case "httpProxy":
			cfg.Docker.HTTPProxy = value
		case "httpsProxy":
			cfg.Docker.HTTPSProxy = value
		case "noProxy":
			cfg.Docker.NoProxy = value
		default:
			return unknownKey(section, key)
		}
	case "dockerSandboxes":
		switch key {
		case "template":
			return fmt.Errorf("dockerSandboxes.template is no longer supported; remove generated template identities and rerun ./start to provision a Docker Sandboxes runner template")
		case "templateDigest":
			return fmt.Errorf("dockerSandboxes.templateDigest is no longer supported; remove generated template identities and rerun ./start to provision a Docker Sandboxes runner template")
		case "policyGeneration":
			cfg.DockerSandboxes.PolicyGeneration = value
		case "networkBaseline":
			cfg.DockerSandboxes.NetworkBaseline = strings.ToLower(value)
		case "additionalAllow", "additionalDeny":
			return setListValue(cfg, section, key, parseList(value))
		case "stagingRoot":
			cfg.DockerSandboxes.StagingRoot = value
		case "cpus":
			v, err := strconv.Atoi(value)
			if err != nil {
				return fmt.Errorf("invalid dockerSandboxes.cpus: %w", err)
			}
			cfg.DockerSandboxes.CPUs = v
		case "memory", "rootDisk", "dockerDisk":
			switch key {
			case "memory":
				cfg.DockerSandboxes.Memory = value
			case "rootDisk":
				cfg.DockerSandboxes.RootDisk = value
			case "dockerDisk":
				cfg.DockerSandboxes.DockerDisk = value
			}
		case "minHostFreeSpace":
			return fmt.Errorf("dockerSandboxes.minHostFreeSpace is no longer supported; remove it and configure the provider-neutral storage.minimumFree value instead")
		case "maxConcurrentCreates":
			v, err := strconv.Atoi(value)
			if err != nil {
				return fmt.Errorf("invalid dockerSandboxes.maxConcurrentCreates: %w", err)
			}
			cfg.DockerSandboxes.MaxConcurrentCreates = v
		default:
			return unknownKey(section, key)
		}
	case "timeouts":
		v, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("invalid timeouts.%s: %w", key, err)
		}
		switch key {
		case "bootSeconds":
			cfg.Timeouts.BootSeconds = v
		case "githubOnlineSeconds":
			cfg.Timeouts.GitHubOnlineSeconds = v
		case "commandSeconds":
			cfg.Timeouts.CommandSeconds = v
		default:
			return unknownKey(section, key)
		}
	default:
		return fmt.Errorf("unknown section %q", section)
	}
	return nil
}

func unknownKey(section, key string) error {
	return fmt.Errorf("unknown key %s.%s", section, key)
}

func isKnownSection(section string) bool {
	switch section {
	case "github", "image", "pool", "storage", "logging", "runner", "security", "provider", "docker", "dockerSandboxes", "timeouts":
		return true
	default:
		return false
	}
}

func applyProviderDefaults(cfg *Config, explicit map[string]bool) {
	switch cfg.Provider.Type {
	case "wsl":
		sourceType := cfg.Image.SourceType
		if !explicit["image.sourceType"] {
			sourceType = ImageSourceDockerImage
			if explicit["image.sourceImage"] && looksLikeRootFSTar(cfg.Image.SourceImage) {
				sourceType = ImageSourceRootFSTar
			}
			cfg.Image.SourceType = sourceType
		}
		if !explicit["image.sourceImage"] {
			if sourceType == ImageSourceRootFSTar {
				cfg.Image.SourceImage = "work/images/ubuntu-24.04-clean.rootfs.tar"
			} else {
				cfg.Image.SourceImage = "ghcr.io/catthehacker/ubuntu:full-latest"
			}
		}
		if !explicit["image.outputImage"] {
			if sourceType == ImageSourceRootFSTar {
				cfg.Image.OutputImage = "work/images/epar-ubuntu-24-wsl.tar"
			} else {
				cfg.Image.OutputImage = "work/images/epar-wsl-catthehacker-ubuntu.tar"
			}
		}
		if sourceType == ImageSourceDockerImage && !explicit["image.sourcePlatform"] {
			cfg.Image.SourcePlatform = "linux/amd64"
		}
		if !explicit["provider.sourceImage"] {
			cfg.Provider.SourceImage = cfg.Image.OutputImage
		}
		if !explicit["runner.labels"] {
			if sourceType == ImageSourceRootFSTar {
				cfg.Runner.Labels = []string{"self-hosted", "linux", "X64", "epar-wsl-ubuntu-24.04-base"}
			} else {
				cfg.Runner.Labels = []string{"self-hosted", "linux", "X64", "epar-wsl-catthehacker-ubuntu"}
			}
		}
		if !explicit["pool.namePrefix"] && !explicit["pool.vmPrefix"] {
			cfg.Pool.NamePrefix = "epar-wsl"
		}
	case "docker-container":
		if !explicit["image.sourceType"] {
			cfg.Image.SourceType = ImageSourceDockerImage
		}
		if !explicit["image.sourceImage"] {
			cfg.Image.SourceImage = "ghcr.io/catthehacker/ubuntu:full-latest"
		}
		if !explicit["image.outputImage"] {
			cfg.Image.OutputImage = "epar-docker-container-catthehacker-ubuntu"
		}
		if !explicit["provider.sourceImage"] {
			cfg.Provider.SourceImage = cfg.Image.OutputImage
		}
		if !explicit["runner.labels"] {
			cfg.Runner.Labels = []string{"self-hosted", "linux", "epar-docker-container-catthehacker-ubuntu"}
		}
		if !explicit["pool.namePrefix"] && !explicit["pool.vmPrefix"] {
			cfg.Pool.NamePrefix = "epar-docker-container"
		}
	case "docker-sandboxes":
		if !explicit["provider.sourceImage"] {
			cfg.Provider.SourceImage = ""
		}
		if !explicit["provider.platform"] {
			cfg.Provider.Platform = "linux/amd64"
		}
		if !explicit["image.sourceType"] {
			cfg.Image.SourceType = ImageSourceDockerImage
		}
		if !explicit["image.sourceImage"] {
			cfg.Image.SourceImage = "ghcr.io/catthehacker/ubuntu:full-latest"
		}
		if !explicit["image.sourcePlatform"] {
			cfg.Image.SourcePlatform = cfg.Provider.Platform
		}
		if !explicit["image.outputImage"] {
			cfg.Image.OutputImage = ""
		}
		if !explicit["runner.labels"] {
			architecture := "X64"
			if cfg.Provider.Platform == "linux/arm64" {
				architecture = "ARM64"
			}
			cfg.Runner.Labels = []string{"self-hosted", "linux", architecture, "epar-docker-sandboxes"}
		}
		if !explicit["pool.namePrefix"] && !explicit["pool.vmPrefix"] {
			cfg.Pool.NamePrefix = "epar-docker-sandboxes"
		}
	}
}

var osHostname = os.Hostname

func HostName() (string, error) {
	if hostname := strings.TrimSpace(os.Getenv(HostNameEnv)); hostname != "" {
		return hostname, nil
	}
	return osHostname()
}

func applyRunnerHostLabel(cfg *Config) {
	if !cfg.Runner.IncludeHostLabel {
		return
	}
	hostname, err := HostName()
	if err != nil {
		return
	}
	hostLabel := HostLabel(hostname)
	if hostLabel == "" {
		return
	}
	for _, label := range cfg.Runner.Labels {
		if strings.EqualFold(label, hostLabel) {
			return
		}
	}
	cfg.Runner.Labels = append(cfg.Runner.Labels, hostLabel)
}

func HostLabel(hostname string) string {
	const prefix = "epar-host-"
	sanitized := sanitizeLabelPart(hostname)
	if sanitized == "" {
		return ""
	}
	maxPartLength := MaxRunnerLabelLength - len(prefix)
	if len(sanitized) > maxPartLength {
		sanitized = strings.Trim(sanitized[:maxPartLength], "-")
	}
	if sanitized == "" {
		return ""
	}
	return prefix + sanitized
}

func SanitizeNamePart(value string) string {
	value = sanitizeLabelPart(value)
	return strings.TrimFunc(value, func(r rune) bool {
		return !isASCIILetterOrDigit(r)
	})
}

func sanitizeLabelPart(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var b strings.Builder
	lastDash := false
	for _, r := range value {
		valid := (r >= 'a' && r <= 'z') ||
			(r >= '0' && r <= '9') ||
			r == '.' ||
			r == '_' ||
			r == '-'
		if valid {
			b.WriteRune(r)
			lastDash = r == '-'
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

func isASCIILetterOrDigit(r rune) bool {
	return (r >= 'a' && r <= 'z') ||
		(r >= 'A' && r <= 'Z') ||
		(r >= '0' && r <= '9')
}

func looksLikeRootFSTar(path string) bool {
	path = strings.ToLower(strings.TrimSpace(path))
	return strings.HasSuffix(path, ".tar") ||
		strings.HasSuffix(path, ".tar.gz") ||
		strings.HasSuffix(path, ".tgz")
}

func isListKey(section, key string) bool {
	switch section {
	case "image":
		return key == "customInstallScripts" || key == "trustedCaCertificatePaths" || key == "hostTrustScopes"
	case "runner":
		return key == "labels"
	case "docker":
		return key == "registryMirrors"
	case "dockerSandboxes":
		return key == "additionalAllow" || key == "additionalDeny"
	case "logging":
		return key == "managerSinks" || key == "transcriptSinks"
	default:
		return false
	}
}

func setListValue(cfg *Config, section, key string, values []string) error {
	switch section {
	case "image":
		switch key {
		case "customInstallScripts":
			cfg.Image.CustomInstallScripts = values
			return nil
		case "trustedCaCertificatePaths":
			cfg.Image.TrustedCACertificatePaths = values
			return nil
		case "hostTrustScopes":
			cfg.Image.HostTrustScopes = values
			return nil
		}
	case "runner":
		if key == "labels" {
			cfg.Runner.Labels = values
			return nil
		}
	case "docker":
		if key == "registryMirrors" {
			cfg.Docker.RegistryMirrors = values
			return nil
		}
	case "dockerSandboxes":
		switch key {
		case "additionalAllow":
			cfg.DockerSandboxes.AdditionalAllow = values
			return nil
		case "additionalDeny":
			cfg.DockerSandboxes.AdditionalDeny = values
			return nil
		}
	case "logging":
		switch key {
		case "managerSinks":
			cfg.Logging.ManagerSinks = values
			return nil
		case "transcriptSinks":
			cfg.Logging.TranscriptSinks = values
			return nil
		}
	}
	return fmt.Errorf("unsupported list key %s.%s", section, key)
}

func appendListValue(cfg *Config, section, key, value string) error {
	item := trimQuotes(strings.TrimSpace(value))
	if item == "" {
		return fmt.Errorf("%s.%s must not contain empty list items", section, key)
	}
	switch section {
	case "image":
		switch key {
		case "customInstallScripts":
			cfg.Image.CustomInstallScripts = append(cfg.Image.CustomInstallScripts, item)
			return nil
		case "trustedCaCertificatePaths":
			cfg.Image.TrustedCACertificatePaths = append(cfg.Image.TrustedCACertificatePaths, item)
			return nil
		case "hostTrustScopes":
			cfg.Image.HostTrustScopes = append(cfg.Image.HostTrustScopes, item)
			return nil
		}
	case "runner":
		if key == "labels" {
			cfg.Runner.Labels = append(cfg.Runner.Labels, item)
			return nil
		}
	case "docker":
		if key == "registryMirrors" {
			cfg.Docker.RegistryMirrors = append(cfg.Docker.RegistryMirrors, item)
			return nil
		}
	case "dockerSandboxes":
		switch key {
		case "additionalAllow":
			cfg.DockerSandboxes.AdditionalAllow = append(cfg.DockerSandboxes.AdditionalAllow, item)
			return nil
		case "additionalDeny":
			cfg.DockerSandboxes.AdditionalDeny = append(cfg.DockerSandboxes.AdditionalDeny, item)
			return nil
		}
	case "logging":
		switch key {
		case "managerSinks":
			cfg.Logging.ManagerSinks = append(cfg.Logging.ManagerSinks, item)
			return nil
		case "transcriptSinks":
			cfg.Logging.TranscriptSinks = append(cfg.Logging.TranscriptSinks, item)
			return nil
		}
	}
	return fmt.Errorf("unsupported list key %s.%s", section, key)
}

func Validate(cfg Config) error {
	if err := ValidateRunnerGroupSecurity(cfg.Security.RunnerGroup); err != nil {
		return err
	}
	if err := ValidateLogging(cfg.Logging); err != nil {
		return err
	}
	if err := ValidateStorage(cfg.Storage); err != nil {
		return err
	}
	if cfg.Provider.Type == "" {
		return fmt.Errorf("provider.type is required")
	}
	switch cfg.Provider.Type {
	case "tart", "wsl", "docker-container", "docker-sandboxes":
	case "docker-socket":
		return fmt.Errorf("provider.type docker-socket is intentionally unsupported; use provider.type=docker-container for a private Docker daemon")
	default:
		return fmt.Errorf("unsupported provider.type %q", cfg.Provider.Type)
	}
	if cfg.Provider.Type != "docker-sandboxes" && cfg.Provider.SourceImage == "" {
		return fmt.Errorf("provider.sourceImage is required")
	}
	if cfg.Provider.Type == "docker-sandboxes" {
		if cfg.Provider.SourceImage != "" {
			return fmt.Errorf("provider.sourceImage is not supported with provider.type=docker-sandboxes; use image.sourceImage")
		}
		if cfg.Provider.Platform != "linux/amd64" && cfg.Provider.Platform != "linux/arm64" {
			return fmt.Errorf("provider.platform must be linux/amd64 or linux/arm64 with provider.type=docker-sandboxes")
		}
		if cfg.Image.SourceType != "" && cfg.Image.SourceType != ImageSourceDockerImage {
			return fmt.Errorf("image.sourceType must be docker-image with provider.type=docker-sandboxes")
		}
		if cfg.Image.SourceType != "" && !validDockerSandboxesSourceImage(cfg.Image.SourceImage) {
			return fmt.Errorf("image.sourceImage with provider.type=docker-sandboxes must be ghcr.io/catthehacker/ubuntu:full-latest or ghcr.io/catthehacker/ubuntu:act-latest; specialized and custom tags do not satisfy the private dockerd template contract")
		}
		if cfg.Image.SourcePlatform != "" && cfg.Image.SourcePlatform != cfg.Provider.Platform {
			return fmt.Errorf("image.sourcePlatform must match provider.platform with provider.type=docker-sandboxes")
		}
		if !cfg.Runner.Ephemeral {
			return fmt.Errorf("runner.ephemeral must be true with provider.type=docker-sandboxes")
		}
		if cfg.Security.RunnerGroup.Enforcement != RunnerGroupEnforcementEnforce {
			return fmt.Errorf("security.runnerGroup.enforcement must be enforce with provider.type=docker-sandboxes")
		}
		if err := ValidateDockerSandboxes(cfg.DockerSandboxes); err != nil {
			return err
		}
	}
	if cfg.Provider.RosettaTag != "" {
		if cfg.Provider.Type != "tart" {
			return fmt.Errorf("provider.rosettaTag is only supported with provider.type=tart")
		}
		if err := ValidateRosettaTag(cfg.Provider.RosettaTag); err != nil {
			return err
		}
	}
	if cfg.Provider.Platform != "" {
		if cfg.Provider.Type != "docker-container" && cfg.Provider.Type != "docker-sandboxes" {
			return fmt.Errorf("provider.platform is only supported with provider.type=docker-container or docker-sandboxes")
		}
		if err := ValidateDockerPlatform(cfg.Provider.Platform); err != nil {
			return err
		}
	}
	for _, script := range cfg.Image.CustomInstallScripts {
		if strings.TrimSpace(script) == "" {
			return fmt.Errorf("image.customInstallScripts must not contain empty paths")
		}
	}
	for _, path := range cfg.Image.TrustedCACertificatePaths {
		if strings.TrimSpace(path) == "" {
			return fmt.Errorf("image.trustedCaCertificatePaths must not contain empty paths")
		}
	}
	if err := ValidateHostTrust(cfg.Image, cfg.Provider, cfg.Runner); err != nil {
		return err
	}
	if err := ValidateImageUpdatePolicy(cfg.Image); err != nil {
		return err
	}
	switch cfg.Image.SourceType {
	case "", ImageSourceDockerImage, ImageSourceRootFSTar:
	default:
		return fmt.Errorf("unsupported image.sourceType %q", cfg.Image.SourceType)
	}
	if cfg.Image.SourcePlatform != "" {
		if err := ValidateDockerPlatform(cfg.Image.SourcePlatform); err != nil {
			return fmt.Errorf("invalid image.sourcePlatform: %w", err)
		}
		if cfg.Image.SourceType == ImageSourceRootFSTar {
			return fmt.Errorf("image.sourcePlatform is only supported with image.sourceType=docker-image")
		}
	}
	if cfg.Pool.Instances < 1 {
		return fmt.Errorf("pool.instances must be 1 or greater")
	}
	if cfg.Pool.ReplacementRetryInitialSeconds <= 0 {
		return fmt.Errorf("pool.replacementRetryInitialSeconds must be greater than zero")
	}
	if cfg.Pool.ReplacementRetryMaxSeconds < cfg.Pool.ReplacementRetryInitialSeconds {
		return fmt.Errorf("pool.replacementRetryMaxSeconds must be greater than or equal to pool.replacementRetryInitialSeconds")
	}
	if math.IsNaN(cfg.Pool.ReplacementRetryMultiplier) || math.IsInf(cfg.Pool.ReplacementRetryMultiplier, 0) || cfg.Pool.ReplacementRetryMultiplier < 1 {
		return fmt.Errorf("pool.replacementRetryMultiplier must be 1 or greater")
	}
	if cfg.Pool.ReplacementRetryJitterPercent < 0 || cfg.Pool.ReplacementRetryJitterPercent > 100 {
		return fmt.Errorf("pool.replacementRetryJitterPercent must be between 0 and 100")
	}
	if err := ValidatePrefix(cfg.Pool.NamePrefix); err != nil {
		return err
	}
	if cfg.Provider.Type == "docker-sandboxes" {
		for _, r := range cfg.Pool.NamePrefix {
			if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '.') {
				return fmt.Errorf("pool.namePrefix for docker-sandboxes must contain only lowercase letters, digits, hyphens, and periods")
			}
		}
	}
	if len(cfg.Runner.Labels) == 0 {
		return fmt.Errorf("runner.labels must not be empty")
	}
	for _, label := range cfg.Runner.Labels {
		if err := ValidateRunnerLabel(label); err != nil {
			return err
		}
	}
	for _, mirror := range cfg.Docker.RegistryMirrors {
		if err := ValidateDockerRegistryMirror(mirror); err != nil {
			return err
		}
	}
	if err := ValidateDockerProxyURL("httpProxy", cfg.Docker.HTTPProxy); err != nil {
		return err
	}
	if err := ValidateDockerProxyURL("httpsProxy", cfg.Docker.HTTPSProxy); err != nil {
		return err
	}
	if err := ValidateDockerNoProxy(cfg.Docker.NoProxy); err != nil {
		return err
	}
	return nil
}

// ValidateImageUpdatePolicy keeps remote freshness scheduling predictable while
// allowing manual mode to retain an otherwise valid update time for later use.
func ValidateImageUpdatePolicy(image ImageConfig) error {
	switch image.UpdateFrequency {
	case ImageUpdateFrequencyDaily, ImageUpdateFrequencyWeekly, ImageUpdateFrequencyBiweekly, ImageUpdateFrequencyMonthly, ImageUpdateFrequencyManual:
	default:
		return fmt.Errorf("unsupported image.updateFrequency %q; supported values are daily, weekly, biweekly, monthly, and manual", image.UpdateFrequency)
	}
	if image.UpdateFrequency == ImageUpdateFrequencyManual {
		return nil
	}
	if _, err := time.Parse("15:04", image.UpdateTime); err != nil {
		return fmt.Errorf("invalid image.updateTime %q; use 24-hour HH:MM local time", image.UpdateTime)
	}
	return nil
}

// ValidateStorage rejects policies that would silently disable capacity or
// leave a supposedly bounded cache without a usable limit.
func ValidateStorage(storage StorageConfig) error {
	for key, value := range map[string]string{
		"minimumFree":     storage.MinimumFree,
		"buildCacheLimit": storage.BuildCacheLimit,
		"goCacheLimit":    storage.GoCacheLimit,
	} {
		if _, err := ParseByteSize(value); err != nil {
			return fmt.Errorf("invalid storage.%s: %w", key, err)
		}
	}
	grace, err := time.ParseDuration(storage.GracePeriod)
	if err != nil {
		return fmt.Errorf("invalid storage.gracePeriod: %w", err)
	}
	if grace <= 0 {
		return fmt.Errorf("storage.gracePeriod must be greater than zero")
	}
	if storage.KeepPrevious < 0 {
		return fmt.Errorf("storage.keepPrevious must be zero or greater")
	}
	switch storage.AutomaticHousekeeping {
	case StorageHousekeepingConservative, StorageHousekeepingDisabled:
	default:
		return fmt.Errorf("unsupported storage.automaticHousekeeping %q; supported values are conservative and disabled", storage.AutomaticHousekeeping)
	}
	return nil
}

// ValidateDockerSandboxes validates provider settings independently so
// callers constructing Config values programmatically receive the same checks as
// YAML-loaded configuration.
func ValidateDockerSandboxes(sandboxes DockerSandboxesConfig) error {
	if err := validateSHA256Fingerprint("dockerSandboxes.policyGeneration", sandboxes.PolicyGeneration); err != nil {
		return err
	}
	if sandboxes.NetworkBaseline != DockerSandboxesNetworkBaselineOpen && sandboxes.NetworkBaseline != DockerSandboxesNetworkBaselineBalanced {
		return fmt.Errorf("unsupported dockerSandboxes.networkBaseline %q; supported values are open and balanced", sandboxes.NetworkBaseline)
	}
	if err := validateDockerSandboxHostnameList("additionalAllow", sandboxes.AdditionalAllow); err != nil {
		return err
	}
	if err := validateDockerSandboxHostnameList("additionalDeny", sandboxes.AdditionalDeny); err != nil {
		return err
	}
	allowSet := make(map[string]struct{}, len(sandboxes.AdditionalAllow))
	for _, resource := range sandboxes.AdditionalAllow {
		allowSet[resource] = struct{}{}
	}
	if sandboxes.NetworkBaseline == DockerSandboxesNetworkBaselineOpen {
		for _, resource := range DockerSandboxesOpenDefaultDenyResources() {
			if _, exists := allowSet[resource]; exists {
				return fmt.Errorf("dockerSandboxes.additionalAllow must not override the Open host-boundary deny for %q", resource)
			}
		}
	}
	for _, resource := range sandboxes.AdditionalDeny {
		if _, exists := allowSet[resource]; exists {
			return fmt.Errorf("dockerSandboxes.additionalAllow and dockerSandboxes.additionalDeny must not both contain %q", resource)
		}
	}
	if err := validateDockerSandboxesStagingRoot(sandboxes.StagingRoot); err != nil {
		return err
	}
	if sandboxes.CPUs <= 0 {
		return fmt.Errorf("dockerSandboxes.cpus must be greater than zero")
	}
	parsedSizes := make(map[string]int64, 2)
	for key, value := range map[string]string{
		"memory":     sandboxes.Memory,
		"dockerDisk": sandboxes.DockerDisk,
	} {
		parsed, err := ParseByteSize(value)
		if err != nil {
			return fmt.Errorf("invalid dockerSandboxes.%s: %w", key, err)
		}
		parsedSizes[key] = parsed
	}
	if sandboxes.RootDisk != DockerSandboxesAutomaticRootDisk {
		rootDisk, err := ParseByteSize(sandboxes.RootDisk)
		if err != nil {
			return fmt.Errorf("invalid dockerSandboxes.rootDisk: %w", err)
		}
		if rootDisk < DockerSandboxesMinimumRootDiskBytes {
			return fmt.Errorf("dockerSandboxes.rootDisk must be auto or at least 20GiB")
		}
	}
	if parsedSizes["dockerDisk"] < DockerSandboxesMinimumDockerDiskBytes {
		return fmt.Errorf("dockerSandboxes.dockerDisk must be at least 1GiB")
	}
	if sandboxes.MaxConcurrentCreates <= 0 {
		return fmt.Errorf("dockerSandboxes.maxConcurrentCreates must be greater than zero")
	}
	return nil
}

func validDockerSandboxesSourceImage(value string) bool {
	switch value {
	case "ghcr.io/catthehacker/ubuntu:full-latest", "ghcr.io/catthehacker/ubuntu:act-latest":
		return true
	default:
		return false
	}
}

func validateDockerSandboxesStagingRoot(value string) error {
	if value == "" || value != strings.TrimSpace(value) || strings.ContainsAny(value, "\x00\r\n:") {
		return fmt.Errorf("dockerSandboxes.stagingRoot must be a canonical project-relative path under .local")
	}
	normalizedInput := strings.ReplaceAll(value, `\`, "/")
	native := filepath.FromSlash(normalizedInput)
	if filepath.IsAbs(native) || filepath.VolumeName(native) != "" {
		return fmt.Errorf("dockerSandboxes.stagingRoot must not select an absolute host path")
	}
	clean := filepath.ToSlash(filepath.Clean(native))
	if clean != normalizedInput || clean == ".local" || !strings.HasPrefix(clean, ".local/") {
		return fmt.Errorf("dockerSandboxes.stagingRoot must be a canonical project-relative path under .local")
	}
	for _, reserved := range []string{".local/bin", ".local/state"} {
		if clean == reserved || strings.HasPrefix(clean, reserved+"/") {
			return fmt.Errorf("dockerSandboxes.stagingRoot must not overlap reserved EPAR path %s", reserved)
		}
	}
	return nil
}

func validateSHA256Fingerprint(key, value string) error {
	const prefix = "sha256:"
	if len(value) != len(prefix)+64 || !strings.HasPrefix(value, prefix) {
		return fmt.Errorf("%s must be a lowercase sha256:<64-hex> fingerprint", key)
	}
	for _, r := range value[len(prefix):] {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return fmt.Errorf("%s must be a lowercase sha256:<64-hex> fingerprint", key)
		}
	}
	return nil
}

func validateDockerSandboxHostnameList(key string, resources []string) error {
	seen := make(map[string]struct{}, len(resources))
	for _, resource := range resources {
		if err := ValidateDockerSandboxHostname(resource); err != nil {
			return fmt.Errorf("invalid dockerSandboxes.%s value %q: %w", key, resource, err)
		}
		if _, exists := seen[resource]; exists {
			return fmt.Errorf("dockerSandboxes.%s must not contain duplicate value %q", key, resource)
		}
		seen[resource] = struct{}{}
	}
	return nil
}

// ValidateDockerSandboxHostname accepts an exact DNS hostname or a wildcard
// constrained to the left-most label (for example, *.githubusercontent.com),
// with an optional numeric port.
func ValidateDockerSandboxHostname(resource string) error {
	if resource == "" || resource != strings.TrimSpace(resource) || strings.ContainsAny(resource, "\\/@?#[]") || strings.ContainsAny(resource, "\t\r\n") {
		return fmt.Errorf("must be an exact hostname or *.domain with an optional port")
	}
	host := resource
	if strings.Contains(resource, ":") {
		parsedHost, port, err := net.SplitHostPort(resource)
		if err != nil || parsedHost == "" || port == "" {
			return fmt.Errorf("must be an exact hostname or *.domain with an optional port")
		}
		portNumber, err := strconv.Atoi(port)
		if err != nil || portNumber < 1 || portNumber > 65535 {
			return fmt.Errorf("port must be between 1 and 65535")
		}
		host = parsedHost
	}
	if strings.HasPrefix(host, "*.") {
		host = strings.TrimPrefix(host, "*.")
		if !strings.Contains(host, ".") {
			return fmt.Errorf("wildcards must be followed by a domain")
		}
	} else if strings.Contains(host, "*") {
		return fmt.Errorf("wildcards are only supported as the left-most label")
	}
	if net.ParseIP(host) != nil || len(host) > 253 || host == "" {
		return fmt.Errorf("must be an exact hostname or *.domain with an optional port")
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return fmt.Errorf("must be an exact hostname or *.domain with an optional port")
		}
		for _, r := range label {
			if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-') {
				return fmt.Errorf("must be an exact hostname or *.domain with an optional port")
			}
		}
	}
	return nil
}

// ParseByteSize parses a positive byte count with an explicit binary unit. The
// original configuration string is retained because the sandbox command surface
// consumes these values verbatim.
func ParseByteSize(value string) (int64, error) {
	if value == "" || value != strings.TrimSpace(value) {
		return 0, fmt.Errorf("must be a positive byte size such as 4GiB")
	}
	units := []struct {
		suffix     string
		multiplier int64
	}{
		{suffix: "TiB", multiplier: 1 << 40},
		{suffix: "GiB", multiplier: 1 << 30},
		{suffix: "MiB", multiplier: 1 << 20},
		{suffix: "KiB", multiplier: 1 << 10},
		{suffix: "B", multiplier: 1},
	}
	for _, unit := range units {
		if !strings.HasSuffix(value, unit.suffix) {
			continue
		}
		number := strings.TrimSuffix(value, unit.suffix)
		if number == "" {
			break
		}
		parsed, err := strconv.ParseInt(number, 10, 64)
		if err != nil || parsed <= 0 || parsed > math.MaxInt64/unit.multiplier {
			return 0, fmt.Errorf("must be a positive byte size such as 4GiB")
		}
		return parsed * unit.multiplier, nil
	}
	return 0, fmt.Errorf("must be a positive byte size such as 4GiB")
}

// EffectiveMinimumFreeBytes returns the single provider-neutral free-space
// reserve used by every storage-consuming provider operation.
func EffectiveMinimumFreeBytes(cfg Config) (uint64, error) {
	value := cfg.Storage.MinimumFree
	if value == "" {
		value = Default().Storage.MinimumFree
	}
	minimum, err := ParseByteSize(value)
	if err != nil {
		return 0, fmt.Errorf("parse storage minimum free reserve: %w", err)
	}
	return uint64(minimum), nil
}

func ValidateRunnerGroupSecurity(policy RunnerGroupSecurityConfig) error {
	switch policy.Enforcement {
	case RunnerGroupEnforcementEnforce, RunnerGroupEnforcementWarn:
	default:
		return fmt.Errorf("unsupported security.runnerGroup.enforcement %q; supported values are enforce and warn", policy.Enforcement)
	}
	switch policy.RequiredRepositoryAccess {
	case RunnerGroupRepositoryAccessSelected, RunnerGroupRepositoryAccessPrivate, RunnerGroupRepositoryAccessAll:
	default:
		return fmt.Errorf("unsupported security.runnerGroup.requiredRepositoryAccess %q; supported values are selected, private, and all", policy.RequiredRepositoryAccess)
	}
	return nil
}

func ValidateLogging(logging LoggingConfig) error {
	if strings.TrimSpace(logging.Directory) == "" {
		return fmt.Errorf("logging.directory is required")
	}
	if err := validateLoggingSinks("managerSinks", logging.ManagerSinks); err != nil {
		return err
	}
	if err := validateLoggingSinks("transcriptSinks", logging.TranscriptSinks); err != nil {
		return err
	}
	if err := validateLoggingFormat("managerConsoleFormat", logging.ManagerConsoleFormat); err != nil {
		return err
	}
	if err := validateLoggingFormat("managerFileFormat", logging.ManagerFileFormat); err != nil {
		return err
	}
	if err := validateLoggingFormat("transcriptConsoleFormat", logging.TranscriptConsoleFormat); err != nil {
		return err
	}
	if err := validateConsoleTextFormat("managerConsoleTextFormat", logging.ManagerConsoleTextFormat, logging.ManagerConsoleFormat, []string{"time", "level", "message", "attributes"}); err != nil {
		return err
	}
	if err := validateConsoleTextFormat("transcriptConsoleTextFormat", logging.TranscriptConsoleTextFormat, logging.TranscriptConsoleFormat, []string{"time", "instance", "component", "stream", "message", "session", "category", "provider", "attributes"}); err != nil {
		return err
	}
	if logging.MaxFileSizeMiB < 1 {
		return fmt.Errorf("logging.maxFileSizeMiB must be 1 or greater")
	}
	if logging.MaxBackups < 1 {
		return fmt.Errorf("logging.maxBackups must be 1 or greater")
	}
	if logging.RetentionMaxTotalMiB < 1 {
		return fmt.Errorf("logging.retentionMaxTotalMiB must be 1 or greater")
	}
	for key, value := range map[string]int{
		"managerMaxAgeDays":        logging.ManagerMaxAgeDays,
		"instanceMaxAgeDays":       logging.InstanceMaxAgeDays,
		"buildMaxAgeDays":          logging.BuildMaxAgeDays,
		"errorMaxAgeDays":          logging.ErrorMaxAgeDays,
		"benchmarkMaxAgeDays":      logging.BenchmarkMaxAgeDays,
		"retentionIntervalMinutes": logging.RetentionIntervalMinutes,
	} {
		if value < 1 {
			return fmt.Errorf("logging.%s must be 1 or greater", key)
		}
	}
	return nil
}

func validateLoggingSinks(key string, sinks []string) error {
	if len(sinks) == 0 {
		return fmt.Errorf("logging.%s must not be empty", key)
	}
	seen := make(map[string]struct{}, len(sinks))
	for _, sink := range sinks {
		if sink != "console" && sink != "file" {
			return fmt.Errorf("unsupported logging.%s value %q", key, sink)
		}
		if _, exists := seen[sink]; exists {
			return fmt.Errorf("logging.%s must not contain duplicate sink %q", key, sink)
		}
		seen[sink] = struct{}{}
	}
	return nil
}

func validateLoggingFormat(key, format string) error {
	switch format {
	case "text", "json":
		return nil
	default:
		return fmt.Errorf("unsupported logging.%s %q; supported values are text and json", key, format)
	}
}

func validateConsoleTextFormat(key, template, outputFormat string, allowed []string) error {
	if template == "" {
		return nil
	}
	if outputFormat != "text" {
		return fmt.Errorf("logging.%s is supported only when the corresponding console format is text", key)
	}
	if strings.ContainsAny(template, "\r\n") {
		return fmt.Errorf("logging.%s must be a single line", key)
	}
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, placeholder := range allowed {
		allowedSet[placeholder] = struct{}{}
	}
	foundMessage := false
	remaining := template
	for {
		open := strings.IndexByte(remaining, '{')
		if open < 0 {
			if strings.ContainsRune(remaining, '}') {
				return fmt.Errorf("logging.%s contains an unmatched closing brace", key)
			}
			break
		}
		if strings.ContainsRune(remaining[:open], '}') {
			return fmt.Errorf("logging.%s contains an unmatched closing brace", key)
		}
		closeOffset := strings.IndexByte(remaining[open+1:], '}')
		if closeOffset < 0 {
			return fmt.Errorf("logging.%s contains an unmatched opening brace", key)
		}
		placeholder := remaining[open+1 : open+1+closeOffset]
		if _, ok := allowedSet[placeholder]; !ok {
			return fmt.Errorf("logging.%s contains unsupported placeholder {%s}", key, placeholder)
		}
		foundMessage = foundMessage || placeholder == "message"
		remaining = remaining[open+closeOffset+2:]
	}
	if !foundMessage {
		return fmt.Errorf("logging.%s must contain {message}", key)
	}
	return nil
}

// ValidateHostTrust applies the provider-neutral ephemeral-runner trust
// contract. The common pool installs and validates the resolved trust snapshot
// in an unregistered guest before requesting a registration token.
func ValidateHostTrust(image ImageConfig, _ ProviderConfig, runner RunnerConfig) error {
	switch image.HostTrustMode {
	case "", HostTrustModeDisabled:
		return nil
	case HostTrustModeOverlay:
		if !runner.Ephemeral {
			return fmt.Errorf("image.hostTrustMode %q requires runner.ephemeral=true", HostTrustModeOverlay)
		}
	default:
		return fmt.Errorf("unsupported image.hostTrustMode %q", image.HostTrustMode)
	}

	if image.HostTrustMode != HostTrustModeOverlay {
		return nil
	}
	if len(image.HostTrustScopes) == 0 {
		return fmt.Errorf("image.hostTrustScopes must not be empty when image.hostTrustMode is %q", HostTrustModeOverlay)
	}
	seen := make(map[string]struct{}, len(image.HostTrustScopes))
	for _, scope := range image.HostTrustScopes {
		scope = strings.ToLower(strings.TrimSpace(scope))
		if scope == "" {
			return fmt.Errorf("image.hostTrustScopes must not contain empty scopes")
		}
		switch scope {
		case HostTrustScopeSystem, HostTrustScopeUser:
		default:
			return fmt.Errorf("unsupported image.hostTrustScopes value %q", scope)
		}
		if _, exists := seen[scope]; exists {
			return fmt.Errorf("image.hostTrustScopes must not contain duplicate scope %q", scope)
		}
		seen[scope] = struct{}{}
	}
	return nil
}

func ValidateRunnerLabel(label string) error {
	if strings.TrimSpace(label) == "" {
		return fmt.Errorf("runner.labels must not contain empty labels")
	}
	if len(label) > MaxRunnerLabelLength {
		return fmt.Errorf("runner label %q exceeds %d characters", label, MaxRunnerLabelLength)
	}
	return nil
}

func ValidateDockerRegistryMirror(mirror string) error {
	if strings.TrimSpace(mirror) != mirror || mirror == "" {
		return fmt.Errorf("docker.registryMirrors must contain non-empty mirror URLs without surrounding whitespace")
	}
	if strings.ContainsAny(mirror, " \t\r\n") {
		return fmt.Errorf("docker.registryMirrors URL %q must not contain whitespace", mirror)
	}
	parsed, err := url.Parse(mirror)
	if err != nil {
		return fmt.Errorf("docker.registryMirrors URL %q is invalid: %w", mirror, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("docker.registryMirrors URL %q must use http or https", mirror)
	}
	if parsed.Host == "" {
		return fmt.Errorf("docker.registryMirrors URL %q must include a host", mirror)
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("docker.registryMirrors URL %q must not include credentials, query, or fragment", mirror)
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return fmt.Errorf("docker.registryMirrors URL %q must point at the registry root", mirror)
	}
	return nil
}

func DockerRegistryMirrorsNeedHostGateway(mirrors []string) bool {
	for _, mirror := range mirrors {
		parsed, err := url.Parse(mirror)
		if err != nil {
			continue
		}
		if strings.EqualFold(parsed.Hostname(), "host.docker.internal") {
			return true
		}
	}
	return false
}

func DockerConfigNeedsHostGateway(cfg DockerConfig) bool {
	if DockerRegistryMirrorsNeedHostGateway(cfg.RegistryMirrors) {
		return true
	}
	for _, proxyURL := range []string{cfg.HTTPProxy, cfg.HTTPSProxy} {
		parsed, err := url.Parse(proxyURL)
		if err == nil && strings.EqualFold(parsed.Hostname(), "host.docker.internal") {
			return true
		}
	}
	return false
}

func ValidateDockerProxyURL(key, value string) error {
	if value == "" {
		return nil
	}
	if strings.TrimSpace(value) != value || strings.ContainsAny(value, " \t\r\n") {
		return fmt.Errorf("docker.%s URL must not contain whitespace", key)
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return fmt.Errorf("docker.%s URL %q is invalid: %w", key, value, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("docker.%s URL %q must use http or https", key, value)
	}
	if parsed.Host == "" {
		return fmt.Errorf("docker.%s URL %q must include a host", key, value)
	}
	if parsed.User != nil {
		return fmt.Errorf("docker.%s URL %q must not include credentials", key, value)
	}
	if (parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("docker.%s URL %q must point at the proxy root without query or fragment", key, value)
	}
	return nil
}

func ValidateDockerNoProxy(value string) error {
	if value == "" {
		return nil
	}
	if len(value) > 4096 {
		return fmt.Errorf("docker.noProxy must be 4096 characters or fewer")
	}
	if strings.TrimSpace(value) != value || strings.ContainsAny(value, " \t\r\n") {
		return fmt.Errorf("docker.noProxy must not contain whitespace")
	}
	for _, item := range strings.Split(value, ",") {
		if item == "" {
			return fmt.Errorf("docker.noProxy must not contain empty comma-separated entries")
		}
		if strings.Contains(item, "://") || strings.Contains(item, "@") {
			return fmt.Errorf("docker.noProxy entry %q must be a host, domain, IP address, CIDR, or *", item)
		}
		if strings.Contains(item, "/") {
			if _, _, err := net.ParseCIDR(item); err != nil {
				return fmt.Errorf("docker.noProxy entry %q has an invalid CIDR", item)
			}
		}
	}
	return nil
}

func DockerSandboxesGuestPlatform(hostOS, hostArch string) (string, error) {
	if strings.TrimSpace(hostOS) == "" {
		return "", fmt.Errorf("docker-sandboxes controller host operating system is empty")
	}
	switch hostArch {
	case "amd64":
		return "linux/amd64", nil
	case "arm64":
		return "linux/arm64", nil
	default:
		return "", fmt.Errorf("docker-sandboxes has no EPAR template for controller architecture %s on %s", hostArch, hostOS)
	}
}

func ValidateDockerPlatform(platform string) error {
	if strings.TrimSpace(platform) != platform || platform == "" {
		return fmt.Errorf("provider.platform must be a non-empty Docker platform")
	}
	if len(platform) > 80 {
		return fmt.Errorf("provider.platform must be 80 characters or fewer")
	}
	for i, r := range platform {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' || r == '/'
		if !ok {
			return fmt.Errorf("provider.platform contains unsupported character %q", r)
		}
		if i == 0 && !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
			return fmt.Errorf("provider.platform must start with a letter or digit")
		}
	}
	return nil
}

func ValidateRosettaTag(tag string) error {
	if strings.TrimSpace(tag) != tag || tag == "" {
		return fmt.Errorf("provider.rosettaTag must be a non-empty simple Tart virtiofs tag")
	}
	if strings.ContainsAny(tag, `/\`) {
		return fmt.Errorf("provider.rosettaTag must not be path-like")
	}
	if len(tag) > 64 {
		return fmt.Errorf("provider.rosettaTag must be 64 characters or fewer")
	}
	for i, r := range tag {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.'
		if !ok {
			return fmt.Errorf("provider.rosettaTag contains unsupported character %q", r)
		}
		if i == 0 && !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
			return fmt.Errorf("provider.rosettaTag must start with a letter or digit")
		}
	}
	return nil
}

func ValidateGitHub(cfg Config) error {
	if cfg.GitHub.AppID == 0 {
		return fmt.Errorf("github.appId is required")
	}
	if cfg.GitHub.Organization == "" {
		return fmt.Errorf("github.organization is required")
	}
	if cfg.GitHub.PrivateKeyPath == "" {
		return fmt.Errorf("github.privateKeyPath is required")
	}
	return nil
}

func ValidatePrefix(prefix string) error {
	if len(prefix) < 2 || len(prefix) > 40 {
		return fmt.Errorf("pool.namePrefix must be 2-40 characters")
	}
	for i, r := range prefix {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.'
		if !ok {
			return fmt.Errorf("pool.namePrefix contains unsupported character %q", r)
		}
		if i == 0 && !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
			return fmt.Errorf("pool.namePrefix must start with a letter or digit")
		}
	}
	return nil
}

func ProjectPath(root, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(root, path)
}

func expandHome(path string) string {
	if path == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
	}
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, path[2:])
		}
	}
	return path
}

func leadingSpaces(s string) int {
	return len(s) - len(strings.TrimLeft(s, " "))
}

func stripComment(s string) string {
	inSingle := false
	inDouble := false
	for i, r := range s {
		switch r {
		case '\'':
			if !inDouble {
				inSingle = !inSingle
			}
		case '"':
			if !inSingle {
				inDouble = !inDouble
			}
		case '#':
			if !inSingle && !inDouble {
				return s[:i]
			}
		}
	}
	return s
}

func trimQuotes(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 {
		if s[0] == '"' && s[len(s)-1] == '"' {
			if value, err := strconv.Unquote(s); err == nil {
				return value
			}
		}
		if s[0] == '\'' && s[len(s)-1] == '\'' {
			return s[1 : len(s)-1]
		}
	}
	return s
}

func parseList(s string) []string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "[")
	s = strings.TrimSuffix(s, "]")
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		item := trimQuotes(strings.TrimSpace(part))
		if item != "" {
			out = append(out, item)
		}
	}
	return out
}
