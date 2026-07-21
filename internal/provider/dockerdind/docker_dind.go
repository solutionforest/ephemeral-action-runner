package dockerdind

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
)

const (
	labelManaged  = "epar.managed=true"
	labelProvider = "epar.provider=docker-dind"
)

type Provider struct {
	Binary      string
	Platform    string
	HostGateway bool
	Environment map[string]string
	DryRun      bool
	runCommand  runCommandFunc
}

type runCommandFunc func(ctx context.Context, stdin io.Reader, logPath string, stdout, stderr io.Writer, args ...string) (provider.ExecResult, error)

func New(binary, platform string, dryRun bool) *Provider {
	return NewWithOptions(binary, platform, false, nil, dryRun)
}

func NewWithOptions(binary, platform string, hostGateway bool, environment map[string]string, dryRun bool) *Provider {
	if binary == "" {
		binary = "docker"
	}
	return &Provider{Binary: binary, Platform: platform, HostGateway: hostGateway, Environment: environment, DryRun: dryRun}
}

func (p *Provider) Clone(ctx context.Context, source, name string) error {
	args := p.createArgs(source, name)
	_, err := p.run(ctx, nil, args...)
	return err
}

func (p *Provider) Start(ctx context.Context, name string, opts provider.StartOptions) (*provider.RunningProcess, error) {
	if opts.Network != "" && opts.Network != "default" {
		return nil, fmt.Errorf("unsupported docker-dind network mode %q", opts.Network)
	}
	if _, err := p.run(ctx, nil, "start", name); err != nil {
		return nil, err
	}
	if err := p.waitDocker(ctx, name, opts); err != nil {
		return nil, err
	}
	return &provider.RunningProcess{Name: name, PID: 0, LogPath: opts.LogPath}, nil
}

func (p *Provider) Exec(ctx context.Context, name string, command []string, opts provider.ExecOptions) (provider.ExecResult, error) {
	args := []string{"exec"}
	if opts.Stdin != "" {
		args = append(args, "-i")
	}
	for key, value := range opts.Env {
		args = append(args, "-e", key+"="+value)
	}
	args = append(args, name)
	args = append(args, command...)
	var stdin io.Reader
	if opts.Stdin != "" {
		stdin = strings.NewReader(opts.Stdin)
	}
	return p.runWithSensitiveLog(ctx, stdin, opts.LogPath, opts.Stdout, opts.Stderr, opts.SensitiveValues, args...)
}

func (p *Provider) IP(ctx context.Context, name string, waitSeconds int) (string, error) {
	if p.DryRun && p.runCommand == nil {
		fmt.Printf("[dry-run] %s inspect -f {{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}} %s\n", p.Binary, name)
		return "127.0.0.1", nil
	}
	if waitSeconds <= 0 {
		waitSeconds = 1
	}
	deadline := time.Now().Add(time.Duration(waitSeconds) * time.Second)
	var lastErr error
	for {
		result, err := p.run(ctx, nil, "inspect", "-f", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", name)
		if err == nil {
			if ip := strings.TrimSpace(result.Stdout); ip != "" {
				return ip, nil
			}
		} else {
			lastErr = err
		}
		if time.Now().After(deadline) {
			if lastErr != nil {
				return "", lastErr
			}
			return "", fmt.Errorf("docker-dind container %q did not report an IP within %d seconds", name, waitSeconds)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func (p *Provider) Stop(ctx context.Context, name string) error {
	result, err := p.run(ctx, nil, "stop", "--time", "10", name)
	if err != nil && isMissingContainer(result.Stdout+"\n"+result.Stderr+"\n"+err.Error()) {
		return nil
	}
	return err
}

func (p *Provider) Delete(ctx context.Context, name string) error {
	result, err := p.run(ctx, nil, "rm", "-f", "-v", name)
	if err != nil && isMissingContainer(result.Stdout+"\n"+result.Stderr+"\n"+err.Error()) {
		return nil
	}
	return err
}

func (p *Provider) List(ctx context.Context) ([]provider.Instance, error) {
	result, err := p.run(ctx, nil, "ps", "-a", "--filter", "label="+labelProvider, "--format", "{{.Names}}\t{{.Image}}\t{{.Status}}")
	if err != nil {
		return nil, err
	}
	var out []provider.Instance
	for _, line := range strings.Split(result.Stdout, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, "\t", 3)
		if len(fields) < 3 {
			continue
		}
		out = append(out, provider.Instance{Name: fields[0], Source: fields[1], State: fields[2]})
	}
	return out, nil
}

func (p *Provider) createArgs(source, name string) []string {
	args := []string{"create"}
	if p.Platform != "" {
		args = append(args, "--platform", p.Platform)
	}
	args = append(args,
		"--name", name,
		"--privileged",
	)
	if p.HostGateway {
		args = append(args, "--add-host", "host.docker.internal:host-gateway")
	}
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY"} {
		if value := p.Environment[key]; value != "" {
			args = append(args, "--env", key+"="+value)
		}
	}
	args = append(args,
		"--label", labelManaged,
		"--label", labelProvider,
		"--label", "epar.instance="+name,
		source,
	)
	return args
}

func (p *Provider) waitDocker(ctx context.Context, name string, opts provider.StartOptions) error {
	deadline := time.Now().Add(120 * time.Second)
	var lastErr error
	for {
		_, err := p.Exec(ctx, name, []string{"docker", "info"}, provider.ExecOptions{
			LogPath: opts.LogPath,
			Stdout:  opts.Stdout,
			Stderr:  opts.Stderr,
		})
		if err == nil {
			return nil
		}
		lastErr = err
		if time.Now().After(deadline) {
			return fmt.Errorf("inner Docker daemon did not become ready in %s: %w", name, lastErr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func (p *Provider) run(ctx context.Context, stdin io.Reader, args ...string) (provider.ExecResult, error) {
	return p.runWithLog(ctx, stdin, "", nil, nil, args...)
}

func (p *Provider) runWithLog(ctx context.Context, stdin io.Reader, logPath string, stdoutSink, stderrSink io.Writer, args ...string) (provider.ExecResult, error) {
	return p.runWithSensitiveLog(ctx, stdin, logPath, stdoutSink, stderrSink, nil, args...)
}

func (p *Provider) runWithSensitiveLog(ctx context.Context, stdin io.Reader, logPath string, stdoutSink, stderrSink io.Writer, sensitiveValues []string, args ...string) (provider.ExecResult, error) {
	bufferedStdout, bufferedStderr, flush := provider.BufferSensitiveSinks(sensitiveValues, stdoutSink, stderrSink)
	result, err := p.runWithLogRaw(ctx, stdin, logPath, bufferedStdout, bufferedStderr, sensitiveValues, args...)
	return provider.FinishSensitiveExecution(result, err, flush(), sensitiveValues)
}

func (p *Provider) runWithLogRaw(ctx context.Context, stdin io.Reader, logPath string, stdoutSink, stderrSink io.Writer, sensitiveValues []string, args ...string) (provider.ExecResult, error) {
	if p.runCommand != nil {
		return p.runCommand(ctx, stdin, logPath, stdoutSink, stderrSink, args...)
	}
	if p.DryRun {
		fmt.Printf("[dry-run] %s %s\n", p.Binary, provider.RedactText(strings.Join(args, " "), sensitiveValues...))
		return provider.ExecResult{}, nil
	}
	cmd := exec.CommandContext(ctx, p.Binary, args...)
	if stdin != nil {
		cmd.Stdin = stdin
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = captureWriter(&stdout, stdoutSink)
	cmd.Stderr = captureWriter(&stderr, stderrSink)
	err := cmd.Run()
	result := provider.ExecResult{Stdout: stdout.String(), Stderr: stderr.String()}
	if err != nil {
		return result, fmt.Errorf("%s %s failed: %w: %s", p.Binary, strings.Join(args, " "), err, strings.TrimSpace(result.Stderr))
	}
	return result, nil
}

func captureWriter(capture io.Writer, sink io.Writer) io.Writer {
	if sink == nil {
		return capture
	}
	return io.MultiWriter(capture, sink)
}

func isMissingContainer(text string) bool {
	text = strings.ToLower(text)
	return strings.Contains(text, "no such container") || strings.Contains(text, "is not a container")
}
