package image

import (
	"context"
	"io"
	"os"

	"github.com/solutionforest/ephemeral-action-runner/internal/hosttrust"
)

// ImageState is the reusable-artifact state returned by CurrentImageState.
type ImageState = imageState
type RunnerImagesCopyMode = runnerImagesCopyMode

const (
	ImageStateMissing      = imageStateMissing
	ImageStateCurrent      = imageStateCurrent
	ImageStateOutdated     = imageStateOutdated
	RunnerImagesCopyNone   = runnerImagesCopyNone
	RunnerImagesCopySubset = runnerImagesCopySubset
)

func (m *Coordinator) DesiredImageManifest(ctx context.Context) (Manifest, error) {
	return m.desiredImageManifest(ctx)
}

func (m *Coordinator) CurrentImageState(ctx context.Context, wantedHash string) (ImageState, error) {
	return m.currentImageState(ctx, wantedHash)
}

func (m *Coordinator) PrepareDockerContainerBuildContext(buildContext, upstreamDirectory, manifestContent string) error {
	return m.prepareDockerContainerBuildContext(buildContext, upstreamDirectory, manifestContent)
}

func (m *Coordinator) PrepareDockerContainerBuildContextWithHostTrust(buildContext, upstreamDirectory, manifestContent string, snapshot hosttrust.Snapshot) error {
	return m.prepareDockerContainerBuildContextWithHostTrust(buildContext, upstreamDirectory, manifestContent, snapshot)
}

func (m *Coordinator) BuildDockerContainerImage(ctx context.Context, options ImageBuildOptions, upstreamDirectory string) error {
	return m.buildDockerContainerImage(ctx, options, upstreamDirectory)
}

func (m *Coordinator) PrepareWSLDockerSourceRootfs(ctx context.Context, outputPath, buildLogPath string, manifest Manifest) (string, string, error) {
	return m.prepareWSLDockerSourceRootfs(ctx, outputPath, buildLogPath, manifest)
}

func (m *Coordinator) WriteDockerPullProgress(logPath string, layers map[string]DockerPullProgress) {
	m.writeDockerPullProgress(logPath, layers)
}

func WriteDockerPullEvent(writer io.Writer, event DockerPullEvent) {
	writeDockerPullEvent(writer, event)
}

func ImageManifestHash(manifest Manifest) (string, error) {
	return ManifestHash(manifest)
}

func SourceImageEnvContent(environment []string) string {
	return sourceImageEnvContent(environment)
}

func CopyFile(source, destination string, mode os.FileMode) error {
	return copyFile(source, destination, mode)
}

func (m *Coordinator) RunnerImageBuildScripts() []string {
	return m.runnerImageBuildScripts()
}

func (m *Coordinator) RunnerImagesCopyMode() RunnerImagesCopyMode {
	return m.runnerImagesCopyMode()
}

func (m *Coordinator) PrepareWSLDockerSourceGuest(ctx context.Context, instance string) error {
	return m.prepareWSLDockerSourceGuest(ctx, instance)
}

func (m *Coordinator) InstallCustomInstallScripts(ctx context.Context, instance string) error {
	return m.installCustomInstallScripts(ctx, instance)
}

func (m *Coordinator) CustomInstallScriptHostPath(script string) (string, error) {
	return m.customInstallScriptHostPath(script)
}

func (m *Coordinator) EnableWSLSystemd(ctx context.Context, instance string) error {
	return m.enableWSLSystemd(ctx, instance)
}
