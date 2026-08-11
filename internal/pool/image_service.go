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
	"github.com/solutionforest/ephemeral-action-runner/internal/storage"
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

var progressTerminalWidth = func() int {
	width, _, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || width <= 0 {
		return 0
	}
	return width
}

var dockerPullProgressConsole io.Writer = os.Stdout

var (
	runHostCommand         = runHost
	runHostLoggedCommand   = runHostLogged
	runHostOutputCommand   = runHostOutput
	runHostOutputToCommand = runHostOutputTo
	runHostQuietCommand    = runHostQuiet
)

func (m *Manager) imageCoordinator() *artifactimage.Coordinator {
	coordinator := artifactimage.NewCoordinator(m.Config, m.Provider, m.Lifecycle, m.ProjectRoot, m.DryRun, imageEnvironment{manager: m})
	coordinator.ConfigPath = m.ConfigPath
	coordinator.Clock = m.currentTime
	return coordinator
}

func (m *Manager) UpdateUpstream(ctx context.Context) error {
	return m.imageCoordinator().UpdateUpstream(ctx)
}

func (m *Manager) BuildImage(ctx context.Context, options ImageBuildOptions) error {
	return m.imageCoordinator().BuildImage(ctx, options)
}

func (m *Manager) EnsureImage(ctx context.Context) error {
	m.imageEnsureMu.Lock()
	defer m.imageEnsureMu.Unlock()
	if m.imageEnsured {
		return nil
	}
	if err := m.imageCoordinator().EnsureImage(ctx); err != nil {
		return err
	}
	if err := m.pinDockerRuntimeImage(ctx); err != nil {
		return err
	}
	m.imageEnsured = true
	return nil
}

func (m *Manager) UpdateImage(ctx context.Context) error {
	m.imageEnsureMu.Lock()
	defer m.imageEnsureMu.Unlock()
	if err := m.imageCoordinator().UpdateImage(ctx); err != nil {
		return err
	}
	if err := m.pinDockerRuntimeImage(ctx); err != nil {
		return err
	}
	m.imageEnsured = true
	return nil
}

func (m *Manager) CheckRemoteImageUpdate(ctx context.Context, now time.Time) (artifactimage.RemoteUpdateCheck, error) {
	m.imageEnsureMu.Lock()
	defer m.imageEnsureMu.Unlock()
	return m.imageCoordinator().CheckRemoteUpdate(ctx, now)
}

func (m *Manager) ApplyPendingImageUpdate(ctx context.Context, now time.Time) error {
	m.imageEnsureMu.Lock()
	defer m.imageEnsureMu.Unlock()
	if err := m.imageCoordinator().ApplyPendingUpdate(ctx, now); err != nil {
		return err
	}
	if err := m.pinDockerRuntimeImage(ctx); err != nil {
		return err
	}
	m.imageEnsured = true
	return nil
}

func (m *Manager) ImageUpdatePolicyStatus() (artifactimage.UpdatePolicyStatus, error) {
	return m.imageCoordinator().UpdatePolicyStatus()
}

func (m *Manager) DeferPendingImageUpdate(reason string) error {
	m.imageEnsureMu.Lock()
	defer m.imageEnsureMu.Unlock()
	return m.imageCoordinator().DeferPendingUpdate(reason)
}

func (m *Manager) pinDockerRuntimeImage(ctx context.Context) error {
	if m.Config.Provider.Type != "docker-container" || m.DryRun {
		return nil
	}
	identity, err := runHostOutputCommand(ctx, "docker", "image", "inspect", "--format", "{{.Id}}", m.Config.Image.OutputImage)
	if err != nil {
		return fmt.Errorf("resolve immutable Docker Container runtime image: %w", err)
	}
	identity = strings.TrimSpace(identity)
	if identity == "" {
		return fmt.Errorf("Docker Container runtime image returned an empty immutable identity")
	}
	// Keep user configuration as the desired mutable name while the live
	// manager and all replacements consume the exact verified image ID.
	m.Config.Provider.SourceImage = identity
	return nil
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

func (m *Manager) desiredLocalImageManifest(ctx context.Context) (ImageManifest, error) {
	return m.imageCoordinator().DesiredLocalImageManifest(ctx)
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

func (environment imageEnvironment) PreflightStorage(operation string, plan storage.OperationPlan) error {
	if plan.ID == "" {
		plan.ID = operation
	}
	return environment.manager.preflightStorage(plan)
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

func (environment imageEnvironment) RunHostBuildxLogged(ctx context.Context, logPath, name string, args ...string) error {
	return environment.manager.runHostBuildxLogged(ctx, logPath, name, args...)
}

func (environment imageEnvironment) RunHost(ctx context.Context, name string, args ...string) error {
	return runHostCommand(ctx, name, args...)
}

func (environment imageEnvironment) RunHostOutput(ctx context.Context, name string, args ...string) (string, error) {
	return runHostOutputCommand(ctx, name, args...)
}

func (environment imageEnvironment) RunHostOutputTo(ctx context.Context, output io.Writer, name string, args ...string) error {
	return runHostOutputToCommand(ctx, output, name, args...)
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

func (environment imageEnvironment) ResolveBuildTrust(ctx context.Context) (hosttrust.Snapshot, error) {
	return environment.manager.resolveBuildTrust(ctx)
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

func (environment imageEnvironment) ProgressTerminal() bool {
	return dockerPullProgressTerminal()
}

func (environment imageEnvironment) ProgressConsole() io.Writer {
	return dockerPullProgressConsole
}

func (environment imageEnvironment) ProgressWidth() int {
	return progressTerminalWidth()
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

func runHostOutputTo(ctx context.Context, output io.Writer, name string, args ...string) error {
	command := exec.CommandContext(ctx, name, args...)
	var stderr strings.Builder
	command.Stdout = output
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("%s %s failed: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return nil
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
