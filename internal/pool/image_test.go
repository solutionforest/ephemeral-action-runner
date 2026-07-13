package pool

import (
	"context"
	"io"
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
	if err := manager.prepareDockerDindBuildContext(buildCtx, t.TempDir(), `{"hash":"test"}`+"\n"); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(buildCtx, "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "FROM ${BASE_IMAGE}\nUSER root\n") {
		t.Fatalf("Dockerfile does not force root user after FROM:\n%s", content)
	}
	if !strings.Contains(string(content), "RUN chmod 0755 /opt/epar/*.sh") {
		t.Fatalf("Dockerfile does not normalize guest script permissions independently of umask:\n%s", content)
	}
	if !strings.Contains(string(content), imageManifestLabel) {
		t.Fatalf("Dockerfile missing EPAR manifest label:\n%s", content)
	}
	if _, err := os.Stat(filepath.Join(buildCtx, "image-manifest.json")); err != nil {
		t.Fatalf("build context missing image manifest: %v", err)
	}
}

func TestDockerDindBuildUsesLegacyBuilderCompatibleArgs(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{
		filepath.Join(root, "scripts", "guest", "ubuntu"),
		filepath.Join(root, "scripts", "container", "ubuntu"),
	} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}

	manager := Manager{
		Config: config.Config{
			Image: config.ImageConfig{
				SourceImage:   "ghcr.io/catthehacker/ubuntu:full-latest",
				OutputImage:   "epar-docker-dind-catthehacker-ubuntu",
				RunnerVersion: "latest",
			},
			Pool:     config.PoolConfig{LogDir: "logs"},
			Provider: config.ProviderConfig{Type: "docker-dind", Platform: "linux/amd64"},
		},
		ProjectRoot: root,
		DryRun:      true,
	}
	manifest := ImageManifest{
		SchemaVersion: imageManifestSchemaVersion,
		ProviderType:  "docker-dind",
		SourceType:    config.ImageSourceDockerImage,
		SourceImage:   "ghcr.io/catthehacker/ubuntu:full-latest",
		OutputImage:   "epar-docker-dind-catthehacker-ubuntu",
		RunnerVersion: "latest",
	}

	out, err := capturePoolStdout(t, func() error {
		return manager.buildDockerDindImage(context.Background(), ImageBuildOptions{Replace: true, Manifest: &manifest}, filepath.Join(root, "third_party", "runner-images"))
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "--progress") {
		t.Fatalf("docker build command should not require BuildKit progress support:\n%s", out)
	}
	if !strings.Contains(out, "docker build -t epar-docker-dind-catthehacker-ubuntu --platform linux/amd64") {
		t.Fatalf("docker build command missing expected base args:\n%s", out)
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

func capturePoolStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()

	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	defer func() {
		os.Stdout = oldStdout
	}()

	fnErr := fn()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(out), fnErr
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

func TestPrepareWSLDockerSourceGuestNeutralizesCloudImageState(t *testing.T) {
	provider := &recordingProvider{}
	manager := Manager{Provider: provider}
	if err := manager.prepareWSLDockerSourceGuest(context.Background(), "image-test"); err != nil {
		t.Fatal(err)
	}
	if len(provider.commands) != 1 {
		t.Fatalf("command count = %d, want 1", len(provider.commands))
	}
	command := strings.Join(provider.commands[0], "\n")
	for _, want := range []string{
		"cat >/etc/fstab",
		"EPAR: Docker image rootfs prepared for WSL imports",
		"/etc/skel/.cargo/env",
		"cloud-init.disabled",
		"walinuxagent.service",
	} {
		if !strings.Contains(command, want) {
			t.Fatalf("prepare command missing %q:\n%s", want, command)
		}
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

func TestPrepareWSLDockerSourceRootfsExportsContainerFilesystem(t *testing.T) {
	root := t.TempDir()
	oldLogged := runHostLoggedCommand
	oldOutput := runHostOutputCommand
	oldQuiet := runHostQuietCommand
	defer func() {
		runHostLoggedCommand = oldLogged
		runHostOutputCommand = oldOutput
		runHostQuietCommand = oldQuiet
	}()

	var calls []string
	runHostLoggedCommand = func(_ context.Context, _ string, name string, args ...string) error {
		calls = append(calls, name+" "+strings.Join(args, " "))
		if len(args) > 0 && args[0] == "export" {
			for i := 0; i+1 < len(args); i++ {
				if args[i] == "-o" {
					if err := os.MkdirAll(filepath.Dir(args[i+1]), 0755); err != nil {
						return err
					}
					return os.WriteFile(args[i+1], []byte("rootfs"), 0644)
				}
			}
		}
		return nil
	}
	runHostOutputCommand = func(_ context.Context, name string, args ...string) (string, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return `["PATH=/opt/bin:/usr/bin","ImageVersion=20260615.205.1","QUOTED=a'b"]`, nil
	}
	runHostQuietCommand = func(_ context.Context, name string, args ...string) error {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return nil
	}

	manager := Manager{
		Config: config.Config{
			Image: config.ImageConfig{
				SourceImage:    "ghcr.io/catthehacker/ubuntu:full-latest",
				SourcePlatform: "linux/amd64",
			},
		},
		ProjectRoot: root,
	}
	outputPath := filepath.Join(root, "work", "images", "epar-wsl-catthehacker-ubuntu.tar")
	rootfsPath, env, err := manager.prepareWSLDockerSourceRootfs(context.Background(), outputPath, filepath.Join(root, "build.log"), ImageManifest{SourceDigest: "digest-1"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := rootfsPath, filepath.Join(root, "work", "images", "epar-wsl-catthehacker-ubuntu.source.rootfs.tar"); got != want {
		t.Fatalf("rootfsPath = %q, want %q", got, want)
	}
	if _, err := os.Stat(rootfsPath); err != nil {
		t.Fatalf("exported rootfs missing: %v", err)
	}
	if _, err := os.Stat(rootfsPath + ".env"); err != nil {
		t.Fatalf("exported env cache missing: %v", err)
	}
	if _, err := os.Stat(sourceCacheManifestPath(rootfsPath)); err != nil {
		t.Fatalf("exported source cache manifest missing: %v", err)
	}
	joined := strings.Join(calls, "\n")
	for _, want := range []string{
		"docker pull --platform linux/amd64 ghcr.io/catthehacker/ubuntu:full-latest",
		"docker create --platform linux/amd64 --name epar-wsl-source-",
		"docker container inspect --format {{json .Config.Env}} epar-wsl-source-",
		"docker export -o " + rootfsPath + ".tmp epar-wsl-source-",
		"docker rm -f epar-wsl-source-",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("calls missing %q:\n%s", want, joined)
		}
	}
	if !strings.Contains(env, "export ImageVersion='20260615.205.1'") {
		t.Fatalf("env missing ImageVersion export:\n%s", env)
	}
	if !strings.Contains(env, "export QUOTED='a'\"'\"'b'") {
		t.Fatalf("env did not shell-quote single quote:\n%s", env)
	}
}

func TestPrepareWSLDockerSourceRootfsUsesCachedRootfs(t *testing.T) {
	origLogged := runHostLoggedCommand
	origOutput := runHostOutputCommand
	origQuiet := runHostQuietCommand
	defer func() {
		runHostLoggedCommand = origLogged
		runHostOutputCommand = origOutput
		runHostQuietCommand = origQuiet
	}()
	runHostLoggedCommand = func(context.Context, string, string, ...string) error {
		t.Fatal("docker command should not run when cached rootfs and env metadata exist")
		return nil
	}
	runHostOutputCommand = func(context.Context, string, ...string) (string, error) {
		t.Fatal("docker inspect should not run when cached env metadata exists")
		return "", nil
	}
	runHostQuietCommand = func(context.Context, string, ...string) error {
		t.Fatal("docker cleanup should not run when cached rootfs is reused")
		return nil
	}

	root := t.TempDir()
	outputPath := filepath.Join(root, "work", "images", "epar-wsl-catthehacker-ubuntu.tar")
	rootfsPath := filepath.Join(root, "work", "images", "epar-wsl-catthehacker-ubuntu.source.rootfs.tar")
	if err := os.MkdirAll(filepath.Dir(rootfsPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rootfsPath, []byte("cached-rootfs"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rootfsPath+".env", []byte("export ImageOS='ubuntu24'\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := writeSourceCacheManifest(sourceCacheManifestPath(rootfsPath), sourceCacheManifest{
		SourceImage:  "ghcr.io/catthehacker/ubuntu:full-latest",
		SourceDigest: "digest-1",
	}); err != nil {
		t.Fatal(err)
	}

	manager := Manager{
		Config: config.Config{
			Image: config.ImageConfig{SourceImage: "ghcr.io/catthehacker/ubuntu:full-latest"},
		},
		ProjectRoot: root,
	}
	gotRootfsPath, env, err := manager.prepareWSLDockerSourceRootfs(context.Background(), outputPath, filepath.Join(root, "build.log"), ImageManifest{SourceDigest: "digest-1"})
	if err != nil {
		t.Fatal(err)
	}
	if gotRootfsPath != rootfsPath {
		t.Fatalf("rootfsPath = %q, want %q", gotRootfsPath, rootfsPath)
	}
	if env != "export ImageOS='ubuntu24'\n" {
		t.Fatalf("env = %q", env)
	}
}

func TestSourceImageEnvContentSkipsUnsafeNames(t *testing.T) {
	content := sourceImageEnvContent([]string{
		"PATH=/usr/local/bin:/usr/bin",
		"GOOD_1=value",
		"BAD-NAME=value",
		"1BAD=value",
		"NO_EQUALS",
	})
	for _, want := range []string{"export PATH=", "export GOOD_1='value'"} {
		if !strings.Contains(content, want) {
			t.Fatalf("env content missing %q:\n%s", want, content)
		}
	}
	for _, bad := range []string{"BAD-NAME", "1BAD", "NO_EQUALS"} {
		if strings.Contains(content, bad) {
			t.Fatalf("env content included unsafe entry %q:\n%s", bad, content)
		}
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
