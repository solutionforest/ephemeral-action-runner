package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
	gh "github.com/solutionforest/ephemeral-action-runner/internal/github"
	"github.com/solutionforest/ephemeral-action-runner/internal/invocation"
	"github.com/solutionforest/ephemeral-action-runner/internal/logging"
	"github.com/solutionforest/ephemeral-action-runner/internal/pool"
	poolstate "github.com/solutionforest/ephemeral-action-runner/internal/pool/state"
	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
	"github.com/solutionforest/ephemeral-action-runner/internal/provider/registry"
	"github.com/solutionforest/ephemeral-action-runner/internal/storage"
)

const binaryName = "ephemeral-action-runner"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, binaryName+":", err)
		if reportPath := writeLastErrorReport(os.Args[1:], err); reportPath != "" {
			fmt.Fprintln(os.Stderr, "error report:", reportPath)
		}
		os.Exit(1)
	}
}

func writeLastErrorReport(args []string, runErr error) string {
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	projectRoot, explicitConfig := errorReportFlags(args, cwd)
	logDir := filepath.Join(projectRoot, "work", "logs")
	if resolvedConfig, resolveErr := resolveConfigPath(projectRoot, explicitConfig); resolveErr == nil && resolvedConfig != "" {
		if cfg, loadErr := config.Load(resolvedConfig); loadErr == nil && config.ValidateLogging(cfg.Logging) == nil {
			logDir = config.ProjectPath(projectRoot, cfg.Logging.Directory)
		}
	}
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return ""
	}
	if err := os.MkdirAll(filepath.Join(logDir, "errors"), 0755); err != nil {
		return ""
	}
	now := time.Now().UTC()
	content := provider.RedactText(fmt.Sprintf(`EPAR failed
time: %s
workingDirectory: %s
command: %q

%s
error: %v
`, now.Format(time.RFC3339), cwd, os.Args, versionString(), runErr))

	lastPath := logging.LastErrorPath(logDir)
	if err := logging.WritePrivateFileAtomic(lastPath, []byte(content)); err != nil {
		return ""
	}
	stampedPath := logging.ErrorPath(logDir, now)
	_ = logging.WritePrivateFileAtomic(stampedPath, []byte(content))
	return lastPath
}

func errorReportFlags(args []string, fallbackRoot string) (string, string) {
	projectRoot := fallbackRoot
	configPath := ""
	for index := 0; index < len(args); index++ {
		value := args[index]
		switch {
		case value == "--project-root" && index+1 < len(args):
			index++
			projectRoot = args[index]
		case strings.HasPrefix(value, "--project-root="):
			projectRoot = strings.TrimPrefix(value, "--project-root=")
		case value == "--config" && index+1 < len(args):
			index++
			configPath = args[index]
		case strings.HasPrefix(value, "--config="):
			configPath = strings.TrimPrefix(value, "--config=")
		}
	}
	if absolute, absErr := filepath.Abs(projectRoot); absErr == nil {
		projectRoot = absolute
	}
	return projectRoot, configPath
}

func run(args []string) error {
	if len(args) == 0 {
		return runStart(nil)
	}
	switch args[0] {
	case "start":
		return runStart(args[1:])
	case "init":
		return runInit(args[1:])
	case "image":
		return runImage(args[1:])
	case "pool":
		return runPool(args[1:])
	case "cleanup":
		return runCleanup(args[1:])
	case "status":
		return runStatus(args[1:])
	case "logs":
		return runLogs(args[1:])
	case "storage":
		return runStorage(args[1:])
	case "version":
		printVersion(os.Stdout)
		return nil
	case "help", "-h", "--help":
		usage()
		return nil
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func runLogs(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("logs requires subcommand: path, list, or prune")
	}
	fs := flag.NewFlagSet("logs "+args[0], flag.ExitOnError)
	common := addCommonFlags(fs)
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("logs %s does not accept positional arguments", args[0])
	}
	projectRoot, cfg, root, err := loadLoggingConfig(*common.projectRoot, *common.configPath)
	if err != nil {
		return err
	}
	_ = projectRoot
	switch args[0] {
	case "path":
		fmt.Fprintln(os.Stdout, root)
		return nil
	case "list":
		report, err := logging.ListRetention(root, retentionPolicy(cfg.Logging))
		if err != nil {
			return err
		}
		for _, entry := range report.Entries {
			if entry.Recognized {
				fmt.Fprintf(os.Stdout, "%s\t%s\t%d\t%s\n", entry.Category, entry.Action, entry.Size, entry.Path)
			}
		}
		fmt.Fprintln(os.Stdout, report.Summary())
		return nil
	case "prune":
		report, err := logging.PruneRetention(root, retentionPolicy(cfg.Logging), *common.dryRun)
		if err != nil {
			return err
		}
		for _, entry := range report.Entries {
			if entry.Action == logging.RetentionWouldDelete || entry.Action == logging.RetentionDeleted || entry.Action == logging.RetentionSkipped {
				fmt.Fprintf(os.Stdout, "%s\t%s\t%s\n", entry.Action, entry.Reason, entry.Path)
			}
		}
		fmt.Fprintln(os.Stdout, report.Summary())
		for _, warning := range report.Warnings {
			fmt.Fprintln(os.Stderr, "warning:", warning)
		}
		return nil
	default:
		return fmt.Errorf("unknown logs subcommand %q", args[0])
	}
}

func loadLoggingConfig(projectRoot, configPath string) (string, config.Config, string, error) {
	projectRoot, err := filepath.Abs(projectRoot)
	if err != nil {
		return "", config.Config{}, "", err
	}
	resolvedConfigPath, err := resolveConfigPath(projectRoot, configPath)
	if err != nil {
		return "", config.Config{}, "", err
	}
	if resolvedConfigPath == "" {
		return "", config.Config{}, "", fmt.Errorf("no config found; run %s init from the EPAR directory to create .local/config.yml", binaryName)
	}
	cfg, err := config.Load(resolvedConfigPath)
	if err != nil {
		return "", config.Config{}, "", err
	}
	printConfigWarnings(cfg)
	if err := config.Validate(cfg); err != nil {
		return "", config.Config{}, "", err
	}
	root, err := filepath.Abs(config.ProjectPath(projectRoot, cfg.Logging.Directory))
	if err != nil {
		return "", config.Config{}, "", err
	}
	return projectRoot, cfg, root, nil
}

func retentionPolicy(cfg config.LoggingConfig) logging.RetentionPolicy {
	days := func(value int) time.Duration { return time.Duration(value) * 24 * time.Hour }
	return logging.RetentionPolicy{
		MaxTotalBytes:   int64(cfg.RetentionMaxTotalMiB) * 1024 * 1024,
		ManagerMaxAge:   days(cfg.ManagerMaxAgeDays),
		InstanceMaxAge:  days(cfg.InstanceMaxAgeDays),
		BuildMaxAge:     days(cfg.BuildMaxAgeDays),
		ErrorMaxAge:     days(cfg.ErrorMaxAgeDays),
		BenchmarkMaxAge: days(cfg.BenchmarkMaxAgeDays),
	}
}

func runImage(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("image requires subcommand: update-upstream or build")
	}
	switch args[0] {
	case "update-upstream":
		fs := flag.NewFlagSet("image update-upstream", flag.ExitOnError)
		common := addCommonFlags(fs)
		allowInsufficientStorage := fs.Bool("allow-insufficient-storage", false, "continue this invocation after storage-only admission warnings")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		m, err := newManager(*common.configPath, *common.projectRoot, *common.dryRun, false)
		if err != nil {
			return err
		}
		defer m.Close()
		m.ConfigureStorageAdmissionOverride(*allowInsufficientStorage, invocation.Command(append([]string{"image", "update-upstream"}, appendStorageOverride(args[1:])...)...))
		if err := rejectDockerSandboxesImageCommand(m, "image update-upstream"); err != nil {
			return err
		}
		controllerLock, err := m.AcquirePoolControllerLock()
		if err != nil {
			return err
		}
		defer controllerLock.Close()
		return m.UpdateUpstream(context.Background())
	case "build":
		fs := flag.NewFlagSet("image build", flag.ExitOnError)
		common := addCommonFlags(fs)
		replace := fs.Bool("replace", false, "delete an existing output image before building")
		update := fs.Bool("update-upstream", false, "refresh runner-images before building")
		skipUpstream := fs.Bool("skip-upstream-check", false, "skip checking the runner-images checkout")
		allowInsufficientStorage := fs.Bool("allow-insufficient-storage", false, "continue this invocation after storage-only admission warnings")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		m, err := newManager(*common.configPath, *common.projectRoot, *common.dryRun, false)
		if err != nil {
			return err
		}
		defer m.Close()
		m.ConfigureStorageAdmissionOverride(*allowInsufficientStorage, invocation.Command(append([]string{"image", "build"}, appendStorageOverride(args[1:])...)...))
		ctx := interruptContext()
		poolControllerLock, err := m.AcquirePoolControllerLock()
		if err != nil {
			return err
		}
		defer poolControllerLock.Close()
		hostTrustControllerLock, err := m.AcquireHostTrustControllerLock()
		if err != nil {
			return err
		}
		if hostTrustControllerLock != nil {
			defer hostTrustControllerLock.Close()
		}
		if *update {
			if err := m.UpdateUpstream(ctx); err != nil {
				return err
			}
		}
		return m.BuildImage(ctx, pool.ImageBuildOptions{Replace: *replace, SkipUpstreamCheck: *skipUpstream})
	case "refresh-scripts":
		fs := flag.NewFlagSet("image refresh-scripts", flag.ExitOnError)
		common := addCommonFlags(fs)
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		m, err := newManager(*common.configPath, *common.projectRoot, *common.dryRun, false)
		if err != nil {
			return err
		}
		defer m.Close()
		if err := rejectDockerSandboxesImageCommand(m, "image refresh-scripts"); err != nil {
			return err
		}
		controllerLock, err := m.AcquirePoolControllerLock()
		if err != nil {
			return err
		}
		defer controllerLock.Close()
		return m.RefreshScripts(interruptContext())
	default:
		return fmt.Errorf("unknown image subcommand %q", args[0])
	}
}

func runPool(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("pool requires subcommand: up, verify, or down")
	}
	switch args[0] {
	case "verify":
		fs := flag.NewFlagSet("pool verify", flag.ExitOnError)
		common := addCommonFlags(fs)
		instances := fs.Int("instances", 0, "number of concurrent instances to verify; overrides pool.instances")
		registerOnly := fs.Bool("register-only", false, "register runners and verify online/idle without dispatching a job")
		cleanup := fs.Bool("cleanup", false, "clean up verification resources; legacy providers use the configured pool prefix, while Docker Sandboxes uses exact owned records")
		allowInsufficientStorage := fs.Bool("allow-insufficient-storage", false, "continue this invocation after storage-only admission warnings")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if flagPassed(fs, "instances") && *instances <= 0 {
			return fmt.Errorf("--instances must be 1 or greater")
		}
		m, err := newManager(*common.configPath, *common.projectRoot, *common.dryRun, *registerOnly)
		if err != nil {
			return err
		}
		defer m.Close()
		m.ConfigureStorageAdmissionOverride(*allowInsufficientStorage, invocation.Command(append([]string{"pool", "verify"}, appendStorageOverride(args[1:])...)...))
		return m.Verify(interruptContext(), pool.VerifyOptions{Instances: *instances, RegisterOnly: *registerOnly, Cleanup: *cleanup})
	case "up":
		fs := flag.NewFlagSet("pool up", flag.ExitOnError)
		common := addCommonFlags(fs)
		instances := fs.Int("instances", 0, "number of instances to create; overrides pool.instances")
		register := fs.Bool("register", true, "register the instances as GitHub ephemeral runners")
		keepOnExit := fs.Bool("keep-on-exit", false, "leave prefixed instances and GitHub runners running when interrupted")
		replaceCompleted := fs.Bool("replace-completed", true, "replace an instance when its ephemeral runner exits after a job")
		monitorInterval := fs.Duration("monitor-interval", 15*time.Second, "interval for runner liveness checks")
		allowInsufficientStorage := fs.Bool("allow-insufficient-storage", false, "continue this invocation after storage-only admission warnings")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if flagPassed(fs, "instances") && *instances <= 0 {
			return fmt.Errorf("--instances must be 1 or greater")
		}
		m, err := newManager(*common.configPath, *common.projectRoot, *common.dryRun, *register)
		if err != nil {
			return err
		}
		m.ConfigureStorageAdmissionOverride(*allowInsufficientStorage, invocation.Command(append([]string{"pool", "up"}, appendStorageOverride(args[1:])...)...))
		return m.RunPool(interruptContext(), pool.RunOptions{
			Instances:        *instances,
			Register:         *register,
			KeepOnExit:       *keepOnExit,
			ReplaceCompleted: *replaceCompleted,
			MonitorInterval:  *monitorInterval,
		})
	case "down":
		return runCleanup(args[1:])
	default:
		return fmt.Errorf("unknown pool subcommand %q", args[0])
	}
}

func runCleanup(args []string) error {
	fs := flag.NewFlagSet("cleanup", flag.ExitOnError)
	common := addCommonFlags(fs)
	noGitHub := fs.Bool("no-github", false, "skip GitHub runner deletion")
	acknowledgeFailedDiagnostics := fs.Bool("acknowledge-failed-diagnostics", false, "allow exact cleanup of retained sandboxes after failed diagnostics evidence has been reviewed")
	if err := fs.Parse(args); err != nil {
		return err
	}
	m, err := newManager(*common.configPath, *common.projectRoot, *common.dryRun, !*noGitHub)
	if err != nil {
		return err
	}
	defer m.Close()
	m.AcknowledgeFailedDiagnostics = *acknowledgeFailedDiagnostics
	return m.Cleanup(context.Background())
}

func runStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	common := addCommonFlags(fs)
	noGitHub := fs.Bool("no-github", false, "skip GitHub runner status")
	if err := fs.Parse(args); err != nil {
		return err
	}
	m, err := newManager(*common.configPath, *common.projectRoot, *common.dryRun, !*noGitHub)
	if err != nil {
		return err
	}
	defer m.Close()
	status, err := m.Status(context.Background())
	if err != nil {
		return err
	}
	fmt.Print(status)
	return nil
}

type commonFlags struct {
	configPath  *string
	projectRoot *string
	dryRun      *bool
}

func addCommonFlags(fs *flag.FlagSet) commonFlags {
	cwd, _ := os.Getwd()
	return commonFlags{
		configPath:  fs.String("config", "", "config file path; defaults to EPAR_CONFIG, .local/config.yml, or ~/.config/ephemeral-action-runner/config.yml"),
		projectRoot: fs.String("project-root", cwd, "project root containing scripts and docs"),
		dryRun:      fs.Bool("dry-run", false, "print provider commands instead of executing them"),
	}
}

func flagPassed(fs *flag.FlagSet, name string) bool {
	found := false
	fs.Visit(func(flag *flag.Flag) {
		if flag.Name == name {
			found = true
		}
	})
	return found
}

func newManager(configPath, projectRoot string, dryRun bool, githubEnabled bool) (*pool.Manager, error) {
	return newManagerWithLifecycleState(configPath, projectRoot, dryRun, githubEnabled, true)
}

func newImageProvisioningManager(configPath, projectRoot string) (*pool.Manager, error) {
	return newManagerWithLifecycleState(configPath, projectRoot, false, false, false)
}

func newManagerWithLifecycleState(configPath, projectRoot string, dryRun bool, githubEnabled bool, openLifecycleState bool) (*pool.Manager, error) {
	projectRoot, err := filepath.Abs(projectRoot)
	if err != nil {
		return nil, err
	}
	resolvedConfigPath, err := resolveConfigPath(projectRoot, configPath)
	if err != nil {
		return nil, err
	}
	if resolvedConfigPath == "" {
		return nil, fmt.Errorf("no config found; run %s init from the EPAR directory to create .local/config.yml", binaryName)
	}
	cfg, err := config.Load(resolvedConfigPath)
	if err != nil {
		return nil, err
	}
	printConfigWarnings(cfg)
	if err := config.Validate(cfg); err != nil {
		return nil, err
	}
	if !dryRun {
		if err := importNativeBootstrapAcquisition(projectRoot, resolvedConfigPath, time.Now().UTC()); err != nil {
			return nil, err
		}
	}
	providerRuntime, err := registry.New(cfg, projectRoot, dryRun)
	if err != nil {
		return nil, err
	}
	if providerRuntime.Lifecycle == nil || providerRuntime.Storage == nil {
		return nil, fmt.Errorf("provider %q registry entry is missing required lifecycle or storage behavior", cfg.Provider.Type)
	}
	var client pool.GitHubClient
	if githubEnabled && !dryRun {
		if err := config.ValidateGitHub(cfg); err != nil {
			return nil, err
		}
		client = gh.New(cfg.GitHub)
	}
	var lifecycleState *poolstate.Store
	if !dryRun && openLifecycleState {
		lifecycleState, err = pool.OpenLifecycleState(projectRoot, resolvedConfigPath)
		if err != nil {
			return nil, err
		}
	}
	runtime, err := logging.NewRuntime(logging.Options{
		Directory:                   config.ProjectPath(projectRoot, cfg.Logging.Directory),
		ManagerSinks:                loggingSinks(cfg.Logging.ManagerSinks),
		ManagerConsoleFormat:        logging.Format(cfg.Logging.ManagerConsoleFormat),
		ManagerConsoleTextFormat:    cfg.Logging.ManagerConsoleTextFormat,
		ManagerFileFormat:           logging.Format(cfg.Logging.ManagerFileFormat),
		TranscriptSinks:             loggingSinks(cfg.Logging.TranscriptSinks),
		TranscriptConsoleFormat:     logging.Format(cfg.Logging.TranscriptConsoleFormat),
		TranscriptConsoleTextFormat: cfg.Logging.TranscriptConsoleTextFormat,
		Rotation: logging.Rotation{
			MaxSizeMiB: cfg.Logging.MaxFileSizeMiB,
			MaxBackups: cfg.Logging.MaxBackups,
			Compress:   cfg.Logging.CompressBackups,
		},
	})
	if err != nil {
		return nil, err
	}
	manager := &pool.Manager{
		Config:                  cfg,
		Provider:                providerRuntime.Legacy,
		Lifecycle:               providerRuntime.Lifecycle,
		PolicyManager:           providerRuntime.PolicyManager,
		Storage:                 providerRuntime.Storage,
		LifecycleState:          lifecycleState,
		GitHub:                  client,
		ProjectRoot:             projectRoot,
		ConfigPath:              resolvedConfigPath,
		DryRun:                  dryRun,
		Logging:                 runtime,
		AutomaticImageLifecycle: true,
	}
	if cfg.Logging.RetentionEnabled {
		report, pruneErr := manager.PruneLogs(false)
		if pruneErr != nil {
			runtime.Logger().Warn("log retention failed", "operation", "logs-prune", "error", pruneErr)
		} else {
			for _, warning := range report.Warnings {
				runtime.Logger().Warn("log retention skipped candidate", "operation", "logs-prune", "warning", warning)
			}
		}
	}
	return manager, nil
}

func preflightControllerStorage(projectRoot string, cfg config.Config, contributions ...provider.StorageContribution) error {
	minimumFree, err := config.EffectiveMinimumFreeBytes(cfg)
	if err != nil {
		return err
	}
	if len(contributions) != 0 && contributions[0] != nil {
		snapshot, err := contributions[0].StorageSnapshot(context.Background(), provider.StorageRequest{
			Operation:        "controller-bootstrap",
			Now:              time.Now(),
			MinimumFreeBytes: minimumFree,
		})
		if err != nil {
			return fmt.Errorf("provider storage surface cannot be measured before controller bootstrap: %w\n\nInspect storage with:\n  %s", err, invocation.Command("storage", "status", "--provider", cfg.Provider.Type))
		}
		surfaces := make(map[string]storage.Surface, len(snapshot.Surfaces))
		for _, surface := range snapshot.Surfaces {
			surfaces[surface.ID] = surface
		}
		for _, requirement := range snapshot.Requirements {
			surface, found := surfaces[requirement.SurfaceID]
			if !found {
				return fmt.Errorf("controller storage requirement %q references unknown surface %q", requirement.ID, requirement.SurfaceID)
			}
			check, err := storage.EvaluateCapacity(surface, requirement)
			if err != nil {
				return err
			}
			if check.Status != storage.CapacityReady {
				return storage.CapacityAdmissionError("initialize the EPAR controller", surface, requirement, check, invocation.Command("storage", "prune", "--provider", cfg.Provider.Type))
			}
		}
		return nil
	}
	capacity, err := storage.ProbeFilesystemCapacity(projectRoot, time.Now())
	if err != nil {
		return fmt.Errorf("controller storage surface %q cannot be measured before bootstrap: %w\n\nInspect storage with:\n  %s", projectRoot, err, invocation.Command("storage", "status", "--provider", cfg.Provider.Type))
	}
	check, err := storage.EvaluateCapacity(storage.Surface{
		ID:       "project",
		Provider: cfg.Provider.Type,
		Kind:     storage.SurfaceHostFilesystem,
		Location: projectRoot,
		Capacity: capacity,
	}, storage.Requirement{
		ID:               "controller-bootstrap",
		Provider:         cfg.Provider.Type,
		SurfaceID:        "project",
		MinimumFreeBytes: minimumFree,
	})
	if err != nil {
		return fmt.Errorf("evaluate controller storage capacity: %w", err)
	}
	if check.Status != storage.CapacityReady {
		return storage.CapacityAdmissionError("initialize the EPAR controller", storage.Surface{
			Location: projectRoot,
			Capacity: check.Capacity,
		}, check.Requirement, check, invocation.Command("storage", "prune", "--provider", cfg.Provider.Type))
	}
	return nil
}

func rejectDockerSandboxesImageCommand(manager *pool.Manager, command string) error {
	if manager.Config.Provider.Type != "docker-sandboxes" {
		return nil
	}
	return fmt.Errorf("%s is not applicable to docker-sandboxes; edit image.sourceImage or image.customInstallScripts, then run %s", command, invocation.Command("image", "build"))
}

func loggingSinks(values []string) logging.Sinks {
	var sinks logging.Sinks
	for _, value := range values {
		switch value {
		case "console":
			sinks |= logging.SinkConsole
		case "file":
			sinks |= logging.SinkFile
		}
	}
	return sinks
}

func printConfigWarnings(cfg config.Config) {
	for _, warning := range cfg.Warnings() {
		fmt.Fprintln(os.Stderr, "warning:", warning)
	}
}

func resolveConfigPath(projectRoot, explicit string) (string, error) {
	if explicit != "" {
		return config.ProjectPath(projectRoot, explicit), nil
	}
	if envPath := os.Getenv("EPAR_CONFIG"); envPath != "" {
		return config.ProjectPath(projectRoot, envPath), nil
	}
	localPath := filepath.Join(projectRoot, ".local", "config.yml")
	if _, err := os.Stat(localPath); err == nil {
		return localPath, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	if home, err := os.UserHomeDir(); err == nil {
		homePath := filepath.Join(home, ".config", "ephemeral-action-runner", "config.yml")
		if _, err := os.Stat(homePath); err == nil {
			return homePath, nil
		} else if !os.IsNotExist(err) {
			return "", err
		}
	}
	return "", nil
}

func interruptContext() context.Context {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ctx.Done()
		cancel()
	}()
	return ctx
}

func usage() {
	fmt.Print(`ephemeral-action-runner (EPAR) manages ephemeral GitHub Actions runners on local providers.

Commands:
  ephemeral-action-runner
  ephemeral-action-runner start [--instances N] [--config .local/config.yml]
  ephemeral-action-runner init
  ephemeral-action-runner image update-upstream [--config .local/config.yml]
  ephemeral-action-runner image build [--replace] [--update-upstream]
  ephemeral-action-runner image refresh-scripts
  ephemeral-action-runner pool verify --instances 2 --register-only --cleanup
  ephemeral-action-runner pool up --instances 2 [--keep-on-exit] [--replace-completed=false]
  ephemeral-action-runner pool down
  ephemeral-action-runner cleanup
  ephemeral-action-runner status
  ephemeral-action-runner logs path
  ephemeral-action-runner logs list
  ephemeral-action-runner logs prune [--dry-run]
  ephemeral-action-runner storage status [--provider NAME] [--json]
  ephemeral-action-runner storage prune [--provider NAME] [--json] [--execute]
  ephemeral-action-runner version
`)
}
