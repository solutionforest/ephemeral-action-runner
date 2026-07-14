package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
	"github.com/solutionforest/ephemeral-action-runner/internal/hosttrust"
)

var dockerAvailable = func(ctx context.Context) error {
	return exec.CommandContext(ctx, "docker", "version", "--format", "{{.Server.Version}}").Run()
}

var initHostname = config.HostName

var initRandomRead = rand.Read

var initGOOS = runtime.GOOS

var initHostTrustOS = detectedInitHostTrustOS()

var initWSLStatus = wslStatus

var initResolveHostTrust = hosttrust.Resolve

func detectedInitHostTrustOS() string {
	if hostOS := strings.TrimSpace(os.Getenv("EPAR_CONTROLLER_HOST_OS")); hostOS != "" {
		return hostOS
	}
	return runtime.GOOS
}

type initOptions struct {
	ProjectRoot        string
	ConfigPath         string
	Force              bool
	SkipDockerCheck    bool
	SkipHostTrustCheck bool
	In                 io.Reader
	Out                io.Writer
}

func runInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	common := addCommonFlags(fs)
	force := fs.Bool("force", false, "overwrite an existing config file")
	skipDockerCheck := fs.Bool("skip-docker-check", false, "create the config without checking for Docker")
	skipHostTrustCheck := fs.Bool("skip-host-trust-check", false, "create the config without collecting host trust roots")
	if err := fs.Parse(args); err != nil {
		return err
	}

	projectRoot, err := filepath.Abs(*common.projectRoot)
	if err != nil {
		return err
	}
	configPath := *common.configPath
	if configPath == "" {
		configPath = filepath.Join(projectRoot, ".local", "config.yml")
	} else {
		configPath = config.ProjectPath(projectRoot, configPath)
	}

	return runInitWithOptions(initOptions{
		ProjectRoot:        projectRoot,
		ConfigPath:         configPath,
		Force:              *force,
		SkipDockerCheck:    *skipDockerCheck,
		SkipHostTrustCheck: *skipHostTrustCheck,
		In:                 os.Stdin,
		Out:                os.Stdout,
	})
}

func runInitWithOptions(opts initOptions) error {
	if opts.In == nil {
		opts.In = os.Stdin
	}
	if opts.Out == nil {
		opts.Out = os.Stdout
	}
	if opts.ConfigPath == "" {
		opts.ConfigPath = filepath.Join(opts.ProjectRoot, ".local", "config.yml")
	}
	if !opts.Force {
		if _, err := os.Stat(opts.ConfigPath); err == nil {
			return fmt.Errorf("config already exists at %s; use init --force to overwrite it", opts.ConfigPath)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if !opts.SkipDockerCheck {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := dockerAvailable(ctx); err != nil {
			return fmt.Errorf("Docker is required for the default Docker-DinD setup. Install Docker Desktop, Docker Engine, or a compatible Docker host, then rerun %s init. If you want WSL or another custom provider, create .local/config.yml from the examples instead", binaryName)
		}
	}

	fmt.Fprintln(opts.Out, "EPAR first-run setup")
	fmt.Fprintln(opts.Out, "")
	fmt.Fprintln(opts.Out, "This creates .local/config.yml for an EPAR runner.")
	fmt.Fprintln(opts.Out, "Before continuing, create a GitHub App with organization self-hosted runner read/write access.")
	fmt.Fprintln(opts.Out, "See README.md and docs/github-app.md for the GitHub App steps.")
	fmt.Fprintln(opts.Out, "")

	reader := bufio.NewReader(opts.In)
	appID, err := promptRequiredInt64(opts.Out, reader, "GitHub App ID")
	if err != nil {
		return err
	}
	organization, err := promptRequired(opts.Out, reader, "GitHub organization")
	if err != nil {
		return err
	}
	privateKeyPath, err := promptRequired(opts.Out, reader, "GitHub App private key path")
	if err != nil {
		return err
	}
	providerType := "docker-dind"
	if wsl2Available() {
		providerType, err = promptProviderType(opts.Out, reader)
		if err != nil {
			return err
		}
	}
	defaultPrefix, err := generatedPoolNamePrefix()
	if err != nil {
		return err
	}
	fmt.Fprintln(opts.Out, "")
	fmt.Fprintln(opts.Out, "Pool name prefix must be unique for this machine/config within the GitHub organization.")
	fmt.Fprintln(opts.Out, "EPAR cleanup deletes GitHub runner records matching this prefix.")
	poolNamePrefix, err := promptPoolNamePrefix(opts.Out, reader, defaultPrefix)
	if err != nil {
		return err
	}

	hostTrustMode := config.HostTrustModeDisabled
	hostTrustScopes := []string{config.HostTrustScopeSystem}
	if providerType == "docker-dind" {
		enabled, promptErr := promptYesNo(opts.Out, reader, "Inherit this host's trusted TLS roots into disposable runners?", true)
		if promptErr != nil {
			return promptErr
		}
		if enabled {
			hostTrustMode = config.HostTrustModeOverlay
			hostTrustScopes = hostTrustScopesForOS(initHostTrustOS)
			deferred := os.Getenv("EPAR_HOST_TRUST_INIT_DEFERRED") == "1"
			if !opts.SkipHostTrustCheck && !deferred {
				preflightCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				_, collectErr := initResolveHostTrust(preflightCtx, hosttrust.Options{
					Mode:             hostTrustMode,
					Scopes:           hostTrustScopes,
					ControllerHostOS: initHostTrustOS,
				})
				cancel()
				if collectErr != nil {
					return fmt.Errorf("collect host trusted TLS roots before writing config: %w", collectErr)
				}
			}
		}
	}

	content := defaultDockerDindConfig(appID, organization, privateKeyPath, poolNamePrefix, hostTrustMode, hostTrustScopes)
	if providerType == "wsl" {
		content = defaultWSLConfig(appID, organization, privateKeyPath, poolNamePrefix)
	}
	if err := os.MkdirAll(filepath.Dir(opts.ConfigPath), 0755); err != nil {
		return err
	}
	if err := os.WriteFile(opts.ConfigPath, []byte(content), 0600); err != nil {
		return err
	}

	fmt.Fprintf(opts.Out, `
Created %s

Next:
  %s start

Manual/advanced:
  %s image build --replace
  %s pool verify --instances 2 --register-only --cleanup
  %s pool up --instances 2
`, opts.ConfigPath, binaryName, binaryName, binaryName, binaryName)
	return nil
}

func promptRequired(out io.Writer, reader *bufio.Reader, label string) (string, error) {
	for {
		fmt.Fprintf(out, "%s: ", label)
		value, err := reader.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", err
		}
		value = strings.TrimSpace(value)
		if strings.ContainsAny(value, "\r\n") {
			return "", fmt.Errorf("%s must be one line", label)
		}
		if value != "" {
			return value, nil
		}
		if errors.Is(err, io.EOF) {
			return "", fmt.Errorf("%s is required", label)
		}
		fmt.Fprintf(out, "%s is required.\n", label)
	}
}

func promptRequiredInt64(out io.Writer, reader *bufio.Reader, label string) (int64, error) {
	for {
		value, err := promptRequired(out, reader, label)
		if err != nil {
			return 0, err
		}
		parsed, parseErr := strconv.ParseInt(value, 10, 64)
		if parseErr == nil && parsed > 0 {
			return parsed, nil
		}
		fmt.Fprintf(out, "%s must be a positive number.\n", label)
	}
}

func promptPoolNamePrefix(out io.Writer, reader *bufio.Reader, defaultValue string) (string, error) {
	for {
		value, hitEOF, err := promptDefault(out, reader, "Pool name prefix", defaultValue)
		if err != nil {
			return "", err
		}
		if err := config.ValidatePrefix(value); err != nil {
			fmt.Fprintf(out, "Pool name prefix is invalid: %v\n", err)
			if hitEOF {
				return "", err
			}
			continue
		}
		return value, nil
	}
}

func promptProviderType(out io.Writer, reader *bufio.Reader) (string, error) {
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Runner provider:")
	fmt.Fprintln(out, "  1. Docker-DinD (default)")
	fmt.Fprintln(out, "  2. WSL2")
	for {
		value, hitEOF, err := promptDefault(out, reader, "Runner provider", "1")
		if err != nil {
			return "", err
		}
		switch strings.ToLower(value) {
		case "1", "docker", "docker-dind":
			return "docker-dind", nil
		case "2", "wsl", "wsl2":
			return "wsl", nil
		default:
			fmt.Fprintln(out, "Runner provider must be 1 (Docker-DinD) or 2 (WSL2).")
			if hitEOF {
				return "", fmt.Errorf("invalid runner provider %q", value)
			}
		}
	}
}

func promptDefault(out io.Writer, reader *bufio.Reader, label string, defaultValue string) (string, bool, error) {
	fmt.Fprintf(out, "%s (press Enter to use %s): ", label, defaultValue)
	value, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", false, err
	}
	hitEOF := errors.Is(err, io.EOF)
	value = strings.TrimSpace(value)
	if strings.ContainsAny(value, "\r\n") {
		return "", hitEOF, fmt.Errorf("%s must be one line", label)
	}
	if value == "" {
		return defaultValue, hitEOF, nil
	}
	return value, hitEOF, nil
}

func promptYesNo(out io.Writer, reader *bufio.Reader, label string, defaultYes bool) (bool, error) {
	defaultValue := "Y"
	if !defaultYes {
		defaultValue = "N"
	}
	for {
		value, hitEOF, err := promptDefault(out, reader, label+" [Y/n]", defaultValue)
		if err != nil {
			return false, err
		}
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		default:
			fmt.Fprintln(out, "Please answer yes or no.")
			if hitEOF {
				return false, fmt.Errorf("invalid yes/no response %q", value)
			}
		}
	}
}

func hostTrustScopesForOS(goos string) []string {
	if goos == "windows" || goos == "darwin" {
		return []string{config.HostTrustScopeSystem, config.HostTrustScopeUser}
	}
	return []string{config.HostTrustScopeSystem}
}

func generatedPoolNamePrefix() (string, error) {
	const (
		maxPrefixLength = 40
		randomHexLength = 6
		fallbackHost    = "runner"
	)
	randomPart, err := randomHex(randomHexLength)
	if err != nil {
		return "", fmt.Errorf("generate pool name prefix: %w", err)
	}
	hostPart := ""
	if hostname, err := initHostname(); err == nil {
		hostPart = config.SanitizeNamePart(hostname)
	}
	if hostPart == "" {
		hostPart = fallbackHost
	}
	maxHostPartLength := maxPrefixLength - 1 - randomHexLength
	if len(hostPart) > maxHostPartLength {
		hostPart = strings.TrimRight(hostPart[:maxHostPartLength], ".-_")
	}
	if hostPart == "" {
		hostPart = fallbackHost
	}
	return hostPart + "-" + randomPart, nil
}

func randomHex(length int) (string, error) {
	if length <= 0 || length%2 != 0 {
		return "", fmt.Errorf("random hex length must be a positive even number")
	}
	data := make([]byte, length/2)
	if n, err := initRandomRead(data); err != nil {
		return "", err
	} else if n != len(data) {
		return "", io.ErrUnexpectedEOF
	}
	return hex.EncodeToString(data), nil
}

func wsl2Available() bool {
	if initGOOS != "windows" {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	status, err := initWSLStatus(ctx)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(cleanWSLStatus(status), "\n") {
		if strings.EqualFold(strings.TrimSpace(line), "default version: 2") {
			return true
		}
	}
	return false
}

func cleanWSLStatus(data []byte) string {
	if len(data) == 0 {
		return ""
	}
	if looksUTF16LE(data) {
		if len(data)%2 != 0 {
			return ""
		}
		units := make([]uint16, 0, len(data)/2)
		for i := 0; i+1 < len(data); i += 2 {
			units = append(units, uint16(data[i])|uint16(data[i+1])<<8)
		}
		text := string(utf16.Decode(units))
		text = strings.TrimPrefix(text, "\ufeff")
		return strings.ReplaceAll(text, "\r\n", "\n")
	}
	text := strings.ReplaceAll(string(data), "\x00", "")
	return strings.ReplaceAll(text, "\r\n", "\n")
}

func looksUTF16LE(data []byte) bool {
	if len(data) >= 2 && data[0] == 0xff && data[1] == 0xfe {
		return true
	}
	if len(data) < 4 {
		return false
	}
	zeros := 0
	pairs := 0
	for i := 1; i < len(data); i += 2 {
		pairs++
		if data[i] == 0 {
			zeros++
		}
	}
	return pairs > 0 && zeros*2 >= pairs
}

func wslStatus(ctx context.Context) ([]byte, error) {
	const maxOutputBytes = 8 * 1024

	var output boundedBuffer
	output.limit = maxOutputBytes
	cmd := exec.CommandContext(ctx, "wsl.exe", "--status")
	cmd.Stdout = &output
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	if output.overflow {
		return nil, fmt.Errorf("wsl.exe --status output exceeds %d bytes", maxOutputBytes)
	}
	return output.Bytes(), nil
}

type boundedBuffer struct {
	bytes.Buffer
	limit    int
	overflow bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	remaining := b.limit - b.Len()
	if remaining <= 0 {
		b.overflow = true
		return len(p), nil
	}
	if len(p) > remaining {
		_, _ = b.Buffer.Write(p[:remaining])
		b.overflow = true
		return len(p), nil
	}
	return b.Buffer.Write(p)
}

func defaultDockerDindConfig(appID int64, organization, privateKeyPath string, poolNamePrefix, hostTrustMode string, hostTrustScopes []string) string {
	return fmt.Sprintf(`github:
  appId: %d
  organization: %s
  privateKeyPath: %s
  apiBaseUrl: https://api.github.com
  webBaseUrl: https://github.com

image:
  sourceType: docker-image
  sourceImage: ghcr.io/catthehacker/ubuntu:full-latest
  outputImage: epar-docker-dind-catthehacker-ubuntu
  upstreamDir: third_party/runner-images
  upstreamLock: third_party/runner-images.lock
  runnerVersion: latest
  hostTrustMode: %s
  hostTrustScopes: [%s]
  customInstallScripts:

pool:
  instances: 1
  namePrefix: %s
  logDir: work/logs

runner:
  labels: [self-hosted, linux, epar-docker-dind-catthehacker-ubuntu]
  includeHostLabel: true
  ephemeral: true

provider:
  type: docker-dind
  sourceImage: epar-docker-dind-catthehacker-ubuntu
  network: default

docker:
  registryMirrors:
    # - http://host.docker.internal:5050

timeouts:
  bootSeconds: 180
  githubOnlineSeconds: 180
  commandSeconds: 900
`, appID, organization, privateKeyPath, hostTrustMode, strings.Join(hostTrustScopes, ", "), poolNamePrefix)
}

func defaultWSLConfig(appID int64, organization, privateKeyPath string, poolNamePrefix string) string {
	return fmt.Sprintf(`github:
  appId: %d
  organization: %s
  privateKeyPath: %s
  apiBaseUrl: https://api.github.com
  webBaseUrl: https://github.com

image:
  sourceType: docker-image
  sourceImage: ghcr.io/catthehacker/ubuntu:full-latest
  sourcePlatform: linux/amd64
  outputImage: work/images/epar-wsl-catthehacker-ubuntu.tar
  upstreamDir: third_party/runner-images
  upstreamLock: third_party/runner-images.lock
  runnerVersion: latest
  customInstallScripts:
    # - examples/custom-install/install-extra-apt-tools.sh

pool:
  instances: 1
  # Must be unique for this machine/config within the GitHub organization.
  namePrefix: %s
  logDir: work/logs

runner:
  labels: [self-hosted, linux, X64, epar-wsl-catthehacker-ubuntu]
  includeHostLabel: true
  ephemeral: true

provider:
  type: wsl
  sourceImage: work/images/epar-wsl-catthehacker-ubuntu.tar
  network: default
  installRoot: work/wsl

docker:
  registryMirrors:
    # - https://mirror.example.test

timeouts:
  bootSeconds: 180
  githubOnlineSeconds: 180
  commandSeconds: 900
`, appID, organization, privateKeyPath, poolNamePrefix)
}

var stdinIsInteractive = func() bool {
	info, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}
