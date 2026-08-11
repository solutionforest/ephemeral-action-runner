package image

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
)

func TestFormatDockerSandboxesPrebuiltArchiveProgressReportsBytesRateAndElapsedWithoutFalseTotal(t *testing.T) {
	snapshot := dockerSandboxesPrebuiltArchiveProgressSnapshot{
		Label:        "Docker Sandboxes prebuilt Full archive",
		Phase:        "downloading/materializing (attempt 1/2)",
		ArchiveBytes: 3 * 1024 * 1024 * 1024,
		Elapsed:      2 * time.Minute,
		ShowRate:     true,
	}
	got := formatDockerSandboxesPrebuiltArchiveProgress(snapshot)
	want := "Docker Sandboxes prebuilt Full archive: downloading/materializing (attempt 1/2); 3.0 GiB written; 25.6 MiB/s archive-write average; elapsed 2m0s"
	if got != want {
		t.Fatalf("progress = %q, want %q", got, want)
	}
	if strings.Contains(got, "%") || strings.Contains(got, "/17") {
		t.Fatalf("progress claimed an unavailable archive total: %q", got)
	}
}

func TestDockerSandboxesPrebuiltArchiveProgressRedrawsInteractivePhaseAndTerminatesLine(t *testing.T) {
	var console bytes.Buffer
	environment := &prebuiltProgressEnvironment{console: &console}
	coordinator := &Coordinator{
		Config: config.Config{Logging: config.LoggingConfig{
			ManagerSinks:         []string{"console"},
			ManagerConsoleFormat: "text",
		}},
		environment: environment,
	}
	progress := newDockerSandboxesPrebuiltArchiveProgress(coordinator, filepath.Join(t.TempDir(), "archive.partial"), filepath.Join(t.TempDir(), "archive"), "act", "linux/arm64", 0, true)
	progress.start()
	progress.setPhase("hashing")
	progress.finish(true)
	output := console.String()
	if !strings.Contains(output, "Docker Sandboxes prebuilt Act archive: hashing; elapsed") {
		t.Fatalf("interactive progress omitted phase: %q", output)
	}
	if !strings.Contains(output, "\r\033[2K\n") {
		t.Fatalf("interactive progress did not terminate its redraw line: %q", output)
	}
	if len(environment.info) != 2 || !strings.Contains(environment.info[0], "started") || !strings.Contains(environment.info[0], "0.8-2 GiB archive output and usually 2-15 minutes") || !strings.Contains(environment.info[1], "complete") {
		t.Fatalf("progress lifecycle notices = %#v", environment.info)
	}
}

func TestDockerSandboxesPrebuiltArchiveProgressEmitsHeartbeatWhileWriterIsSilent(t *testing.T) {
	root := t.TempDir()
	partialPath := filepath.Join(root, "archive.partial")
	if err := os.WriteFile(partialPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	environment := &prebuiltAcquisitionEnvironment{}
	coordinator := &Coordinator{environment: environment}
	progress := newDockerSandboxesPrebuiltArchiveProgress(coordinator, partialPath, filepath.Join(root, "archive"), "full", "linux/amd64", 5*time.Millisecond, false)
	progress.start()
	progress.setPhase("downloading/materializing (attempt 1/2)")
	if err := os.Truncate(partialPath, 3*1024*1024); err != nil {
		progress.finish(false)
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for !strings.Contains(environment.infoMessages(), "3.0 MiB written") && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	progress.finish(true)
	output := environment.infoMessages()
	if !strings.Contains(output, "3.0 MiB written") || !strings.Contains(output, "archive-write average") {
		t.Fatalf("silent writer heartbeat missing from progress output: %s", output)
	}
}

func TestDockerSandboxesPrebuiltAcquisitionReferenceIsProfileAndPlatformSpecific(t *testing.T) {
	tests := []struct {
		profile, platform, archive, duration string
	}{
		{profile: "act", platform: "linux/amd64", archive: "0.8-2 GiB", duration: "usually 2-15 minutes"},
		{profile: "full", platform: "linux/amd64", archive: "16-24 GiB", duration: "usually 15-60 minutes"},
		{profile: "full", platform: "linux/arm64", archive: "8-16 GiB", duration: "usually 15-60 minutes"},
	}
	for _, test := range tests {
		archive, duration, ok := dockerSandboxesPrebuiltAcquisitionReference(test.profile, test.platform)
		if !ok || archive != test.archive || duration != test.duration {
			t.Errorf("reference %s/%s = %q/%q/%v, want %q/%q/true", test.profile, test.platform, archive, duration, ok, test.archive, test.duration)
		}
	}
	if _, _, ok := dockerSandboxesPrebuiltAcquisitionReference("custom", "linux/amd64"); ok {
		t.Fatal("unsupported profile received a historical acquisition reference")
	}
}

type prebuiltProgressEnvironment struct {
	Environment
	console *bytes.Buffer
	info    []string
}

func (environment *prebuiltProgressEnvironment) Infof(format string, args ...any) {
	environment.info = append(environment.info, strings.TrimSpace(fmt.Sprintf(format, args...)))
}

func (*prebuiltProgressEnvironment) Warnf(string, ...any) {}

func (*prebuiltProgressEnvironment) ProgressTerminal() bool { return true }

func (environment *prebuiltProgressEnvironment) ProgressConsole() io.Writer {
	return environment.console
}
