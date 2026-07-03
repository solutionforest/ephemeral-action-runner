package pool

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
)

func TestRunnerImageBuildScriptsIncludeOptionalScriptDependencies(t *testing.T) {
	manager := Manager{}
	got := manager.runnerImageBuildScripts()
	want := []string{"install-docker.sh", "install-google-chrome.sh", "install-nodejs.sh"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("runnerImageBuildScripts() = %#v, want %#v", got, want)
	}
}

func TestNeedsRunnerImagesSubsetOnlyForBuiltInScripts(t *testing.T) {
	root := t.TempDir()
	tests := []struct {
		name    string
		scripts []string
		want    bool
	}{
		{name: "empty", scripts: nil, want: false},
		{name: "generic custom apt script", scripts: []string{"examples/custom-install/install-extra-apt-tools.sh"}, want: false},
		{name: "docker browser", scripts: []string{"scripts/guest/ubuntu/install-docker-browser.sh"}, want: true},
		{name: "web e2e", scripts: []string{"scripts/guest/ubuntu/install-web-e2e.sh"}, want: true},
		{name: "absolute web e2e", scripts: []string{filepath.Join(root, "scripts", "guest", "ubuntu", "install-web-e2e.sh")}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager := Manager{
				Config:      config.Config{Image: config.ImageConfig{CustomInstallScripts: tt.scripts}},
				ProjectRoot: root,
			}
			if got := manager.needsRunnerImagesSubset(); got != tt.want {
				t.Fatalf("needsRunnerImagesSubset() = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestInstallCustomScriptsRunsInOrder(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".local"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".local", "one.sh"), []byte("#!/usr/bin/env bash\necho one\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".local", "two.sh"), []byte("#!/usr/bin/env bash\necho two\n"), 0644); err != nil {
		t.Fatal(err)
	}

	provider := &recordingProvider{}
	manager := Manager{
		Config: config.Config{
			Image: config.ImageConfig{
				CustomInstallScripts: []string{".local/one.sh", ".local/two.sh"},
			},
			Pool: config.PoolConfig{LogDir: "logs"},
		},
		Provider:    provider,
		ProjectRoot: root,
	}

	if err := manager.installCustomInstallScripts(context.Background(), "image-test"); err != nil {
		t.Fatal(err)
	}

	runCommands := provider.commandsMatching(func(command []string) bool {
		return len(command) >= 3 && command[0] == "sudo" && command[1] == "bash"
	})
	want := [][]string{
		{"sudo", "bash", "/opt/epar/custom-install/001-one.sh"},
		{"sudo", "bash", "/opt/epar/custom-install/002-two.sh"},
	}
	if !reflect.DeepEqual(runCommands, want) {
		t.Fatalf("custom script run commands = %#v, want %#v", runCommands, want)
	}
}

func TestRelativeCustomInstallScriptCannotEscapeProjectRoot(t *testing.T) {
	manager := Manager{ProjectRoot: t.TempDir()}
	if _, err := manager.customInstallScriptHostPath("../outside.sh"); err == nil {
		t.Fatal("customInstallScriptHostPath accepted path escaping project root")
	}
}

func TestEnableWSLSystemdDisablesWindowsPathInjection(t *testing.T) {
	provider := &recordingProvider{}
	manager := Manager{Provider: provider}
	if err := manager.enableWSLSystemd(context.Background(), "image-test"); err != nil {
		t.Fatal(err)
	}
	if len(provider.execOptions) != 1 {
		t.Fatalf("exec option count = %d, want 1", len(provider.execOptions))
	}
	stdin := provider.execOptions[0].Stdin
	if !strings.Contains(stdin, "systemd=true") {
		t.Fatalf("wsl.conf missing systemd=true: %q", stdin)
	}
	if !strings.Contains(stdin, "appendWindowsPath=false") {
		t.Fatalf("wsl.conf missing appendWindowsPath=false: %q", stdin)
	}
}

type recordingProvider struct {
	commands    [][]string
	execOptions []provider.ExecOptions
}

func (p *recordingProvider) Clone(context.Context, string, string) error {
	return nil
}

func (p *recordingProvider) Start(context.Context, string, provider.StartOptions) (*provider.RunningProcess, error) {
	return &provider.RunningProcess{}, nil
}

func (p *recordingProvider) Exec(_ context.Context, _ string, command []string, opts provider.ExecOptions) (provider.ExecResult, error) {
	p.commands = append(p.commands, append([]string(nil), command...))
	p.execOptions = append(p.execOptions, opts)
	return provider.ExecResult{}, nil
}

func (p *recordingProvider) IP(context.Context, string, int) (string, error) {
	return "127.0.0.1", nil
}

func (p *recordingProvider) Stop(context.Context, string) error {
	return nil
}

func (p *recordingProvider) Delete(context.Context, string) error {
	return nil
}

func (p *recordingProvider) List(context.Context) ([]provider.Instance, error) {
	return nil, nil
}

func (p *recordingProvider) commandsMatching(match func([]string) bool) [][]string {
	var out [][]string
	for _, command := range p.commands {
		if match(command) {
			out = append(out, command)
		}
	}
	return out
}
