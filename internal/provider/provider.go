package provider

import (
	"context"
	"fmt"
	"strings"
)

type Instance struct {
	Name   string
	Source string
	State  string
}

type StartOptions struct {
	Network string
	LogPath string
}

type RunningProcess struct {
	Name    string
	PID     int
	LogPath string
}

type ExecOptions struct {
	Stdin   string
	Env     map[string]string
	LogPath string
}

type ExecResult struct {
	Stdout string
	Stderr string
}

type Provider interface {
	Clone(ctx context.Context, source, name string) error
	Start(ctx context.Context, name string, opts StartOptions) (*RunningProcess, error)
	Exec(ctx context.Context, name string, command []string, opts ExecOptions) (ExecResult, error)
	IP(ctx context.Context, name string, waitSeconds int) (string, error)
	Stop(ctx context.Context, name string) error
	Delete(ctx context.Context, name string) error
	List(ctx context.Context) ([]Instance, error)
}

func CopyText(ctx context.Context, p Provider, vmName, path, mode, content string) error {
	tmp := "/tmp/epar-copy"
	cmd := []string{"bash", "-lc", fmt.Sprintf("cat > %s && sudo install -m %s %s %s && rm -f %s", shellQuote(tmp), shellQuote(mode), shellQuote(tmp), shellQuote(path), shellQuote(tmp))}
	_, err := p.Exec(ctx, vmName, cmd, ExecOptions{Stdin: content})
	return err
}

func ShellCommand(script string) []string {
	return []string{"bash", "-lc", script}
}

func EnvCommand(env map[string]string, command []string) []string {
	if len(env) == 0 {
		return command
	}
	out := []string{"env"}
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return append(out, command...)
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}
