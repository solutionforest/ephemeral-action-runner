package wsl

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
)

type Provider struct {
	Binary      string
	InstallRoot string
	ProjectRoot string
	DryRun      bool
	runCommand  runCommandFunc
}

type runCommandFunc func(ctx context.Context, stdin io.Reader, logPath string, args ...string) (provider.ExecResult, error)

func New(binary, installRoot, projectRoot string, dryRun bool) *Provider {
	if binary == "" {
		binary = "wsl.exe"
	}
	if projectRoot == "" {
		projectRoot, _ = os.Getwd()
	}
	if installRoot == "" {
		installRoot = "work/wsl"
	}
	if !filepath.IsAbs(installRoot) {
		installRoot = filepath.Join(projectRoot, installRoot)
	}
	return &Provider{
		Binary:      binary,
		InstallRoot: filepath.Clean(installRoot),
		ProjectRoot: projectRoot,
		DryRun:      dryRun,
	}
}

func (p *Provider) Clone(ctx context.Context, source, name string) error {
	sourcePath := p.projectPath(source)
	installDir, err := p.instanceDir(name)
	if err != nil {
		return err
	}
	if !p.DryRun {
		if _, err := os.Stat(sourcePath); err != nil {
			return fmt.Errorf("wsl source image %s: %w", sourcePath, err)
		}
		if err := os.MkdirAll(installDir, 0755); err != nil {
			return err
		}
	}
	_, err = p.run(ctx, nil, p.cloneArgs(sourcePath, name, installDir)...)
	return err
}

func (p *Provider) Start(ctx context.Context, name string, opts provider.StartOptions) (*provider.RunningProcess, error) {
	switch opts.Network {
	case "", "default":
	default:
		return nil, fmt.Errorf("unsupported wsl network mode %q", opts.Network)
	}
	keeper, err := p.startKeepAlive(name, opts.LogPath)
	if err != nil {
		return nil, err
	}
	_, err = p.Exec(ctx, name, []string{"/bin/sh", "-c", "true"}, provider.ExecOptions{LogPath: opts.LogPath})
	if err != nil {
		if keeper != nil && keeper.Process != nil {
			_ = keeper.Process.Kill()
		}
		return nil, err
	}
	pid := 0
	if keeper != nil && keeper.Process != nil {
		pid = keeper.Process.Pid
	}
	return &provider.RunningProcess{Name: name, PID: pid, LogPath: opts.LogPath}, nil
}

func (p *Provider) Exec(ctx context.Context, name string, command []string, opts provider.ExecOptions) (provider.ExecResult, error) {
	var stdin io.Reader
	if opts.Stdin != "" {
		stdin = strings.NewReader(opts.Stdin)
	}
	return p.runWithLog(ctx, stdin, opts.LogPath, p.execArgs(name, command, opts.Env)...)
}

func (p *Provider) IP(ctx context.Context, name string, waitSeconds int) (string, error) {
	if p.DryRun && p.runCommand == nil {
		fmt.Printf("[dry-run] %s -d %s --user root --exec /bin/sh -c hostname -I 2>/dev/null || hostname -i 2>/dev/null || echo 127.0.0.1\n", p.Binary, name)
		return "127.0.0.1", nil
	}
	if waitSeconds <= 0 {
		waitSeconds = 1
	}
	deadline := time.Now().Add(time.Duration(waitSeconds) * time.Second)
	var lastErr error
	for {
		result, err := p.Exec(ctx, name, []string{"/bin/sh", "-c", "hostname -I 2>/dev/null || hostname -i 2>/dev/null || echo 127.0.0.1"}, provider.ExecOptions{})
		if err == nil {
			fields := strings.Fields(result.Stdout)
			if len(fields) > 0 {
				return fields[0], nil
			}
		} else {
			lastErr = err
		}
		if time.Now().After(deadline) {
			if lastErr != nil {
				return "", lastErr
			}
			return "", fmt.Errorf("wsl distro %q did not report an IP within %d seconds", name, waitSeconds)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func (p *Provider) Stop(ctx context.Context, name string) error {
	result, err := p.run(ctx, nil, "--terminate", name)
	if err != nil && isMissingDistro(result.Stdout+"\n"+result.Stderr+"\n"+err.Error()) {
		return nil
	}
	return err
}

func (p *Provider) Delete(ctx context.Context, name string) error {
	result, err := p.run(ctx, nil, "--unregister", name)
	if err != nil {
		text := result.Stdout + "\n" + result.Stderr + "\n" + err.Error()
		if !isMissingDistro(text) {
			absent, listErr := p.distroAbsent(ctx, name)
			if listErr != nil || !absent {
				return err
			}
		}
	}
	if p.DryRun {
		return nil
	}
	installDir, dirErr := p.instanceDir(name)
	if dirErr != nil {
		return dirErr
	}
	return os.RemoveAll(installDir)
}

func (p *Provider) distroAbsent(ctx context.Context, name string) (bool, error) {
	instances, err := p.List(ctx)
	if err != nil {
		return false, err
	}
	for _, instance := range instances {
		if instance.Name == name {
			return false, nil
		}
	}
	return true, nil
}

func (p *Provider) List(ctx context.Context) ([]provider.Instance, error) {
	result, err := p.run(ctx, nil, "--list", "--verbose")
	if err != nil {
		text := result.Stdout + "\n" + result.Stderr + "\n" + err.Error()
		if isNoInstalledDistros(text) {
			return nil, nil
		}
		return nil, err
	}
	return parseList(result.Stdout), nil
}

func (p *Provider) Export(ctx context.Context, name, outputPath string) error {
	outputPath = p.projectPath(outputPath)
	tmpPath := outputPath + ".tmp"
	if !p.DryRun {
		if err := os.MkdirAll(filepath.Dir(outputPath), 0755); err != nil {
			return err
		}
		if err := os.Remove(tmpPath); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if _, err := p.run(ctx, nil, "--export", name, tmpPath); err != nil {
		return err
	}
	if p.DryRun {
		return nil
	}
	if err := os.Remove(outputPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return os.Rename(tmpPath, outputPath)
}

func (p *Provider) cloneArgs(sourcePath, name, installDir string) []string {
	return []string{"--import", name, installDir, sourcePath, "--version", "2"}
}

func (p *Provider) keepAliveArgs(name string) []string {
	return []string{"-d", name, "--user", "root", "--exec", "/bin/sh", "-c", "trap 'exit 0' TERM INT; while :; do sleep 3600; done"}
}

func (p *Provider) execArgs(name string, command []string, env map[string]string) []string {
	args := []string{"-d", name, "--user", "root", "--exec"}
	return append(args, provider.EnvCommand(env, command)...)
}

func (p *Provider) startKeepAlive(name, logPath string) (*exec.Cmd, error) {
	args := p.keepAliveArgs(name)
	if p.DryRun {
		fmt.Printf("[dry-run] %s %s\n", p.Binary, strings.Join(args, " "))
		return nil, nil
	}
	cmd := exec.Command(p.Binary, args...)
	var logFile *os.File
	if logPath != "" {
		if err := os.MkdirAll(filepath.Dir(logPath), 0755); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			return nil, err
		}
		logFile = f
		cmd.Stdout = f
		cmd.Stderr = f
	}
	if err := cmd.Start(); err != nil {
		if logFile != nil {
			_ = logFile.Close()
		}
		return nil, fmt.Errorf("%s %s failed: %w", p.Binary, strings.Join(args, " "), err)
	}
	go func() {
		err := cmd.Wait()
		if logFile != nil {
			if err != nil {
				_, _ = fmt.Fprintf(logFile, "\n[wsl keepalive exited: %v]\n", err)
			}
			_ = logFile.Close()
		}
	}()
	return cmd, nil
}

func (p *Provider) projectPath(path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Clean(filepath.Join(p.ProjectRoot, path))
}

func (p *Provider) instanceDir(name string) (string, error) {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
		return "", fmt.Errorf("invalid wsl distro name %q", name)
	}
	root := filepath.Clean(p.InstallRoot)
	dir := filepath.Clean(filepath.Join(root, name))
	rel, err := filepath.Rel(root, dir)
	if err != nil {
		return "", err
	}
	if rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." || filepath.IsAbs(rel) {
		return "", fmt.Errorf("wsl install dir %s escapes install root %s", dir, root)
	}
	return dir, nil
}

func (p *Provider) run(ctx context.Context, stdin io.Reader, args ...string) (provider.ExecResult, error) {
	return p.runWithLog(ctx, stdin, "", args...)
}

func (p *Provider) runWithLog(ctx context.Context, stdin io.Reader, logPath string, args ...string) (provider.ExecResult, error) {
	if p.runCommand != nil {
		return p.runCommand(ctx, stdin, logPath, args...)
	}
	if p.DryRun {
		fmt.Printf("[dry-run] %s %s\n", p.Binary, strings.Join(args, " "))
		return provider.ExecResult{}, nil
	}
	cmd := exec.CommandContext(ctx, p.Binary, args...)
	if stdin != nil {
		cmd.Stdin = stdin
	}
	var stdout, stderr bytes.Buffer
	var logFile *os.File
	if logPath != "" {
		if err := os.MkdirAll(filepath.Dir(logPath), 0755); err != nil {
			return provider.ExecResult{}, err
		}
		f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			return provider.ExecResult{}, err
		}
		defer f.Close()
		logFile = f
	}
	if logFile != nil {
		cmd.Stdout = io.MultiWriter(&stdout, logFile)
		cmd.Stderr = io.MultiWriter(&stderr, logFile)
	} else {
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
	}
	err := cmd.Run()
	result := provider.ExecResult{
		Stdout: cleanWSLOutput(stdout.Bytes()),
		Stderr: cleanWSLOutput(stderr.Bytes()),
	}
	if err != nil {
		return result, fmt.Errorf("%s %s failed: %w: %s", p.Binary, strings.Join(args, " "), err, strings.TrimSpace(result.Stderr))
	}
	return result, nil
}

func parseList(text string) []provider.Instance {
	var out []provider.Instance
	for _, line := range strings.Split(cleanWSLOutput([]byte(text)), "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "*"))
		if line == "" || strings.HasPrefix(strings.ToUpper(line), "NAME ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		out = append(out, provider.Instance{Name: fields[0], State: fields[1]})
	}
	return out
}

func cleanWSLOutput(data []byte) string {
	if len(data) == 0 {
		return ""
	}
	if looksUTF16LE(data) {
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

func isNoInstalledDistros(text string) bool {
	text = strings.ToLower(cleanWSLOutput([]byte(text)))
	return strings.Contains(text, "has no installed distributions") ||
		strings.Contains(text, "no installed distributions")
}

func isMissingDistro(text string) bool {
	text = strings.ToLower(cleanWSLOutput([]byte(text)))
	return strings.Contains(text, "there is no distribution with the supplied name") ||
		strings.Contains(text, "the specified distribution was not found")
}
