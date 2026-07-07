package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
)

var dockerAvailable = func(ctx context.Context) error {
	return exec.CommandContext(ctx, "docker", "version", "--format", "{{.Server.Version}}").Run()
}

type initOptions struct {
	ProjectRoot     string
	ConfigPath      string
	Force           bool
	SkipDockerCheck bool
	In              io.Reader
	Out             io.Writer
}

func runInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	common := addCommonFlags(fs)
	force := fs.Bool("force", false, "overwrite an existing config file")
	skipDockerCheck := fs.Bool("skip-docker-check", false, "create the config without checking for Docker")
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
		ProjectRoot:     projectRoot,
		ConfigPath:      configPath,
		Force:           *force,
		SkipDockerCheck: *skipDockerCheck,
		In:              os.Stdin,
		Out:             os.Stdout,
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
	fmt.Fprintln(opts.Out, "This creates .local/config.yml for the default Docker-DinD runner.")
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

	content := defaultDockerDindConfig(appID, organization, privateKeyPath)
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

func defaultDockerDindConfig(appID int64, organization, privateKeyPath string) string {
	return fmt.Sprintf(`github:
  appId: %d
  organization: %s
  privateKeyPath: %s
  apiBaseUrl: https://api.github.com
  webBaseUrl: https://github.com

image:
  sourceType: docker-image
  sourceImage: gitea/runner-images:ubuntu-latest-full
  outputImage: epar-docker-dind-gitea-ubuntu
  upstreamDir: third_party/runner-images
  upstreamLock: third_party/runner-images.lock
  runnerVersion: latest
  customInstallScripts:

pool:
  instances: 1
  namePrefix: epar-dind
  logDir: work/logs

runner:
  labels: [self-hosted, linux, epar-docker-dind-gitea-ubuntu]
  includeHostLabel: true
  ephemeral: true

provider:
  type: docker-dind
  sourceImage: epar-docker-dind-gitea-ubuntu
  network: default

docker:
  registryMirrors:
    # - http://host.docker.internal:5050

timeouts:
  bootSeconds: 180
  githubOnlineSeconds: 180
  commandSeconds: 900
`, appID, organization, privateKeyPath)
}

var stdinIsInteractive = func() bool {
	info, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}
