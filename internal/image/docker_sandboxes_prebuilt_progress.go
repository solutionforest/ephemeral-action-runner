package image

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	dockerSandboxesPrebuiltInteractiveProgressInterval = 2 * time.Second
	dockerSandboxesPrebuiltLoggedProgressInterval      = 30 * time.Second
)

type dockerSandboxesPrebuiltArchiveProgressSnapshot struct {
	Label        string
	Phase        string
	ArchiveBytes int64
	Elapsed      time.Duration
	ShowRate     bool
}

type dockerSandboxesPrebuiltArchiveProgress struct {
	coordinator *Coordinator
	partialPath string
	archivePath string
	label       string
	reference   string
	startedAt   time.Time
	interactive bool
	interval    time.Duration

	mu       sync.RWMutex
	phase    string
	done     chan struct{}
	stopped  chan struct{}
	stopOnce sync.Once
	renderMu sync.Mutex
	rendered bool
}

func (m *Coordinator) startDockerSandboxesPrebuiltArchiveProgress(partialPath, archivePath, profile, platform string) *dockerSandboxesPrebuiltArchiveProgress {
	interactive := m.dockerPullProgressIsInteractive()
	interval := dockerSandboxesPrebuiltLoggedProgressInterval
	if interactive {
		interval = dockerSandboxesPrebuiltInteractiveProgressInterval
	}
	progress := newDockerSandboxesPrebuiltArchiveProgress(m, partialPath, archivePath, profile, platform, interval, interactive)
	progress.start()
	return progress
}

func newDockerSandboxesPrebuiltArchiveProgress(m *Coordinator, partialPath, archivePath, profile, platform string, interval time.Duration, interactive bool) *dockerSandboxesPrebuiltArchiveProgress {
	profileName := strings.ToUpper(strings.TrimSpace(profile))
	if profileName == "" {
		profileName = "package"
	} else {
		profileName = strings.ToUpper(profileName[:1]) + strings.ToLower(profileName[1:])
	}
	progress := &dockerSandboxesPrebuiltArchiveProgress{
		coordinator: m,
		partialPath: partialPath,
		archivePath: archivePath,
		label:       "Docker Sandboxes prebuilt " + profileName + " archive",
		startedAt:   time.Now(),
		interactive: interactive,
		interval:    interval,
		phase:       "resolving immutable platform",
		done:        make(chan struct{}),
		stopped:     make(chan struct{}),
	}
	if archiveRange, durationRange, ok := dockerSandboxesPrebuiltAcquisitionReference(profile, platform); ok {
		progress.reference = fmt.Sprintf("typical first-acquisition reference for %s: %s archive output and %s", platform, archiveRange, durationRange)
	}
	return progress
}

func (progress *dockerSandboxesPrebuiltArchiveProgress) start() {
	if progress.reference != "" {
		progress.coordinator.infof("%s started; %s; actual duration varies with registry, network, CPU, and disk\n", progress.label, progress.reference)
	} else {
		progress.coordinator.infof("%s started\n", progress.label)
	}
	go func() {
		defer close(progress.stopped)
		if progress.interval <= 0 {
			<-progress.done
			return
		}
		ticker := time.NewTicker(progress.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				progress.render()
			case <-progress.done:
				return
			}
		}
	}()
}

func dockerSandboxesPrebuiltAcquisitionReference(profile, platform string) (archiveRange, durationRange string, ok bool) {
	profile = strings.ToLower(strings.TrimSpace(profile))
	platform = strings.ToLower(strings.TrimSpace(platform))
	switch profile {
	case "act":
		return "0.8-2 GiB", "usually 2-15 minutes", true
	case "full":
		switch platform {
		case "linux/amd64":
			return "16-24 GiB", "usually 15-60 minutes", true
		case "linux/arm64", "linux/arm64/v8":
			return "8-16 GiB", "usually 15-60 minutes", true
		default:
			return "8-24 GiB", "usually 15-60 minutes", true
		}
	default:
		return "", "", false
	}
}

func (progress *dockerSandboxesPrebuiltArchiveProgress) setPhase(phase string) {
	progress.mu.Lock()
	progress.phase = strings.TrimSpace(phase)
	progress.mu.Unlock()
	progress.render()
}

func (progress *dockerSandboxesPrebuiltArchiveProgress) finish(success bool) {
	progress.stopOnce.Do(func() {
		close(progress.done)
		<-progress.stopped
		progress.renderMu.Lock()
		if progress.interactive && progress.rendered {
			_, _ = fmt.Fprint(progress.coordinator.environment.ProgressConsole(), "\r\033[2K\n")
		}
		progress.renderMu.Unlock()
		if success {
			snapshot := progress.snapshot()
			progress.coordinator.infof("%s acquisition complete; %s written; elapsed %s\n", progress.label, FormatDockerPullBytes(snapshot.ArchiveBytes), formatBuildxElapsed(snapshot.Elapsed))
		}
	})
}

func (progress *dockerSandboxesPrebuiltArchiveProgress) render() {
	progress.renderMu.Lock()
	defer progress.renderMu.Unlock()
	line := formatDockerSandboxesPrebuiltArchiveProgress(progress.snapshot())
	if progress.interactive {
		_, _ = fmt.Fprintf(progress.coordinator.environment.ProgressConsole(), "\r\033[2K%s", line)
		progress.rendered = true
		return
	}
	progress.coordinator.infof("%s\n", line)
}

func (progress *dockerSandboxesPrebuiltArchiveProgress) snapshot() dockerSandboxesPrebuiltArchiveProgressSnapshot {
	progress.mu.RLock()
	phase := progress.phase
	progress.mu.RUnlock()
	var archiveBytes int64
	for _, path := range []string{progress.partialPath, progress.archivePath} {
		if info, err := os.Lstat(path); err == nil && info.Mode().IsRegular() {
			archiveBytes = info.Size()
			break
		}
	}
	return dockerSandboxesPrebuiltArchiveProgressSnapshot{
		Label:        progress.label,
		Phase:        phase,
		ArchiveBytes: archiveBytes,
		Elapsed:      time.Since(progress.startedAt),
		ShowRate:     strings.Contains(phase, "downloading") || strings.Contains(phase, "materializing"),
	}
}

func formatDockerSandboxesPrebuiltArchiveProgress(snapshot dockerSandboxesPrebuiltArchiveProgressSnapshot) string {
	parts := []string{snapshot.Phase}
	if snapshot.ArchiveBytes > 0 {
		parts = append(parts, FormatDockerPullBytes(snapshot.ArchiveBytes)+" written")
		if snapshot.ShowRate && snapshot.Elapsed > 0 {
			rate := int64(float64(snapshot.ArchiveBytes) / snapshot.Elapsed.Seconds())
			if rate > 0 {
				parts = append(parts, FormatDockerPullBytes(rate)+"/s archive-write average")
			}
		}
	}
	parts = append(parts, "elapsed "+formatBuildxElapsed(snapshot.Elapsed))
	return snapshot.Label + ": " + strings.Join(parts, "; ")
}
