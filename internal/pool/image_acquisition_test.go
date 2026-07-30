package pool

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
	"github.com/solutionforest/ephemeral-action-runner/internal/image"
	"github.com/solutionforest/ephemeral-action-runner/internal/logging"
)

func TestBuildxProgressHeartbeatDefaultIsFiveSeconds(t *testing.T) {
	if got, want := buildxProgressHeartbeatInterval, 5*time.Second; got != want {
		t.Fatalf("Buildx progress heartbeat = %s, want %s", got, want)
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
