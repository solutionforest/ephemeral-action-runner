package image

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/moby/moby/client"
	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
	"github.com/solutionforest/ephemeral-action-runner/internal/terminalprogress"
)

const dockerPullProgressInterval = 250 * time.Millisecond

type DockerSourcePullOptions struct {
	Image              string
	Platform           string
	LogPath            string
	AnnounceRemoteSize bool
}

func (m *Coordinator) PullDockerSource(ctx context.Context, opts DockerSourcePullOptions) error {
	attemptCtx, cancelAttempt := boundedImageAttempt(ctx, dockerPullAttemptTimeout)
	defer cancelAttempt()
	result, err := PullDockerImage(attemptCtx, DockerPullOptions{
		Image:            opts.Image,
		Platform:         opts.Platform,
		FallbackPlatform: m.Config.Provider.Platform,
		QueryRemoteSize:  opts.AnnounceRemoteSize,
	})
	var enginePullErr *DockerEnginePullError
	if err != nil && !errors.As(err, &enginePullErr) {
		return m.pullDockerSourceWithCLI(attemptCtx, opts, err)
	}
	if opts.AnnounceRemoteSize {
		if result.RemoteCompressedError != nil {
			m.WriteDockerPullNotice(opts.LogPath, "warning: could not determine remote compressed layer size: "+sanitizeImageError(result.RemoteCompressedError))
		} else {
			m.WriteDockerPullNotice(opts.LogPath, fmt.Sprintf("Remote compressed layers: %s; actual transfer may be lower when Docker reuses layers.", FormatDockerPullBytes(result.RemoteCompressedSize)))
		}
	}
	if result.RegistryAuthError != nil {
		m.WriteDockerPullNotice(opts.LogPath, "warning: could not load Docker registry credentials; continuing without explicit credentials: "+sanitizeImageError(result.RegistryAuthError))
	}
	if err != nil {
		return classifyImageDependencyFailure("OCI registry", "pull Docker source image", err)
	}
	if err := m.renderDockerPullProgress(attemptCtx, result.Response, opts.LogPath); err != nil {
		cause := fmt.Errorf("Docker Engine pull %s: %w", opts.Image, err)
		return classifyImageDependencyFailure("OCI registry", "stream Docker source image", cause)
	}
	m.WriteDockerPullNotice(opts.LogPath, "Docker source pull complete: "+opts.Image)
	return nil
}

func (m *Coordinator) pullDockerSourceWithCLI(ctx context.Context, opts DockerSourcePullOptions, apiErr error) error {
	m.WriteDockerPullNotice(opts.LogPath, "warning: "+sanitizeImageError(apiErr)+"; falling back to docker pull CLI")
	args := []string{"pull"}
	if opts.Platform != "" {
		args = append(args, "--platform", opts.Platform)
	}
	args = append(args, opts.Image)
	err := m.runHostLogged(ctx, opts.LogPath, "docker", args...)
	return classifyImageCommandFailure("OCI registry", "pull Docker source image", err, boundedRedactedLogTail(opts.LogPath, 16*1024), false)
}

func (m *Coordinator) renderDockerPullProgress(ctx context.Context, response client.ImagePullResponse, logPath string) error {
	transcript, err := m.transcript(logPath, "", "docker-pull")
	if err != nil {
		return err
	}
	layers := map[string]DockerPullProgress{}
	lastRender := time.Time{}
	rendered := false
	progressRenderer := m.newTerminalProgressRenderer()
	err = ConsumeDockerPullProgress(ctx, response, func(message DockerPullEvent) error {
		writeDockerPullEvent(transcript.Stdout, message)
		if message.Error != nil {
			return nil
		}
		if message.ID != "" {
			layer := layers[message.ID]
			if message.Progress != nil {
				layer.Current = message.Progress.Current
				if message.Progress.Total > 0 {
					layer.Total = message.Progress.Total
				}
			}
			if IsDockerPullLayerComplete(message.Status) {
				layer.Completed = true
				if layer.Total > 0 {
					layer.Current = layer.Total
				}
			}
			layers[message.ID] = layer
		}
		if time.Since(lastRender) >= dockerPullProgressInterval {
			m.writeDockerPullProgressWithRenderer(logPath, layers, progressRenderer)
			lastRender = time.Now()
			rendered = true
		}
		return nil
	})
	if err != nil {
		return err
	}
	if rendered {
		m.writeDockerPullProgressWithRenderer(logPath, layers, progressRenderer)
		progressRenderer.Finish()
	}
	return nil
}

func (m *Coordinator) WriteDockerPullNotice(logPath, message string) {
	attributes := []any{"provider", m.Config.Provider.Type, "operation", "docker-pull", "logPath", logPath}
	if strings.HasPrefix(message, "warning:") {
		m.environment.LogWarn(strings.TrimSpace(strings.TrimPrefix(message, "warning:")), attributes...)
	} else {
		m.environment.LogInfo(message, attributes...)
	}
	transcript, err := m.transcript(logPath, "", "docker-pull")
	if err != nil {
		m.environment.LogWarn("docker pull transcript unavailable", "operation", "docker-pull", "logPath", logPath, "error", err)
		return
	}
	_, _ = fmt.Fprintf(transcript.Stdout, "%s\n", message)
}

func writeDockerPullEvent(logFile io.Writer, event DockerPullEvent) {
	if logFile == nil {
		return
	}
	parts := make([]string, 0, 4)
	if event.ID != "" {
		parts = append(parts, event.ID)
	}
	if event.Status != "" {
		parts = append(parts, event.Status)
	}
	if event.Progress != nil {
		parts = append(parts, fmt.Sprintf("progress=%d/%d", event.Progress.Current, event.Progress.Total))
	}
	if event.Stream != "" {
		parts = append(parts, strings.TrimSpace(event.Stream))
	}
	if event.Error != nil {
		parts = append(parts, "error="+sanitizeImageError(event.Error))
	}
	fmt.Fprintf(logFile, "%s %s\n", time.Now().UTC().Format(time.RFC3339Nano), strings.Join(parts, " "))
}

func (m *Coordinator) writeDockerPullProgress(logPath string, layers map[string]DockerPullProgress) {
	m.writeDockerPullProgressWithRenderer(logPath, layers, m.newTerminalProgressRenderer())
}

func (m *Coordinator) writeDockerPullProgressWithRenderer(logPath string, layers map[string]DockerPullProgress, progressRenderer *terminalprogress.Renderer) {
	line := DockerPullProgressSummary(layers)
	if progressRenderer != nil {
		progressRenderer.Write(line)
		return
	}
	m.environment.LogInfo(line, "provider", m.Config.Provider.Type, "operation", "docker-pull", "logPath", logPath)
}

func (m *Coordinator) dockerPullProgressIsInteractive() bool {
	return m.environment.ProgressTerminal() && containsString(m.Config.Logging.ManagerSinks, "console") && m.Config.Logging.ManagerConsoleFormat == "text"
}

func (m *Coordinator) newTerminalProgressRenderer() *terminalprogress.Renderer {
	if !m.dockerPullProgressIsInteractive() {
		return nil
	}
	renderer := terminalprogress.New(m.environment.ProgressConsole(), m.environment.ProgressWidth)
	if !renderer.Available() {
		return nil
	}
	return renderer
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func sanitizeImageError(err error) string {
	text := provider.RedactText(strings.Join(strings.Fields(err.Error()), " "))
	if len(text) > 500 {
		return text[:500] + "..."
	}
	return text
}
