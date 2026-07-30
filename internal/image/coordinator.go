package image

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
	"github.com/solutionforest/ephemeral-action-runner/internal/hosttrust"
	"github.com/solutionforest/ephemeral-action-runner/internal/logging"
	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
	"github.com/solutionforest/ephemeral-action-runner/internal/storage"
)

const (
	sourceUpdateExpansionBytes = 5 * storage.GiB
	hostTrustGuestDir          = "/usr/local/share/ca-certificates/epar-host"
	hostTrustMarkerGuest       = "/opt/epar/host-trust-generation.json"
)

// Environment exposes the provider-neutral host and guest operations needed
// while creating reusable artifacts. The pool supplies these operations but
// does not own image policy or provider-specific build selection.
type Environment interface {
	PreflightStorage(operation string, peakBytes uint64) error
	BuildLogPath(name string) string
	ReleaseTranscript(path string) error
	Infof(format string, args ...any)
	Warnf(format string, args ...any)
	RunHostLogged(ctx context.Context, logPath, name string, args ...string) error
	RunHostBuildxLogged(ctx context.Context, logPath, name string, args ...string) error
	RunHost(ctx context.Context, name string, args ...string) error
	RunHostOutput(ctx context.Context, name string, args ...string) (string, error)
	RunHostOutputTo(ctx context.Context, output io.Writer, name string, args ...string) error
	RunHostQuiet(ctx context.Context, name string, args ...string) error
	TimeStartupStage(stage string, fn func() error) error
	HostTrustEnabled() bool
	ResolveHostTrust(ctx context.Context) (hosttrust.Snapshot, error)
	ResolveBuildTrust(ctx context.Context) (hosttrust.Snapshot, error)
	WriteHostTrustBuildInputs(buildContext string, snapshot hosttrust.Snapshot) error
	ValidateRuntime(ctx context.Context, instance string) error
	ExecGuest(ctx context.Context, instance string, command []string, opts provider.ExecOptions) (provider.ExecResult, error)
	Transcript(path, instance, component string) (*logging.Transcript, error)
	PullDockerSource(ctx context.Context, options DockerSourcePullOptions) error
	LogInfo(message string, args ...any)
	LogWarn(message string, args ...any)
	ProgressTerminal() bool
	ProgressConsole() io.Writer
	RunnerName(prefix string, sequence int, now time.Time) string
	TranscriptComponent(path string) string
}

type Coordinator struct {
	Config      config.Config
	Provider    provider.Provider
	Lifecycle   provider.Lifecycle
	ProjectRoot string
	ConfigPath  string
	DryRun      bool
	environment Environment
}

func NewCoordinator(cfg config.Config, legacy provider.Provider, lifecycle provider.Lifecycle, projectRoot string, dryRun bool, environment Environment) *Coordinator {
	return &Coordinator{
		Config:      cfg,
		Provider:    legacy,
		Lifecycle:   lifecycle,
		ProjectRoot: projectRoot,
		DryRun:      dryRun,
		environment: environment,
	}
}

func (m *Coordinator) preflightStorage(operation string, peakBytes uint64) error {
	return m.environment.PreflightStorage(operation, peakBytes)
}

func (m *Coordinator) buildLogPath(name string) string {
	return m.environment.BuildLogPath(name)
}

func (m *Coordinator) releaseTranscript(path string) error {
	return m.environment.ReleaseTranscript(path)
}

func (m *Coordinator) infof(format string, args ...any) {
	m.environment.Infof(format, args...)
}

func (m *Coordinator) warnf(format string, args ...any) {
	m.environment.Warnf(format, args...)
}

func (m *Coordinator) HousekeepStorage(ctx context.Context) error {
	return m.cleanupSupersededCatalog(ctx)
}

func (m *Coordinator) runHostLogged(ctx context.Context, logPath, name string, args ...string) error {
	return m.environment.RunHostLogged(ctx, logPath, name, args...)
}

func (m *Coordinator) runHostBuildxLogged(ctx context.Context, logPath, name string, args ...string) error {
	return m.environment.RunHostBuildxLogged(ctx, logPath, name, args...)
}

func (m *Coordinator) runHost(ctx context.Context, name string, args ...string) error {
	return m.environment.RunHost(ctx, name, args...)
}

func (m *Coordinator) runHostOutput(ctx context.Context, name string, args ...string) (string, error) {
	return m.environment.RunHostOutput(ctx, name, args...)
}

func (m *Coordinator) runHostOutputTo(ctx context.Context, output io.Writer, name string, args ...string) error {
	return m.environment.RunHostOutputTo(ctx, output, name, args...)
}

func (m *Coordinator) runHostQuiet(ctx context.Context, name string, args ...string) error {
	return m.environment.RunHostQuiet(ctx, name, args...)
}

func (m *Coordinator) timeStartupStage(stage string, fn func() error) error {
	return m.environment.TimeStartupStage(stage, fn)
}

func (m *Coordinator) hostTrustEnabled() bool {
	return m.environment.HostTrustEnabled()
}

func (m *Coordinator) resolveHostTrust(ctx context.Context) (hosttrust.Snapshot, error) {
	return m.environment.ResolveHostTrust(ctx)
}

func (m *Coordinator) resolveBuildTrust(ctx context.Context) (hosttrust.Snapshot, error) {
	snapshot, err := m.environment.ResolveBuildTrust(ctx)
	if err != nil {
		return hosttrust.Snapshot{}, err
	}
	explicit, err := m.trustedCACertificates()
	if err != nil {
		return hosttrust.Snapshot{}, err
	}
	for _, certificate := range explicit {
		parsed, err := hosttrust.CertificatesFromBytes(certificate.PEM)
		if err != nil {
			return hosttrust.Snapshot{}, fmt.Errorf("canonicalize explicit build CA %s: %w", certificate.DestinationName, err)
		}
		snapshot.Certificates = append(snapshot.Certificates, parsed...)
	}
	return hosttrust.Canonicalize(snapshot)
}

func (m *Coordinator) writeHostTrustBuildInputs(buildContext string, snapshot hosttrust.Snapshot) error {
	return m.environment.WriteHostTrustBuildInputs(buildContext, snapshot)
}

func (m *Coordinator) validateTrustedCACertificates() error {
	_, err := m.trustedCACertificates()
	return err
}

func (m *Coordinator) validateRuntime(ctx context.Context, instance string) error {
	return m.environment.ValidateRuntime(ctx, instance)
}

func (m *Coordinator) execGuest(ctx context.Context, instance string, command []string, opts provider.ExecOptions) (provider.ExecResult, error) {
	return m.environment.ExecGuest(ctx, instance, command, opts)
}

func (m *Coordinator) transcript(path, instance, component string) (*logging.Transcript, error) {
	return m.environment.Transcript(path, instance, component)
}

func (m *Coordinator) pullDockerSource(ctx context.Context, options DockerSourcePullOptions) error {
	return m.environment.PullDockerSource(ctx, options)
}

func (m *Coordinator) runnerName(prefix string, sequence int, now time.Time) string {
	return m.environment.RunnerName(prefix, sequence, now)
}

func (m *Coordinator) transcriptComponent(path string) string {
	return m.environment.TranscriptComponent(path)
}
