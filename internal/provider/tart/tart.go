package tart

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
)

type Provider struct {
	Binary string
	DryRun bool
}

func New(binary string, dryRun bool) *Provider {
	if binary == "" {
		binary = "tart"
	}
	return &Provider{Binary: binary, DryRun: dryRun}
}

func (p *Provider) Clone(ctx context.Context, source, name string) error {
	_, err := p.run(ctx, nil, "clone", source, name)
	return err
}

func (p *Provider) Start(ctx context.Context, name string, opts provider.StartOptions) (*provider.RunningProcess, error) {
	args := []string{"run", "--no-graphics"}
	switch opts.Network {
	case "", "default":
	case "softnet":
		args = append(args, "--net-softnet")
	case "host":
		args = append(args, "--net-host")
	default:
		return nil, fmt.Errorf("unsupported tart network mode %q", opts.Network)
	}
	if opts.RosettaTag != "" {
		args = append(args, "--rosetta", opts.RosettaTag)
	}
	args = append(args, name)
	if p.DryRun {
		fmt.Printf("[dry-run] %s %s\n", p.Binary, strings.Join(args, " "))
		return &provider.RunningProcess{Name: name, PID: 0, LogPath: opts.LogPath}, nil
	}
	cmd := exec.CommandContext(ctx, p.Binary, args...)
	cmd.Stdout = writerOrDiscard(opts.Stdout)
	cmd.Stderr = writerOrDiscard(opts.Stderr)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go func() {
		_ = cmd.Wait()
	}()
	return &provider.RunningProcess{Name: name, PID: cmd.Process.Pid, LogPath: opts.LogPath}, nil
}

func (p *Provider) Exec(ctx context.Context, name string, command []string, opts provider.ExecOptions) (provider.ExecResult, error) {
	full := []string{"exec"}
	if opts.Stdin != "" {
		full = append(full, "-i")
	}
	full = append(full, name)
	full = append(full, provider.EnvCommand(opts.Env, command)...)
	return p.runWithLog(ctx, strings.NewReader(opts.Stdin), opts.Stdout, opts.Stderr, full...)
}

func (p *Provider) IP(ctx context.Context, name string, waitSeconds int) (string, error) {
	result, err := p.run(ctx, nil, "ip", name, "--wait", fmt.Sprintf("%d", waitSeconds), "--resolver", "agent")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(result.Stdout), nil
}

func (p *Provider) Stop(ctx context.Context, name string) error {
	_, err := p.run(ctx, nil, "stop", name)
	return err
}

func (p *Provider) Delete(ctx context.Context, name string) error {
	_, err := p.run(ctx, nil, "delete", name)
	return err
}

func (p *Provider) List(ctx context.Context) ([]provider.Instance, error) {
	result, err := p.run(ctx, nil, "list")
	if err != nil {
		return nil, err
	}
	var out []provider.Instance
	for i, line := range strings.Split(result.Stdout, "\n") {
		if i == 0 || strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		out = append(out, provider.Instance{
			Source: fields[0],
			Name:   fields[1],
			State:  fields[len(fields)-1],
		})
	}
	return out, nil
}

func (p *Provider) run(ctx context.Context, stdin io.Reader, args ...string) (provider.ExecResult, error) {
	return p.runWithLog(ctx, stdin, nil, nil, args...)
}

func (p *Provider) runWithLog(ctx context.Context, stdin io.Reader, stdoutSink, stderrSink io.Writer, args ...string) (provider.ExecResult, error) {
	if p.DryRun {
		fmt.Printf("[dry-run] %s %s\n", p.Binary, strings.Join(args, " "))
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

func writerOrDiscard(writer io.Writer) io.Writer {
	if writer == nil {
		return io.Discard
	}
	return writer
}
