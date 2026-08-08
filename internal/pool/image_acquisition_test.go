package pool

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
	"github.com/solutionforest/ephemeral-action-runner/internal/hosttrust"
	"github.com/solutionforest/ephemeral-action-runner/internal/image"
	"github.com/solutionforest/ephemeral-action-runner/internal/logging"
)

func TestBuildxProgressHeartbeatDefaultIsFiveSeconds(t *testing.T) {
	if got, want := buildxProgressHeartbeatInterval, 5*time.Second; got != want {
		t.Fatalf("Buildx progress heartbeat = %s, want %s", got, want)
	}
}

func TestDockerContainerBuildUsesMonitoredBuildxProgress(t *testing.T) {
	t.Setenv("EPAR_STATE_HOME", filepath.Join(t.TempDir(), "state"))
	root := t.TempDir()
	for _, dir := range []string{
		filepath.Join(root, "scripts", "guest", "ubuntu"),
		filepath.Join(root, "scripts", "container", "ubuntu"),
	} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	var console bytes.Buffer
	runtime, err := logging.NewRuntime(logging.Options{
		Directory:            filepath.Join(root, "work", "logs"),
		ManagerSinks:         logging.SinkConsole,
		ManagerConsoleFormat: logging.FormatJSON,
		TranscriptSinks:      logging.SinkFile,
		Stdout:               &console,
		Stderr:               &console,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	cfg := config.Default()
	cfg.Image.SourceImage = "source:latest"
	cfg.Image.OutputImage = "output:latest"
	cfg.Image.RunnerVersion = "2.332.0"
	cfg.Logging.Directory = "work/logs"
	cfg.Logging.ManagerSinks = []string{"console"}
	cfg.Logging.TranscriptSinks = []string{"file"}
	cfg.Logging.ManagerConsoleFormat = "json"
	cfg.Provider.Type = "docker-container"
	manager := Manager{Config: cfg, Logging: runtime, ProjectRoot: root}
	trustPath := filepath.Join(root, "build-trust.pem")
	writeTestCACertificate(t, trustPath, "Docker Container Build Trust")
	buildTrust := hostTrustSnapshotFromFile(t, trustPath, "windows", []string{"system"})
	manager.buildTrustResolver = func(context.Context) (hosttrust.Snapshot, error) { return buildTrust, nil }
	var buildTrustBundle strings.Builder
	for _, certificate := range buildTrust.Certificates {
		buildTrustBundle.Write(certificate.PEM)
	}

	previousTerminal := dockerPullProgressTerminal
	previousInterval := buildxProgressHeartbeatInterval
	previousLogged := runHostLoggedCommand
	previousOutput := runHostOutputCommand
	previousQuiet := runHostQuietCommand
	previousRun := runHostCommand
	previousPull := pullDockerSourceCommand
	dockerPullProgressTerminal = func() bool { return false }
	buildxProgressHeartbeatInterval = 5 * time.Millisecond
	t.Cleanup(func() {
		dockerPullProgressTerminal = previousTerminal
		buildxProgressHeartbeatInterval = previousInterval
		runHostLoggedCommand = previousLogged
		runHostOutputCommand = previousOutput
		runHostQuietCommand = previousQuiet
		runHostCommand = previousRun
		pullDockerSourceCommand = previousPull
	})

	builds := 0
	runHostLoggedCommand = func(_ context.Context, _ string, stdout, stderr io.Writer, name string, args ...string) error {
		if name != "docker" || len(args) < 2 || args[0] != "buildx" || args[1] != "build" {
			t.Fatalf("unexpected logged command: %s %v", name, args)
		}
		if strings.Contains(strings.Join(args, " "), "--progress") {
			t.Fatalf("Docker Container build unexpectedly added a Buildx progress flag: %v", args)
		}
		builds++
		digest := "sha256:" + strings.Repeat("c", 64)
		_, _ = io.WriteString(stderr, "#12 "+digest+" 1.00GB / 2.00GB 10.0s\n")
		time.Sleep(16 * time.Millisecond)
		_, _ = io.WriteString(stdout, "#12 "+digest+" 2.00GB / 2.00GB 20.0s done\n")
		return nil
	}
	runHostOutputCommand = func(_ context.Context, _ string, args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "buildx" && args[1] == "inspect" {
			if len(args) >= 3 && args[2] == "--bootstrap" {
				return "Status: running\n", nil
			}
			return "", errors.New("builder not found")
		}
		if len(args) >= 1 && args[0] == "exec" {
			if strings.Contains(strings.Join(args, " "), "/etc/buildkit/buildkitd.toml") {
				return "[registry.\"docker.io\"]\n", nil
			}
			return buildTrustBundle.String(), nil
		}
		if len(args) >= 1 && (args[0] == "inspect" || args[0] == "image") {
			return "test-buildkit-image", nil
		}
		if len(args) >= 1 && args[0] == "info" {
			return "test-docker-engine", nil
		}
		return "", nil
	}
	runHostQuietCommand = func(context.Context, string, ...string) error { return nil }
	runHostCommand = func(_ context.Context, name string, args ...string) error {
		if name == "docker" && len(args) > 0 && args[0] == "pull" {
			if len(args) != 2 || args[1] != "moby/buildkit:buildx-stable-1" {
				t.Fatalf("Docker Container Buildx path unexpectedly pre-pulled %v", args)
			}
		}
		return nil
	}
	pullDockerSourceCommand = func(*Manager, context.Context, dockerSourcePullOptions) error {
		t.Fatal("Docker Container Buildx path must not call pullDockerSource")
		return nil
	}

	runnerPackage := []byte("test-actions-runner-package")
	runnerServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		_, _ = response.Write(runnerPackage)
	}))
	defer runnerServer.Close()
	runnerDigest := sha256.Sum256(runnerPackage)
	manifest := ImageManifest{
		SchemaVersion:     imageManifestSchemaVersion,
		ProviderType:      "docker-container",
		SourceType:        config.ImageSourceDockerImage,
		SourceImage:       cfg.Image.SourceImage,
		OutputImage:       cfg.Image.OutputImage,
		RunnerSelector:    "latest",
		RunnerVersion:     cfg.Image.RunnerVersion,
		RunnerAssetName:   "actions-runner-linux-x64-2.332.0.tar.gz",
		RunnerAssetURL:    runnerServer.URL,
		RunnerAssetDigest: "sha256:" + fmt.Sprintf("%x", runnerDigest),
	}
	if err := manager.buildDockerContainerImage(context.Background(), ImageBuildOptions{Replace: true, Manifest: &manifest}, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if builds != 1 {
		t.Fatalf("Docker Container Buildx commands = %d, want 1", builds)
	}

	consoleText := console.String()
	if strings.Count(consoleText, "Docker Container image build:") < 2 {
		t.Fatalf("non-TTY Docker Container build omitted periodic progress heartbeat: %q", consoleText)
	}
	buildLogPath := filepath.Join(root, "work", "logs", "builds", "outputlatest.docker-build.log")
	for _, wanted := range []string{"\"provider\":\"docker-container\"", "\"operation\":\"buildx-build\"", fmt.Sprintf("\"logPath\":%q", buildLogPath), "Docker Container image build complete"} {
		if !strings.Contains(consoleText, wanted) {
			t.Fatalf("Docker Container build progress missing %q: %q", wanted, consoleText)
		}
	}
	raw, err := os.ReadFile(buildLogPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "1.00GB / 2.00GB") || !strings.Contains(string(raw), "2.00GB / 2.00GB") {
		t.Fatalf("Docker Container Buildx raw transcript was not preserved: %q", raw)
	}
}

func TestDockerContainerBuildxInteractiveProgressRedrawIsBounded(t *testing.T) {
	root := t.TempDir()
	var managerConsole, terminalConsole bytes.Buffer
	runtime, err := logging.NewRuntime(logging.Options{
		Directory:       root,
		ManagerSinks:    logging.SinkConsole,
		TranscriptSinks: logging.SinkFile,
		Stdout:          &managerConsole,
		Stderr:          &managerConsole,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	cfg := config.Default()
	cfg.Provider.Type = "docker-container"
	cfg.Logging.Directory = root
	cfg.Logging.ManagerSinks = []string{"console"}
	cfg.Logging.TranscriptSinks = []string{"file"}
	cfg.Logging.ManagerConsoleFormat = "text"
	manager := Manager{Config: cfg, Logging: runtime}
	previousTerminal, previousWidth, previousConsole := dockerPullProgressTerminal, progressTerminalWidth, dockerPullProgressConsole
	previousLogged := runHostLoggedCommand
	dockerPullProgressTerminal = func() bool { return true }
	progressTerminalWidth = func() int { return 40 }
	dockerPullProgressConsole = &terminalConsole
	runHostLoggedCommand = func(_ context.Context, _ string, _, stderr io.Writer, _ string, _ ...string) error {
		digest := "sha256:" + strings.Repeat("b", 64)
		_, _ = io.WriteString(stderr, "#123 "+digest+" 1.00GB / 2.00GB 10.0s\n")
		return nil
	}
	t.Cleanup(func() {
		dockerPullProgressTerminal = previousTerminal
		progressTerminalWidth = previousWidth
		dockerPullProgressConsole = previousConsole
		runHostLoggedCommand = previousLogged
	})

	if err := manager.runHostBuildxLogged(context.Background(), filepath.Join(root, "builds", "docker-container.docker-build.log"), "docker", "buildx", "build"); err != nil {
		t.Fatal(err)
	}
	terminalText := terminalConsole.String()
	if !strings.HasPrefix(terminalText, "\r\033[2K") || strings.Count(terminalText, "\r\033[2K") != 1 {
		t.Fatalf("interactive Docker Container progress was not rendered as one redraw: %q", terminalText)
	}
	line := strings.TrimSuffix(strings.TrimPrefix(terminalText, "\r\033[2K"), "\n")
	if len([]rune(line)) > 39 || !strings.HasPrefix(line, "Docker Container") || !strings.Contains(line, terminalProgressEllipsis) || !strings.HasSuffix(line, "elapsed 0s") {
		t.Fatalf("interactive Docker Container progress was not bounded with a middle ellipsis: %q", line)
	}
	if strings.Contains(managerConsole.String(), "Docker Container image build:") {
		t.Fatalf("interactive Docker Container progress was duplicated through the manager logger: %q", managerConsole.String())
	}
}

func TestDockerContainerBuildxRawConsoleSuppressesProgressSummary(t *testing.T) {
	root := t.TempDir()
	var console bytes.Buffer
	runtime, err := logging.NewRuntime(logging.Options{
		Directory:       root,
		ManagerSinks:    logging.SinkConsole,
		TranscriptSinks: logging.SinkConsole,
		Stdout:          &console,
		Stderr:          &console,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	cfg := config.Default()
	cfg.Provider.Type = "docker-container"
	cfg.Logging.Directory = root
	cfg.Logging.ManagerSinks = []string{"console"}
	cfg.Logging.TranscriptSinks = []string{"console"}
	manager := Manager{Config: cfg, Logging: runtime}
	previousTerminal := dockerPullProgressTerminal
	previousLogged := runHostLoggedCommand
	dockerPullProgressTerminal = func() bool { return false }
	runHostLoggedCommand = func(_ context.Context, _ string, _, stderr io.Writer, _ string, _ ...string) error {
		_, _ = io.WriteString(stderr, "#12 raw Buildx output\n")
		return nil
	}
	t.Cleanup(func() {
		dockerPullProgressTerminal = previousTerminal
		runHostLoggedCommand = previousLogged
	})

	if err := manager.runHostBuildxLogged(context.Background(), filepath.Join(root, "builds", "docker-container.docker-build.log"), "docker", "buildx", "build"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(console.String(), "#12 raw Buildx output") {
		t.Fatalf("raw Buildx console transcript was not emitted: %q", console.String())
	}
	if strings.Contains(console.String(), "Docker Container image build:") {
		t.Fatalf("raw Buildx console transcript was duplicated by summarized progress: %q", console.String())
	}
}

func TestDockerContainerBuildxFailureDoesNotReportCompletion(t *testing.T) {
	root := t.TempDir()
	var console bytes.Buffer
	runtime, err := logging.NewRuntime(logging.Options{
		Directory:       root,
		ManagerSinks:    logging.SinkConsole,
		TranscriptSinks: logging.SinkFile,
		Stdout:          &console,
		Stderr:          &console,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	cfg := config.Default()
	cfg.Provider.Type = "docker-container"
	cfg.Logging.Directory = root
	cfg.Logging.ManagerSinks = []string{"console"}
	cfg.Logging.TranscriptSinks = []string{"file"}
	manager := Manager{Config: cfg, Logging: runtime}
	previousTerminal := dockerPullProgressTerminal
	previousLogged := runHostLoggedCommand
	dockerPullProgressTerminal = func() bool { return false }
	wantErr := errors.New("Buildx failed")
	runHostLoggedCommand = func(_ context.Context, _ string, _, stderr io.Writer, _ string, _ ...string) error {
		digest := "sha256:" + strings.Repeat("f", 64)
		_, _ = io.WriteString(stderr, "#12 "+digest+" 1.00GB / 2.00GB 10.0s\n")
		return wantErr
	}
	t.Cleanup(func() {
		dockerPullProgressTerminal = previousTerminal
		runHostLoggedCommand = previousLogged
	})

	err = manager.runHostBuildxLogged(context.Background(), filepath.Join(root, "builds", "docker-container-failed.docker-build.log"), "docker", "buildx", "build")
	if !errors.Is(err, wantErr) {
		t.Fatalf("Buildx error = %v, want %v", err, wantErr)
	}
	if !strings.Contains(console.String(), "Docker Container image build:") {
		t.Fatalf("failed Docker Container build omitted progress: %q", console.String())
	}
	if strings.Contains(console.String(), "Docker Container image build complete") {
		t.Fatalf("failed Docker Container build incorrectly reported completion: %q", console.String())
	}
}

func TestDockerContainerBuildxCancellationDoesNotReportCompletion(t *testing.T) {
	root := t.TempDir()
	var console bytes.Buffer
	runtime, err := logging.NewRuntime(logging.Options{
		Directory:       root,
		ManagerSinks:    logging.SinkConsole,
		TranscriptSinks: logging.SinkFile,
		Stdout:          &console,
		Stderr:          &console,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	cfg := config.Default()
	cfg.Provider.Type = "docker-container"
	cfg.Logging.Directory = root
	cfg.Logging.ManagerSinks = []string{"console"}
	cfg.Logging.TranscriptSinks = []string{"file"}
	manager := Manager{Config: cfg, Logging: runtime}
	previousTerminal := dockerPullProgressTerminal
	previousLogged := runHostLoggedCommand
	dockerPullProgressTerminal = func() bool { return false }
	runHostLoggedCommand = func(ctx context.Context, _ string, _, _ io.Writer, _ string, _ ...string) error { return ctx.Err() }
	t.Cleanup(func() {
		dockerPullProgressTerminal = previousTerminal
		runHostLoggedCommand = previousLogged
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = manager.runHostBuildxLogged(ctx, filepath.Join(root, "builds", "docker-container-cancelled.docker-build.log"), "docker", "buildx", "build")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Buildx error = %v, want context.Canceled", err)
	}
	if strings.Contains(console.String(), "Docker Container image build complete") {
		t.Fatalf("cancelled Docker Container build incorrectly reported completion: %q", console.String())
	}
}

func TestBuildxProgressUsesManagerLoggerAndPreservesRawTranscript(t *testing.T) {
	root := t.TempDir()
	var console bytes.Buffer
	runtime, err := logging.NewRuntime(logging.Options{
		Directory:       root,
		ManagerSinks:    logging.SinkConsole,
		TranscriptSinks: logging.SinkFile,
		Stdout:          &console,
		Stderr:          &console,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	cfg := config.Default()
	cfg.Provider.Type = "docker-sandboxes"
	cfg.Logging.Directory = root
	cfg.Logging.ManagerSinks = []string{"console"}
	cfg.Logging.TranscriptSinks = []string{"file"}
	manager := Manager{Config: cfg, Logging: runtime}
	logPath := filepath.Join(root, "builds", "docker-sandboxes-test.docker-build.log")
	previousTerminal := dockerPullProgressTerminal
	dockerPullProgressTerminal = func() bool { return false }
	t.Cleanup(func() { dockerPullProgressTerminal = previousTerminal })
	previousLogged := runHostLoggedCommand
	runHostLoggedCommand = func(_ context.Context, _ string, stdout, stderr io.Writer, name string, args ...string) error {
		if name != "docker" || len(args) < 2 || args[0] != "buildx" || args[1] != "build" {
			t.Fatalf("unexpected command: %s %v", name, args)
		}
		digest := "sha256:" + strings.Repeat("d", 64)
		_, _ = io.WriteString(stderr, "#12 "+digest+" 1.00GB / 2.00GB 10.0s\n")
		_, _ = io.WriteString(stdout, "#12 "+digest+" 2.00GB / 2.00GB 20.0s done\n")
		return nil
	}
	t.Cleanup(func() { runHostLoggedCommand = previousLogged })

	if err := manager.runHostBuildxLogged(context.Background(), logPath, "docker", "buildx", "build"); err != nil {
		t.Fatal(err)
	}
	if err := manager.releaseTranscript(logPath); err != nil {
		t.Fatal(err)
	}

	consoleText := console.String()
	if !strings.Contains(consoleText, "Docker Sandboxes template build: 953.7 MiB/1.9 GiB (50%); 0/1 layer downloads complete; BuildKit step #12") {
		t.Fatalf("manager console did not receive Buildx progress: %q", consoleText)
	}
	if !strings.Contains(consoleText, "Docker Sandboxes template Buildx phase complete; finalizing evidence and importing the template next") {
		t.Fatalf("manager console did not receive Buildx completion: %q", consoleText)
	}
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "1.00GB / 2.00GB") || !strings.Contains(string(raw), "2.00GB / 2.00GB") {
		t.Fatalf("raw Buildx transcript was not preserved: %q", raw)
	}
}

func TestBuildxProgressHeartbeatShowsLongSilentStepIsAlive(t *testing.T) {
	root := t.TempDir()
	var console bytes.Buffer
	runtime, err := logging.NewRuntime(logging.Options{
		Directory:       root,
		ManagerSinks:    logging.SinkConsole,
		TranscriptSinks: logging.SinkFile,
		Stdout:          &console,
		Stderr:          &console,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	cfg := config.Default()
	cfg.Provider.Type = "docker-sandboxes"
	cfg.Logging.Directory = root
	cfg.Logging.ManagerSinks = []string{"console"}
	cfg.Logging.TranscriptSinks = []string{"file"}
	manager := Manager{Config: cfg, Logging: runtime}
	previousTerminal := dockerPullProgressTerminal
	dockerPullProgressTerminal = func() bool { return false }
	t.Cleanup(func() { dockerPullProgressTerminal = previousTerminal })
	previousInterval := buildxProgressHeartbeatInterval
	buildxProgressHeartbeatInterval = 5 * time.Millisecond
	t.Cleanup(func() { buildxProgressHeartbeatInterval = previousInterval })
	previousLogged := runHostLoggedCommand
	runHostLoggedCommand = func(_ context.Context, _ string, _, _ io.Writer, _ string, _ ...string) error {
		time.Sleep(20 * time.Millisecond)
		return nil
	}
	t.Cleanup(func() { runHostLoggedCommand = previousLogged })

	if err := manager.runHostBuildxLogged(context.Background(), filepath.Join(root, "builds", "docker-sandboxes-heartbeat.docker-build.log"), "docker", "buildx", "build"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(console.String(), "Docker Sandboxes template build: elapsed") {
		t.Fatalf("long silent Buildx step had no heartbeat: %q", console.String())
	}
}

func TestDockerPullProgressUsesManagerLoggerAndPreservesSourceTranscript(t *testing.T) {
	root := t.TempDir()
	var console bytes.Buffer
	runtime, err := logging.NewRuntime(logging.Options{
		Directory:       root,
		ManagerSinks:    logging.SinkConsole,
		TranscriptSinks: logging.SinkFile,
		Stdout:          &console,
		Stderr:          &console,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	cfg := config.Default()
	cfg.Logging.Directory = root
	manager := Manager{Config: cfg, Logging: runtime}
	logPath := filepath.Join(root, "builds", "source.docker-pull.log")
	previousTerminal := dockerPullProgressTerminal
	dockerPullProgressTerminal = func() bool { return false }
	t.Cleanup(func() { dockerPullProgressTerminal = previousTerminal })
	manager.writeDockerPullProgress(logPath, map[string]image.DockerPullProgress{
		"layer-a": {Current: 512, Total: 1024, Completed: false},
		"layer-b": {Completed: true},
	})
	transcriptWriter, err := manager.transcript(logPath, "", "docker-pull")
	if err != nil {
		t.Fatal(err)
	}
	writeDockerPullEvent(transcriptWriter.Stdout, image.DockerPullEvent{ID: "layer-a", Status: "Downloading"})
	manager.writeDockerPullNotice(logPath, "Docker source pull complete: example.invalid/source:latest")
	if err := manager.releaseTranscript(logPath); err != nil {
		t.Fatal(err)
	}

	consoleText := console.String()
	if !strings.Contains(consoleText, "Docker source pull: 1/2 layers complete; 512 B/1.0 KiB (50%); 1 layer(s) size pending") {
		t.Fatalf("manager console did not receive pull progress: %q", consoleText)
	}
	if !strings.Contains(consoleText, "Docker source pull complete: example.invalid/source:latest") {
		t.Fatalf("manager console did not receive pull completion notice: %q", consoleText)
	}
	transcript, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(transcript), "Docker source pull complete: example.invalid/source:latest") {
		t.Fatalf("source transcript did not retain pull completion notice: %q", transcript)
	}
	if !strings.Contains(string(transcript), "layer-a Downloading") {
		t.Fatalf("source transcript did not retain raw pull event: %q", transcript)
	}
}

func TestDockerPullProgressHonorsManagerJSONConsoleFormat(t *testing.T) {
	root := t.TempDir()
	var console bytes.Buffer
	runtime, err := logging.NewRuntime(logging.Options{
		Directory:            root,
		ManagerSinks:         logging.SinkConsole,
		ManagerConsoleFormat: logging.FormatJSON,
		TranscriptSinks:      logging.SinkFile,
		Stdout:               &console,
		Stderr:               &console,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	cfg := config.Default()
	cfg.Logging.Directory = root
	cfg.Logging.ManagerConsoleFormat = "json"
	cfg.Provider.Type = "docker-container"
	manager := Manager{Config: cfg, Logging: runtime}
	previousTerminal := dockerPullProgressTerminal
	dockerPullProgressTerminal = func() bool { return true }
	t.Cleanup(func() { dockerPullProgressTerminal = previousTerminal })
	logPath := filepath.Join(root, "builds", "source.docker-pull.log")
	manager.writeDockerPullProgress(logPath, map[string]image.DockerPullProgress{"layer-a": {Current: 1, Total: 2}})

	var record map[string]any
	if err := json.Unmarshal(console.Bytes(), &record); err != nil {
		t.Fatalf("decode manager JSON console: %v: %q", err, console.String())
	}
	if record["msg"] != "Docker source pull: 0/1 layers complete; 1 B/2 B (50%)" || record["provider"] != "docker-container" || record["operation"] != "docker-pull" || record["logPath"] != logPath {
		t.Fatalf("manager JSON console missing pull context: %#v", record)
	}
}

func TestDockerPullProgressUsesSingleLineTerminalDisplayForTextManagerConsole(t *testing.T) {
	root := t.TempDir()
	var managerConsole, terminalConsole bytes.Buffer
	runtime, err := logging.NewRuntime(logging.Options{
		Directory:       root,
		ManagerSinks:    logging.SinkConsole,
		TranscriptSinks: logging.SinkFile,
		Stdout:          &managerConsole,
		Stderr:          &managerConsole,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	previousTerminal, previousConsole := dockerPullProgressTerminal, dockerPullProgressConsole
	dockerPullProgressTerminal = func() bool { return true }
	dockerPullProgressConsole = &terminalConsole
	t.Cleanup(func() {
		dockerPullProgressTerminal = previousTerminal
		dockerPullProgressConsole = previousConsole
	})

	manager := Manager{Config: config.Default(), Logging: runtime}
	manager.writeDockerPullProgress("source.docker-pull.log", map[string]image.DockerPullProgress{"layer-a": {Current: 1, Total: 2}})

	if got, want := terminalConsole.String(), "\r\033[2KDocker source pull: 0/1 layers complete; 1 B/2 B (50%)"; got != want {
		t.Fatalf("terminal pull progress = %q, want %q", got, want)
	}
	if got := managerConsole.String(); got != "" {
		t.Fatalf("interactive pull progress was duplicated through manager logger: %q", got)
	}
}

func TestBuildxDockerArchiveDestinationRecognizesDirectExporter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runner-template.tar.partial")
	for _, args := range [][]string{
		{"buildx", "build", "--output", "type=docker,dest=" + path},
		{"buildx", "build", "--output=type=docker,dest=" + path},
	} {
		if got := buildxDockerArchiveDestination(args); got != path {
			t.Fatalf("buildxDockerArchiveDestination(%v) = %q, want %q", args, got, path)
		}
	}
	if got := buildxDockerArchiveDestination([]string{"buildx", "build", "--load"}); got != "" {
		t.Fatalf("non-archive output returned %q", got)
	}
}
