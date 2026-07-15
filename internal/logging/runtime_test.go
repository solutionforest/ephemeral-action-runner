package logging

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestManagerFansOutByLevelAndWritesJSONFile(t *testing.T) {
	root := t.TempDir()
	var stdout, stderr bytes.Buffer
	runtime, err := NewRuntime(Options{
		Directory:            root,
		ManagerSinks:         SinkBoth,
		ManagerConsoleFormat: FormatText,
		ManagerFileFormat:    FormatJSON,
		TranscriptSinks:      SinkNone,
		Level:                slog.LevelDebug,
		Stdout:               &stdout,
		Stderr:               &stderr,
	})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	runtime.Manager().Debug("debug message")
	runtime.Manager().Info("info message")
	runtime.Manager().Warn("warn message")
	runtime.Manager().Error("error message")
	if err := runtime.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if got := stdout.String(); !strings.Contains(got, "debug message") || !strings.Contains(got, "info message") || strings.Contains(got, "warn message") {
		t.Fatalf("stdout routing = %q", got)
	}
	if got := stderr.String(); !strings.Contains(got, "warn message") || !strings.Contains(got, "error message") || strings.Contains(got, "info message") {
		t.Fatalf("stderr routing = %q", got)
	}
	data, err := os.ReadFile(ManagerPath(root))
	if err != nil {
		t.Fatalf("ReadFile manager: %v", err)
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	var messages []string
	for scanner.Scan() {
		var record map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			t.Fatalf("manager line is not JSON: %v", err)
		}
		messages = append(messages, record["msg"].(string))
	}
	if got := strings.Join(messages, ","); got != "debug message,info message,warn message,error message" {
		t.Fatalf("manager messages = %q", got)
	}
}

func TestDefaultManagerTextFormatIsHumanReadable(t *testing.T) {
	var stdout bytes.Buffer
	runtime, err := NewRuntime(Options{Directory: t.TempDir(), ManagerSinks: SinkConsole, Stdout: &stdout})
	if err != nil {
		t.Fatal(err)
	}
	runtime.Manager().Info("runner ready", "provider", "docker-dind", "attempt", 2)
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	parts := strings.SplitN(strings.TrimSpace(stdout.String()), " ", 3)
	if len(parts) != 3 {
		t.Fatalf("console line = %q", stdout.String())
	}
	if _, err := time.Parse(humanTimestampLayout, parts[0]); err != nil {
		t.Fatalf("timestamp %q is not human RFC3339 milliseconds: %v", parts[0], err)
	}
	if parts[1] != "[INFO]" || parts[2] != "runner ready" {
		t.Fatalf("console line = %q", stdout.String())
	}
}

func TestCustomManagerAndTranscriptTextFormats(t *testing.T) {
	var stdout bytes.Buffer
	runtime, err := NewRuntime(Options{
		Directory:                   t.TempDir(),
		ManagerSinks:                SinkConsole,
		ManagerConsoleTextFormat:    "[{level}] {message}{attributes}",
		TranscriptSinks:             SinkConsole,
		TranscriptConsoleTextFormat: "[{stream}] {instance}/{component}: {message}{attributes}",
		Stdout:                      &stdout,
	})
	if err != nil {
		t.Fatal(err)
	}
	runtime.Manager().Info("manager ready", "provider", "wsl")
	path, _ := InstancePath(runtime.Directory(), "runner-1", "guest")
	transcript, err := runtime.OpenTranscript(TranscriptMetadata{Category: CategoryInstances, Instance: "runner-1", Component: "guest", Attributes: map[string]string{"attempt": "2"}}, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transcript.Stdout.Write([]byte("guest ready\n")); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	want := "[INFO] manager ready provider=wsl\n[stdout] runner-1/guest: guest ready attempt=2\n"
	if got := stdout.String(); got != want {
		t.Fatalf("custom text output = %q, want %q", got, want)
	}
}

func TestJSONConsoleRejectsCustomTextFormat(t *testing.T) {
	_, err := NewRuntime(Options{Directory: t.TempDir(), ManagerSinks: SinkConsole, ManagerConsoleFormat: FormatJSON, ManagerConsoleTextFormat: "{level} {message}"})
	if err == nil || !strings.Contains(err.Error(), "only with text output") {
		t.Fatalf("NewRuntime() error = %v, want text-only validation", err)
	}
}

func TestManagerFileInitializationFallbackRequiresConsole(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(ManagerPath(root), 0o755); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	runtime, err := NewRuntime(Options{Directory: root, ManagerSinks: SinkBoth, Stdout: &stdout, Stderr: &stderr})
	if err != nil {
		t.Fatalf("NewRuntime with console fallback: %v", err)
	}
	if len(runtime.InitializationWarnings()) != 1 || strings.Count(stderr.String(), "warning: logging file sink disabled:") != 1 {
		t.Fatalf("warnings = %#v, stderr = %q", runtime.InitializationWarnings(), stderr.String())
	}
	runtime.Manager().Info("console survived")
	if !strings.Contains(stdout.String(), "console survived") {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if err := runtime.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := NewRuntime(Options{Directory: root, ManagerSinks: SinkFile}); err == nil {
		t.Fatal("sole manager file sink unexpectedly fell back")
	}
}

func TestManagerFanoutIsolatesSinkFailureAndWarnsOnce(t *testing.T) {
	root := t.TempDir()
	var stderr bytes.Buffer
	runtime, err := NewRuntime(Options{Directory: root, ManagerSinks: SinkBoth, Stdout: failingWriter{}, Stderr: &stderr})
	if err != nil {
		t.Fatal(err)
	}
	runtime.Manager().Info("first survives")
	runtime.Manager().Info("second survives")
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	if count := strings.Count(stderr.String(), "warning: logging sink write failed:"); count != 1 {
		t.Fatalf("warning count = %d, stderr=%q", count, stderr.String())
	}
	data, err := os.ReadFile(ManagerPath(root))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "first survives") || !strings.Contains(string(data), "second survives") {
		t.Fatalf("surviving file sink = %q", data)
	}
}

func TestTranscriptFileInitializationFallbackRequiresConsole(t *testing.T) {
	root := t.TempDir()
	path, _ := InstancePath(root, "runner-fallback", "guest")
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	runtime, err := NewRuntime(Options{Directory: root, TranscriptSinks: SinkBoth, Stdout: &stdout, Stderr: &stderr})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	transcript, err := runtime.OpenTranscript(TranscriptMetadata{Category: CategoryInstances, Instance: "runner-fallback", Component: "guest"}, path)
	if err != nil {
		t.Fatalf("OpenTranscript with console fallback: %v", err)
	}
	_, _ = transcript.Stdout.Write([]byte("console only\n"))
	if !strings.Contains(stdout.String(), "console only") || strings.Count(stderr.String(), "warning: logging file sink disabled:") != 1 {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	_ = transcript.Close()
	_ = runtime.Close()

	fileOnly, err := NewRuntime(Options{Directory: root, TranscriptSinks: SinkFile})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fileOnly.OpenTranscript(TranscriptMetadata{Category: CategoryInstances, Instance: "runner-fallback", Component: "guest"}, path); err == nil {
		t.Fatal("sole transcript file sink unexpectedly fell back")
	}
	_ = fileOnly.Close()
}

func TestTranscriptCoalescesRawWritesAndFlushesContextualPartialLines(t *testing.T) {
	root := t.TempDir()
	var stdout, stderr bytes.Buffer
	fixed := time.Date(2026, 7, 15, 12, 0, 0, 123, time.UTC)
	runtime, err := NewRuntime(Options{
		Directory:               root,
		TranscriptSinks:         SinkBoth,
		TranscriptConsoleFormat: FormatJSON,
		Stdout:                  &stdout,
		Stderr:                  &stderr,
		Now:                     func() time.Time { return fixed },
	})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	path, err := InstancePath(root, "runner-1", "docker-dind")
	if err != nil {
		t.Fatalf("InstancePath: %v", err)
	}
	transcript, err := runtime.OpenTranscript(TranscriptMetadata{SessionID: "session-1", Category: CategoryInstances, Instance: "runner-1", Component: "provider", Provider: "docker-dind"}, path)
	if err != nil {
		t.Fatalf("OpenTranscript: %v", err)
	}
	if _, err := transcript.Stdout.Write([]byte("one")); err != nil {
		t.Fatalf("write stdout 1: %v", err)
	}
	if _, err := transcript.Stderr.Write([]byte("err\n")); err != nil {
		t.Fatalf("write stderr: %v", err)
	}
	if _, err := transcript.File.Write([]byte("raw-event\n")); err != nil {
		t.Fatalf("write raw file: %v", err)
	}
	if _, err := transcript.Stdout.Write([]byte(" two\npartial")); err != nil {
		t.Fatalf("write stdout 2: %v", err)
	}
	if err := transcript.Close(); err != nil {
		t.Fatalf("Close transcript: %v", err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatalf("Close runtime: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile transcript: %v", err)
	}
	if got, want := string(raw), "oneerr\nraw-event\n two\npartial"; got != want {
		t.Fatalf("raw transcript = %q, want %q", got, want)
	}
	stdoutEnvelopes := decodeEnvelopeLines(t, stdout.Bytes())
	if len(stdoutEnvelopes) != 2 || stdoutEnvelopes[0].Message != "one two" || stdoutEnvelopes[1].Message != "partial" {
		t.Fatalf("stdout envelopes = %#v", stdoutEnvelopes)
	}
	if stdoutEnvelopes[0].Instance != "runner-1" || stdoutEnvelopes[0].Component != "provider" || stdoutEnvelopes[0].Stream != "stdout" || stdoutEnvelopes[0].Timestamp != fixed.Format(time.RFC3339Nano) {
		t.Fatalf("stdout context = %#v", stdoutEnvelopes[0])
	}
	stderrEnvelopes := decodeEnvelopeLines(t, stderr.Bytes())
	if len(stderrEnvelopes) != 1 || stderrEnvelopes[0].Message != "err" || stderrEnvelopes[0].Stream != "stderr" {
		t.Fatalf("stderr envelopes = %#v", stderrEnvelopes)
	}
}

func TestTranscriptFanoutStillFramesConsoleWhenFileWriteFails(t *testing.T) {
	root := t.TempDir()
	var stdout bytes.Buffer
	runtime, err := NewRuntime(Options{Directory: root, TranscriptSinks: SinkBoth, TranscriptConsoleFormat: FormatJSON, Stdout: &stdout})
	if err != nil {
		t.Fatal(err)
	}
	path, _ := InstancePath(root, "runner-failure", "guest")
	transcript, err := runtime.OpenTranscript(TranscriptMetadata{Category: CategoryInstances, Instance: "runner-failure", Component: "guest"}, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := transcript.file.Close(); err != nil {
		t.Fatal(err)
	}
	written, err := transcript.Stdout.Write([]byte("console survives\n"))
	if err == nil || written != 0 {
		t.Fatalf("write = %d, %v; want file error", written, err)
	}
	envelopes := decodeEnvelopeLines(t, stdout.Bytes())
	if len(envelopes) != 1 || envelopes[0].Message != "console survives" {
		t.Fatalf("console envelopes = %#v", envelopes)
	}
	_ = transcript.Close()
	_ = runtime.Close()
}

func TestTranscriptConcurrentWritesRemainWhole(t *testing.T) {
	root := t.TempDir()
	runtime, err := NewRuntime(Options{Directory: root, TranscriptSinks: SinkFile})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	path, _ := InstancePath(root, "runner-race", "guest")
	transcript, err := runtime.OpenTranscript(TranscriptMetadata{Category: CategoryInstances, Instance: "runner-race", Component: "guest"}, path)
	if err != nil {
		t.Fatalf("OpenTranscript: %v", err)
	}
	const writers = 8
	const linesPerWriter = 40
	var wait sync.WaitGroup
	for writer := 0; writer < writers; writer++ {
		wait.Add(1)
		go func(writer int) {
			defer wait.Done()
			target := transcript.Stdout
			if writer%2 == 1 {
				target = transcript.Stderr
			}
			for line := 0; line < linesPerWriter; line++ {
				_, _ = fmt.Fprintf(target, "writer-%02d-line-%02d\n", writer, line)
			}
		}(writer)
	}
	wait.Wait()
	if err := transcript.Close(); err != nil {
		t.Fatalf("Close transcript: %v", err)
	}
	_ = runtime.Close()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != writers*linesPerWriter {
		t.Fatalf("line count = %d, want %d", len(lines), writers*linesPerWriter)
	}
}

func TestProcessRegistrySharesOneWriterAndTracksSessions(t *testing.T) {
	root := t.TempDir()
	firstRuntime, err := NewRuntime(Options{Directory: root, TranscriptSinks: SinkFile})
	if err != nil {
		t.Fatal(err)
	}
	secondRuntime, err := NewRuntime(Options{Directory: root, TranscriptSinks: SinkFile})
	if err != nil {
		t.Fatal(err)
	}
	path, _ := InstancePath(root, "runner-shared", "guest")
	first, err := firstRuntime.OpenTranscript(TranscriptMetadata{SessionID: "first", Category: CategoryInstances, Instance: "runner-shared", Component: "guest"}, path)
	if err != nil {
		t.Fatal(err)
	}
	second, err := secondRuntime.OpenTranscript(TranscriptMetadata{SessionID: "second", Category: CategoryInstances, Instance: "runner-shared", Component: "guest"}, path)
	if err != nil {
		t.Fatal(err)
	}
	state := readActiveStateForTest(t, root, path)
	if len(state.Sessions) != 2 {
		t.Fatalf("active sessions = %#v", state.Sessions)
	}
	_, _ = first.Stdout.Write([]byte("first\n"))
	_, _ = second.Stdout.Write([]byte("second\n"))
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	state = readActiveStateForTest(t, root, path)
	if len(state.Sessions) != 1 || state.Sessions[0].SessionID != "second" {
		t.Fatalf("remaining active sessions = %#v", state.Sessions)
	}
	_, _ = second.Stdout.Write([]byte("after-first-close\n"))
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	_ = firstRuntime.Close()
	_ = secondRuntime.Close()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); got != "first\nsecond\nafter-first-close\n" {
		t.Fatalf("shared transcript = %q", got)
	}
	metadataPath := filepath.Join(root, controlDirectoryName, "active", pathHash(mustCanonicalPath(t, path))+".json")
	if _, err := os.Stat(metadataPath); !os.IsNotExist(err) {
		t.Fatalf("active metadata remains after last close: %v", err)
	}
}

func TestRotationCompressesAndHonorsBackupLimit(t *testing.T) {
	root := t.TempDir()
	runtime, err := NewRuntime(Options{Directory: root, TranscriptSinks: SinkFile, Rotation: Rotation{MaxSizeMiB: 1, MaxBackups: 2, Compress: true}})
	if err != nil {
		t.Fatal(err)
	}
	path, _ := InstancePath(root, "runner-rotate", "wsl")
	transcript, err := runtime.OpenTranscript(TranscriptMetadata{Category: CategoryInstances, Instance: "runner-rotate", Component: "provider"}, path)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	defer transcript.Close()
	chunk := bytes.Repeat([]byte("x"), 700*1024)
	for index := 0; index < 4; index++ {
		var writeErr error
		for attempt := 0; attempt < 5; attempt++ {
			if _, writeErr = transcript.File.Write(chunk); writeErr == nil {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if writeErr != nil {
			t.Fatal(writeErr)
		}
		// Lumberjack backup names have millisecond precision. Keep forced test
		// rotations distinct so this test verifies compression and backup limits
		// rather than colliding timestamped filenames on faster hosts.
		if index < 3 {
			time.Sleep(10 * time.Millisecond)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	var backups []string
	for time.Now().Before(deadline) {
		backups = backups[:0]
		entries, _ := os.ReadDir(filepath.Dir(path))
		uncompressedBackup := false
		for _, entry := range entries {
			if strings.HasSuffix(entry.Name(), ".gz") {
				backups = append(backups, filepath.Join(filepath.Dir(path), entry.Name()))
			} else if entry.Name() != filepath.Base(path) {
				uncompressedBackup = true
			}
		}
		if len(backups) == 2 && !uncompressedBackup {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(backups) != 2 {
		t.Fatalf("compressed backups = %v, want exactly two", backups)
	}
	file, err := os.Open(backups[0])
	if err != nil {
		t.Fatal(err)
	}
	reader, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	decompressed, err := io.ReadAll(reader)
	_ = reader.Close()
	_ = file.Close()
	if err != nil || len(decompressed) != len(chunk) {
		t.Fatalf("decompressed backup bytes = %d, err=%v", len(decompressed), err)
	}
}

func TestRuntimePathLockExcludesAnotherProcess(t *testing.T) {
	if os.Getenv("EPAR_LOGGING_LOCK_HELPER") == "1" {
		runtime, err := NewRuntime(Options{Directory: os.Getenv("EPAR_LOGGING_LOCK_ROOT"), ManagerSinks: SinkFile})
		if err != nil {
			os.Exit(23)
		}
		_ = runtime.Close()
		os.Exit(0)
	}
	root := t.TempDir()
	runtime, err := NewRuntime(Options{Directory: root, ManagerSinks: SinkFile})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	command := exec.Command(os.Args[0], "-test.run=^TestRuntimePathLockExcludesAnotherProcess$")
	command.Env = append(os.Environ(), "EPAR_LOGGING_LOCK_HELPER=1", "EPAR_LOGGING_LOCK_ROOT="+root)
	err = command.Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 23 {
		t.Fatalf("child result = %v, want active-path exclusion", err)
	}
}

func decodeEnvelopeLines(t *testing.T, data []byte) []transcriptEnvelope {
	t.Helper()
	var envelopes []transcriptEnvelope
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		var envelope transcriptEnvelope
		if err := json.Unmarshal(scanner.Bytes(), &envelope); err != nil {
			t.Fatalf("decode envelope: %v", err)
		}
		envelopes = append(envelopes, envelope)
	}
	return envelopes
}

func readActiveStateForTest(t *testing.T, root, path string) activeState {
	t.Helper()
	metadataPath := filepath.Join(root, controlDirectoryName, "active", pathHash(mustCanonicalPath(t, path))+".json")
	data, err := os.ReadFile(metadataPath)
	if err != nil {
		t.Fatal(err)
	}
	var state activeState
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	return state
}

func mustCanonicalPath(t *testing.T, path string) string {
	t.Helper()
	canonical, err := canonicalPath(path)
	if err != nil {
		t.Fatal(err)
	}
	return canonical
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) {
	return 0, errors.New("injected sink failure")
}
