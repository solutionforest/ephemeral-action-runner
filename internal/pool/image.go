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

type wslExporter interface {
	Export(ctx context.Context, name, outputPath string) error
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
	if !opts.SkipUpstreamCheck && m.needsRunnerImagesSubset() {
		for _, name := range m.runnerImageBuildScripts() {
			if _, err := os.Stat(filepath.Join(upstreamDir, "images", "ubuntu", "scripts", "build", name)); err != nil {
				return fmt.Errorf("runner-images checkout missing script %s; run `ephemeral-action-runner image update-upstream` first: %w", name, err)
			}
		}
	}
	switch m.Config.Provider.Type {
	case "tart":
		return m.buildTartImage(ctx, opts, upstreamDir)
	case "wsl":
		return m.buildWSLImage(ctx, opts, upstreamDir)
	default:
		return fmt.Errorf("unsupported provider.type %q", m.Config.Provider.Type)
	}
}

func (m *Manager) buildTartImage(ctx context.Context, opts ImageBuildOptions, upstreamDir string) error {
	buildLogPath := filepath.Join(config.ProjectPath(m.ProjectRoot, m.Config.Pool.LogDir), imageLogStem(m.Config.Image.OutputImage)+".build.log")
	guestLogPath := filepath.Join(config.ProjectPath(m.ProjectRoot, m.Config.Pool.LogDir), imageLogStem(m.Config.Image.OutputImage)+".guest.log")
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
	if m.needsRunnerImagesSubset() {
		fmt.Printf("copying runner-images script subset\n")
		if err := m.copyRunnerImagesSubset(ctx, m.Config.Image.OutputImage, upstreamDir); err != nil {
			return err
		}
	} else {
		fmt.Printf("skipping runner-images script subset; no selected install script requires it\n")
	}
	fmt.Printf("installing base runner runtime\n")
	if _, err := m.execGuest(ctx, m.Config.Image.OutputImage, []string{"sudo", "bash", "/opt/epar/install-base.sh", "/opt/epar/upstream/runner-images"}, provider.ExecOptions{}); err != nil {
		return err
	}
	fmt.Printf("installing GitHub Actions runner\n")
	if _, err := m.execGuest(ctx, m.Config.Image.OutputImage, []string{"sudo", "bash", "/opt/epar/install-runner.sh", m.Config.Image.RunnerVersion}, provider.ExecOptions{}); err != nil {
		return err
	}
	if err := m.installCustomInstallScripts(ctx, m.Config.Image.OutputImage); err != nil {
		return err
	}
	fmt.Printf("validating runner runtime inside the instance\n")
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

func (m *Manager) buildWSLImage(ctx context.Context, opts ImageBuildOptions, upstreamDir string) error {
	exporter, ok := m.Provider.(wslExporter)
	if !ok {
		return fmt.Errorf("provider.type=wsl requires provider export support")
	}
	sourcePath := config.ProjectPath(m.ProjectRoot, m.Config.Image.SourceImage)
	outputPath := config.ProjectPath(m.ProjectRoot, m.Config.Image.OutputImage)
	if !m.DryRun {
		if _, err := os.Stat(sourcePath); err != nil {
			return fmt.Errorf("wsl source image %s: %w", sourcePath, err)
		}
		if _, err := os.Stat(outputPath); err == nil && !opts.Replace {
			return fmt.Errorf("wsl output image %s already exists; rerun with --replace", outputPath)
		} else if err != nil && !os.IsNotExist(err) {
			return err
		}
		if opts.Replace {
			if err := os.Remove(outputPath); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	buildName := RunnerName(m.Config.Pool.NamePrefix+"-image", 1, time.Now())
	buildLogPath := filepath.Join(config.ProjectPath(m.ProjectRoot, m.Config.Pool.LogDir), imageLogStem(m.Config.Image.OutputImage)+".wsl-build.log")
	guestLogPath := filepath.Join(config.ProjectPath(m.ProjectRoot, m.Config.Pool.LogDir), buildName+".guest.log")
	if err := resetLogs(buildLogPath, guestLogPath); err != nil {
		return err
	}
	fmt.Printf("building WSL image %s from %s using temporary distro %s\n", outputPath, sourcePath, buildName)
	fmt.Printf("logs: %s, %s\n", buildLogPath, guestLogPath)
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		_ = m.Provider.Stop(cleanupCtx, buildName)
		_ = m.Provider.Delete(cleanupCtx, buildName)
	}()
	fmt.Printf("importing source rootfs\n")
	if err := m.Provider.Clone(ctx, m.Config.Image.SourceImage, buildName); err != nil {
		return err
	}
	fmt.Printf("enabling WSL systemd\n")
	if err := m.enableWSLSystemd(ctx, buildName); err != nil {
		return err
	}
	fmt.Printf("restarting temporary distro for systemd\n")
	if err := m.Provider.Stop(ctx, buildName); err != nil {
		return err
	}
	if _, err := m.Provider.Start(ctx, buildName, provider.StartOptions{Network: m.Config.Provider.Network, LogPath: buildLogPath}); err != nil {
		return err
	}
	ip, err := m.Provider.IP(ctx, buildName, m.Config.Timeouts.BootSeconds)
	if err != nil {
		return err
	}
	fmt.Printf("guest reachable at %s\n", ip)
	if err := m.waitForSystemd(ctx, buildName); err != nil {
		return err
	}
	fmt.Printf("copying guest scripts\n")
	if err := m.installGuestScripts(ctx, buildName); err != nil {
		return err
	}
	if m.needsRunnerImagesSubset() {
		fmt.Printf("copying runner-images script subset\n")
		if err := m.copyRunnerImagesSubset(ctx, buildName, upstreamDir); err != nil {
			return err
		}
	} else {
		fmt.Printf("skipping runner-images script subset; no selected install script requires it\n")
	}
	fmt.Printf("installing base runner runtime\n")
	if _, err := m.execGuest(ctx, buildName, []string{"sudo", "bash", "/opt/epar/install-base.sh", "/opt/epar/upstream/runner-images"}, provider.ExecOptions{}); err != nil {
		return err
	}
	fmt.Printf("installing GitHub Actions runner\n")
	if _, err := m.execGuest(ctx, buildName, []string{"sudo", "bash", "/opt/epar/install-runner.sh", m.Config.Image.RunnerVersion}, provider.ExecOptions{}); err != nil {
		return err
	}
	if err := m.installCustomInstallScripts(ctx, buildName); err != nil {
		return err
	}
	fmt.Printf("validating runner runtime inside the distro\n")
	if err := m.validateRuntime(ctx, buildName); err != nil {
		return err
	}
	fmt.Printf("finalizing image for clean WSL imports\n")
	if _, err := m.execGuest(ctx, buildName, []string{"sudo", "bash", "/opt/epar/finalize-image.sh"}, provider.ExecOptions{}); err != nil {
		return err
	}
	if err := m.Provider.Stop(ctx, buildName); err != nil {
		return err
	}
	fmt.Printf("exporting reusable WSL image to %s\n", outputPath)
	if err := exporter.Export(ctx, buildName, m.Config.Image.OutputImage); err != nil {
		return err
	}
	fmt.Printf("image build complete: %s is available for WSL imports\n", outputPath)
	return nil
}

func (m *Manager) RefreshScripts(ctx context.Context) error {
	switch m.Config.Provider.Type {
	case "tart":
		return m.refreshTartScripts(ctx)
	case "wsl":
		return m.refreshWSLScripts(ctx)
	default:
		return fmt.Errorf("unsupported provider.type %q", m.Config.Provider.Type)
	}
}

func (m *Manager) refreshTartScripts(ctx context.Context) error {
	logPath := filepath.Join(config.ProjectPath(m.ProjectRoot, m.Config.Pool.LogDir), imageLogStem(m.Config.Image.OutputImage)+".refresh.log")
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

func (m *Manager) refreshWSLScripts(ctx context.Context) error {
	exporter, ok := m.Provider.(wslExporter)
	if !ok {
		return fmt.Errorf("provider.type=wsl requires provider export support")
	}
	imagePath := config.ProjectPath(m.ProjectRoot, m.Config.Image.OutputImage)
	if !m.DryRun {
		if _, err := os.Stat(imagePath); err != nil {
			return fmt.Errorf("wsl image %s: %w", imagePath, err)
		}
	}
	name := RunnerName(m.Config.Pool.NamePrefix+"-refresh", 1, time.Now())
	logPath := filepath.Join(config.ProjectPath(m.ProjectRoot, m.Config.Pool.LogDir), imageLogStem(m.Config.Image.OutputImage)+".wsl-refresh.log")
	fmt.Printf("refreshing guest scripts in WSL image %s using temporary distro %s\n", imagePath, name)
	fmt.Printf("log: %s\n", logPath)
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		_ = m.Provider.Stop(cleanupCtx, name)
		_ = m.Provider.Delete(cleanupCtx, name)
	}()
	if err := m.Provider.Clone(ctx, m.Config.Image.OutputImage, name); err != nil {
		return err
	}
	if _, err := m.Provider.Start(ctx, name, provider.StartOptions{Network: m.Config.Provider.Network, LogPath: logPath}); err != nil {
		return err
	}
	if _, err := m.Provider.IP(ctx, name, m.Config.Timeouts.BootSeconds); err != nil {
		return err
	}
	if err := m.waitForSystemd(ctx, name); err != nil {
		return err
	}
	if err := m.installGuestScripts(ctx, name); err != nil {
		return err
	}
	if _, err := m.execGuest(ctx, name, []string{"sudo", "bash", "/opt/epar/finalize-image.sh"}, provider.ExecOptions{}); err != nil {
		return err
	}
	if err := m.Provider.Stop(ctx, name); err != nil {
		return err
	}
	if err := exporter.Export(ctx, name, m.Config.Image.OutputImage); err != nil {
		return err
	}
	fmt.Printf("script refresh complete: %s is available for WSL imports\n", imagePath)
	return nil
}

func (m *Manager) installGuestScripts(ctx context.Context, vmName string) error {
	scriptDir := filepath.Join(m.ProjectRoot, "scripts", "guest", "ubuntu")
	entries, err := os.ReadDir(scriptDir)
	if err != nil {
		return err
	}
	if _, err := m.Provider.Exec(ctx, vmName, provider.ShellCommand("if command -v sudo >/dev/null 2>&1; then sudo mkdir -p /opt/epar; else mkdir -p /opt/epar; fi"), provider.ExecOptions{}); err != nil {
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
		if err := provider.CopyText(ctx, m.Provider, vmName, "/opt/epar/"+entry.Name(), "0755", guestText(content)); err != nil {
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
		if _, err := m.Provider.Exec(ctx, vmName, provider.ShellCommand(mkdirGuestCommand(root.guest)), provider.ExecOptions{}); err != nil {
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
			if _, err := m.Provider.Exec(ctx, vmName, provider.ShellCommand(mkdirGuestCommand(filepath.ToSlash(filepath.Dir(guestPath)))), provider.ExecOptions{}); err != nil {
				return err
			}
			return provider.CopyText(ctx, m.Provider, vmName, guestPath, "0644", guestText(content))
		}); err != nil {
			return err
		}
	}
	buildGuestDir := "/opt/epar/upstream/runner-images/images/ubuntu/scripts/build"
	if _, err := m.Provider.Exec(ctx, vmName, provider.ShellCommand(mkdirGuestCommand(buildGuestDir)), provider.ExecOptions{}); err != nil {
		return err
	}
	for _, name := range m.runnerImageBuildScripts() {
		hostPath := filepath.Join(upstreamDir, "images", "ubuntu", "scripts", "build", name)
		content, err := os.ReadFile(hostPath)
		if err != nil {
			if m.DryRun && os.IsNotExist(err) {
				fmt.Printf("[dry-run] skipping missing runner-images file %s\n", hostPath)
				continue
			}
			return err
		}
		if err := provider.CopyText(ctx, m.Provider, vmName, buildGuestDir+"/"+name, "0644", guestText(content)); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) runnerImageBuildScripts() []string {
	return []string{"install-docker.sh", "install-google-chrome.sh", "install-nodejs.sh"}
}

func (m *Manager) needsRunnerImagesSubset() bool {
	for _, script := range m.Config.Image.CustomInstallScripts {
		normalized := m.normalizedCustomInstallScript(script)
		switch normalized {
		case "scripts/guest/ubuntu/install-docker-browser.sh",
			"scripts/guest/ubuntu/install-web-e2e.sh":
			return true
		}
	}
	return false
}

func (m *Manager) normalizedCustomInstallScript(script string) string {
	script = strings.TrimSpace(script)
	if script == "" {
		return ""
	}
	if filepath.IsAbs(script) && m.ProjectRoot != "" {
		root, rootErr := filepath.Abs(m.ProjectRoot)
		path, pathErr := filepath.Abs(script)
		if rootErr == nil && pathErr == nil {
			if rel, err := filepath.Rel(root, path); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel) {
				script = rel
			}
		}
	}
	return filepath.ToSlash(filepath.Clean(script))
}

func (m *Manager) installCustomInstallScripts(ctx context.Context, vmName string) error {
	scripts := m.Config.Image.CustomInstallScripts
	if len(scripts) == 0 {
		return nil
	}
	fmt.Printf("running %d image install script(s)\n", len(scripts))
	if _, err := m.execGuest(ctx, vmName, provider.ShellCommand("if command -v sudo >/dev/null 2>&1; then sudo mkdir -p /opt/epar/custom-install; else mkdir -p /opt/epar/custom-install; fi"), provider.ExecOptions{}); err != nil {
		return err
	}
	for i, script := range scripts {
		hostPath, err := m.customInstallScriptHostPath(script)
		if err != nil {
			return err
		}
		content, err := os.ReadFile(hostPath)
		if err != nil {
			return fmt.Errorf("read custom install script %s: %w", hostPath, err)
		}
		guestPath := fmt.Sprintf("/opt/epar/custom-install/%03d-%s", i+1, guestScriptName(filepath.Base(hostPath)))
		if err := provider.CopyText(ctx, m.Provider, vmName, guestPath, "0755", guestText(content)); err != nil {
			return err
		}
		fmt.Printf("running image install script %d/%d: %s\n", i+1, len(scripts), script)
		if _, err := m.execGuest(ctx, vmName, []string{"sudo", "bash", guestPath}, provider.ExecOptions{}); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) customInstallScriptHostPath(script string) (string, error) {
	script = strings.TrimSpace(script)
	if script == "" {
		return "", fmt.Errorf("custom install script path is empty")
	}
	if filepath.IsAbs(script) {
		return filepath.Clean(script), nil
	}
	root, err := filepath.Abs(m.ProjectRoot)
	if err != nil {
		return "", err
	}
	path, err := filepath.Abs(filepath.Join(root, script))
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("relative custom install script %q escapes project root", script)
	}
	return path, nil
}

func guestScriptName(name string) string {
	if name == "" || name == "." || name == string(filepath.Separator) {
		return "install.sh"
	}
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "install.sh"
	}
	return b.String()
}

func (m *Manager) enableWSLSystemd(ctx context.Context, name string) error {
	content := "[boot]\nsystemd=true\n\n[interop]\nappendWindowsPath=false\n\n[user]\ndefault=root\n"
	_, err := m.Provider.Exec(ctx, name, provider.ShellCommand("mkdir -p /etc && cat >/etc/wsl.conf"), provider.ExecOptions{Stdin: content})
	return err
}

func (m *Manager) waitForSystemd(ctx context.Context, name string) error {
	waitSeconds := m.Config.Timeouts.BootSeconds
	if waitSeconds <= 0 {
		waitSeconds = 180
	}
	deadline := time.Now().Add(time.Duration(waitSeconds) * time.Second)
	var lastErr error
	for {
		result, err := m.Provider.Exec(ctx, name, provider.ShellCommand(`test "$(ps -p 1 -o comm=)" = systemd && state="$(systemctl is-system-running 2>/dev/null || true)" && case "$state" in running|degraded) echo "$state"; exit 0 ;; *) echo "$state"; exit 1 ;; esac`), provider.ExecOptions{
			LogPath: filepath.Join(config.ProjectPath(m.ProjectRoot, m.Config.Pool.LogDir), name+".guest.log"),
		})
		if err == nil {
			fmt.Printf("systemd is %s\n", strings.TrimSpace(result.Stdout))
			return nil
		}
		lastErr = err
		if time.Now().After(deadline) {
			return fmt.Errorf("systemd did not become ready in WSL distro %s within %d seconds: %w", name, waitSeconds, lastErr)
		}
		if err := sleepContext(ctx, 2*time.Second); err != nil {
			return err
		}
	}
}

func mkdirGuestCommand(path string) string {
	return "if command -v sudo >/dev/null 2>&1; then sudo mkdir -p " + shellQuote(path) + "; else mkdir -p " + shellQuote(path) + "; fi"
}

func imageLogStem(image string) string {
	stem := strings.ReplaceAll(filepath.ToSlash(image), "/", "-")
	stem = strings.ReplaceAll(stem, ":", "")
	if stem == "" {
		return "image"
	}
	return stem
}

func guestText(content []byte) string {
	return strings.ReplaceAll(string(content), "\r\n", "\n")
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
