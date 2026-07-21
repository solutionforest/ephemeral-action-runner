package pool

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
)

type startupTimingEvent struct {
	Timestamp string `json:"timestamp"`
	Provider  string `json:"provider"`
	Stage     string `json:"stage"`
	Outcome   string `json:"outcome"`
	ElapsedMS int64  `json:"elapsedMs"`
	Error     string `json:"error,omitempty"`
}

type startupTimingStage struct {
	name    string
	elapsed time.Duration
}

type startupTiming struct {
	mu            sync.Mutex
	file          *os.File
	encoder       *json.Encoder
	path          string
	provider      string
	startedAt     time.Time
	stages        []startupTimingStage
	firstInstance string
	closed        bool
	logger        *slog.Logger
}

// StartStartupTiming records the initial Docker-DinD or WSL start path.
func (m *Manager) StartStartupTiming() (string, error) {
	provider := m.Config.Provider.Type
	if !supportsStartupTiming(provider) {
		return "", nil
	}
	dir := filepath.Join(m.logRoot(), "benchmarks")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", err
	}
	startedAt := time.Now().UTC()
	path := filepath.Join(dir, startedAt.Format("20060102T150405.000000000Z")+"-"+provider+".jsonl")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return "", err
	}
	timing := &startupTiming{
		file:      file,
		encoder:   json.NewEncoder(file),
		path:      path,
		provider:  provider,
		startedAt: startedAt,
		logger:    m.logger(),
	}
	m.startupTiming = timing
	timing.eventLocked("total_startup", "started", 0, nil)
	return path, nil
}

// FinishStartupTiming records a terminal error before the first runner is ready.
func (m *Manager) FinishStartupTiming(err error) {
	timing := m.startupTiming
	if timing == nil {
		return
	}
	timing.finish(err)
}

func (m *Manager) timeStartupStage(stage string, fn func() error) error {
	timing := m.startupTiming
	if timing == nil {
		return fn()
	}
	return timing.measure(stage, fn)
}

func (m *Manager) timeFirstInstanceStage(name, stage string, fn func() error) error {
	timing := m.startupTiming
	if timing == nil || !timing.isFirstInstance(name) {
		return fn()
	}
	return timing.measure(stage, fn)
}

func (m *Manager) finishFirstRunnerReady(name string) {
	timing := m.startupTiming
	if timing == nil || !timing.isFirstInstance(name) {
		return
	}
	timing.finish(nil)
}

func (t *startupTiming) isFirstInstance(name string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return false
	}
	if t.firstInstance == "" {
		t.firstInstance = name
	}
	return t.firstInstance == name
}

func (t *startupTiming) measure(stage string, fn func() error) error {
	startedAt := time.Now()
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return fn()
	}
	t.eventLocked(stage, "started", 0, nil)
	t.mu.Unlock()

	err := fn()
	elapsed := time.Since(startedAt)
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return err
	}
	if err != nil {
		t.eventLocked(stage, "failed", elapsed, err)
		return err
	}
	t.stages = append(t.stages, startupTimingStage{name: stage, elapsed: elapsed})
	t.eventLocked(stage, "completed", elapsed, nil)
	return nil
}

func (t *startupTiming) finish(err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return
	}
	elapsed := time.Since(t.startedAt)
	if err != nil {
		t.eventLocked("total_startup", "failed", elapsed, err)
	} else {
		t.stages = append(t.stages, startupTimingStage{name: "total_startup", elapsed: elapsed})
		t.eventLocked("total_startup", "completed", elapsed, nil)
		for _, stage := range t.stages {
			t.logger.Info("startup timing", "provider", t.provider, "operation", "startup", "stage", stage.name, "duration", stage.elapsed.Round(time.Millisecond), "logPath", t.path)
		}
	}
	t.closed = true
	_ = t.file.Close()
}

func (t *startupTiming) eventLocked(stage, outcome string, elapsed time.Duration, err error) {
	event := startupTimingEvent{
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Provider:  t.provider,
		Stage:     stage,
		Outcome:   outcome,
		ElapsedMS: elapsed.Milliseconds(),
	}
	if err != nil {
		event.Error = sanitizeTimingError(err)
	}
	_ = t.encoder.Encode(event)
}

func supportsStartupTiming(provider string) bool {
	return provider == "docker-dind" || provider == "wsl"
}

func startupTimingLabel(provider string) string {
	switch provider {
	case "docker-dind":
		return "DinD"
	case "wsl":
		return "WSL"
	default:
		return provider
	}
}

func sanitizeTimingError(err error) string {
	text := provider.RedactText(strings.Join(strings.Fields(err.Error()), " "))
	if len(text) > 500 {
		return text[:500] + "..."
	}
	return text
}
