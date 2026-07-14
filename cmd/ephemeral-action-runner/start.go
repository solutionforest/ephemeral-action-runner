package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
	"github.com/solutionforest/ephemeral-action-runner/internal/pool"
)

type starterManager interface {
	EnsureImage(context.Context) error
	RunPool(context.Context, pool.RunOptions) error
}

type hostTrustLockingStarterManager interface {
	AcquireHostTrustControllerLock() (io.Closer, error)
}

type starterManagerFactory func(configPath, projectRoot string, dryRun bool, githubEnabled bool) (starterManager, error)

var newStarterManager starterManagerFactory = func(configPath, projectRoot string, dryRun bool, githubEnabled bool) (starterManager, error) {
	return newManager(configPath, projectRoot, dryRun, githubEnabled)
}

type startOptions struct {
	Context          context.Context
	ProjectRoot      string
	ConfigPath       string
	DryRun           bool
	Instances        int
	Register         bool
	KeepOnExit       bool
	ReplaceCompleted bool
	MonitorInterval  time.Duration
	In               io.Reader
	Out              io.Writer
	ManagerFactory   starterManagerFactory
}

func runStart(args []string) error {
	fs := flag.NewFlagSet("start", flag.ExitOnError)
	common := addCommonFlags(fs)
	instances := fs.Int("instances", 0, "number of instances to create; overrides pool.instances")
	register := fs.Bool("register", true, "register the instances as GitHub ephemeral runners")
	keepOnExit := fs.Bool("keep-on-exit", false, "leave prefixed instances and GitHub runners running when interrupted")
	replaceCompleted := fs.Bool("replace-completed", true, "replace an instance when its ephemeral runner exits after a job")
	monitorInterval := fs.Duration("monitor-interval", 15*time.Second, "interval for runner liveness checks")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if flagPassed(fs, "instances") && *instances <= 0 {
		return fmt.Errorf("--instances must be 1 or greater")
	}
	return runStartWithOptions(startOptions{
		Context:          interruptContext(),
		ProjectRoot:      *common.projectRoot,
		ConfigPath:       *common.configPath,
		DryRun:           *common.dryRun,
		Instances:        *instances,
		Register:         *register,
		KeepOnExit:       *keepOnExit,
		ReplaceCompleted: *replaceCompleted,
		MonitorInterval:  *monitorInterval,
		In:               os.Stdin,
		Out:              os.Stdout,
		ManagerFactory:   newStarterManager,
	})
}

func runStartWithOptions(opts startOptions) error {
	if opts.Context == nil {
		opts.Context = context.Background()
	}
	if opts.In == nil {
		opts.In = os.Stdin
	}
	if opts.Out == nil {
		opts.Out = os.Stdout
	}
	if opts.ManagerFactory == nil {
		opts.ManagerFactory = newStarterManager
	}
	if opts.Instances < 0 {
		return fmt.Errorf("instances must be 1 or greater")
	}
	projectRoot, err := filepath.Abs(opts.ProjectRoot)
	if err != nil {
		return err
	}
	configPath, err := ensureConfigForStart(startOptions{
		ProjectRoot: projectRoot,
		ConfigPath:  opts.ConfigPath,
		In:          opts.In,
		Out:         opts.Out,
	})
	if err != nil {
		return err
	}
	manager, err := opts.ManagerFactory(configPath, projectRoot, opts.DryRun, opts.Register)
	if err != nil {
		return err
	}
	hostTrustLockHeld := false
	if lockingManager, ok := manager.(hostTrustLockingStarterManager); ok {
		controllerLock, err := lockingManager.AcquireHostTrustControllerLock()
		if err != nil {
			return err
		}
		if controllerLock != nil {
			defer controllerLock.Close()
			hostTrustLockHeld = true
		}
	}
	fmt.Fprintf(opts.Out, "Ensuring runner image is current for %s\n", configPath)
	if err := manager.EnsureImage(opts.Context); err != nil {
		return err
	}
	if opts.Instances > 0 {
		fmt.Fprintf(opts.Out, "Starting EPAR pool with %d instance(s). Press Ctrl-C to stop; cleanup is enabled by default.\n", opts.Instances)
	} else {
		fmt.Fprintf(opts.Out, "Starting EPAR pool using pool.instances from config. Press Ctrl-C to stop; cleanup is enabled by default.\n")
	}
	return manager.RunPool(opts.Context, pool.RunOptions{
		Instances:         opts.Instances,
		Register:          opts.Register,
		KeepOnExit:        opts.KeepOnExit,
		ReplaceCompleted:  opts.ReplaceCompleted,
		MonitorInterval:   opts.MonitorInterval,
		HostTrustLockHeld: hostTrustLockHeld,
	})
}

func ensureConfigForStart(opts startOptions) (string, error) {
	path, exists, err := resolveStartConfigPath(opts.ProjectRoot, opts.ConfigPath)
	if err != nil {
		return "", err
	}
	if exists {
		return path, nil
	}
	if path == "" {
		path = filepath.Join(opts.ProjectRoot, ".local", "config.yml")
	}
	if !stdinIsInteractive() {
		return "", fmt.Errorf("no EPAR config found; run %s init from the EPAR directory, or pass --config <path> after creating a config. See README.md and docs/github-app.md for GitHub App setup", binaryName)
	}
	fmt.Fprintf(opts.Out, "No EPAR config found. Starting first-run setup.\n\n")
	if err := runInitWithOptions(initOptions{
		ProjectRoot: projectRootOrCwd(opts.ProjectRoot),
		ConfigPath:  path,
		In:          opts.In,
		Out:         opts.Out,
	}); err != nil {
		return "", err
	}
	fmt.Fprintf(opts.Out, "\nContinuing with %s\n", path)
	return path, nil
}

func resolveStartConfigPath(projectRoot, explicit string) (string, bool, error) {
	if explicit != "" {
		return existingPath(config.ProjectPath(projectRoot, explicit))
	}
	if envPath := os.Getenv("EPAR_CONFIG"); envPath != "" {
		return existingPath(config.ProjectPath(projectRoot, envPath))
	}
	localPath := filepath.Join(projectRoot, ".local", "config.yml")
	if path, exists, err := existingPath(localPath); err != nil || exists {
		return path, exists, err
	}
	if home, err := os.UserHomeDir(); err == nil {
		homePath := filepath.Join(home, ".config", "ephemeral-action-runner", "config.yml")
		if path, exists, err := existingPath(homePath); err != nil || exists {
			return path, exists, err
		}
	}
	return "", false, nil
}

func existingPath(path string) (string, bool, error) {
	if _, err := os.Stat(path); err == nil {
		return path, true, nil
	} else if errors.Is(err, os.ErrNotExist) {
		return path, false, nil
	} else {
		return path, false, err
	}
}

func projectRootOrCwd(projectRoot string) string {
	if projectRoot != "" {
		return projectRoot
	}
	cwd, _ := os.Getwd()
	return cwd
}
