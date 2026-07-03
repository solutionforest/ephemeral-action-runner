package pool

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
)

type ImageBuildOptions struct {
	Replace           bool
	SkipUpstreamCheck bool
}

func (m *Manager) UpdateUpstream(ctx context.Context) error {
	dir := config.ProjectPath(m.ProjectRoot, m.Config.Image.UpstreamDir)
	fmt.Printf("updating runner-images checkout at %s\n", dir)
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		if err := runHost(ctx, "git", "-C", dir, "fetch", "--depth", "1", "origin", "main"); err != nil {
			return err
		}
		if err := runHost(ctx, "git", "-C", dir, "checkout", "FETCH_HEAD"); err != nil {
			return err
		}
	} else {
		if err := os.MkdirAll(filepath.Dir(dir), 0755); err != nil {
			return err
		}
		if err := runHost(ctx, "git", "clone", "--depth", "1", "https://github.com/actions/runner-images.git", dir); err != nil {
			return err
		}
	}
	commitBytes, err := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		return err
	}
	lockPath := config.ProjectPath(m.ProjectRoot, m.Config.Image.UpstreamLock)
	if err := os.MkdirAll(filepath.Dir(lockPath), 0755); err != nil {
		return err
	}
	commit := strings.TrimSpace(string(commitBytes))
	if err := os.WriteFile(lockPath, []byte(commit+"\n"), 0644); err != nil {
		return err
	}
	fmt.Printf("runner-images pinned at %s\n", commit)
	fmt.Printf("lock file written to %s\n", lockPath)
	return nil
}

func (m *Manager) BuildImage(ctx context.Context, opts ImageBuildOptions) error {
	upstreamDir := config.ProjectPath(m.ProjectRoot, m.Config.Image.UpstreamDir)
	if !opts.SkipUpstreamCheck {
		if _, err := os.Stat(filepath.Join(upstreamDir, "images", "ubuntu", "scripts", "build", "install-docker.sh")); err != nil {
			return fmt.Errorf("runner-images checkout missing; run `ephemeral-action-runner image update-upstream` first: %w", err)
		}
	}
	buildLogPath := filepath.Join(config.ProjectPath(m.ProjectRoot, m.Config.Pool.LogDir), m.Config.Image.OutputImage+".build.log")
	guestLogPath := filepath.Join(config.ProjectPath(m.ProjectRoot, m.Config.Pool.LogDir), m.Config.Image.OutputImage+".guest.log")
	if err := resetLogs(buildLogPath, guestLogPath); err != nil {
		return err
	}
	fmt.Printf("building Tart image %s from %s\n", m.Config.Image.OutputImage, m.Config.Image.SourceImage)
	fmt.Printf("logs: %s, %s\n", buildLogPath, guestLogPath)
	if opts.Replace {
		fmt.Printf("replacing existing Tart image %s if present\n", m.Config.Image.OutputImage)
		_ = m.Provider.Stop(ctx, m.Config.Image.OutputImage)
		_ = m.Provider.Delete(ctx, m.Config.Image.OutputImage)
	}
	fmt.Printf("cloning source image\n")
	if err := m.Provider.Clone(ctx, m.Config.Image.SourceImage, m.Config.Image.OutputImage); err != nil {
		return err
	}
	fmt.Printf("starting image with Tart network mode %q\n", m.Config.Provider.Network)
	if _, err := m.Provider.Start(ctx, m.Config.Image.OutputImage, provider.StartOptions{Network: m.Config.Provider.Network, LogPath: buildLogPath}); err != nil {
		return err
	}
	ip, err := m.Provider.IP(ctx, m.Config.Image.OutputImage, m.Config.Timeouts.BootSeconds)
	if err != nil {
		return err
	}
	fmt.Printf("guest reachable at %s\n", ip)
	fmt.Printf("copying guest scripts\n")
	if err := m.installGuestScripts(ctx, m.Config.Image.OutputImage); err != nil {
		return err
	}
	fmt.Printf("copying runner-images script subset\n")
	if err := m.copyRunnerImagesSubset(ctx, m.Config.Image.OutputImage, upstreamDir); err != nil {
		return err
	}
	fmt.Printf("installing base runtime: Docker and browser support\n")
	if _, err := m.execGuest(ctx, m.Config.Image.OutputImage, []string{"sudo", "bash", "/opt/epar/install-base.sh", "/opt/epar/upstream/runner-images"}, provider.ExecOptions{}); err != nil {
		return err
	}
	fmt.Printf("installing GitHub Actions runner\n")
	if _, err := m.execGuest(ctx, m.Config.Image.OutputImage, []string{"sudo", "bash", "/opt/epar/install-runner.sh", m.Config.Image.RunnerVersion}, provider.ExecOptions{}); err != nil {
		return err
	}
	fmt.Printf("validating Docker and browser inside the instance\n")
	if err := m.validateRuntime(ctx, m.Config.Image.OutputImage); err != nil {
		return err
	}
	fmt.Printf("finalizing image for clean Tart clones\n")
	if _, err := m.execGuest(ctx, m.Config.Image.OutputImage, []string{"sudo", "bash", "/opt/epar/finalize-image.sh"}, provider.ExecOptions{}); err != nil {
		return err
	}
	fmt.Printf("stopping image\n")
	if err := m.Provider.Stop(ctx, m.Config.Image.OutputImage); err != nil {
		return err
	}
	fmt.Printf("image build complete: %s is available in `tart list`\n", m.Config.Image.OutputImage)
	return nil
}

func (m *Manager) RefreshScripts(ctx context.Context) error {
	logPath := filepath.Join(config.ProjectPath(m.ProjectRoot, m.Config.Pool.LogDir), m.Config.Image.OutputImage+".refresh.log")
	fmt.Printf("refreshing guest scripts in Tart image %s\n", m.Config.Image.OutputImage)
	fmt.Printf("log: %s\n", logPath)
	if _, err := m.Provider.Start(ctx, m.Config.Image.OutputImage, provider.StartOptions{Network: m.Config.Provider.Network, LogPath: logPath}); err != nil {
		return err
	}
	shouldStop := true
	defer func() {
		if shouldStop {
			stopCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			_ = m.Provider.Stop(stopCtx, m.Config.Image.OutputImage)
		}
	}()
	if _, err := m.Provider.IP(ctx, m.Config.Image.OutputImage, m.Config.Timeouts.BootSeconds); err != nil {
		return err
	}
	if err := m.installGuestScripts(ctx, m.Config.Image.OutputImage); err != nil {
		return err
	}
	if _, err := m.execGuest(ctx, m.Config.Image.OutputImage, []string{"sudo", "bash", "/opt/epar/finalize-image.sh"}, provider.ExecOptions{}); err != nil {
		return err
	}
	if _, err := m.Provider.Exec(ctx, m.Config.Image.OutputImage, provider.ShellCommand("sync"), provider.ExecOptions{LogPath: logPath}); err != nil {
		return err
	}
	shouldStop = false
	if err := m.Provider.Stop(ctx, m.Config.Image.OutputImage); err != nil {
		return err
	}
	fmt.Printf("script refresh complete: %s is available in `tart list`\n", m.Config.Image.OutputImage)
	return nil
}

func (m *Manager) installGuestScripts(ctx context.Context, vmName string) error {
	scriptDir := filepath.Join(m.ProjectRoot, "scripts", "guest", "ubuntu")
	entries, err := os.ReadDir(scriptDir)
	if err != nil {
		return err
	}
	if _, err := m.Provider.Exec(ctx, vmName, provider.ShellCommand("sudo mkdir -p /opt/epar"), provider.ExecOptions{}); err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sh") {
			continue
		}
		path := filepath.Join(scriptDir, entry.Name())
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := provider.CopyText(ctx, m.Provider, vmName, "/opt/epar/"+entry.Name(), "0755", string(content)); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) copyRunnerImagesSubset(ctx context.Context, vmName, upstreamDir string) error {
	type copyRoot struct {
		host  string
		guest string
	}
	roots := []copyRoot{
		{
			host:  filepath.Join(upstreamDir, "images", "ubuntu", "scripts", "helpers"),
			guest: "/opt/epar/upstream/runner-images/images/ubuntu/scripts/helpers",
		},
		{
			host:  filepath.Join(upstreamDir, "images", "ubuntu", "toolsets"),
			guest: "/opt/epar/upstream/runner-images/images/ubuntu/toolsets",
		},
	}
	for _, root := range roots {
		if _, err := m.Provider.Exec(ctx, vmName, provider.ShellCommand("sudo mkdir -p "+shellQuote(root.guest)), provider.ExecOptions{}); err != nil {
			return err
		}
		if _, err := os.Stat(root.host); err != nil {
			if m.DryRun && os.IsNotExist(err) {
				fmt.Printf("[dry-run] skipping missing runner-images path %s\n", root.host)
				continue
			}
			return err
		}
		if err := filepath.WalkDir(root.host, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			if root.guest != "" && strings.Contains(path, "/.git/") {
				return nil
			}
			rel, err := filepath.Rel(root.host, path)
			if err != nil {
				return err
			}
			guestPath := filepath.ToSlash(filepath.Join(root.guest, rel))
			content, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if _, err := m.Provider.Exec(ctx, vmName, provider.ShellCommand("sudo mkdir -p "+shellQuote(filepath.ToSlash(filepath.Dir(guestPath)))), provider.ExecOptions{}); err != nil {
				return err
			}
			return provider.CopyText(ctx, m.Provider, vmName, guestPath, "0644", string(content))
		}); err != nil {
			return err
		}
	}
	buildGuestDir := "/opt/epar/upstream/runner-images/images/ubuntu/scripts/build"
	if _, err := m.Provider.Exec(ctx, vmName, provider.ShellCommand("sudo mkdir -p "+shellQuote(buildGuestDir)), provider.ExecOptions{}); err != nil {
		return err
	}
	for _, name := range []string{"install-docker.sh", "install-google-chrome.sh"} {
		hostPath := filepath.Join(upstreamDir, "images", "ubuntu", "scripts", "build", name)
		content, err := os.ReadFile(hostPath)
		if err != nil {
			if m.DryRun && os.IsNotExist(err) {
				fmt.Printf("[dry-run] skipping missing runner-images file %s\n", hostPath)
				continue
			}
			return err
		}
		if err := provider.CopyText(ctx, m.Provider, vmName, buildGuestDir+"/"+name, "0644", string(content)); err != nil {
			return err
		}
	}
	return nil
}

func runHost(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func resetLogs(paths ...string) error {
	for _, path := range paths {
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return err
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
