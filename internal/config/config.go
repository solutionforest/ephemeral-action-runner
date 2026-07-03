package config

import (
	"bufio"
	"fmt"
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
	SourceImage   string
	OutputImage   string
	UpstreamDir   string
	UpstreamLock  string
	RunnerVersion string
}

type PoolConfig struct {
	MinIdle      int
	MaxInstances int
	NamePrefix   string
	LogDir       string
}

type RunnerConfig struct {
	Labels    []string
	Ephemeral bool
}

type ProviderConfig struct {
	Type        string
	SourceImage string
	Network     string
}

type TimeoutConfig struct {
	BootSeconds         int
	GitHubOnlineSeconds int
	CommandSeconds      int
}

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
			MinIdle:      2,
			MaxInstances: 2,
			NamePrefix:   "epar",
			LogDir:       "work/logs",
		},
		Runner: RunnerConfig{
			Labels:    []string{"self-hosted", "linux", "ARM64", "epar-ubuntu-24.04-docker"},
			Ephemeral: true,
		},
		Provider: ProviderConfig{
			Type:        "tart",
			SourceImage: "epar-ubuntu-24-arm64",
			Network:     "default",
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
	for scanner.Scan() {
		lineNo++
		raw := strings.TrimRight(scanner.Text(), " \t")
		line := strings.TrimSpace(stripComment(raw))
		if line == "" {
			continue
		}
		indent := leadingSpaces(raw)
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
		if err := apply(&cfg, section, key, value); err != nil {
			return cfg, fmt.Errorf("%s:%d: %w", path, lineNo, err)
		}
	}
	if err := scanner.Err(); err != nil {
		return cfg, err
	}
	cfg.GitHub.PrivateKeyPath = expandHome(cfg.GitHub.PrivateKeyPath)
	return cfg, nil
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
		case "outputImage":
			cfg.Image.OutputImage = value
		case "upstreamDir":
			cfg.Image.UpstreamDir = value
		case "upstreamLock":
			cfg.Image.UpstreamLock = value
		case "runnerVersion":
			cfg.Image.RunnerVersion = value
		}
	case "pool":
		switch key {
		case "minIdle":
			v, err := strconv.Atoi(value)
			if err != nil {
				return fmt.Errorf("invalid pool.minIdle: %w", err)
			}
			cfg.Pool.MinIdle = v
		case "maxInstances":
			v, err := strconv.Atoi(value)
			if err != nil {
				return fmt.Errorf("invalid pool.maxInstances: %w", err)
			}
			cfg.Pool.MaxInstances = v
		case "namePrefix", "vmPrefix":
			cfg.Pool.NamePrefix = value
		case "logDir":
			cfg.Pool.LogDir = value
		}
	case "runner":
		switch key {
		case "labels":
			cfg.Runner.Labels = parseList(value)
		case "ephemeral":
			v, err := strconv.ParseBool(value)
			if err != nil {
				return fmt.Errorf("invalid runner.ephemeral: %w", err)
			}
			cfg.Runner.Ephemeral = v
		}
	case "provider":
		switch key {
		case "type":
			cfg.Provider.Type = value
		case "sourceImage":
			cfg.Provider.SourceImage = value
		case "network":
			cfg.Provider.Network = value
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

func Validate(cfg Config) error {
	if cfg.Provider.Type == "" {
		return fmt.Errorf("provider.type is required")
	}
	switch cfg.Provider.Type {
	case "tart", "wsl":
	default:
		return fmt.Errorf("unsupported provider.type %q", cfg.Provider.Type)
	}
	if cfg.Provider.SourceImage == "" {
		return fmt.Errorf("provider.sourceImage is required")
	}
	if cfg.Pool.MinIdle < 0 || cfg.Pool.MaxInstances < 1 || cfg.Pool.MinIdle > cfg.Pool.MaxInstances {
		return fmt.Errorf("pool sizing must satisfy 0 <= minIdle <= maxInstances")
	}
	if err := ValidatePrefix(cfg.Pool.NamePrefix); err != nil {
		return err
	}
	if len(cfg.Runner.Labels) == 0 {
		return fmt.Errorf("runner.labels must not be empty")
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
