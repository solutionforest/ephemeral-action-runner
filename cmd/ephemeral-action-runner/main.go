package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
	gh "github.com/solutionforest/ephemeral-action-runner/internal/github"
	"github.com/solutionforest/ephemeral-action-runner/internal/pool"
	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
	dockerdindprovider "github.com/solutionforest/ephemeral-action-runner/internal/provider/dockerdind"
	tartprovider "github.com/solutionforest/ephemeral-action-runner/internal/provider/tart"
	wslprovider "github.com/solutionforest/ephemeral-action-runner/internal/provider/wsl"
)

const binaryName = "ephemeral-action-runner"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, binaryName+":", err)
		if reportPath := writeLastErrorReport(err); reportPath != "" {
			fmt.Fprintln(os.Stderr, "error report:", reportPath)
		}
		os.Exit(1)
	}
}

func writeLastErrorReport(runErr error) string {
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	logDir := filepath.Join(cwd, "work", "logs")
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return ""
	}
	now := time.Now().UTC()
	content := fmt.Sprintf(`EPAR failed
time: %s
workingDirectory: %s
command: %q

%s
error: %v
`, now.Format(time.RFC3339), cwd, os.Args, versionString(), runErr)

	lastPath := filepath.Join(logDir, "epar-last-error.log")
	if err := os.WriteFile(lastPath, []byte(content), 0644); err != nil {
		return ""
	}
	stampedPath := filepath.Join(logDir, fmt.Sprintf("epar-%s-error.log", now.Format("20060102-150405")))
	_ = os.WriteFile(stampedPath, []byte(content), 0644)
	return lastPath
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

func runImage(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("image requires subcommand: update-upstream or build")
	}
	switch args[0] {
	case "update-upstream":
		fs := flag.NewFlagSet("image update-upstream", flag.ExitOnError)
		common := addCommonFlags(fs)
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		m, err := newManager(*common.configPath, *common.projectRoot, *common.dryRun, false)
		if err != nil {
			return err
		}
		return m.UpdateUpstream(context.Background())
	case "build":
		fs := flag.NewFlagSet("image build", flag.ExitOnError)
		common := addCommonFlags(fs)
		replace := fs.Bool("replace", false, "delete an existing output image before building")
		update := fs.Bool("update-upstream", false, "refresh runner-images before building")
		skipUpstream := fs.Bool("skip-upstream-check", false, "skip checking the runner-images checkout")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		m, err := newManager(*common.configPath, *common.projectRoot, *common.dryRun, false)
		if err != nil {
			return err
		}
		ctx := interruptContext()
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
		cleanup := fs.Bool("cleanup", false, "clean up prefixed instances and GitHub runners after verification")
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
		return m.Verify(interruptContext(), pool.VerifyOptions{Instances: *instances, RegisterOnly: *registerOnly, Cleanup: *cleanup})
	case "up":
		fs := flag.NewFlagSet("pool up", flag.ExitOnError)
		common := addCommonFlags(fs)
		instances := fs.Int("instances", 0, "number of instances to create; overrides pool.instances")
		register := fs.Bool("register", true, "register the instances as GitHub ephemeral runners")
		keepOnExit := fs.Bool("keep-on-exit", false, "leave prefixed instances and GitHub runners running when interrupted")
		replaceCompleted := fs.Bool("replace-completed", true, "replace an instance when its ephemeral runner exits after a job")
		monitorInterval := fs.Duration("monitor-interval", 15*time.Second, "interval for runner liveness checks")
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
	if err := fs.Parse(args); err != nil {
		return err
	}
	m, err := newManager(*common.configPath, *common.projectRoot, *common.dryRun, !*noGitHub)
	if err != nil {
		return err
	}
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
	if err := config.Validate(cfg); err != nil {
		return nil, err
	}
	var client pool.GitHubClient
	if githubEnabled && !dryRun {
		if err := config.ValidateGitHub(cfg); err != nil {
			return nil, err
		}
		client = gh.New(cfg.GitHub)
	}
	provider, err := newProvider(cfg, projectRoot, dryRun)
	if err != nil {
		return nil, err
	}
	return &pool.Manager{
		Config:      cfg,
		Provider:    provider,
		GitHub:      client,
		ProjectRoot: projectRoot,
		DryRun:      dryRun,
	}, nil
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

func newProvider(cfg config.Config, projectRoot string, dryRun bool) (provider.Provider, error) {
	switch cfg.Provider.Type {
	case "tart":
		return tartprovider.New("", dryRun), nil
	case "wsl":
		return wslprovider.New("", config.ProjectPath(projectRoot, cfg.Provider.InstallRoot), projectRoot, dryRun), nil
	case "docker-dind":
		hostGateway := config.DockerConfigNeedsHostGateway(cfg.Docker)
		environment := map[string]string{
			"HTTP_PROXY":  cfg.Docker.HTTPProxy,
			"HTTPS_PROXY": cfg.Docker.HTTPSProxy,
			"NO_PROXY":    cfg.Docker.NoProxy,
		}
		return dockerdindprovider.NewWithOptions("", cfg.Provider.Platform, hostGateway, environment, dryRun), nil
	default:
		return nil, provider.UnsupportedTypeError(cfg.Provider.Type)
	}
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
  ephemeral-action-runner version
`)
}
