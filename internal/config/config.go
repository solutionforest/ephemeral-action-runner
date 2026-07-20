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
)

type Config struct {
	GitHub   GitHubConfig
	Image    ImageConfig
	Pool     PoolConfig
	Logging  LoggingConfig
	Runner   RunnerConfig
	Provider ProviderConfig
	Docker   DockerConfig
	Timeouts TimeoutConfig
	warnings []string
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
)

type PoolConfig struct {
	Instances                      int
	NamePrefix                     string
	ReplacementRetryInitialSeconds int
	ReplacementRetryMaxSeconds     int
	ReplacementRetryMultiplier     float64
	ReplacementRetryJitterPercent  int
}

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
			SourceImage:   "ghcr.io/cirruslabs/ubuntu:latest",
			OutputImage:   "epar-ubuntu-24-arm64",
			UpstreamDir:   "third_party/runner-images",
			UpstreamLock:  "third_party/runner-images.lock",
			RunnerVersion: "latest",
			HostTrustMode: HostTrustModeDisabled,
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
		Provider: ProviderConfig{
			Type:        "tart",
			SourceImage: "epar-ubuntu-24-arm64",
			Network:     "default",
			InstallRoot: "work/wsl",
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
			continue
		}
		if indent == 0 {
			return cfg, fmt.Errorf("%s:%d: section %q must not have a scalar value", path, lineNo, key)
		}
		if section == "" {
			return cfg, fmt.Errorf("%s:%d: key %q must be under a section", path, lineNo, key)
		}
		if section == "pool" && key == "logDir" {
			legacyLogDir = trimQuotes(value)
			legacyLogDirLine = lineNo
			explicit["pool.logDir"] = true
			continue
		}
		if value == "" && isListKey(section, key) {
			if err := setListValue(&cfg, section, key, nil); err != nil {
				return cfg, fmt.Errorf("%s:%d: %w", path, lineNo, err)
			}
			explicit[section+"."+key] = true
			pendingList = &pendingListKey{section: section, key: key, indent: indent}
			continue
		}
		if err := apply(&cfg, section, key, value); err != nil {
			return cfg, fmt.Errorf("%s:%d: %w", path, lineNo, err)
		}
		explicit[section+"."+key] = true
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
	case "github", "image", "pool", "logging", "runner", "provider", "docker", "timeouts":
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
	case "docker-dind":
		if !explicit["image.sourceType"] {
			cfg.Image.SourceType = ImageSourceDockerImage
		}
		if !explicit["image.sourceImage"] {
			cfg.Image.SourceImage = "ghcr.io/catthehacker/ubuntu:full-latest"
		}
		if !explicit["image.outputImage"] {
			cfg.Image.OutputImage = "epar-docker-dind-catthehacker-ubuntu"
		}
		if !explicit["provider.sourceImage"] {
			cfg.Provider.SourceImage = cfg.Image.OutputImage
		}
		if !explicit["runner.labels"] {
			cfg.Runner.Labels = []string{"self-hosted", "linux", "epar-docker-dind-catthehacker-ubuntu"}
		}
		if !explicit["pool.namePrefix"] && !explicit["pool.vmPrefix"] {
			cfg.Pool.NamePrefix = "epar-dind"
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
	if err := ValidateLogging(cfg.Logging); err != nil {
		return err
	}
	if cfg.Provider.Type == "" {
		return fmt.Errorf("provider.type is required")
	}
	switch cfg.Provider.Type {
	case "tart", "wsl", "docker-dind":
	case "docker-socket":
		return fmt.Errorf("provider.type docker-socket is intentionally unsupported; use provider.type=docker-dind for a private Docker daemon")
	default:
		return fmt.Errorf("unsupported provider.type %q", cfg.Provider.Type)
	}
	if cfg.Provider.SourceImage == "" {
		return fmt.Errorf("provider.sourceImage is required")
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
		if cfg.Provider.Type != "docker-dind" {
			return fmt.Errorf("provider.platform is only supported with provider.type=docker-dind")
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

// ValidateHostTrust keeps host trust inheritance deliberately limited to the
// ephemeral Docker-in-Docker image path. Other providers do not have a
// portable, unambiguous host trust boundary.
func ValidateHostTrust(image ImageConfig, provider ProviderConfig, runner RunnerConfig) error {
	switch image.HostTrustMode {
	case "", HostTrustModeDisabled:
		return nil
	case HostTrustModeOverlay:
		if provider.Type != "docker-dind" {
			return fmt.Errorf("image.hostTrustMode %q is only supported with provider.type=docker-dind", HostTrustModeOverlay)
		}
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
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
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
