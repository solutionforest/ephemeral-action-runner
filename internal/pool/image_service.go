package pool

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/hosttrust"
	artifactimage "github.com/solutionforest/ephemeral-action-runner/internal/image"
	"github.com/solutionforest/ephemeral-action-runner/internal/logging"
	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
	"golang.org/x/term"
)

type ImageBuildOptions = artifactimage.ImageBuildOptions
type ImageManifest = artifactimage.Manifest
type imageState = artifactimage.ImageState
type runnerImagesCopyMode = artifactimage.RunnerImagesCopyMode
type dockerSourcePullOptions = artifactimage.DockerSourcePullOptions
type sourceCacheManifest = artifactimage.SourceCacheManifest
type TrustedCACertificate = artifactimage.TrustedCACertificate

const (
	imageStateMissing          = artifactimage.ImageStateMissing
	imageStateCurrent          = artifactimage.ImageStateCurrent
	imageStateOutdated         = artifactimage.ImageStateOutdated
	imageManifestSchemaVersion = artifactimage.ManifestSchemaVersion
	imageManifestLabel         = artifactimage.ManifestLabel
	runnerImagesCopyNone       = artifactimage.RunnerImagesCopyNone
	runnerImagesCopySubset     = artifactimage.RunnerImagesCopySubset
	trustedCAGuestDir          = "/usr/local/share/ca-certificates/epar"
)

var pullDockerSourceCommand = (*Manager).pullDockerSource

var dockerPullProgressTerminal = func() bool {
	return term.IsTerminal(int(os.Stdout.Fd()))
}

var dockerPullProgressConsole io.Writer = os.Stdout

var (
	runHostCommand       = runHost
	runHostLoggedCommand = runHostLogged
	runHostOutputCommand = runHostOutput
	runHostQuietCommand  = runHostQuiet
)

func (m *Manager) imageCoordinator() *artifactimage.Coordinator {
	return artifactimage.NewCoordinator(m.Config, m.Provider, m.Lifecycle, m.ProjectRoot, m.DryRun, imageEnvironment{manager: m})
}

func (m *Manager) UpdateUpstream(ctx context.Context) error {
	return m.imageCoordinator().UpdateUpstream(ctx)
}

func (m *Manager) BuildImage(ctx context.Context, options ImageBuildOptions) error {
	return m.imageCoordinator().BuildImage(ctx, options)
}

func (m *Manager) EnsureImage(ctx context.Context) error {
	return m.imageCoordinator().EnsureImage(ctx)
}

func (m *Manager) RefreshScripts(ctx context.Context) error {
	return m.imageCoordinator().RefreshScripts(ctx)
}

func (m *Manager) pullDockerSource(ctx context.Context, options dockerSourcePullOptions) error {
	return m.imageCoordinator().PullDockerSource(ctx, options)
}

func (m *Manager) writeDockerPullNotice(logPath, message string) {
	m.imageCoordinator().WriteDockerPullNotice(logPath, message)
}

func (m *Manager) writeDockerPullProgress(logPath string, layers map[string]artifactimage.DockerPullProgress) {
	m.imageCoordinator().WriteDockerPullProgress(logPath, layers)
}

func writeDockerPullEvent(writer io.Writer, event artifactimage.DockerPullEvent) {
	artifactimage.WriteDockerPullEvent(writer, event)
}

func (m *Manager) desiredImageManifest(ctx context.Context) (ImageManifest, error) {
	return m.imageCoordinator().DesiredImageManifest(ctx)
}

func (m *Manager) currentImageState(ctx context.Context, wantedHash string) (imageState, error) {
	return m.imageCoordinator().CurrentImageState(ctx, wantedHash)
}

func imageManifestHash(manifest ImageManifest) (string, error) {
	return artifactimage.ImageManifestHash(manifest)
}

func writeStoredImageManifest(path string, manifest ImageManifest) error {
	return artifactimage.WriteStoredManifest(path, manifest)
}

func readStoredImageManifest(path string) (artifactimage.StoredManifest, error) {
	return artifactimage.ReadStoredManifest(path)
}

func wslImageManifestSidecarPath(outputPath string) string {
	return artifactimage.WSLImageManifestPath(outputPath)
}

func sourceCacheManifestPath(rootfsPath string) string {
	return artifactimage.SourceCacheManifestPath(rootfsPath)
}

func writeSourceCacheManifest(path string, manifest sourceCacheManifest) error {
	return artifactimage.WriteSourceCacheManifest(path, manifest)
}

func wslDockerSourceRootfsPath(outputPath string) string {
	return artifactimage.WSLSourceRootfsPath(outputPath)
}

func sourceImageEnvContent(environment []string) string {
	return artifactimage.SourceImageEnvContent(environment)
}

func (m *Manager) prepareDockerContainerBuildContext(buildContext, upstreamDirectory, manifestContent string) error {
	return m.imageCoordinator().PrepareDockerContainerBuildContext(buildContext, upstreamDirectory, manifestContent)
}

func (m *Manager) prepareDockerContainerBuildContextWithHostTrust(buildContext, upstreamDirectory, manifestContent string, snapshot hosttrust.Snapshot) error {
	return m.imageCoordinator().PrepareDockerContainerBuildContextWithHostTrust(buildContext, upstreamDirectory, manifestContent, snapshot)
}

func (m *Manager) buildDockerContainerImage(ctx context.Context, options ImageBuildOptions, upstreamDirectory string) error {
	return m.imageCoordinator().BuildDockerContainerImage(ctx, options, upstreamDirectory)
}

func (m *Manager) trustedCACertificates() ([]TrustedCACertificate, error) {
	return m.imageCoordinator().TrustedCACertificates()
}

func (m *Manager) TrustedCACertificates() ([]TrustedCACertificate, error) {
	return m.trustedCACertificates()
}

func (m *Manager) prepareWSLDockerSourceRootfs(ctx context.Context, outputPath, buildLogPath string, manifest ImageManifest) (string, string, error) {
	return m.imageCoordinator().PrepareWSLDockerSourceRootfs(ctx, outputPath, buildLogPath, manifest)
}

func (m *Manager) runnerImageBuildScripts() []string {
	return m.imageCoordinator().RunnerImageBuildScripts()
}

func (m *Manager) runnerImagesCopyMode() runnerImagesCopyMode {
	return m.imageCoordinator().RunnerImagesCopyMode()
}

func (m *Manager) prepareWSLDockerSourceGuest(ctx context.Context, instance string) error {
	return m.imageCoordinator().PrepareWSLDockerSourceGuest(ctx, instance)
}

func (m *Manager) installCustomInstallScripts(ctx context.Context, instance string) error {
	return m.imageCoordinator().InstallCustomInstallScripts(ctx, instance)
}

func (m *Manager) customInstallScriptHostPath(script string) (string, error) {
	return m.imageCoordinator().CustomInstallScriptHostPath(script)
}

func (m *Manager) enableWSLSystemd(ctx context.Context, instance string) error {
	return m.imageCoordinator().EnableWSLSystemd(ctx, instance)
}

func (m *Manager) startOptions(logPath, instance string) (provider.StartOptions, error) {
	transcript, err := m.transcript(logPath, instance, transcriptComponent(logPath))
	if err != nil {
		return provider.StartOptions{}, err
	}
	return provider.StartOptions{
		Network:    m.Config.Provider.Network,
		RosettaTag: m.Config.Provider.RosettaTag,
		LogPath:    logPath,
		Stdout:     transcript.Stdout,
		Stderr:     transcript.Stderr,
	}, nil
}

type imageEnvironment struct {
	manager *Manager
}

func (environment imageEnvironment) PreflightStorage(operation string, peakBytes uint64) error {
	return environment.manager.preflightStorage(operation, peakBytes)
}

func (environment imageEnvironment) BuildLogPath(name string) string {
	return environment.manager.buildLogPath(name)
}

func (environment imageEnvironment) ReleaseTranscript(path string) error {
	return environment.manager.releaseTranscript(path)
}

func (environment imageEnvironment) Infof(format string, args ...any) {
	environment.manager.infof(format, args...)
}

func (environment imageEnvironment) Warnf(format string, args ...any) {
	environment.manager.warnf(format, args...)
}

func (environment imageEnvironment) RunHostLogged(ctx context.Context, logPath, name string, args ...string) error {
	return environment.manager.runHostLogged(ctx, logPath, name, args...)
}

func (environment imageEnvironment) RunHost(ctx context.Context, name string, args ...string) error {
	return runHostCommand(ctx, name, args...)
}

func (environment imageEnvironment) RunHostOutput(ctx context.Context, name string, args ...string) (string, error) {
	return runHostOutputCommand(ctx, name, args...)
}

func (environment imageEnvironment) RunHostQuiet(ctx context.Context, name string, args ...string) error {
	return runHostQuietCommand(ctx, name, args...)
}

func (environment imageEnvironment) TimeStartupStage(stage string, fn func() error) error {
	return environment.manager.timeStartupStage(stage, fn)
}

func (environment imageEnvironment) HostTrustEnabled() bool {
	return environment.manager.hostTrustEnabled()
}

func (environment imageEnvironment) ResolveHostTrust(ctx context.Context) (hosttrust.Snapshot, error) {
	return environment.manager.resolveHostTrust(ctx)
}

func (environment imageEnvironment) WriteHostTrustBuildInputs(buildContext string, snapshot hosttrust.Snapshot) error {
	return environment.manager.writeHostTrustBuildInputs(buildContext, snapshot)
}

func (environment imageEnvironment) ValidateRuntime(ctx context.Context, instance string) error {
	return environment.manager.validateRuntime(ctx, instance)
}

func (environment imageEnvironment) ExecGuest(ctx context.Context, instance string, command []string, options provider.ExecOptions) (provider.ExecResult, error) {
	return environment.manager.execGuest(ctx, instance, command, options)
}

func (environment imageEnvironment) Transcript(path, instance, component string) (*logging.Transcript, error) {
	return environment.manager.transcript(path, instance, component)
}

func (environment imageEnvironment) PullDockerSource(ctx context.Context, options artifactimage.DockerSourcePullOptions) error {
	return pullDockerSourceCommand(environment.manager, ctx, options)
}

func (environment imageEnvironment) LogInfo(message string, args ...any) {
	environment.manager.logger().Info(message, args...)
}

func (environment imageEnvironment) LogWarn(message string, args ...any) {
	environment.manager.logger().Warn(message, args...)
}

func (environment imageEnvironment) DockerPullProgressTerminal() bool {
	return dockerPullProgressTerminal()
}

func (environment imageEnvironment) DockerPullProgressConsole() io.Writer {
	return dockerPullProgressConsole
}

func (environment imageEnvironment) RunnerName(prefix string, sequence int, now time.Time) string {
	return RunnerName(prefix, sequence, now)
}

func (environment imageEnvironment) TranscriptComponent(path string) string {
	return transcriptComponent(path)
}

func guestText(content []byte) string {
	return strings.ReplaceAll(string(content), "\r\n", "\n")
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func runHost(ctx context.Context, name string, args ...string) error {
	return exec.CommandContext(ctx, name, args...).Run()
}

func runHostOutput(ctx context.Context, name string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, name, args...)
	output, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s %s failed: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return string(output), nil
}

func runHostQuiet(ctx context.Context, name string, args ...string) error {
	return exec.CommandContext(ctx, name, args...).Run()
}

func runHostLogged(ctx context.Context, _ string, stdout, stderr io.Writer, name string, args ...string) error {
	command := exec.CommandContext(ctx, name, args...)
	command.Stdout = stdout
	command.Stderr = stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("%s %s failed: %w", name, strings.Join(args, " "), err)
	}
	return nil
}

func copyFile(source, destination string, mode os.FileMode) error {
	return artifactimage.CopyFile(source, destination, mode)
}
