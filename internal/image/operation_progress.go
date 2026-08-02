package image

import (
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/storage"
)

var imageOperationHeartbeatInterval = 5 * time.Second

// runProgressOperation keeps long, otherwise-silent image and storage work
// visible without mixing command output into the manager console.
func (m *Coordinator) runProgressOperation(label string, detail func() string, operation func() error) error {
	return runProgressOperation(label, imageOperationHeartbeatInterval, m.infof, detail, operation)
}

func runProgressOperation(label string, interval time.Duration, logf func(string, ...any), detail func() string, operation func() error) (err error) {
	started := time.Now()
	logf("%s started\n", label)

	done := make(chan struct{})
	var heartbeat sync.WaitGroup
	if interval > 0 {
		heartbeat.Add(1)
		go func() {
			defer heartbeat.Done()
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					suffix := ""
					if detail != nil {
						if current := detail(); current != "" {
							suffix = "; " + current
						}
					}
					logf("%s: still working; elapsed %s%s\n", label, time.Since(started).Round(time.Second), suffix)
				case <-done:
					return
				}
			}
		}()
	}

	finished := false
	defer func() {
		close(done)
		heartbeat.Wait()
		if finished && err == nil {
			logf("%s complete; elapsed %s\n", label, time.Since(started).Round(time.Second))
		}
	}()
	err = operation()
	finished = true
	return err
}

func regularFileSizeDetail(path, description string) func() string {
	return func() string {
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Size() < 0 {
			return ""
		}
		return fmt.Sprintf("%s %s", description, storage.FormatBytes(uint64(info.Size())))
	}
}
