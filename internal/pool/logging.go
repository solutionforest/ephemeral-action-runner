package pool

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/logging"
)

func (m *Manager) logger() *slog.Logger {
	if m != nil && m.Logging != nil {
		return m.Logging.Logger()
	}
	return slog.New(slog.NewTextHandler(logging.ConsoleWriter(os.Stdout), &slog.HandlerOptions{ReplaceAttr: func(_ []string, attribute slog.Attr) slog.Attr {
		if attribute.Key == slog.TimeKey || attribute.Key == slog.LevelKey {
			return slog.Attr{}
		}
		return attribute
	}}))
}

func (m *Manager) infof(format string, args ...any) {
	m.logger().Info(fmt.Sprintf(strings.TrimSuffix(format, "\n"), args...))
}

func (m *Manager) warnf(format string, args ...any) {
	m.logger().Warn(fmt.Sprintf(strings.TrimSuffix(format, "\n"), args...))
}

func (m *Manager) Close() error {
	if m == nil || m.Logging == nil {
		return nil
	}
	return m.Logging.Close()
}

func (m *Manager) transcript(path, instance, component string) (*logging.Transcript, error) {
	if m.Logging == nil {
		return &logging.Transcript{Stdout: io.Discard, Stderr: io.Discard}, nil
	}
	canonical, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	m.transcriptMu.Lock()
	defer m.transcriptMu.Unlock()
	if transcript := m.transcripts[canonical]; transcript != nil {
		return transcript, nil
	}
	category := logging.CategoryInstances
	clean := filepath.Clean(canonical)
	switch filepath.Base(filepath.Dir(clean)) {
	case "builds":
		category = logging.CategoryBuilds
	case "benchmarks":
		category = logging.CategoryBenchmarks
	case "errors":
		category = logging.CategoryErrors
	case "instances":
		category = logging.CategoryInstances
	default:
		return nil, fmt.Errorf("cannot classify transcript path %s", path)
	}
	transcript, err := m.Logging.OpenTranscript(logging.TranscriptMetadata{
		Category:  category,
		Instance:  instance,
		Component: component,
		Provider:  m.Config.Provider.Type,
	}, canonical)
	if err != nil {
		return nil, err
	}
	if m.transcripts == nil {
		m.transcripts = make(map[string]*logging.Transcript)
	}
	m.transcripts[canonical] = transcript
	return transcript, nil
}

func (m *Manager) releaseTranscript(path string) error {
	if m == nil || m.Logging == nil || path == "" {
		return nil
	}
	canonical, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	m.transcriptMu.Lock()
	transcript := m.transcripts[canonical]
	delete(m.transcripts, canonical)
	m.transcriptMu.Unlock()
	if transcript == nil {
		return nil
	}
	return transcript.Close()
}

func (m *Manager) releaseInstanceTranscripts(vm ProvisionedInstance) error {
	return errors.Join(m.releaseTranscript(vm.LogPath), m.releaseTranscript(vm.GuestLogPath))
}

func transcriptComponent(path string) string {
	name := strings.ToLower(filepath.Base(path))
	switch {
	case strings.Contains(name, "guest"):
		return "guest"
	case strings.Contains(name, "build"):
		return "build"
	case strings.Contains(name, "source"):
		return "source"
	default:
		return "provider"
	}
}

func (m *Manager) runHostLogged(ctx context.Context, logPath, name string, args ...string) error {
	transcript, err := m.transcript(logPath, "", transcriptComponent(logPath))
	if err != nil {
		return err
	}
	return runHostLoggedCommand(ctx, logPath, transcript.Stdout, transcript.Stderr, name, args...)
}

func (m *Manager) retentionPolicy() logging.RetentionPolicy {
	days := func(value int) time.Duration { return time.Duration(value) * 24 * time.Hour }
	return logging.RetentionPolicy{
		MaxTotalBytes:   int64(m.Config.Logging.RetentionMaxTotalMiB) * 1024 * 1024,
		ManagerMaxAge:   days(m.Config.Logging.ManagerMaxAgeDays),
		InstanceMaxAge:  days(m.Config.Logging.InstanceMaxAgeDays),
		BuildMaxAge:     days(m.Config.Logging.BuildMaxAgeDays),
		ErrorMaxAge:     days(m.Config.Logging.ErrorMaxAgeDays),
		BenchmarkMaxAge: days(m.Config.Logging.BenchmarkMaxAgeDays),
	}
}

func (m *Manager) PruneLogs(dryRun bool) (logging.RetentionReport, error) {
	if m.Logging == nil {
		return logging.RetentionReport{}, nil
	}
	return m.Logging.PruneRetention(m.retentionPolicy(), dryRun)
}

func (m *Manager) pruneLogsBestEffort() {
	report, err := m.PruneLogs(false)
	if err != nil {
		m.logger().Warn("log retention failed", "operation", "logs-prune", "error", err)
		return
	}
	for _, warning := range report.Warnings {
		m.logger().Warn("log retention skipped candidate", "operation", "logs-prune", "warning", warning)
	}
	if report.Deleted > 0 {
		m.logger().Info("log retention completed", "operation", "logs-prune", "deleted", report.Deleted, "reclaimedBytes", report.ReclaimedBytes)
	}
}
