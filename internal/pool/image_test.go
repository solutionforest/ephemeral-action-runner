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
		want    runnerImagesCopyMode
	}{
		{name: "empty", scripts: nil, want: runnerImagesCopyNone},
		{name: "generic custom apt script", scripts: []string{"examples/custom-install/install-extra-apt-tools.sh"}, want: runnerImagesCopyNone},
		{name: "docker browser", scripts: []string{"scripts/guest/ubuntu/install-docker-browser.sh"}, want: runnerImagesCopySubset},
		{name: "web e2e", scripts: []string{"scripts/guest/ubuntu/install-web-e2e.sh"}, want: runnerImagesCopySubset},
		{name: "absolute web e2e", scripts: []string{filepath.Join(root, "scripts", "guest", "ubuntu", "install-web-e2e.sh")}, want: runnerImagesCopySubset},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager := Manager{
				Config:      config.Config{Image: config.ImageConfig{CustomInstallScripts: tt.scripts}},
				ProjectRoot: root,
			}
			if got := manager.runnerImagesCopyMode(); got != tt.want {
				t.Fatalf("runnerImagesCopyMode() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDockerDindBaseImageDoesNotRequireRunnerImages(t *testing.T) {
	manager := Manager{Config: config.Config{Provider: config.ProviderConfig{Type: "docker-dind"}}}
	if got := manager.runnerImagesCopyMode(); got != runnerImagesCopyNone {
		t.Fatalf("runnerImagesCopyMode() = %v, want none", got)
	}
}

func TestDockerDindDockerfileRunsBuildStepsAsRoot(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{
		filepath.Join(root, "scripts", "guest", "ubuntu"),
		filepath.Join(root, "scripts", "container", "ubuntu"),
	} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	buildCtx := t.TempDir()
	manager := Manager{
		Config: config.Config{
			Image: config.ImageConfig{
				UpstreamLock: "missing.lock",
			},
		},
		ProjectRoot: root,
	}
	if err := manager.prepareDockerDindBuildContext(buildCtx, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(buildCtx, "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "FROM ${BASE_IMAGE}\nUSER root\n") {
		t.Fatalf("Dockerfile does not force root user after FROM:\n%s", content)
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

func TestConfigureDockerRegistryMirrors(t *testing.T) {
	root := t.TempDir()
	scriptDir := filepath.Join(root, "scripts", "guest", "ubuntu")
	if err := os.MkdirAll(scriptDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scriptDir, "configure-docker-daemon.sh"), []byte("#!/usr/bin/env bash\n"), 0644); err != nil {
		t.Fatal(err)
	}
	provider := &recordingProvider{}
	manager := Manager{
		Config: config.Config{
			Docker: config.DockerConfig{
				RegistryMirrors: []string{"http://host.docker.internal:5000", "https://mirror.example.test"},
			},
			Pool: config.PoolConfig{LogDir: "logs"},
		},
		Provider:    provider,
		ProjectRoot: root,
	}
	if err := manager.configureDockerRegistryMirrors(context.Background(), "runner-1"); err != nil {
		t.Fatal(err)
	}
	wantCommand := []string{"sudo", "-E", "bash", "/opt/epar/configure-docker-daemon.sh"}
	if !reflect.DeepEqual(provider.commands[1], wantCommand) {
		t.Fatalf("command = %#v, want %#v", provider.commands[1], wantCommand)
	}
	gotEnv := provider.execOptions[1].Env["EPAR_DOCKER_REGISTRY_MIRRORS"]
	wantEnv := "http://host.docker.internal:5000\nhttps://mirror.example.test"
	if gotEnv != wantEnv {
		t.Fatalf("mirror env = %q, want %q", gotEnv, wantEnv)
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
