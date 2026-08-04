package pool

import (
	"fmt"
	"sync"
	"time"
)

var dockerSandboxesCreateHeartbeatInterval = 5 * time.Second

func (m *Manager) runDockerSandboxesCreateProgress(instance string, operation func() error) (err error) {
	if m.Config.Provider.Type != "docker-sandboxes" {
		return operation()
	}

	const label = "Docker Sandboxes instance preparation"
	attributes := []any{
		"provider", m.Config.Provider.Type,
		"instance", instance,
		"operation", "create",
		"stage", "sandbox-create-and-initial-verification",
	}
	startedAt := time.Now()
	m.logger().Info(label+" started; first use may materialize cached template layers and prepare the private Docker filesystem and VM", attributes...)

	interactive := dockerPullProgressTerminal() && stringSliceContains(m.Config.Logging.ManagerSinks, "console") && m.Config.Logging.ManagerConsoleFormat == "text"
	done := make(chan struct{})
	var heartbeat sync.WaitGroup
	interactiveRendered := false
	if dockerSandboxesCreateHeartbeatInterval > 0 {
		heartbeat.Add(1)
		go func() {
			defer heartbeat.Done()
			ticker := time.NewTicker(dockerSandboxesCreateHeartbeatInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					elapsed := time.Since(startedAt).Round(time.Second)
					line := fmt.Sprintf("%s: still working; elapsed %s", label, elapsed)
					if interactive {
						writeInteractiveProgressLine(dockerPullProgressConsole, line)
						interactiveRendered = true
						continue
					}
					m.logger().Info(line, attributes...)
				case <-done:
					return
				}
			}
		}()
	}

	err = operation()
	close(done)
	heartbeat.Wait()
	elapsed := time.Since(startedAt).Round(time.Second)
	if interactive && interactiveRendered {
		_, _ = fmt.Fprint(dockerPullProgressConsole, "\r\033[2K")
	}
	if err != nil {
		m.logger().Warn(label+" failed", append(attributes, "elapsed", elapsed)...)
		return err
	}
	m.logger().Info(label+" complete", append(attributes, "elapsed", elapsed)...)
	return nil
}

func (m *Manager) runDockerSandboxesPostCreateStage(instance, stage, label string, operation func() error) (err error) {
	startedAt := time.Now()
	attributes := []any{
		"provider", m.Config.Provider.Type,
		"instance", instance,
		"operation", "create",
		"stage", stage,
	}
	m.logger().Info(label+" started", attributes...)
	err = operation()
	elapsed := time.Since(startedAt).Round(time.Millisecond)
	if err != nil {
		m.logger().Warn(label+" failed", append(attributes, "elapsed", elapsed)...)
		return err
	}
	m.logger().Info(label+" complete", append(attributes, "elapsed", elapsed)...)
	return nil
}
