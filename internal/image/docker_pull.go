// Package image contains provider-neutral image acquisition primitives.
package image

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	gcrv1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/moby/moby/api/types/jsonstream"
	"github.com/moby/moby/api/types/registry"
	"github.com/moby/moby/client"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// DockerEnginePullError means Docker Engine accepted the connection but rejected the image pull.
// Callers may distinguish it from connection and platform errors that can safely use a CLI fallback.
type DockerEnginePullError struct {
	Image string
	Err   error
}

func (err *DockerEnginePullError) Error() string {
	return fmt.Sprintf("Docker Engine pull %s: %v", err.Image, err.Err)
}

func (err *DockerEnginePullError) Unwrap() error { return err.Err }

// DockerPullOptions defines a Docker Engine acquisition without any pool or provider logging policy.
type DockerPullOptions struct {
	Image            string
	Platform         string
	FallbackPlatform string
	QueryRemoteSize  bool
}

// DockerPullResult holds the pulled stream and optional non-fatal lookup failures.
type DockerPullResult struct {
	Response              client.ImagePullResponse
	Platform              ocispec.Platform
	RemoteCompressedSize  int64
	RemoteCompressedError error
	RegistryAuthError     error
}

// DockerPullProgress is the shared per-layer state used to summarize progress.
type DockerPullProgress struct {
	Current   int64
	Total     int64
	Completed bool
}

// DockerPullEvent is one decoded Docker Engine pull event.
type DockerPullEvent struct {
	ID       string
	Status   string
	Progress *jsonstream.Progress
	Stream   string
	Error    error
}

// PullDockerImage opens the Docker Engine, resolves the requested platform, optionally queries registry layer sizes, and starts a pull.
// Remote metadata and explicit registry-auth failures are non-fatal and are returned on the result for the caller to report.
func PullDockerImage(ctx context.Context, opts DockerPullOptions) (DockerPullResult, error) {
	cli, err := client.New(client.FromEnv)
	if err != nil {
		return DockerPullResult{}, fmt.Errorf("initialize Docker Engine client: %w", err)
	}
	if _, err := cli.Ping(ctx, client.PingOptions{}); err != nil {
		return DockerPullResult{}, fmt.Errorf("connect to Docker Engine: %w", err)
	}

	platform, err := ResolveDockerPlatform(ctx, cli, opts.Platform, opts.FallbackPlatform)
	if err != nil {
		return DockerPullResult{}, err
	}
	result := DockerPullResult{Platform: platform}
	if opts.QueryRemoteSize {
		result.RemoteCompressedSize, result.RemoteCompressedError = RemoteCompressedLayerSize(opts.Image, platform)
	}

	registryAuth, err := DockerRegistryAuth(opts.Image)
	if err != nil {
		result.RegistryAuthError = err
	}
	response, err := cli.ImagePull(ctx, opts.Image, client.ImagePullOptions{
		RegistryAuth: registryAuth,
		Platforms:    []ocispec.Platform{platform},
	})
	if err != nil && !nilLikeError(err) {
		return result, &DockerEnginePullError{Image: opts.Image, Err: err}
	}
	result.Response = response
	return result, nil
}

// nilLikeError protects the Engine API boundary from an error interface that
// contains a typed nil pointer. Such a value represents no failure but compares
// non-nil as an interface and otherwise renders as the misleading text "<nil>".
func nilLikeError(err error) bool {
	for err != nil {
		if strings.TrimSpace(err.Error()) == "<nil>" {
			return true
		}
		value := reflect.ValueOf(err)
		switch value.Kind() {
		case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
			if value.IsNil() {
				return true
			}
		}
		next := errors.Unwrap(err)
		if next == nil {
			return false
		}
		err = next
	}
	return true
}

// ResolveDockerPlatform chooses an explicit platform, provider fallback platform, engine platform, then the local runtime platform.
func ResolveDockerPlatform(ctx context.Context, cli *client.Client, configured, fallback string) (ocispec.Platform, error) {
	if platform, ok := NormalizedDockerPlatform(configured, ""); ok {
		return platform, nil
	}
	if platform, ok := NormalizedDockerPlatform(fallback, ""); ok {
		return platform, nil
	}
	info, err := cli.Info(ctx, client.InfoOptions{})
	if err != nil {
		return ocispec.Platform{}, fmt.Errorf("inspect Docker Engine platform: %w", err)
	}
	if platform, ok := NormalizedDockerPlatform(info.Info.OSType+"/"+info.Info.Architecture, ""); ok {
		return platform, nil
	}
	if platform, ok := NormalizedDockerPlatform(runtime.GOOS+"/"+runtime.GOARCH, ""); ok {
		return platform, nil
	}
	return ocispec.Platform{}, fmt.Errorf("Docker Engine did not report a usable platform")
}

// NormalizedDockerPlatform parses a Docker platform and normalizes common architecture aliases.
func NormalizedDockerPlatform(value, fallbackOS string) (ocispec.Platform, bool) {
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

// RemoteCompressedLayerSize returns the total compressed size of the selected remote image layers.
func RemoteCompressedLayerSize(image string, platform ocispec.Platform) (int64, error) {
	ref, authenticator, err := DockerImageReferenceAndAuth(image)
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

// DockerRegistryAuth returns Docker Engine's base64-encoded registry auth payload.
func DockerRegistryAuth(image string) (string, error) {
	ref, authenticator, err := DockerImageReferenceAndAuth(image)
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

// DockerImageReferenceAndAuth resolves an image reference and default credential helper.
func DockerImageReferenceAndAuth(image string) (name.Reference, authn.Authenticator, error) {
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

// ConsumeDockerPullProgress decodes an Engine pull response and delivers each event in order.
func ConsumeDockerPullProgress(ctx context.Context, response client.ImagePullResponse, handle func(DockerPullEvent) error) error {
	for message, streamErr := range response.JSONMessages(ctx) {
		if streamErr != nil && !nilLikeError(streamErr) {
			return streamErr
		}
		event := DockerPullEvent{ID: message.ID, Status: message.Status, Progress: message.Progress, Stream: message.Stream, Error: message.Error}
		if nilLikeError(event.Error) {
			event.Error = nil
		}
		if err := handle(event); err != nil {
			return err
		}
		if event.Error != nil {
			return event.Error
		}
	}
	return nil
}

// IsDockerPullLayerComplete reports whether an Engine status marks a layer complete.
func IsDockerPullLayerComplete(status string) bool {
	status = strings.ToLower(strings.TrimSpace(status))
	return status == "pull complete" || status == "already exists" || status == "exists"
}

// DockerPullProgressSummary renders one concise progress line.
func DockerPullProgressSummary(layers map[string]DockerPullProgress) string {
	var complete, known int
	var currentBytes, totalBytes int64
	for _, layer := range layers {
		if layer.Completed {
			complete++
		}
		if layer.Total > 0 {
			known++
			totalBytes += layer.Total
			currentBytes += min(layer.Current, layer.Total)
		}
	}
	line := fmt.Sprintf("Docker source pull: %d/%d layers complete; %s/%s", complete, len(layers), FormatDockerPullBytes(currentBytes), FormatDockerPullBytes(totalBytes))
	if totalBytes > 0 {
		line += fmt.Sprintf(" (%.0f%%)", float64(currentBytes)*100/float64(totalBytes))
	}
	if known < len(layers) {
		line += fmt.Sprintf("; %d layer(s) size pending", len(layers)-known)
	}
	return line
}

// FormatDockerPullBytes renders byte counts used by pull progress and remote-size notices.
func FormatDockerPullBytes(value int64) string {
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
