package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
	"github.com/solutionforest/ephemeral-action-runner/internal/invocation"
	"github.com/solutionforest/ephemeral-action-runner/internal/pool"
)

type starterManager interface {
	PreflightRunnerGroup(context.Context) error
	EnsureImage(context.Context) error
	RunPool(context.Context, pool.RunOptions) error
}

type hostTrustLockingStarterManager interface {
	AcquireHostTrustControllerLock() (io.Closer, error)
}

type poolLockingStarterManager interface {
	AcquirePoolControllerLock() (io.Closer, error)
}

type startupTimingStarterManager interface {
	StartStartupTiming() (string, error)
	FinishStartupTiming(error)
}

type closingStarterManager interface {
	Close() error
}

type storageAdmissionConfiguringStarterManager interface {
	ConfigureStorageAdmissionOverride(bool, string)
}

type starterManagerFactory func(configPath, projectRoot string, dryRun bool, githubEnabled bool) (starterManager, error)

var newStarterManager starterManagerFactory = func(configPath, projectRoot string, dryRun bool, githubEnabled bool) (starterManager, error) {
	return newManager(configPath, projectRoot, dryRun, githubEnabled)
}

type startOptions struct {
	Context                  context.Context
	ProjectRoot              string
	ConfigPath               string
	DryRun                   bool
	Instances                int
	Register                 bool
	KeepOnExit               bool
	ReplaceCompleted         bool
	MonitorInterval          time.Duration
	AllowInsufficientStorage bool
	StorageOverrideCommand   string
	In                       io.Reader
	Out                      io.Writer
	ManagerFactory           starterManagerFactory
}

func runStart(args []string) error {
	fs := flag.NewFlagSet("start", flag.ExitOnError)
	common := addCommonFlags(fs)
	instances := fs.Int("instances", 0, "number of instances to create; overrides pool.instances")
	register := fs.Bool("register", true, "register the instances as GitHub ephemeral runners")
	keepOnExit := fs.Bool("keep-on-exit", false, "leave prefixed instances and GitHub runners running when interrupted")
	replaceCompleted := fs.Bool("replace-completed", true, "replace an instance when its ephemeral runner exits after a job")
	monitorInterval := fs.Duration("monitor-interval", 15*time.Second, "interval for runner liveness checks")
	allowInsufficientStorage := fs.Bool("allow-insufficient-storage", false, "continue this invocation after storage-only admission warnings")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if flagPassed(fs, "instances") && *instances <= 0 {
		return fmt.Errorf("--instances must be 1 or greater")
	}
	return runStartWithOptions(startOptions{
		Context:                  interruptContext(),
		ProjectRoot:              *common.projectRoot,
		ConfigPath:               *common.configPath,
		DryRun:                   *common.dryRun,
		Instances:                *instances,
		Register:                 *register,
		KeepOnExit:               *keepOnExit,
		ReplaceCompleted:         *replaceCompleted,
		MonitorInterval:          *monitorInterval,
		AllowInsufficientStorage: *allowInsufficientStorage,
		StorageOverrideCommand:   matchingStartCommand(appendStorageOverride(args)),
		In:                       os.Stdin,
		Out:                      os.Stdout,
		ManagerFactory:           newStarterManager,
	})
}

func runStartWithOptions(opts startOptions) (err error) {
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
	configPath, startNow, err := ensureConfigForStart(startOptions{
		Context:     opts.Context,
		ProjectRoot: projectRoot,
		ConfigPath:  opts.ConfigPath,
		In:          opts.In,
		Out:         opts.Out,
	})
	if err != nil {
		return err
	}
	if !startNow {
		return nil
	}
	manager, err := opts.ManagerFactory(configPath, projectRoot, opts.DryRun, opts.Register)
	if err != nil {
		return err
	}
	if closingManager, ok := manager.(closingStarterManager); ok {
		defer closingManager.Close()
	}
	if configuringManager, ok := manager.(storageAdmissionConfiguringStarterManager); ok {
		overrideCommand := opts.StorageOverrideCommand
		if overrideCommand == "" {
			overrideCommand = matchingStartCommand([]string{"--allow-insufficient-storage"})
		}
		configuringManager.ConfigureStorageAdmissionOverride(opts.AllowInsufficientStorage, overrideCommand)
	}
	if timingManager, ok := manager.(startupTimingStarterManager); ok {
		if _, err := timingManager.StartStartupTiming(); err != nil {
			return fmt.Errorf("start startup timing log: %w", err)
		}
		defer func() {
			timingManager.FinishStartupTiming(err)
		}()
	}
	if opts.Register {
		fmt.Fprintf(opts.Out, "Checking GitHub runner-group security policy for %s\n", configPath)
		if err = manager.PreflightRunnerGroup(opts.Context); err != nil {
			return err
		}
	}
	poolLockHeld := false
	if lockingManager, ok := manager.(poolLockingStarterManager); ok {
		controllerLock, err := lockingManager.AcquirePoolControllerLock()
		if err != nil {
			return err
		}
		if controllerLock != nil {
			defer controllerLock.Close()
			poolLockHeld = true
		}
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
	fmt.Fprintf(opts.Out, "Ensuring the runner image or sandbox template is current for %s\n", configPath)
	if err = manager.EnsureImage(opts.Context); err != nil {
		return err
	}
	stopGuidance := "Press Ctrl-C once to stop; wait for cleanup confirmation before closing this window."
	if opts.KeepOnExit {
		stopGuidance = "Press Ctrl-C once to stop; --keep-on-exit will leave owned runner resources running."
	}
	if opts.Instances > 0 {
		fmt.Fprintf(opts.Out, "Starting EPAR pool with %d instance(s). %s\n", opts.Instances, stopGuidance)
	} else {
		fmt.Fprintf(opts.Out, "Starting EPAR pool using pool.instances from config. %s\n", stopGuidance)
	}
	err = manager.RunPool(opts.Context, pool.RunOptions{
		Instances:         opts.Instances,
		Register:          opts.Register,
		KeepOnExit:        opts.KeepOnExit,
		ReplaceCompleted:  opts.ReplaceCompleted,
		MonitorInterval:   opts.MonitorInterval,
		PoolLockHeld:      poolLockHeld,
		HostTrustLockHeld: hostTrustLockHeld,
	})
	return err
}

func appendStorageOverride(args []string) []string {
	result := append([]string(nil), args...)
	for _, arg := range result {
		if arg == "--allow-insufficient-storage" || arg == "--allow-insufficient-storage=true" {
			return result
		}
	}
	return append(result, "--allow-insufficient-storage")
}

func matchingStartCommand(args []string) string {
	if os.Getenv(invocation.Environment) == "start" {
		return invocation.Command(args...)
	}
	return invocation.Command(append([]string{"start"}, args...)...)
}

func ensureConfigForStart(opts startOptions) (string, bool, error) {
	path, exists, err := resolveStartConfigPath(opts.ProjectRoot, opts.ConfigPath)
	if err != nil {
		return "", false, err
	}
	if exists {
		return path, true, nil
	}
	if path == "" {
		path = filepath.Join(opts.ProjectRoot, ".local", "config.yml")
	}
	if !stdinIsInteractive() {
		return "", false, fmt.Errorf("no EPAR config found; run %s init from the EPAR directory, or pass --config <path> after creating a config. See README.md and docs/github-app.md for GitHub App setup", binaryName)
	}
	fmt.Fprintf(opts.Out, "No EPAR config found. Starting first-run setup.\n\n")
	reader := bufio.NewReader(opts.In)
	if err := runInitWithOptions(initOptions{
		Context:         opts.Context,
		ProjectRoot:     projectRootOrCwd(opts.ProjectRoot),
		ConfigPath:      path,
		EmbeddedInStart: true,
		In:              opts.In,
		Reader:          reader,
		Out:             opts.Out,
	}); err != nil {
		return "", false, err
	}
	fmt.Fprintln(opts.Out, "")
	startNow, err := promptYesNo(opts.Out, reader, fmt.Sprintf("Start runners now? Choose No to exit and review %s", path), true)
	if err != nil {
		return "", false, err
	}
	if !startNow {
		fmt.Fprintf(opts.Out, "\nConfig saved at %s. Exiting before runner startup.\nReview the config, then run %s when ready.\n", path, invocation.Command())
		return path, false, nil
	}
	fmt.Fprintf(opts.Out, "\nContinuing with %s\n", path)
	return path, true, nil
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
