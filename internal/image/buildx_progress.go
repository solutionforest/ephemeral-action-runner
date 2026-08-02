package image

import (
	"bytes"
	"fmt"
	"io"
	"math"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const buildxProgressReportInterval = 2 * time.Second

var (
	buildxLayerProgressPattern = regexp.MustCompile(`^#([0-9]+)\s+(sha256:[0-9a-f]{64})\s+([0-9]+(?:\.[0-9]+)?)([kMGTPE]?i?B)\s+/\s+([0-9]+(?:\.[0-9]+)?)([kMGTPE]?i?B)(?:\s+[0-9]+(?:\.[0-9]+)?s)?(?:\s+(done))?\s*$`)
	buildxStepPattern          = regexp.MustCompile(`^#([0-9]+)(?:\s|$)`)
)

type buildxLayerProgress struct {
	current  int64
	total    int64
	complete bool
}

// BuildxProgressSnapshot is a bounded summary of BuildKit plain progress.
type BuildxProgressSnapshot struct {
	CurrentBytes      int64
	TotalBytes        int64
	CompletedLayers   int
	KnownLayers       int
	ActiveStep        int
	Elapsed           time.Duration
	ObservedProgress  bool
	ObservedByteTotal bool
}

// BuildxProgressMonitor consumes one or more BuildKit plain-progress streams.
// Call NewStream separately for stdout and stderr so partial writes cannot
// corrupt each other's line framing.
type BuildxProgressMonitor struct {
	mu         sync.Mutex
	started    time.Time
	now        func() time.Time
	interval   time.Duration
	lastReport time.Time
	layers     map[string]buildxLayerProgress
	activeStep int
	observed   bool
	report     func(BuildxProgressSnapshot)
}

// NewBuildxProgressMonitor creates a monitor suitable for a live Buildx build.
func NewBuildxProgressMonitor(report func(BuildxProgressSnapshot)) *BuildxProgressMonitor {
	return newBuildxProgressMonitor(buildxProgressReportInterval, time.Now, report)
}

func newBuildxProgressMonitor(interval time.Duration, now func() time.Time, report func(BuildxProgressSnapshot)) *BuildxProgressMonitor {
	started := now()
	return &BuildxProgressMonitor{
		started:  started,
		now:      now,
		interval: interval,
		layers:   make(map[string]buildxLayerProgress),
		report:   report,
	}
}

// NewStream returns an independent line-framing writer for one process stream.
func (monitor *BuildxProgressMonitor) NewStream() *BuildxProgressStream {
	return &BuildxProgressStream{monitor: monitor}
}

// Snapshot returns the current summary.
func (monitor *BuildxProgressMonitor) Snapshot() BuildxProgressSnapshot {
	monitor.mu.Lock()
	defer monitor.mu.Unlock()
	return monitor.snapshotLocked(monitor.now())
}

func (monitor *BuildxProgressMonitor) consumeLine(line string) {
	line = strings.TrimSpace(line)
	if line == "" {
		return
	}
	now := monitor.now()
	monitor.mu.Lock()
	changed := monitor.consumeLineLocked(line)
	if !changed || (monitor.observed && !monitor.lastReport.IsZero() && now.Sub(monitor.lastReport) < monitor.interval) {
		monitor.mu.Unlock()
		return
	}
	monitor.observed = true
	monitor.lastReport = now
	snapshot := monitor.snapshotLocked(now)
	report := monitor.report
	monitor.mu.Unlock()
	if report != nil {
		report(snapshot)
	}
}

func (monitor *BuildxProgressMonitor) consumeLineLocked(line string) bool {
	layerMatch := buildxLayerProgressPattern.FindStringSubmatch(line)
	if layerMatch == nil && strings.Contains(line, "sha256:") && strings.Contains(line, " / ") {
		return false
	}
	var current, total int64
	if layerMatch != nil {
		var currentOK, totalOK bool
		current, currentOK = parseBuildxBytes(layerMatch[3], layerMatch[4])
		total, totalOK = parseBuildxBytes(layerMatch[5], layerMatch[6])
		if !currentOK || !totalOK || total <= 0 {
			return false
		}
	}
	stepMatch := buildxStepPattern.FindStringSubmatch(line)
	if stepMatch == nil {
		return false
	}
	step, err := strconv.Atoi(stepMatch[1])
	if err != nil {
		return false
	}
	changed := step != monitor.activeStep
	monitor.activeStep = step

	if layerMatch == nil {
		return changed
	}
	if current > total {
		current = total
	}
	layer := buildxLayerProgress{
		current:  current,
		total:    total,
		complete: layerMatch[7] == "done" || current == total,
	}
	previous, exists := monitor.layers[layerMatch[2]]
	if !exists || previous != layer {
		monitor.layers[layerMatch[2]] = layer
		changed = true
	}
	return changed
}

func (monitor *BuildxProgressMonitor) snapshotLocked(now time.Time) BuildxProgressSnapshot {
	snapshot := BuildxProgressSnapshot{
		ActiveStep:       monitor.activeStep,
		Elapsed:          max(now.Sub(monitor.started), 0),
		ObservedProgress: monitor.observed || monitor.activeStep > 0 || len(monitor.layers) > 0,
	}
	for _, layer := range monitor.layers {
		snapshot.KnownLayers++
		snapshot.CurrentBytes += layer.current
		snapshot.TotalBytes += layer.total
		if layer.complete {
			snapshot.CompletedLayers++
		}
	}
	snapshot.ObservedByteTotal = snapshot.TotalBytes > 0
	return snapshot
}

func parseBuildxBytes(number, unit string) (int64, bool) {
	value, err := strconv.ParseFloat(number, 64)
	if err != nil || value < 0 {
		return 0, false
	}
	multipliers := map[string]float64{
		"B":  1,
		"kB": 1e3, "MB": 1e6, "GB": 1e9, "TB": 1e12, "PB": 1e15, "EB": 1e18,
		"KiB": 1 << 10, "MiB": 1 << 20, "GiB": 1 << 30, "TiB": 1 << 40, "PiB": 1 << 50, "EiB": 1 << 60,
	}
	multiplier, ok := multipliers[unit]
	if !ok || value > float64(math.MaxInt64)/multiplier {
		return 0, false
	}
	return int64(value * multiplier), true
}

// FormatBuildxProgress renders a concise console-safe summary.
func FormatBuildxProgress(prefix string, snapshot BuildxProgressSnapshot) string {
	parts := make([]string, 0, 4)
	if snapshot.ObservedByteTotal {
		percent := float64(snapshot.CurrentBytes) * 100 / float64(snapshot.TotalBytes)
		parts = append(parts, fmt.Sprintf("%s/%s (%.0f%%)", FormatDockerPullBytes(snapshot.CurrentBytes), FormatDockerPullBytes(snapshot.TotalBytes), percent))
		parts = append(parts, fmt.Sprintf("%d/%d layer downloads complete", snapshot.CompletedLayers, snapshot.KnownLayers))
	}
	if snapshot.ActiveStep > 0 {
		parts = append(parts, fmt.Sprintf("BuildKit step #%d", snapshot.ActiveStep))
	}
	parts = append(parts, "elapsed "+formatBuildxElapsed(snapshot.Elapsed))
	return prefix + ": " + strings.Join(parts, "; ")
}

func formatBuildxElapsed(duration time.Duration) string {
	duration = duration.Round(time.Second)
	if duration < time.Second {
		return "0s"
	}
	return duration.String()
}

// BuildxProgressStream frames arbitrary process writes into complete lines.
type BuildxProgressStream struct {
	mu      sync.Mutex
	monitor *BuildxProgressMonitor
	pending []byte
}

func (stream *BuildxProgressStream) Write(content []byte) (int, error) {
	stream.mu.Lock()
	defer stream.mu.Unlock()
	stream.pending = append(stream.pending, content...)
	for {
		index := bytes.IndexByte(stream.pending, '\n')
		if index < 0 {
			break
		}
		line := string(bytes.TrimSuffix(stream.pending[:index], []byte{'\r'}))
		stream.pending = stream.pending[index+1:]
		stream.monitor.consumeLine(line)
	}
	return len(content), nil
}

// Flush consumes a final unterminated line.
func (stream *BuildxProgressStream) Flush() {
	stream.mu.Lock()
	defer stream.mu.Unlock()
	if len(stream.pending) == 0 {
		return
	}
	line := string(bytes.TrimSuffix(stream.pending, []byte{'\r'}))
	stream.pending = nil
	stream.monitor.consumeLine(line)
}

var _ io.Writer = (*BuildxProgressStream)(nil)
