package pool

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	gcrv1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/moby/moby/api/types/jsonstream"
	"github.com/moby/moby/api/types/registry"
	"github.com/moby/moby/client"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"golang.org/x/term"
)

const dockerPullProgressInterval = 250 * time.Millisecond

type dockerSourcePullOptions struct {
	Image              string
	Platform           string
	LogPath            string
	AnnounceRemoteSize bool
}

type dockerLayerProgress struct {
	current   int64
	total     int64
	completed bool
}

// pullDockerSourceCommand is kept as a small seam for existing command-based
// image preparation tests. Production always uses Manager.pullDockerSource.
var pullDockerSourceCommand = (*Manager).pullDockerSource

func (m *Manager) pullDockerSource(ctx context.Context, opts dockerSourcePullOptions) error {
	cli, err := client.New(client.FromEnv)
	if err != nil {
		return m.pullDockerSourceWithCLI(ctx, opts, fmt.Errorf("initialize Docker Engine client: %w", err))
	}
	if _, err := cli.Ping(ctx, client.PingOptions{}); err != nil {
		return m.pullDockerSourceWithCLI(ctx, opts, fmt.Errorf("connect to Docker Engine: %w", err))
	}

	platform, err := m.resolveDockerPullPlatform(ctx, cli, opts.Platform)
	if err != nil {
		return m.pullDockerSourceWithCLI(ctx, opts, err)
	}
	if opts.AnnounceRemoteSize {
		if size, err := remoteCompressedLayerSize(opts.Image, platform); err != nil {
			m.writeDockerPullNotice(opts.LogPath, "warning: could not determine remote compressed layer size: "+sanitizeTimingError(err))
		} else {
			m.writeDockerPullNotice(opts.LogPath, fmt.Sprintf("Remote compressed layers: %s; actual transfer may be lower when Docker reuses layers.", formatDockerPullBytes(size)))
		}
	}

	registryAuth, err := dockerRegistryAuth(opts.Image)
	if err != nil {
		m.writeDockerPullNotice(opts.LogPath, "warning: could not load Docker registry credentials; continuing without explicit credentials: "+sanitizeTimingError(err))
	}
	response, err := cli.ImagePull(ctx, opts.Image, client.ImagePullOptions{
		RegistryAuth: registryAuth,
		Platforms:    []ocispec.Platform{platform},
	})
	if err != nil {
		return fmt.Errorf("Docker Engine pull %s: %w", opts.Image, err)
	}
	if err := m.renderDockerPullProgress(ctx, response, opts.LogPath); err != nil {
		return fmt.Errorf("Docker Engine pull %s: %w", opts.Image, err)
	}
	m.writeDockerPullNotice(opts.LogPath, "Docker source pull complete: "+opts.Image)
	return nil
}

func (m *Manager) pullDockerSourceWithCLI(ctx context.Context, opts dockerSourcePullOptions, apiErr error) error {
	m.warnf("warning: %v; falling back to docker pull CLI\n", apiErr)
	m.writeDockerPullNotice(opts.LogPath, "warning: "+sanitizeTimingError(apiErr)+"; falling back to docker pull CLI")
	args := []string{"pull"}
	if opts.Platform != "" {
		args = append(args, "--platform", opts.Platform)
	}
	args = append(args, opts.Image)
	return m.runHostLogged(ctx, opts.LogPath, "docker", args...)
}

func (m *Manager) resolveDockerPullPlatform(ctx context.Context, cli *client.Client, configured string) (ocispec.Platform, error) {
	if platform, ok := normalizedDockerPlatform(configured, ""); ok {
		return platform, nil
	}
	if platform, ok := normalizedDockerPlatform(m.Config.Provider.Platform, ""); ok {
		return platform, nil
	}
	info, err := cli.Info(ctx, client.InfoOptions{})
	if err != nil {
		return ocispec.Platform{}, fmt.Errorf("inspect Docker Engine platform: %w", err)
	}
	if platform, ok := normalizedDockerPlatform(info.Info.OSType+"/"+info.Info.Architecture, ""); ok {
		return platform, nil
	}
	if platform, ok := normalizedDockerPlatform(runtime.GOOS+"/"+runtime.GOARCH, ""); ok {
		return platform, nil
	}
	return ocispec.Platform{}, fmt.Errorf("Docker Engine did not report a usable platform")
}

func normalizedDockerPlatform(value, fallbackOS string) (ocispec.Platform, bool) {
	parts := strings.Split(strings.Trim(strings.ToLower(value), "/"), "/")
	if len(parts) == 0 || len(parts) > 3 || parts[0] == "" {
		return ocispec.Platform{}, false
	}
	platform := ocispec.Platform{OS: fallbackOS}
	if len(parts) == 1 {
		platform.Architecture = normalizeDockerArchitecture(parts[0])
	} else {
		platform.OS = parts[0]
		platform.Architecture = normalizeDockerArchitecture(parts[1])
		if len(parts) == 3 {
			platform.Variant = parts[2]
		}
	}
	if platform.OS == "" {
		platform.OS = "linux"
	}
	if platform.Architecture == "" {
		return ocispec.Platform{}, false
	}
	return platform, true
}

func normalizeDockerArchitecture(architecture string) string {
	switch architecture {
	case "x86_64", "x64":
		return "amd64"
	case "aarch64":
		return "arm64"
	default:
		return architecture
	}
}

func remoteCompressedLayerSize(image string, platform ocispec.Platform) (int64, error) {
	ref, authenticator, err := dockerImageReferenceAndAuth(image)
	if err != nil {
		return 0, err
	}
	remoteImage, err := remote.Image(ref, remote.WithAuth(authenticator), remote.WithPlatform(gcrv1.Platform{
		OS:           platform.OS,
		Architecture: platform.Architecture,
		Variant:      platform.Variant,
	}))
	if err != nil {
		return 0, err
	}
	layers, err := remoteImage.Layers()
	if err != nil {
		return 0, err
	}
	var total int64
	for _, layer := range layers {
		size, err := layer.Size()
		if err != nil {
			return 0, err
		}
		total += size
	}
	return total, nil
}

func dockerRegistryAuth(image string) (string, error) {
	ref, authenticator, err := dockerImageReferenceAndAuth(image)
	if err != nil {
		return "", err
	}
	credentials, err := authenticator.Authorization()
	if err != nil {
		return "", err
	}
	content, err := json.Marshal(registry.AuthConfig{
		Username:      credentials.Username,
		Password:      credentials.Password,
		Auth:          credentials.Auth,
		ServerAddress: ref.Context().RegistryStr(),
		IdentityToken: credentials.IdentityToken,
		RegistryToken: credentials.RegistryToken,
	})
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(content), nil
}

func dockerImageReferenceAndAuth(image string) (name.Reference, authn.Authenticator, error) {
	ref, err := name.ParseReference(image)
	if err != nil {
		return nil, nil, err
	}
	authenticator, err := authn.DefaultKeychain.Resolve(ref.Context().Registry)
	if err != nil {
		return nil, nil, err
	}
	return ref, authenticator, nil
}

func (m *Manager) renderDockerPullProgress(ctx context.Context, response client.ImagePullResponse, logPath string) error {
	transcript, err := m.transcript(logPath, "", "docker-pull")
	if err != nil {
		return err
	}
	interactive := term.IsTerminal(int(os.Stdout.Fd())) && containsString(m.Config.Logging.TranscriptSinks, "console") && m.Config.Logging.TranscriptConsoleFormat == "text"
	eventWriter := transcript.Stdout
	if interactive {
		eventWriter = transcript.File
	}
	layers := map[string]dockerLayerProgress{}
	lastRender := time.Time{}
	rendered := false
	for message, streamErr := range response.JSONMessages(ctx) {
		if streamErr != nil {
			return streamErr
		}
		writeDockerPullEvent(eventWriter, message.ID, message.Status, message.Progress, message.Stream, message.Error)
		if message.Error != nil {
			return message.Error
		}
		if message.ID != "" {
			layer := layers[message.ID]
			if message.Progress != nil {
				layer.current = message.Progress.Current
				if message.Progress.Total > 0 {
					layer.total = message.Progress.Total
				}
			}
			if dockerPullLayerComplete(message.Status) {
				layer.completed = true
				if layer.total > 0 {
					layer.current = layer.total
				}
			}
			layers[message.ID] = layer
		}
		if time.Since(lastRender) >= dockerPullProgressInterval {
			if interactive {
				writeDockerPullSummary(os.Stdout, true, layers)
			} else {
				writeDockerPullSummary(transcript.Stdout, false, layers)
			}
			lastRender = time.Now()
			rendered = true
		}
	}
	if rendered {
		if interactive {
			writeDockerPullSummary(os.Stdout, true, layers)
		} else {
			writeDockerPullSummary(transcript.Stdout, false, layers)
		}
		if interactive {
			fmt.Fprintln(os.Stdout)
		}
	}
	return nil
}

func (m *Manager) writeDockerPullNotice(logPath, message string) {
	transcript, err := m.transcript(logPath, "", "docker-pull")
	if err != nil {
		m.logger().Warn("docker pull transcript unavailable", "operation", "docker-pull", "logPath", logPath, "error", err)
		return
	}
	_, _ = fmt.Fprintf(transcript.Stdout, "%s\n", message)
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func writeDockerPullEvent(logFile io.Writer, id, status string, progress *jsonstream.Progress, stream string, pullErr error) {
	if logFile == nil {
		return
	}
	parts := make([]string, 0, 4)
	if id != "" {
		parts = append(parts, id)
	}
	if status != "" {
		parts = append(parts, status)
	}
	if progress != nil {
		parts = append(parts, fmt.Sprintf("progress=%d/%d", progress.Current, progress.Total))
	}
	if stream != "" {
		parts = append(parts, strings.TrimSpace(stream))
	}
	if pullErr != nil {
		parts = append(parts, "error="+sanitizeTimingError(pullErr))
	}
	fmt.Fprintf(logFile, "%s %s\n", time.Now().UTC().Format(time.RFC3339Nano), strings.Join(parts, " "))
}

func dockerPullLayerComplete(status string) bool {
	status = strings.ToLower(strings.TrimSpace(status))
	return status == "pull complete" || status == "already exists" || status == "exists"
}

func writeDockerPullSummary(w io.Writer, interactive bool, layers map[string]dockerLayerProgress) {
	var complete, known int
	var currentBytes, totalBytes int64
	for _, layer := range layers {
		if layer.completed {
			complete++
		}
		if layer.total > 0 {
			known++
			totalBytes += layer.total
			currentBytes += min(layer.current, layer.total)
		}
	}
	line := fmt.Sprintf("Docker source pull: %d/%d layers complete; %s/%s", complete, len(layers), formatDockerPullBytes(currentBytes), formatDockerPullBytes(totalBytes))
	if totalBytes > 0 {
		line += fmt.Sprintf(" (%.0f%%)", float64(currentBytes)*100/float64(totalBytes))
	}
	if known < len(layers) {
		line += fmt.Sprintf("; %d layer(s) size pending", len(layers)-known)
	}
	if interactive {
		fmt.Fprintf(w, "\r\033[2K%s", line)
		return
	}
	fmt.Fprintf(w, "%s %s\n", time.Now().UTC().Format(time.RFC3339Nano), line)
}

func formatDockerPullBytes(value int64) string {
	const unit = 1024
	if value < unit {
		return fmt.Sprintf("%d B", value)
	}
	units := []string{"KiB", "MiB", "GiB", "TiB"}
	size := float64(value)
	index := -1
	for size >= unit && index+1 < len(units) {
		size /= unit
		index++
	}
	return fmt.Sprintf("%.1f %s", size, units[index])
}
