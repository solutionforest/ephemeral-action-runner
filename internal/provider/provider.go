package provider

import (
	"context"
	"fmt"
	"io"
	"strings"
)

type Instance struct {
	Name   string
	Source string
	State  string
}

type StartOptions struct {
	Network    string
	RosettaTag string
	LogPath    string
	Stdout     io.Writer
	Stderr     io.Writer
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
	Stdout  io.Writer
	Stderr  io.Writer
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
	cmd := []string{"bash", "-lc", fmt.Sprintf("cat > %s && if command -v sudo >/dev/null 2>&1; then sudo install -m %s %s %s; else install -m %s %s %s; fi && rm -f %s", shellQuote(tmp), shellQuote(mode), shellQuote(tmp), shellQuote(path), shellQuote(mode), shellQuote(tmp), shellQuote(path), shellQuote(tmp))}
	_, err := p.Exec(ctx, vmName, cmd, ExecOptions{Stdin: content})
	return err
}

// CopyTextAtomic installs content through a sibling temporary file and then
// renames it over path. Callers must ensure the destination directory exists.
func CopyTextAtomic(ctx context.Context, p Provider, vmName, path, mode, content string) error {
	tmp := path + ".tmp"
	staging := "/tmp/epar-copy"
	cmd := []string{"bash", "-lc", fmt.Sprintf("cat > %s && if command -v sudo >/dev/null 2>&1; then sudo install -m %s %s %s && sudo mv -f %s %s; else install -m %s %s %s && mv -f %s %s; fi && rm -f %s", shellQuote(staging), shellQuote(mode), shellQuote(staging), shellQuote(tmp), shellQuote(tmp), shellQuote(path), shellQuote(mode), shellQuote(staging), shellQuote(tmp), shellQuote(tmp), shellQuote(path), shellQuote(staging))}
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
