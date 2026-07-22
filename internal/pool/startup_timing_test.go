package pool

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
	"github.com/solutionforest/ephemeral-action-runner/internal/logging"
)

func TestSanitizeTimingErrorRedactsSecretAssignments(t *testing.T) {
	const sentinel = "timing-secret-sentinel"
	got := sanitizeTimingError(errors.New("configure failed RUNNER_TOKEN=" + sentinel + " PASSWORD=" + sentinel))
	if strings.Contains(got, sentinel) {
		t.Fatalf("sanitizeTimingError leaked sentinel: %q", got)
	}
	if !strings.Contains(got, "RUNNER_TOKEN=[REDACTED]") || !strings.Contains(got, "PASSWORD=[REDACTED]") {
		t.Fatalf("sanitizeTimingError did not retain sanitized keys: %q", got)
	}
}

func TestStartupTimingWritesOneReadableConsoleSummaryAndStructuredRecord(t *testing.T) {
	root := t.TempDir()
	logDirectory := filepath.Join(root, "logs")
	var console bytes.Buffer
	runtime, err := logging.NewRuntime(logging.Options{
		Directory:            logDirectory,
		ManagerSinks:         logging.SinkBoth,
		ManagerFileFormat:    logging.FormatJSON,
		TranscriptSinks:      logging.SinkNone,
		Stdout:               &console,
		Stderr:               &console,
	})
	if err != nil {
		t.Fatalf("create logging runtime: %v", err)
	}
	manager := Manager{
		Config:      config.Config{Provider: config.ProviderConfig{Type: "docker-dind"}, Logging: config.LoggingConfig{Directory: "logs"}},
		ProjectRoot: root,
		Logging:     runtime,
	}
	path, err := manager.StartStartupTiming()
	if err != nil {
		t.Fatalf("start startup timing: %v", err)
	}
	for _, stage := range []string{"source_image_pull", "instance_container_create"} {
		if err := manager.timeStartupStage(stage, func() error { return nil }); err != nil {
			t.Fatalf("measure %s: %v", stage, err)
		}
	}
	manager.FinishStartupTiming(nil)
	if err := runtime.Close(); err != nil {
		t.Fatalf("close logging runtime: %v", err)
	}

	output := console.String()
	if count := strings.Count(output, "\n"); count != 1 {
		t.Fatalf("console emitted %d timing records, want 1: %q", count, output)
	}
	for _, want := range []string{"DinD startup timing:", "source_image_pull=", "instance_container_create=", "total_startup=", "log: " + path} {
		if !strings.Contains(output, want) {
			t.Fatalf("console summary missing %q: %q", want, output)
		}
	}

	content, err := os.ReadFile(filepath.Join(logDirectory, logging.ManagerFilename))
	if err != nil {
		t.Fatalf("read structured manager log: %v", err)
	}
	var record map[string]any
	if err := json.Unmarshal(content, &record); err != nil {
		t.Fatalf("decode structured manager record: %v\n%s", err, content)
	}
	message, ok := record["msg"].(string)
	if !ok || !strings.Contains(message, "DinD startup timing:") || !strings.Contains(message, "source_image_pull=") || !strings.Contains(message, "instance_container_create=") || !strings.Contains(message, "total_startup=") || !strings.Contains(message, "log: "+path) {
		t.Fatalf("unexpected structured message: %#v", record["msg"])
	}
	if record["provider"] != "docker-dind" || record["operation"] != "startup" || record["logPath"] != path {
		t.Fatalf("structured record missing context: %#v", record)
	}
	stages, ok := record["stages"].(map[string]any)
	if !ok || stages["source_image_pull"] == nil || stages["instance_container_create"] == nil || stages["total_startup"] == nil {
		t.Fatalf("structured record missing stage durations: %#v", record["stages"])
	}
}
