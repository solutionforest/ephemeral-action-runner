package config

import (
	"bufio"
	"fmt"
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
	Runner   RunnerConfig
	Provider ProviderConfig
	Docker   DockerConfig
	Timeouts TimeoutConfig
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
}

type PoolConfig struct {
	Instances  int
	NamePrefix string
	LogDir     string
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
		},
		Pool: PoolConfig{
			Instances:  1,
			NamePrefix: "epar",
			LogDir:     "work/logs",
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
			section = key
			continue
		}
		if section == "" {
			return cfg, fmt.Errorf("%s:%d: key %q must be under a section", path, lineNo, key)
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
		case "logDir":
			cfg.Pool.LogDir = value
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
		}
	default:
		return fmt.Errorf("unknown section %q", section)
	}
	return nil
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
		return key == "customInstallScripts" || key == "trustedCaCertificatePaths"
	case "runner":
		return key == "labels"
	case "docker":
		return key == "registryMirrors"
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
	}
	return fmt.Errorf("unsupported list key %s.%s", section, key)
}

func Validate(cfg Config) error {
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
