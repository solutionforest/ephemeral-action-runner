package tart

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
)

type Provider struct {
	Binary     string
	DryRun     bool
	runCommand runCommandFunc
	identities func() (map[string]string, error)
}

type runCommandFunc func(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, args ...string) (provider.ExecResult, error)

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
	return p.runWithSensitiveLog(ctx, strings.NewReader(opts.Stdin), opts.Stdout, opts.Stderr, opts.SensitiveValues, full...)
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
	if p.identities == nil && (runtime.GOOS != "darwin" || p.runCommand != nil) {
		return out, nil
	}
	resolve := p.identities
	if resolve == nil {
		resolve = readLocalVMIdentities
	}
	identities, err := resolve()
	if err != nil {
		return nil, fmt.Errorf("read immutable Tart VM identities: %w", err)
	}
	for index := range out {
		if identity := identities[out[index].Name]; identity != "" {
			out[index].ProviderID = "tart-mac:" + strings.ToLower(identity)
		}
	}
	return out, nil
}

func readLocalVMIdentities() (map[string]string, error) {
	home := strings.TrimSpace(os.Getenv("TART_HOME"))
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		home = filepath.Join(userHome, ".tart")
	}
	entries, err := os.ReadDir(filepath.Join(home, "vms"))
	if os.IsNotExist(err) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	result := make(map[string]string)
	for _, entry := range entries {
		if !entry.IsDir() || strings.ContainsAny(entry.Name(), `/\`) {
			continue
		}
		data, readErr := os.ReadFile(filepath.Join(home, "vms", entry.Name(), "config.json"))
		if readErr != nil {
			continue
		}
		var config struct {
			MACAddress string `json:"macAddress"`
		}
		if json.Unmarshal(data, &config) != nil || strings.TrimSpace(config.MACAddress) == "" {
			continue
		}
		result[entry.Name()] = strings.TrimSpace(config.MACAddress)
	}
	return result, nil
}

func (p *Provider) run(ctx context.Context, stdin io.Reader, args ...string) (provider.ExecResult, error) {
	return p.runWithLog(ctx, stdin, nil, nil, args...)
}

func (p *Provider) runWithLog(ctx context.Context, stdin io.Reader, stdoutSink, stderrSink io.Writer, args ...string) (provider.ExecResult, error) {
	return p.runWithSensitiveLog(ctx, stdin, stdoutSink, stderrSink, nil, args...)
}

func (p *Provider) runWithSensitiveLog(ctx context.Context, stdin io.Reader, stdoutSink, stderrSink io.Writer, sensitiveValues []string, args ...string) (provider.ExecResult, error) {
	bufferedStdout, bufferedStderr, flush := provider.BufferSensitiveSinks(sensitiveValues, stdoutSink, stderrSink)
	result, err := p.runWithLogRaw(ctx, stdin, bufferedStdout, bufferedStderr, sensitiveValues, args...)
	return provider.FinishSensitiveExecution(result, err, flush(), sensitiveValues)
}

func (p *Provider) runWithLogRaw(ctx context.Context, stdin io.Reader, stdoutSink, stderrSink io.Writer, sensitiveValues []string, args ...string) (provider.ExecResult, error) {
	if p.runCommand != nil {
		return p.runCommand(ctx, stdin, stdoutSink, stderrSink, args...)
	}
	if p.DryRun {
		fmt.Printf("[dry-run] %s %s\n", p.Binary, provider.RedactText(strings.Join(args, " "), sensitiveValues...))
		return provider.ExecResult{}, nil
	}
	cmd := exec.CommandContext(ctx, p.Binary, args...)
	if len(args) > 0 && args[0] == "clone" {
		cmd.Env = tartCloneEnvironment(os.Environ())
	}
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

func tartCloneEnvironment(base []string) []string {
	result := make([]string, 0, len(base)+1)
	for _, value := range base {
		if !strings.HasPrefix(value, "TART_NO_AUTO_PRUNE=") {
			result = append(result, value)
		}
	}
	return append(result, "TART_NO_AUTO_PRUNE=1")
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
