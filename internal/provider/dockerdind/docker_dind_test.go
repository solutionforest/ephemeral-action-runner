package dockerdind

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
)

func TestExecPassesInjectedTranscriptWriters(t *testing.T) {
	p := New("docker", "", false)
	var stdout, stderr bytes.Buffer
	p.runCommand = func(_ context.Context, _ io.Reader, logPath string, gotStdout, gotStderr io.Writer, _ ...string) (provider.ExecResult, error) {
		if logPath != "display.log" {
			t.Fatalf("logPath = %q", logPath)
		}
		_, _ = gotStdout.Write([]byte("out"))
		_, _ = gotStderr.Write([]byte("err"))
		return provider.ExecResult{}, nil
	}
	_, err := p.Exec(context.Background(), "runner", []string{"true"}, provider.ExecOptions{
		LogPath: "display.log",
		Stdout:  &stdout,
		Stderr:  &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "out" || stderr.String() != "err" {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestCreateArgsUsePrivilegedWithoutHostSocketOrPorts(t *testing.T) {
	p := NewWithOptions("docker", "linux/arm64", true, map[string]string{
		"NO_PROXY":    "localhost,127.0.0.1",
		"HTTPS_PROXY": "http://proxy.example.test:3128",
		"HTTP_PROXY":  "http://proxy.example.test:3128",
	}, true)
	args := p.createArgs("runner-image", "epar-dind-1")
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"create",
		"--platform linux/arm64",
		"--name epar-dind-1",
		"--privileged",
		"--add-host host.docker.internal:host-gateway",
		"--env HTTP_PROXY=http://proxy.example.test:3128",
		"--env HTTPS_PROXY=http://proxy.example.test:3128",
		"--env NO_PROXY=localhost,127.0.0.1",
		"--label epar.provider=docker-dind",
		"runner-image",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("create args %q missing %q", joined, want)
		}
	}
	if strings.Index(joined, "HTTP_PROXY=") > strings.Index(joined, "HTTPS_PROXY=") || strings.Index(joined, "HTTPS_PROXY=") > strings.Index(joined, "NO_PROXY=") {
		t.Fatalf("create args proxy environment is not deterministic: %q", joined)
	}
	for _, forbidden := range []string{"/var/run/docker.sock", ".orbstack", "-p ", "--publish", "--network host"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("create args %q contains forbidden %q", joined, forbidden)
		}
	}
}

func TestExecArgsPreserveEnvAndStdin(t *testing.T) {
	p := New("docker", "", false)
	var gotArgs []string
	var gotStdin bool
	p.runCommand = func(_ context.Context, stdin io.Reader, _ string, _, _ io.Writer, args ...string) (provider.ExecResult, error) {
		gotArgs = append([]string(nil), args...)
		gotStdin = stdin != nil
		return provider.ExecResult{}, nil
	}
	_, err := p.Exec(context.Background(), "epar-dind-1", []string{"bash", "-lc", "echo ok"}, provider.ExecOptions{
		Stdin: "input",
		Env:   map[string]string{"A": "B"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"exec", "-i", "-e", "A=B", "epar-dind-1", "bash", "-lc", "echo ok"}
	if !reflect.DeepEqual(gotArgs, want) {
		t.Fatalf("exec args = %#v, want %#v", gotArgs, want)
	}
	if !gotStdin {
		t.Fatal("stdin was not passed to docker exec")
	}
}

func TestListParsesDockerPSOutput(t *testing.T) {
	p := New("docker", "", false)
	p.runCommand = func(_ context.Context, _ io.Reader, _ string, _, _ io.Writer, args ...string) (provider.ExecResult, error) {
		if strings.Join(args, " ") != "ps -a --filter label=epar.provider=docker-dind --format {{.Names}}\t{{.Image}}\t{{.Status}}" {
			t.Fatalf("unexpected list args: %#v", args)
		}
		return provider.ExecResult{Stdout: "epar-dind-1\trunner-image\tUp 2 minutes\n"}, nil
	}
	instances, err := p.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []provider.Instance{{Name: "epar-dind-1", Source: "runner-image", State: "Up 2 minutes"}}
	if !reflect.DeepEqual(instances, want) {
		t.Fatalf("instances = %#v, want %#v", instances, want)
	}
}

func TestDryRunIPReturnsPlaceholder(t *testing.T) {
	p := New("docker", "", true)
	ip, err := p.IP(context.Background(), "epar-dind-1", 1)
	if err != nil {
		t.Fatal(err)
	}
	if ip != "127.0.0.1" {
		t.Fatalf("ip = %q, want placeholder", ip)
	}
}

func TestStopAndDeleteIgnoreMissingContainer(t *testing.T) {
	for _, test := range []struct {
		name string
		call func(*Provider) error
	}{
		{name: "stop", call: func(p *Provider) error { return p.Stop(context.Background(), "epar-core-1") }},
		{name: "delete", call: func(p *Provider) error { return p.Delete(context.Background(), "epar-core-1") }},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := New("docker", "", false)
			p.runCommand = func(_ context.Context, _ io.Reader, _ string, _, _ io.Writer, _ ...string) (provider.ExecResult, error) {
				return provider.ExecResult{Stderr: "Error response from daemon: No such container: epar-core-1"}, errors.New("exit status 1")
			}
			if err := test.call(p); err != nil {
				t.Fatalf("missing container should be idempotent, got %v", err)
			}
		})
	}
}
