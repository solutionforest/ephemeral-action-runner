package dockerdind

import (
	"context"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
)

func TestCreateArgsUsePrivilegedWithoutHostSocketOrPorts(t *testing.T) {
	p := New("docker", "linux/arm64", true)
	args := p.createArgs("runner-image", "epar-dind-1")
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"create",
		"--platform linux/arm64",
		"--name epar-dind-1",
		"--privileged",
		"--label epar.provider=docker-dind",
		"runner-image",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("create args %q missing %q", joined, want)
		}
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
	p.runCommand = func(_ context.Context, stdin io.Reader, _ string, args ...string) (provider.ExecResult, error) {
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
	p.runCommand = func(_ context.Context, _ io.Reader, _ string, args ...string) (provider.ExecResult, error) {
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
