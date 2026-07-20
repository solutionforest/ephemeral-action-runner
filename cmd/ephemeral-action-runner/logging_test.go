package main

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestLogsPathListAndPrune(t *testing.T) {
	root := t.TempDir()
	configPath := writeLoggingCommandConfig(t, root, "artifact-logs")
	logRoot := filepath.Join(root, "artifact-logs")
	instanceDir := filepath.Join(logRoot, "instances")
	if err := os.MkdirAll(instanceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	oldPath := filepath.Join(instanceDir, "runner-1.guest.log")
	if err := os.WriteFile(oldPath, []byte("old transcript\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(oldPath, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	pathOutput, err := captureStdout(t, func() error {
		return run([]string{"logs", "path", "--project-root", root, "--config", configPath})
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(pathOutput) != logRoot {
		t.Fatalf("logs path output = %q, want %q", pathOutput, logRoot)
	}

	listOutput, err := captureStdout(t, func() error {
		return run([]string{"logs", "list", "--project-root", root, "--config", configPath})
	})
	if err != nil {
		t.Fatal(err)
	}
	canonicalOldPath, err := filepath.EvalSymlinks(oldPath)
	if runtime.GOOS == "windows" && errors.Is(err, fs.ErrPermission) {
		canonicalOldPath, err = filepath.Abs(oldPath)
	}
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.ToLower(listOutput), strings.ToLower(canonicalOldPath)) {
		t.Fatalf("logs list output missing %s:\n%s", canonicalOldPath, listOutput)
	}

	if _, err := captureStdout(t, func() error {
		return run([]string{"logs", "prune", "--dry-run", "--project-root", root, "--config", configPath})
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldPath); err != nil {
		t.Fatalf("dry-run removed transcript: %v", err)
	}
	if _, err := captureStdout(t, func() error {
		return run([]string{"logs", "prune", "--project-root", root, "--config", configPath})
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("prune left old transcript, stat error = %v", err)
	}
}

func TestCLIConfigLoadWarnsForLegacyPoolLogDir(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "legacy.yml")
	if err := os.WriteFile(configPath, []byte("pool:\n  logDir: legacy/logs\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stderr, err := captureStderr(t, func() error {
		_, cfg, resolved, err := loadLoggingConfig(root, configPath)
		if err != nil {
			return err
		}
		if cfg.Logging.Directory != "legacy/logs" || resolved != filepath.Join(root, "legacy", "logs") {
			t.Fatalf("legacy directory migration = %q, resolved = %q", cfg.Logging.Directory, resolved)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, "warning:") || !strings.Contains(stderr, "pool.logDir is deprecated") {
		t.Fatalf("stderr = %q, want migration warning", stderr)
	}
}

func TestWriteLastErrorReportUsesConfiguredDirectoryAndFallback(t *testing.T) {
	root := t.TempDir()
	configPath := writeLoggingCommandConfig(t, root, "custom-logs")
	path := writeLastErrorReport([]string{"start", "--project-root", root, "--config", configPath}, errors.New("configured failure"))
	if path != filepath.Join(root, "custom-logs", "epar-last-error.log") {
		t.Fatalf("configured report path = %q", path)
	}
	archives, err := filepath.Glob(filepath.Join(root, "custom-logs", "errors", "epar-*-error.log"))
	if err != nil || len(archives) != 1 {
		t.Fatalf("configured error archives = %v, err = %v", archives, err)
	}

	fallbackRoot := t.TempDir()
	fallback := writeLastErrorReport([]string{"start", "--project-root", fallbackRoot, "--config", "missing.yml"}, errors.New("fallback failure"))
	if fallback != filepath.Join(fallbackRoot, "work", "logs", "epar-last-error.log") {
		t.Fatalf("fallback report path = %q", fallback)
	}
}

func TestWriteLastErrorReportRedactsSecretsAndRestrictsExistingFile(t *testing.T) {
	root := t.TempDir()
	configPath := writeLoggingCommandConfig(t, root, "custom-logs")
	logRoot := filepath.Join(root, "custom-logs")
	if err := os.MkdirAll(logRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	lastPath := filepath.Join(logRoot, "epar-last-error.log")
	if err := os.WriteFile(lastPath, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	const sentinel = "registration-token-sentinel"
	path := writeLastErrorReport(
		[]string{"start", "--project-root", root, "--config", configPath},
		errors.New("docker exec -e RUNNER_TOKEN="+sentinel+" failed"),
	)
	if path != lastPath {
		t.Fatalf("report path = %q, want %q", path, lastPath)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(content), sentinel) {
		t.Fatalf("last error report exposed sentinel token: %s", content)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("last error report mode = %#o, want 0600", got)
		}
	}
	archives, err := filepath.Glob(filepath.Join(logRoot, "errors", "epar-*-error.log"))
	if err != nil || len(archives) != 1 {
		t.Fatalf("error archives = %v, err = %v", archives, err)
	}
	archiveContent, err := os.ReadFile(archives[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(archiveContent), sentinel) {
		t.Fatalf("archived error report exposed sentinel token: %s", archiveContent)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(archives[0])
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("archived error report mode = %#o, want 0600", got)
		}
	}
}

func writeLoggingCommandConfig(t *testing.T, root, directory string) string {
	t.Helper()
	path := filepath.Join(root, "config.yml")
	content := "logging:\n  directory: " + directory + "\n  instanceMaxAgeDays: 1\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func captureStderr(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	oldStderr := os.Stderr
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = writer
	defer func() { os.Stderr = oldStderr }()
	fnErr := fn()
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	return string(data), fnErr
}
