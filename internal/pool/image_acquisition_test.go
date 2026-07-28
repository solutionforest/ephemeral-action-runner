package pool

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
	"github.com/solutionforest/ephemeral-action-runner/internal/image"
	"github.com/solutionforest/ephemeral-action-runner/internal/logging"
)

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
