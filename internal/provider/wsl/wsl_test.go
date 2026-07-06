package wsl

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
)

func TestCommandConstruction(t *testing.T) {
	p := New("wsl.exe", filepath.Join("C:", "ephemeral"), `D:\repo`, true)
	installDir, err := p.instanceDir("epar-test-1")
	if err != nil {
		t.Fatal(err)
	}
	cloneArgs := p.cloneArgs(`D:\repo\work\images\base.tar`, "epar-test-1", installDir)
	wantClone := []string{"--import", "epar-test-1", installDir, `D:\repo\work\images\base.tar`, "--version", "2"}
	if !reflect.DeepEqual(cloneArgs, wantClone) {
		t.Fatalf("clone args = %#v, want %#v", cloneArgs, wantClone)
	}
	execArgs := p.execArgs("epar-test-1", []string{"bash", "-lc", "echo ok"}, map[string]string{"A": "B"})
	wantExec := []string{"-d", "epar-test-1", "--user", "root", "--exec", "env", "A=B", "bash", "-lc", "echo ok"}
	if !reflect.DeepEqual(execArgs, wantExec) {
		t.Fatalf("exec args = %#v, want %#v", execArgs, wantExec)
	}
	keepAliveArgs := p.keepAliveArgs("epar-test-1")
	wantKeepAlive := []string{"-d", "epar-test-1", "--user", "root", "--exec", "/bin/sh", "-c", "trap 'exit 0' TERM INT; while :; do sleep 3600; done"}
	if !reflect.DeepEqual(keepAliveArgs, wantKeepAlive) {
		t.Fatalf("keepalive args = %#v, want %#v", keepAliveArgs, wantKeepAlive)
	}
}

func TestCleanWSLOutputHandlesUTF16LE(t *testing.T) {
	data := []byte{
		0xff, 0xfe,
		'N', 0, 'A', 0, 'M', 0, 'E', 0, '\r', 0, '\n', 0,
		'U', 0, 'b', 0, 'u', 0, 'n', 0, 't', 0, 'u', 0, '\r', 0, '\n', 0,
	}
	got := cleanWSLOutput(data)
	want := "NAME\nUbuntu\n"
	if got != want {
		t.Fatalf("cleanWSLOutput() = %q, want %q", got, want)
	}
}

func TestParseListParsesVerboseOutput(t *testing.T) {
	text := "  NAME                   STATE           VERSION\r\n* Ubuntu-24.04           Running         2\r\n  epar-wsl-1             Stopped         2\r\n"
	got := parseList(text)
	want := []provider.Instance{
		{Name: "Ubuntu-24.04", State: "Running"},
		{Name: "epar-wsl-1", State: "Stopped"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseList() = %#v, want %#v", got, want)
	}
}

func TestNoInstalledDistrosReturnsEmptyList(t *testing.T) {
	p := New("wsl.exe", t.TempDir(), t.TempDir(), true)
	out, err := p.List(context.Background())
	if err != nil {
		t.Fatalf("dry-run list failed: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("dry-run list = %#v, want empty", out)
	}
	if !isNoInstalledDistros("Windows Subsystem for Linux has no installed distributions.") {
		t.Fatal("expected no-installed-distros message to be detected")
	}
}

func TestDryRunIPReturnsPlaceholder(t *testing.T) {
	p := New("wsl.exe", t.TempDir(), t.TempDir(), true)
	ip, err := p.IP(context.Background(), "epar-wsl-1", 1)
	if err != nil {
		t.Fatal(err)
	}
	if ip != "127.0.0.1" {
		t.Fatalf("ip = %q, want placeholder", ip)
	}
}

func TestInstanceDirRejectsPathTraversal(t *testing.T) {
	p := New("wsl.exe", t.TempDir(), t.TempDir(), true)
	for _, name := range []string{"..", `bad\name`, "bad/name", ""} {
		if _, err := p.instanceDir(name); err == nil {
			t.Fatalf("instanceDir(%q) succeeded, want error", name)
		}
	}
	dir, err := p.instanceDir("epar-wsl-1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(dir, p.InstallRoot) {
		t.Fatalf("instance dir %q does not start with install root %q", dir, p.InstallRoot)
	}
}

func TestDeleteIgnoresUnregisterFailureOnlyWhenDistroIsAbsent(t *testing.T) {
	root := t.TempDir()
	p := New("wsl.exe", root, t.TempDir(), false)
	instanceDir, err := p.instanceDir("epar-wsl-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(instanceDir, 0755); err != nil {
		t.Fatal(err)
	}
	p.runCommand = func(_ context.Context, _ io.Reader, _ string, args ...string) (provider.ExecResult, error) {
		switch strings.Join(args, " ") {
		case "--unregister epar-wsl-1":
			return provider.ExecResult{}, errors.New("wsl.exe --unregister epar-wsl-1 failed: exit status 0xffffffff:")
		case "--list --verbose":
			return provider.ExecResult{Stdout: "  NAME                   STATE           VERSION\r\n  Ubuntu-24.04           Stopped         2\r\n"}, nil
		default:
			t.Fatalf("unexpected args: %#v", args)
		}
		return provider.ExecResult{}, nil
	}

	if err := p.Delete(context.Background(), "epar-wsl-1"); err != nil {
		t.Fatalf("Delete() failed: %v", err)
	}
	if _, err := os.Stat(instanceDir); !os.IsNotExist(err) {
		t.Fatalf("instance dir still exists or stat failed unexpectedly: %v", err)
	}
}

func TestDeleteReturnsUnregisterFailureWhenDistroStillExists(t *testing.T) {
	p := New("wsl.exe", t.TempDir(), t.TempDir(), false)
	p.runCommand = func(_ context.Context, _ io.Reader, _ string, args ...string) (provider.ExecResult, error) {
		switch strings.Join(args, " ") {
		case "--unregister epar-wsl-1":
			return provider.ExecResult{}, errors.New("wsl.exe --unregister epar-wsl-1 failed: exit status 0xffffffff:")
		case "--list --verbose":
			return provider.ExecResult{Stdout: "  NAME                   STATE           VERSION\r\n  epar-wsl-1             Stopped         2\r\n"}, nil
		default:
			t.Fatalf("unexpected args: %#v", args)
		}
		return provider.ExecResult{}, nil
	}

	if err := p.Delete(context.Background(), "epar-wsl-1"); err == nil {
		t.Fatal("Delete() error = nil, want unregister error")
	}
}
