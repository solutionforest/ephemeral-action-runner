package dockersandboxes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
	"github.com/solutionforest/ephemeral-action-runner/internal/provider/dockersandboxes/staging"
)

const (
	defaultOutputLimit        = 8 << 20
	diagnosticOutputLimit     = 256 << 10
	commandWaitDelay          = 5 * time.Second
	keepaliveStartupDelay     = 500 * time.Millisecond
	providerReadbackTimeout   = 30 * time.Second
	providerCleanupTimeout    = 2 * time.Minute
	providerCreateTimeout     = 10 * time.Minute
	daemonStatePollInterval   = 250 * time.Millisecond
	maximumRecoveryQuiescence = 5 * time.Minute
)

const sandboxContainerFailureSignature = "failed to run sandbox container"

const sandboxContainerFailureRemediation = "The shared Docker Sandboxes daemon may have inherited host SSH-agent forwarding. EPAR removes SSH-agent variables when its commands start a stopped daemon. In recoveryMode=exclusive-auto, the pool may make one bounded stop-wait-start recovery attempt for this create-stage signature; recoveryMode=observe never mutates the daemon. Coordinate with every process using that daemon before an interruption, then run `sbx daemon stop` followed by `env -u SSH_AUTH_SOCK -u SSH_AUTH_SOCK_GATEWAY -u SSH_AGENT_PID sbx daemon start --detach` and retry."

const directWorkspaceVerificationScript = `set -euo pipefail
if test -n "${SSH_AUTH_SOCK:-}" || test -n "${SSH_AUTH_SOCK_GATEWAY:-}" || test -n "${SSH_AGENT_PID:-}" || test -e /run/ssh-agent.sock || test -L /run/ssh-agent.sock; then
  echo "Docker Sandboxes exposed host SSH-agent forwarding; stop the daemon and restart it with SSH_AUTH_SOCK, SSH_AUTH_SOCK_GATEWAY, and SSH_AGENT_PID unset" >&2
  exit 1
fi
workspace="$(pwd -P)"
test -n "${workspace}"
source_options="$(findmnt -T "${workspace}" -n -o OPTIONS)"
case ",${source_options}," in
  *,rw,*) ;;
  *) echo "dedicated host staging workspace is not read-write" >&2; exit 1 ;;
esac
test ! -e .git
test -z "$(find . -mindepth 1 -maxdepth 1 -print -quit)"
test ! -e /run/sandbox/source
! pgrep -x git-daemon >/dev/null`

const runtimeVerificationScript = `set -euo pipefail
test -x /opt/epar/verify-template.sh
test -s /opt/epar/helpers.sha256
cd /opt/epar
sha256sum -c helpers.sha256 >/dev/null
/opt/epar/verify-template.sh >/dev/null
docker info --format '{{json .ServerVersion}}'`

type Provider struct {
	Binary string

	runCommand            runCommandFunc
	wait                  func(context.Context, time.Duration) error
	controlPlaneGate      controlPlaneCommandGate
	recoverySlotOnce      sync.Once
	recoverySlot          chan struct{}
	activationMu          sync.RWMutex
	admissionBlockReason  string
	activeMu              sync.RWMutex
	activeTemplate        provider.TemplateArtifact
	dryRun                bool
	architectureEmulation architectureEmulationEnabler
	architectureLogged    sync.Map
	logger                *slog.Logger
	relayMu               sync.Mutex
	relay                 *egressRelay
	relayTokens           map[string]string
	relayConnections      map[string]map[net.Conn]struct{}
	hostTrustRelayEnabled bool
	hostTrustRelayPort    int
}

type instanceReceipt struct {
	SchemaVersion   int    `json:"schemaVersion"`
	StagingPath     string `json:"stagingPath"`
	StagingIdentity string `json:"stagingIdentity"`
	Template        string `json:"template"`
	TemplateDigest  string `json:"templateDigest"`
}

// CachedTemplate is one image retained in the Docker Sandboxes template cache.
// CacheID is the provider's short cache identity, not a content digest.
type CachedTemplate struct {
	Reference string
	CacheID   string
	CreatedAt time.Time
	SizeBytes int64
}

// HostReadiness is the validated machine-readable summary returned by
// `sbx diagnose --output json`.
type HostReadiness struct {
	ChecksPassed  int
	ChecksWarned  int
	ChecksFailed  int
	ChecksSkipped int
}

type commandRequest struct {
	args            []string
	stdin           io.Reader
	environment     map[string]string
	stdout          io.Writer
	stderr          io.Writer
	sensitiveValues []string
	operation       string
	outputLimit     int
	// timeout bounds this provider CLI operation; zero preserves the caller's
	// lifetime for long-running guest commands and the detached keepalive.
	timeout time.Duration
	// preserveDescendantsOnSuccess is reserved for the exact detached daemon
	// start command. Transient commands retain kill-on-close containment.
	preserveDescendantsOnSuccess bool
}

type runCommandFunc func(ctx context.Context, request commandRequest) (provider.ExecResult, error)

func New(binary string) *Provider {
	return NewWithDryRun(binary, false)
}

func NewWithDryRun(binary string, dryRun bool) *Provider {
	return newWithArchitectureEmulation(binary, dryRun, qemuBinfmtEnabler{})
}

func NewWithArchitectureMode(binary string, dryRun bool, mode, platform string) *Provider {
	var enabler architectureEmulationEnabler
	switch mode {
	case architectureEmulationBestEffort:
		enabler = bestEffortArchitectureEnabler{platform: platform}
	case architectureEmulationRequired:
		enabler = qemuBinfmtEnabler{}
	case architectureEmulationNativeOnly:
		enabler = nativeArchitectureEnabler{platform: platform}
	}
	return newWithArchitectureEmulation(binary, dryRun, enabler)
}

func newWithArchitectureEmulation(binary string, dryRun bool, enabler architectureEmulationEnabler) *Provider {
	if binary == "" {
		binary = "sbx"
	}
	return &Provider{Binary: binary, wait: waitForContext, dryRun: dryRun, architectureEmulation: enabler, relayTokens: make(map[string]string), relayConnections: make(map[string]map[net.Conn]struct{})}
}

// ConfigureHostTrustRelay enables the Windows-host trust transport used by
// Docker Sandboxes overlay mode. It is configured once by the provider
// registry before lifecycle operations begin.
func (p *Provider) ConfigureHostTrustRelay(enabled bool, stableIdentity ...string) {
	p.hostTrustRelayEnabled = enabled
	p.hostTrustRelayPort = 0
	if enabled && len(stableIdentity) != 0 && stableIdentity[0] != "" {
		p.hostTrustRelayPort = stableRelayPort(stableIdentity[0])
	}
}

// SetLogger attaches the controller's structured logger before any concurrent
// lifecycle operations begin. Standalone provider consumers may omit it.
func (p *Provider) SetLogger(logger *slog.Logger) {
	p.logger = logger
}

// StartDaemon asks Docker Sandboxes to start its host daemon in the
// background. The command is intentionally exact so onboarding cannot invoke
// other daemon mutations through this path.
func (p *Provider) StartDaemon(ctx context.Context) error {
	_, err := p.run(ctx, commandRequest{
		args:                         []string{"daemon", "start", "--detach"},
		operation:                    "start docker sandboxes daemon",
		outputLimit:                  diagnosticOutputLimit,
		timeout:                      providerReadbackTimeout,
		preserveDescendantsOnSuccess: true,
	})
	return err
}

type daemonControlState string

const (
	daemonControlStateRunning daemonControlState = "running"
	daemonControlStateStopped daemonControlState = "stopped"
)

// RecoverControlPlane performs the provider-owned mutation for an exclusive
// Docker Sandboxes host. It never starts the daemon unless stopped state was
// observed both after the cold stop and again after the quiescence interval.
func (p *Provider) RecoverControlPlane(ctx context.Context, request provider.ControlPlaneRecoveryRequest) (err error) {
	defer func() {
		if err == nil {
			return
		}
		var failure *provider.ControlPlaneRecoveryFailure
		if !errors.As(err, &failure) {
			err = provider.NewControlPlaneRecoveryFailure("Docker Sandboxes control-plane recovery", err)
		}
	}()
	if request.Quiescence <= 0 || request.Quiescence > maximumRecoveryQuiescence {
		return fmt.Errorf("Docker Sandboxes recovery quiescence must be greater than zero and no more than %s", maximumRecoveryQuiescence)
	}
	if p.dryRun {
		return fmt.Errorf("Docker Sandboxes control-plane recovery is unavailable in dry-run mode")
	}
	releaseRecoverySlot, err := p.acquireRecoverySlot(ctx)
	if err != nil {
		return err
	}
	defer releaseRecoverySlot()
	releaseControlPlaneGate, err := p.controlPlaneGate.beginRecovery(ctx)
	if err != nil {
		return err
	}
	defer releaseControlPlaneGate()
	var releaseHostLock func()
	if p.runCommand == nil {
		releaseHostLock, err = provider.TryAcquireControlPlaneRecoveryLock()
		if err != nil {
			return err
		}
		defer releaseHostLock()
	}
	recoveryCtx := provider.WithControlPlaneLock(withControlPlaneGate(ctx))

	_, stopErr := p.run(recoveryCtx, commandRequest{
		args:        []string{"daemon", "stop"},
		operation:   "stop docker sandboxes daemon for control-plane recovery",
		outputLimit: diagnosticOutputLimit,
		timeout:     providerCleanupTimeout,
	})
	if stateErr := p.waitForDaemonState(recoveryCtx, daemonControlStateStopped, providerCleanupTimeout); stateErr != nil {
		if stopErr != nil {
			return errors.Join(stopErr, fmt.Errorf("confirm Docker Sandboxes daemon stopped: %w", stateErr))
		}
		return fmt.Errorf("confirm Docker Sandboxes daemon stopped: %w", stateErr)
	}
	if stopErr != nil && p.logger != nil {
		p.logger.Warn("Docker Sandboxes daemon stop returned an error but authoritative status confirmed stopped; continuing exclusive recovery", "provider", "docker-sandboxes")
	}

	if err := p.wait(recoveryCtx, request.Quiescence); err != nil {
		return fmt.Errorf("wait for Docker Sandboxes daemon quiescence: %w", err)
	}
	state, err := p.readDaemonControlState(recoveryCtx)
	if err != nil {
		return fmt.Errorf("refusing Docker Sandboxes daemon start because stopped state is unknown after quiescence: %w", err)
	}
	if state != daemonControlStateStopped {
		return fmt.Errorf("refusing Docker Sandboxes daemon start after quiescence: state is %q, want %q", state, daemonControlStateStopped)
	}

	startErr := p.StartDaemon(recoveryCtx)
	stateErr := p.waitForDaemonState(recoveryCtx, daemonControlStateRunning, providerReadbackTimeout)
	if stateErr != nil {
		if startErr != nil {
			return errors.Join(startErr, fmt.Errorf("confirm Docker Sandboxes daemon running: %w", stateErr))
		}
		return fmt.Errorf("confirm Docker Sandboxes daemon running: %w", stateErr)
	}
	if startErr != nil && p.logger != nil {
		p.logger.Warn("Docker Sandboxes daemon detached start returned an error but authoritative status confirmed running; recovery succeeded", "provider", "docker-sandboxes")
	}
	return nil
}

func (p *Provider) waitForDaemonState(ctx context.Context, expected daemonControlState, timeout time.Duration) error {
	confirmationCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		state, err := p.readDaemonControlState(confirmationCtx)
		if err != nil {
			return err
		}
		if state == expected {
			return nil
		}
		if err := p.wait(confirmationCtx, daemonStatePollInterval); err != nil {
			return fmt.Errorf("Docker Sandboxes daemon remained %q while waiting for %q: %w", state, expected, err)
		}
	}
}

func (p *Provider) readDaemonControlState(ctx context.Context) (daemonControlState, error) {
	result, err := p.run(ctx, commandRequest{
		args:        []string{"daemon", "status", "--json"},
		operation:   "read docker sandboxes daemon control state",
		outputLimit: diagnosticOutputLimit,
		timeout:     providerReadbackTimeout,
	})
	if err != nil {
		return "", err
	}
	return parseDaemonControlState([]byte(result.Stdout))
}

func parseDaemonControlState(data []byte) (daemonControlState, error) {
	state, _, err := parseDaemonStatus(data)
	if err != nil {
		return "", err
	}
	if state != strings.TrimSpace(state) {
		return "", fmt.Errorf("docker sandboxes daemon status returned a non-canonical state")
	}
	switch normalized := daemonControlState(strings.ToLower(state)); normalized {
	case daemonControlStateRunning, daemonControlStateStopped:
		return normalized, nil
	default:
		return "", fmt.Errorf("docker sandboxes daemon status returned unsupported state %q", state)
	}
}

func waitForContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// VerifyHostReadiness requires machine-readable Docker Sandboxes diagnostics
// to contain at least one passing check and no failed checks. Warnings and
// skipped checks do not make an otherwise healthy installation unavailable.
func (p *Provider) VerifyHostReadiness(ctx context.Context) (HostReadiness, error) {
	readiness, err := p.readHostReadiness(ctx)
	if err != nil {
		return HostReadiness{}, fmt.Errorf("%w; run 'sbx diagnose --output json' and review its output", err)
	}
	if readiness.ChecksFailed != 0 {
		return HostReadiness{}, fmt.Errorf("docker sandboxes diagnostics reported %d failed check(s); run 'sbx diagnose --output json' and review the hints for each failed check", readiness.ChecksFailed)
	}
	if readiness.ChecksPassed == 0 {
		return HostReadiness{}, fmt.Errorf("docker sandboxes diagnostics reported no passing checks; run 'sbx diagnose --output json' and review its check details")
	}
	return readiness, nil
}

func (p *Provider) Create(ctx context.Context, request provider.CreateRequest) (provider.Instance, error) {
	if p.dryRun {
		return provider.Instance{}, fmt.Errorf("docker-sandboxes does not support dry-run instance creation because exact sandbox and template-cache readback is required")
	}
	p.activationMu.RLock()
	defer p.activationMu.RUnlock()
	if p.admissionBlockReason != "" {
		return provider.Instance{}, fmt.Errorf("Docker Sandboxes admission is blocked: %s", p.admissionBlockReason)
	}
	p.activeMu.RLock()
	active := p.activeTemplate
	p.activeMu.RUnlock()
	if request.Template == "" && request.TemplateDigest == "" {
		request.Template = active.Reference
		request.TemplateDigest = active.Digest
		if request.RootDisk == "auto" {
			request.RootDisk = active.RootDisk
		}
	}
	if err := validateCreateRequest(request); err != nil {
		return provider.Instance{}, err
	}
	if err := p.VerifyAdmission(ctx); err != nil {
		return provider.Instance{}, err
	}
	cacheID := strings.TrimPrefix(request.TemplateDigest, "sha256:")[:12]
	if request.Template == active.Reference && request.TemplateDigest == active.Digest && validTemplateCacheID(active.CacheID) {
		cacheID = active.CacheID
	}
	if err := p.verifyImportedTemplate(ctx, request.Template, cacheID); err != nil {
		return provider.Instance{}, err
	}
	items, err := p.inventoryVerified(ctx)
	if err != nil {
		return provider.Instance{}, err
	}
	for _, item := range items {
		if item.Instance.Name == request.Name {
			return provider.Instance{}, fmt.Errorf("docker sandbox name is already allocated")
		}
	}
	var (
		stagingRoot  *staging.Staging
		ownedStaging staging.OwnedDirectory
	)
	if p.runCommand == nil {
		var openErr error
		stagingRoot, openErr = staging.Open(filepath.Dir(request.StagingPath))
		if openErr != nil {
			return provider.Instance{}, openErr
		}
		if filepath.Clean(request.StagingPath) != filepath.Join(stagingRoot.Root(), request.Name) {
			return provider.Instance{}, fmt.Errorf("Docker Sandboxes staging path must be the exact provider-owned path for %q", request.Name)
		}
		ownedStaging, err = stagingRoot.CreateOwned(request.Name)
		if err != nil {
			return provider.Instance{}, err
		}
	} else {
		ownedStaging = staging.OwnedDirectory{Path: request.StagingPath, Identity: "test-staging-identity"}
	}

	args := []string{
		"create",
		"--name", request.Name,
		"--cpus", strconv.Itoa(request.CPUs),
		"--memory", request.Memory,
		"--template", request.Template,
	}
	args = append(args, "shell", request.StagingPath)
	environment := make(map[string]string, 2)
	if request.RootDisk != "" {
		environment["DOCKER_SANDBOXES_ROOT_SIZE"] = request.RootDisk
	}
	if request.DockerDisk != "" {
		environment["DOCKER_SANDBOXES_DOCKER_SIZE"] = request.DockerDisk
	}
	result, createErr := p.run(ctx, commandRequest{args: args, environment: environment, operation: "create docker sandbox", timeout: providerCreateTimeout})
	if createErr != nil {
		if stagingRoot != nil {
			if cleanupErr := stagingRoot.RemoveEmptyOwned(request.Name, ownedStaging.Identity); cleanupErr != nil {
				createErr = errors.Join(createErr, fmt.Errorf("remove failed Docker Sandboxes staging workspace: %w", cleanupErr))
			}
		}
		failure := withSandboxContainerFailureRemediation(createErr)
		if hasSandboxCreateAdmissionSignature(result.Stderr) {
			return provider.Instance{}, provider.NewControlPlaneAdmissionFailure("create Docker Sandboxes instance", failure)
		}
		return provider.Instance{}, failure
	}
	items, err = p.inventoryVerified(ctx)
	if err != nil {
		return provider.Instance{}, fmt.Errorf("docker sandbox was created but identity readback failed: %w", err)
	}
	for _, item := range items {
		if item.Instance.Name == request.Name {
			if item.Instance.ProviderID == "" {
				return provider.Instance{}, fmt.Errorf("docker sandbox inventory omitted the stable provider id")
			}
			if item.Source != "shell" || !containsExactWorkspace(item.Workspaces, request.StagingPath) {
				return provider.Instance{}, fmt.Errorf("docker sandbox inventory did not bind the exact shell workspace")
			}
			receipt, encodeErr := json.Marshal(instanceReceipt{
				SchemaVersion:   1,
				StagingPath:     ownedStaging.Path,
				StagingIdentity: ownedStaging.Identity,
				Template:        request.Template,
				TemplateDigest:  request.TemplateDigest,
			})
			if encodeErr != nil {
				return provider.Instance{}, encodeErr
			}
			instance := item.Instance
			instance.ReceiptVersion = "v1"
			instance.Receipt = receipt
			if err := p.verifyNoPublishedPorts(ctx, item.Instance); err != nil {
				return instance, err
			}
			if err := p.verifyInspection(ctx, item.Instance, &request, cacheID); err != nil {
				return instance, err
			}
			if err := p.verifyDirectWorkspace(ctx, item.Instance); err != nil {
				return instance, err
			}
			if p.architectureEmulation == nil {
				return instance, fmt.Errorf("Docker Sandboxes architecture emulation enabler is unavailable")
			}
			emulation, err := p.architectureEmulation.Enable(ctx, p, item.Instance)
			if err != nil {
				return instance, err
			}
			p.logArchitectureCapability(item.Instance.Name, emulation)
			return instance, nil
		}
	}
	return provider.Instance{}, fmt.Errorf("docker sandbox was not present in inventory after create")
}

func withSandboxContainerFailureRemediation(err error) error {
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), sandboxContainerFailureSignature) {
		return err
	}
	return fmt.Errorf("%w; %s", err, sandboxContainerFailureRemediation)
}

// hasSandboxCreateAdmissionSignature deliberately inspects only the stderr
// captured from the immediate `sbx create` operation. Matching a wrapped
// controller error would turn unrelated failures into daemon-restart triggers.
func hasSandboxCreateAdmissionSignature(stderr string) bool {
	return strings.Contains(strings.ToLower(stderr), sandboxContainerFailureSignature)
}

func (p *Provider) logArchitectureCapability(instanceName string, emulation architectureEmulationResult) {
	if p.logger == nil {
		return
	}
	if _, alreadyLogged := p.architectureLogged.LoadOrStore(instanceName, struct{}{}); alreadyLogged {
		return
	}
	if emulation.Mode == architectureEmulationNativeOnly {
		message := fmt.Sprintf("Docker Sandboxes native-only architecture verified: platform=%s; foreign-architecture containers are unsupported", emulation.Platform)
		p.logger.Warn(message, "provider", "docker-sandboxes", "instance", instanceName, "architectureEmulation", emulation.Mode, "backend", emulation.Backend, "platform", emulation.Platform, "registeredHandlers", emulation.HandlerCount)
		return
	}
	if emulation.Mode == architectureEmulationBestEffort && emulation.Backend == "native" {
		message := fmt.Sprintf("Docker Sandboxes QEMU/binfmt unavailable; continuing with verified native platform=%s; foreign-architecture containers may fail", emulation.Platform)
		p.logger.Warn(message, "provider", "docker-sandboxes", "instance", instanceName, "architectureEmulation", emulation.Mode, "backend", emulation.Backend, "platform", emulation.Platform, "registeredHandlers", emulation.HandlerCount, "qemuError", emulation.Warning)
		return
	}
	message := fmt.Sprintf("Docker Sandboxes architecture emulation enabled: backend=%s registeredHandlers=%d", emulation.Backend, emulation.HandlerCount)
	p.logger.Info(message, "provider", "docker-sandboxes", "instance", instanceName, "architectureEmulation", emulation.Mode, "backend", emulation.Backend, "registeredHandlers", emulation.HandlerCount)
}

// ImportTemplate performs the one exact provider mutation required after the
// shared image coordinator has built and verified a local template archive.
func (p *Provider) ImportTemplate(ctx context.Context, archivePath string) error {
	if archivePath == "" || strings.ContainsRune(archivePath, 0) {
		return fmt.Errorf("Docker Sandboxes template archive path is required")
	}
	info, err := os.Lstat(archivePath)
	if err != nil {
		return fmt.Errorf("inspect Docker Sandboxes template archive: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("Docker Sandboxes template archive must be a regular file")
	}
	if _, err := p.run(ctx, commandRequest{
		args:        []string{"template", "load", archivePath},
		operation:   "load exact Docker Sandboxes runner template",
		outputLimit: diagnosticOutputLimit,
		timeout:     providerCreateTimeout,
	}); err != nil {
		return err
	}
	return nil
}

func (p *Provider) VerifyImportedTemplate(ctx context.Context, artifact provider.TemplateArtifact) error {
	if artifact.Reference == "" || !validFullTemplateDigest(artifact.Digest) {
		return fmt.Errorf("Docker Sandboxes template reference and digest are required")
	}
	if !validTemplateCacheID(artifact.CacheID) {
		return fmt.Errorf("Docker Sandboxes template cache ID must be exactly 12 lowercase hexadecimal characters")
	}
	return p.verifyImportedTemplate(ctx, artifact.Reference, artifact.CacheID)
}

func (p *Provider) ActivateTemplate(artifact provider.TemplateArtifact) error {
	if artifact.Reference == "" || !validFullTemplateDigest(artifact.Digest) {
		return fmt.Errorf("cannot activate an invalid Docker Sandboxes template identity")
	}
	if !validTemplateCacheID(artifact.CacheID) {
		return fmt.Errorf("cannot activate a Docker Sandboxes template without an exact cache ID")
	}
	if artifact.Platform != "linux/amd64" && artifact.Platform != "linux/arm64" {
		return fmt.Errorf("cannot activate a Docker Sandboxes template for unsupported platform %q", artifact.Platform)
	}
	if !sizePattern.MatchString(artifact.RootDisk) {
		return fmt.Errorf("cannot activate a Docker Sandboxes template without a resolved root-disk size")
	}
	p.activeMu.Lock()
	p.activeTemplate = artifact
	p.activeMu.Unlock()
	return nil
}

// WithTemplateActivation blocks new Create calls while the shared image
// coordinator activates and durably commits one exact template generation.
// Existing instances are unaffected; scheduled updates already drain them in
// the common pool before entering this transaction.
func (p *Provider) WithTemplateActivation(operation func() error) error {
	if operation == nil {
		return fmt.Errorf("Docker Sandboxes template activation operation is required")
	}
	p.activationMu.Lock()
	defer p.activationMu.Unlock()
	return operation()
}

func (p *Provider) SetTemplateAdmissionBlock(reason string) {
	p.activationMu.Lock()
	defer p.activationMu.Unlock()
	p.admissionBlockReason = strings.TrimSpace(reason)
}

func (p *Provider) ClearTemplateAdmissionBlock() {
	p.activationMu.Lock()
	defer p.activationMu.Unlock()
	p.admissionBlockReason = ""
}

func (p *Provider) TemplateAdmissionBlock() (string, bool) {
	p.activationMu.RLock()
	defer p.activationMu.RUnlock()
	return p.admissionBlockReason, p.admissionBlockReason != ""
}

// ActiveTemplate returns the exact in-process generation used when Create
// requests omit an explicit template identity.
func (p *Provider) ActiveTemplate() (provider.TemplateArtifact, bool) {
	p.activeMu.RLock()
	defer p.activeMu.RUnlock()
	return p.activeTemplate, p.activeTemplate.Reference != ""
}

// ClearActiveTemplate removes only the expected uncommitted generation. It is
// used when first activation fails and there is no previous receipt to restore.
func (p *Provider) ClearActiveTemplate(expected provider.TemplateArtifact) error {
	p.activeMu.Lock()
	defer p.activeMu.Unlock()
	if p.activeTemplate != expected {
		return fmt.Errorf("cannot clear Docker Sandboxes template because the active identity changed")
	}
	p.activeTemplate = provider.TemplateArtifact{}
	return nil
}

// RemoveTemplate removes one exact imported template cache identity. The
// Docker-managed shell-docker base template is never an EPAR cleanup target.
func (p *Provider) RemoveTemplate(ctx context.Context, artifact provider.TemplateArtifact) error {
	if artifact.Reference == "docker.io/docker/sandbox-templates:shell-docker" || artifact.Reference == "docker/sandbox-templates:shell-docker" {
		return fmt.Errorf("refusing to remove the Docker Sandboxes shell-docker base template")
	}
	if artifact.CacheID == "" || len(artifact.CacheID) != 12 {
		return fmt.Errorf("Docker Sandboxes template cleanup requires an exact 12-character cache ID")
	}
	instances, err := p.Inventory(ctx)
	if err != nil {
		return fmt.Errorf("verify active Docker Sandboxes before template cleanup: %w", err)
	}
	if len(instances) != 0 {
		return fmt.Errorf("refusing template cleanup while %d Docker Sandbox instance(s) exist", len(instances))
	}
	templates, err := p.CachedTemplates(ctx)
	if err != nil {
		return err
	}
	found := false
	for _, item := range templates {
		if item.CacheID != artifact.CacheID {
			continue
		}
		if item.Reference != artifact.Reference {
			return fmt.Errorf("Docker Sandboxes template cache identity %s now belongs to %s, not %s", artifact.CacheID, item.Reference, artifact.Reference)
		}
		found = true
		break
	}
	if !found {
		return nil
	}
	if _, err := p.run(ctx, commandRequest{
		args:        []string{"template", "rm", artifact.CacheID},
		operation:   "remove exact Docker Sandboxes runner template",
		outputLimit: diagnosticOutputLimit,
		timeout:     providerCleanupTimeout,
	}); err != nil {
		return err
	}
	templates, err = p.CachedTemplates(ctx)
	if err != nil {
		return err
	}
	for _, item := range templates {
		if item.CacheID == artifact.CacheID {
			return fmt.Errorf("Docker Sandboxes template %s still exists after exact removal", artifact.CacheID)
		}
	}
	return nil
}

func (p *Provider) ObserveTemplate(ctx context.Context, artifact provider.TemplateArtifact) (bool, error) {
	if artifact.Reference == "" || artifact.CacheID == "" || len(artifact.CacheID) != 12 {
		return false, fmt.Errorf("Docker Sandboxes template observation requires an exact reference and 12-character cache ID")
	}
	templates, err := p.CachedTemplates(ctx)
	if err != nil {
		return false, err
	}
	for _, item := range templates {
		if item.CacheID != artifact.CacheID {
			continue
		}
		if item.Reference != artifact.Reference {
			return false, fmt.Errorf("Docker Sandboxes template cache identity %s now belongs to %s, not %s", artifact.CacheID, item.Reference, artifact.Reference)
		}
		return true, nil
	}
	return false, nil
}

func (p *Provider) ResolveTemplateCacheID(ctx context.Context, reference string) (string, bool, error) {
	repository, tag, err := splitTemplateReference(reference)
	if err != nil {
		return "", false, err
	}
	templates, err := p.CachedTemplates(ctx)
	if err != nil {
		return "", false, err
	}
	cacheID := ""
	for _, item := range templates {
		itemRepository, itemTag, splitErr := splitTemplateReference(item.Reference)
		if splitErr != nil {
			return "", false, splitErr
		}
		if itemRepository != repository || itemTag != tag {
			continue
		}
		if cacheID != "" && cacheID != item.CacheID {
			return "", false, fmt.Errorf("Docker Sandboxes template reference %s has multiple cache identities", reference)
		}
		cacheID = item.CacheID
	}
	if cacheID == "" {
		return "", false, nil
	}
	return cacheID, true, nil
}

// VerifyAdmission checks only host-level Docker Sandboxes readiness. Capability
// checks that are specific to an EPAR-managed sandbox are performed by
// VerifyInstanceAdmission after that exact sandbox has been created.
func (p *Provider) VerifyAdmission(ctx context.Context) error {
	_, err := p.VerifyHostReadiness(ctx)
	return err
}

func (p *Provider) VerifyInstanceAdmission(ctx context.Context, instance provider.Instance) error {
	present, err := p.assertIdentity(ctx, instance)
	if err != nil {
		return err
	}
	if !present {
		return fmt.Errorf("docker sandbox is missing")
	}
	if err := p.verifyNoPublishedPorts(ctx, instance); err != nil {
		return err
	}
	if err := p.verifyInspection(ctx, instance, nil, ""); err != nil {
		return err
	}
	if p.architectureEmulation == nil {
		return fmt.Errorf("Docker Sandboxes architecture emulation enabler is unavailable")
	}
	emulation, err := p.architectureEmulation.Enable(ctx, p, instance)
	if err != nil {
		return err
	}
	p.logArchitectureCapability(instance.Name, emulation)
	return nil
}

func (p *Provider) verifyInspection(ctx context.Context, instance provider.Instance, expected *provider.CreateRequest, expectedCacheID string) error {
	result, err := p.run(ctx, commandRequest{
		args:        []string{"inspect", "--json", instance.Name},
		operation:   "verify docker sandbox attached capabilities",
		outputLimit: diagnosticOutputLimit,
		timeout:     providerReadbackTimeout,
	})
	if err != nil {
		return err
	}
	var inspection map[string]json.RawMessage
	if err := decodeStrictJSON([]byte(result.Stdout), &inspection); err != nil {
		return fmt.Errorf("docker sandbox inspection returned an unsupported JSON schema")
	}
	if stringValue(inspection["name"]) != instance.Name || stringValue(inspection["agent"]) != "shell" || strings.TrimSpace(stringValue(inspection["daemon_version"])) == "" {
		return fmt.Errorf("docker sandbox inspection did not match the exact shell runtime")
	}
	if expected != nil {
		inspectionDigest := stringValue(inspection["image_digest"])
		runtimeCacheID := ""
		if validFullTemplateDigest(inspectionDigest) {
			runtimeCacheID = strings.TrimPrefix(inspectionDigest, "sha256:")[:12]
		}
		if stringValue(inspection["image"]) != expected.Template || runtimeCacheID != expectedCacheID || stringValue(inspection["workspace"]) != expected.StagingPath {
			return fmt.Errorf("docker sandbox inspection did not bind the exact template reference, cache identity, and staging path")
		}
	}
	for _, field := range []string{"kits", "published_ports", "ports", "auth", "auth_mode", "docker_auth"} {
		value, ok := inspection[field]
		if field == "kits" && !ok {
			return fmt.Errorf("docker sandbox inspection omitted required attached-capability field %q", field)
		}
		if ok && !emptyJSONValue(value) {
			return fmt.Errorf("docker sandbox inspection reported forbidden attached capability %q", field)
		}
	}
	return nil
}

func stringValue(raw json.RawMessage) string {
	var value string
	if len(raw) == 0 || json.Unmarshal(raw, &value) != nil {
		return ""
	}
	return value
}

func emptyJSONValue(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return true
	}
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return false
	}
	switch typed := value.(type) {
	case nil:
		return true
	case bool:
		return !typed
	case string:
		return typed == ""
	case float64:
		return typed == 0
	case []any:
		return len(typed) == 0
	case map[string]any:
		return len(typed) == 0
	default:
		return false
	}
}

func (p *Provider) verifyNoPublishedPorts(ctx context.Context, instance provider.Instance) error {
	result, err := p.run(ctx, commandRequest{
		args:        []string{"ports", instance.Name, "--json"},
		operation:   "verify docker sandbox has no published ports",
		outputLimit: diagnosticOutputLimit,
		timeout:     providerReadbackTimeout,
	})
	if err != nil {
		return err
	}
	var ports []map[string]json.RawMessage
	if err := decodeStrictJSON([]byte(result.Stdout), &ports); err != nil || ports == nil {
		return fmt.Errorf("docker sandbox published-port inventory returned an unsupported JSON schema")
	}
	if len(ports) != 0 {
		return fmt.Errorf("docker sandbox reported a forbidden published port")
	}
	return nil
}

func (p *Provider) verifyDirectWorkspace(ctx context.Context, instance provider.Instance) error {
	result, err := p.run(ctx, commandRequest{
		args:      []string{"exec", "-i", instance.Name, "--", "bash", "-lc", directWorkspaceVerificationScript},
		stdin:     strings.NewReader(""),
		operation: "verify dedicated docker sandbox staging workspace",
		timeout:   providerReadbackTimeout,
	})
	if err != nil {
		return err
	}
	if strings.TrimSpace(result.Stdout) != "" {
		return fmt.Errorf("dedicated docker sandbox staging verification returned unexpected output")
	}
	return nil
}

func containsExactWorkspace(workspaces []string, expected string) bool {
	for _, workspace := range workspaces {
		if workspace == expected {
			return true
		}
	}
	return false
}

func (p *Provider) Start(ctx context.Context, instance provider.Instance, opts provider.StartOptions) (*provider.RunningProcess, error) {
	present, err := p.assertIdentity(ctx, instance)
	if err != nil || !present {
		if err == nil {
			return nil, fmt.Errorf("docker sandbox is missing")
		}
		return nil, err
	}
	request := commandRequest{
		args:      []string{"exec", "-i", instance.Name, "--", "/bin/sleep", "infinity"},
		stdin:     strings.NewReader(""),
		stdout:    opts.Stdout,
		stderr:    opts.Stderr,
		operation: "start docker sandbox with a managed keepalive",
	}
	if p.runCommand != nil {
		if _, err := p.run(ctx, request); err != nil {
			return nil, err
		}
		return &provider.RunningProcess{Name: instance.Name}, nil
	}
	return p.startKeepalive(ctx, instance.Name, request)
}

func (p *Provider) startKeepalive(ctx context.Context, name string, request commandRequest) (*provider.RunningProcess, error) {
	if err := validateCommandRequest(request); err != nil {
		return nil, err
	}
	releaseGate, err := p.controlPlaneGate.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer releaseGate()
	releaseHostLock, err := provider.AcquireControlPlaneCommandLock(ctx)
	if err != nil {
		return nil, err
	}
	defer releaseHostLock()
	// The caller's context bounds startup only. The returned keepalive owns the
	// sandbox lifetime and must survive the provisioning-attempt context that the
	// pool cancels as soon as Start returns.
	command := exec.Command(p.Binary, request.args...)
	isolateKeepaliveProcess(command)
	command.WaitDelay = commandWaitDelay
	command.Stdin = request.stdin
	command.Env = childEnvironment(request.environment)
	stdout := &boundedBuffer{limit: defaultOutputLimit}
	stderr := &boundedBuffer{limit: defaultOutputLimit}
	command.Stdout = captureWriter(stdout, request.stdout)
	command.Stderr = captureWriter(stderr, request.stderr)
	if err := command.Start(); err != nil {
		return nil, fmt.Errorf("%s failed: %w", request.operation, err)
	}
	cleanup, attachErr := attachManagedProcess(command, false)
	if attachErr != nil {
		killErr := killManagedProcess(command)
		waitErr := waitForManagedCommandExit(command, commandWaitDelay)
		return nil, fmt.Errorf("%s failed to establish process containment: %w", request.operation, errors.Join(attachErr, killErr, waitErr))
	}
	finished := make(chan error, 1)
	go func() {
		err := command.Wait()
		cleanup()
		finished <- err
	}()
	timer := time.NewTimer(keepaliveStartupDelay)
	defer timer.Stop()
	select {
	case err := <-finished:
		detail := strings.TrimSpace(stderr.String())
		if err == nil {
			err = fmt.Errorf("keepalive command exited before startup completed")
		}
		if detail != "" {
			return nil, fmt.Errorf("%s failed: %w: %s", request.operation, err, detail)
		}
		return nil, fmt.Errorf("%s failed: %w", request.operation, err)
	case <-ctx.Done():
		killErr := killManagedProcess(command)
		waitErr := waitForManagedProcessExit(finished, commandWaitDelay)
		if waitErr != nil {
			if killErr != nil && !errors.Is(killErr, os.ErrProcessDone) {
				return nil, errors.Join(ctx.Err(), fmt.Errorf("stop keepalive after canceled startup: %w", killErr), waitErr)
			}
			return nil, errors.Join(ctx.Err(), waitErr)
		}
		return nil, ctx.Err()
	case <-timer.C:
		return &provider.RunningProcess{Name: name, PID: command.Process.Pid}, nil
	}
}

func waitForManagedCommandExit(command *exec.Cmd, timeout time.Duration) error {
	finished := make(chan error, 1)
	go func() { finished <- command.Wait() }()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-finished:
		return err
	case <-timer.C:
		_ = command.Process.Kill()
		select {
		case err := <-finished:
			return errors.Join(fmt.Errorf("managed process did not exit within %s", timeout), err)
		case <-time.After(timeout):
			return fmt.Errorf("managed process did not exit after forced termination")
		}
	}
}

func waitForManagedProcessExit(finished <-chan error, timeout time.Duration) error {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-finished:
		return nil
	case <-timer.C:
		return fmt.Errorf("managed process did not exit within %s", timeout)
	}
}

func (p *Provider) VerifyRuntime(ctx context.Context, instance provider.Instance) (provider.RuntimeInfo, error) {
	present, err := p.assertIdentity(ctx, instance)
	if err != nil {
		return provider.RuntimeInfo{}, err
	}
	if !present {
		return provider.RuntimeInfo{}, fmt.Errorf("docker sandbox is missing")
	}
	result, err := p.run(ctx, commandRequest{
		args:      []string{"exec", "-i", instance.Name, "--", "bash", "-lc", runtimeVerificationScript},
		stdin:     strings.NewReader(""),
		operation: "verify docker sandbox runtime",
		timeout:   providerReadbackTimeout,
	})
	if err != nil {
		return provider.RuntimeInfo{}, err
	}
	var version string
	if err := decodeStrictJSON([]byte(strings.TrimSpace(result.Stdout)), &version); err != nil || strings.TrimSpace(version) == "" {
		return provider.RuntimeInfo{}, fmt.Errorf("docker sandbox runtime returned an unsupported verification schema")
	}
	return provider.RuntimeInfo{Ready: true, Runtime: "docker", Version: version}, nil
}

func (*Provider) Address(context.Context, provider.Instance, int) (string, bool, error) {
	return "", false, nil
}

func (p *Provider) Exec(ctx context.Context, instance provider.Instance, command []string, opts provider.ExecOptions) (provider.ExecResult, error) {
	if err := validateGuestCommand(command, opts); err != nil {
		return provider.ExecResult{}, err
	}
	present, err := p.assertIdentity(ctx, instance)
	if err != nil {
		return provider.ExecResult{}, err
	}
	if !present {
		return provider.ExecResult{}, fmt.Errorf("docker sandbox is missing")
	}
	args := make([]string, 0, len(command)+5)
	args = append(args, "exec", "-i", instance.Name, "--")
	args = append(args, command...)
	return p.run(ctx, commandRequest{
		args:            args,
		stdin:           execOptionsReader(opts),
		stdout:          opts.Stdout,
		stderr:          opts.Stderr,
		sensitiveValues: opts.SensitiveValues,
		operation:       "execute in docker sandbox",
	})
}

func execOptionsReader(opts provider.ExecOptions) io.Reader {
	if opts.StdinReader != nil {
		return opts.StdinReader
	}
	return strings.NewReader(opts.Stdin)
}

func (p *Provider) Diagnostics(ctx context.Context, instance provider.Instance) (provider.Diagnostics, error) {
	present, err := p.assertIdentity(ctx, instance)
	if err != nil {
		return provider.Diagnostics{}, err
	}
	if !present {
		return provider.Diagnostics{}, fmt.Errorf("docker sandbox is missing")
	}
	statusResult, err := p.run(ctx, commandRequest{
		args:        []string{"daemon", "status", "--json"},
		operation:   "read docker sandbox daemon status",
		outputLimit: diagnosticOutputLimit,
		timeout:     providerReadbackTimeout,
	})
	if err != nil {
		return provider.Diagnostics{}, err
	}
	daemonState, daemonHealthy, err := parseDaemonStatus([]byte(statusResult.Stdout))
	if err != nil {
		return provider.Diagnostics{}, err
	}
	readiness, err := p.readHostReadiness(ctx)
	if err != nil {
		return provider.Diagnostics{}, err
	}
	return provider.Diagnostics{
		Healthy:       daemonHealthy && readiness.ChecksPassed > 0 && readiness.ChecksFailed == 0,
		DaemonState:   daemonState,
		ChecksPassed:  readiness.ChecksPassed,
		ChecksWarned:  readiness.ChecksWarned,
		ChecksFailed:  readiness.ChecksFailed,
		ChecksSkipped: readiness.ChecksSkipped,
	}, nil
}

func (p *Provider) readHostReadiness(ctx context.Context) (HostReadiness, error) {
	diagnoseResult, err := p.run(ctx, commandRequest{
		args:        []string{"diagnose", "--output", "json"},
		operation:   "diagnose docker sandboxes",
		outputLimit: diagnosticOutputLimit,
		timeout:     providerReadbackTimeout,
	})
	if err != nil {
		return HostReadiness{}, err
	}
	passed, warned, failed, skipped, err := parseDiagnose([]byte(diagnoseResult.Stdout))
	if err != nil {
		return HostReadiness{}, err
	}
	return HostReadiness{
		ChecksPassed:  passed,
		ChecksWarned:  warned,
		ChecksFailed:  failed,
		ChecksSkipped: skipped,
	}, nil
}

func (p *Provider) Stop(ctx context.Context, instance provider.Instance) error {
	if err := validateInstance(instance, true); err != nil {
		return err
	}
	defer p.releaseRelayToken(instance.Name)
	present, err := p.assertIdentity(ctx, instance)
	if err != nil || !present {
		return err
	}
	result, err := p.run(ctx, commandRequest{args: []string{"stop", instance.Name}, operation: "stop docker sandbox", timeout: providerCleanupTimeout})
	if err != nil && isMissingSandbox(result.Stdout+"\n"+result.Stderr+"\n"+err.Error()) {
		return nil
	}
	return err
}

func (p *Provider) Delete(ctx context.Context, instance provider.Instance) error {
	if err := validateInstance(instance, true); err != nil {
		return err
	}
	defer p.releaseRelayToken(instance.Name)
	present, err := p.assertIdentity(ctx, instance)
	if err != nil || !present {
		return err
	}
	var receipt instanceReceipt
	if p.runCommand == nil {
		switch instance.ReceiptVersion {
		case "v1":
			if json.Unmarshal(instance.Receipt, &receipt) != nil || receipt.SchemaVersion != 1 || receipt.StagingPath == "" || receipt.StagingIdentity == "" {
				return fmt.Errorf("refusing Docker Sandbox deletion without an exact staging ownership receipt")
			}
		case "v2":
			var parseErr error
			receipt, parseErr = parseExperimentalInstanceReceipt(instance.Receipt)
			if parseErr != nil {
				return fmt.Errorf("refusing Docker Sandbox deletion without an exact staging ownership receipt: %w", parseErr)
			}
		default:
			return fmt.Errorf("refusing Docker Sandbox deletion without an exact staging ownership receipt")
		}
	}
	result, err := p.run(ctx, commandRequest{args: []string{"rm", "--force", instance.Name}, operation: "delete docker sandbox", timeout: providerCleanupTimeout})
	if err != nil && isMissingSandbox(result.Stdout+"\n"+result.Stderr+"\n"+err.Error()) {
		err = nil
	}
	if err != nil {
		return err
	}
	p.architectureLogged.Delete(instance.Name)
	if p.runCommand == nil {
		stagingRoot, openErr := staging.Open(filepath.Dir(receipt.StagingPath))
		if openErr != nil {
			return openErr
		}
		if filepath.Clean(receipt.StagingPath) != filepath.Join(stagingRoot.Root(), instance.Name) {
			return fmt.Errorf("refusing Docker Sandbox staging cleanup outside the exact owned path")
		}
		if purgeErr := stagingRoot.PurgeOwned(instance.Name, receipt.StagingIdentity); purgeErr != nil {
			return purgeErr
		}
	}
	return nil
}

func (p *Provider) Inventory(ctx context.Context) ([]provider.InventoryItem, error) {
	return p.inventoryVerified(ctx)
}

func (p *Provider) inventoryVerified(ctx context.Context) ([]provider.InventoryItem, error) {
	for attempt := 1; attempt <= 2; attempt++ {
		result, err := p.run(ctx, commandRequest{args: []string{"ls", "--json"}, operation: "inventory docker sandboxes", timeout: providerReadbackTimeout})
		if err != nil {
			return nil, provider.NewControlPlaneFailure("inventory Docker Sandboxes", err)
		}
		items, parseErr := parseInventory([]byte(result.Stdout))
		if parseErr == nil {
			p.reconcileRelayTokens(items)
			return items, nil
		}
		if attempt == 2 {
			return nil, provider.NewControlPlaneFailure("inventory Docker Sandboxes", parseErr)
		}
		if p.logger != nil {
			p.logger.Debug("Docker Sandboxes inventory returned invalid machine-readable output; retrying once", "provider", "docker-sandboxes", "stdoutBytes", len(result.Stdout))
		}
	}
	return nil, fmt.Errorf("inventory docker sandboxes did not complete")
}

// CachedTemplates returns the strictly parsed, host-level Docker Sandboxes
// template cache inventory. It does not create, load, or otherwise mutate a
// template.
func (p *Provider) CachedTemplates(ctx context.Context) ([]CachedTemplate, error) {
	result, err := p.run(ctx, commandRequest{
		args:        []string{"template", "ls", "--json"},
		operation:   "read docker sandbox template cache",
		outputLimit: diagnosticOutputLimit,
		timeout:     providerReadbackTimeout,
	})
	if err != nil {
		return nil, err
	}
	images, err := parseTemplateInventory([]byte(result.Stdout))
	if err != nil {
		return nil, err
	}
	templates := make([]CachedTemplate, 0, len(images))
	for _, image := range images {
		templates = append(templates, CachedTemplate{
			Reference: image.Repository + ":" + image.Tag,
			CacheID:   image.ID,
			CreatedAt: image.CreatedAt,
			SizeBytes: image.Size,
		})
	}
	return templates, nil
}

func (p *Provider) verifyImportedTemplate(ctx context.Context, reference, cacheID string) error {
	result, err := p.run(ctx, commandRequest{args: []string{"template", "ls", "--json"}, operation: "verify cached docker sandbox template", timeout: providerReadbackTimeout})
	if err != nil {
		return err
	}
	images, err := parseTemplateInventory([]byte(result.Stdout))
	if err != nil {
		return err
	}
	repository, tag, err := splitTemplateReference(reference)
	if err != nil {
		return err
	}
	for _, image := range images {
		if image.Repository == repository && image.Tag == tag {
			if image.ID != cacheID {
				return fmt.Errorf("cached Docker Sandbox template ID %s does not match recorded cache identity %s", image.ID, cacheID)
			}
			return nil
		}
	}
	return fmt.Errorf("%w: configured Docker Sandbox template was not present in the authoritative Sandbox cache", provider.ErrTemplateNotFound)
}

func validTemplateCacheID(value string) bool {
	if len(value) != 12 {
		return false
	}
	for _, character := range value {
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f')) {
			return false
		}
	}
	return true
}

func validFullTemplateDigest(value string) bool {
	if len(value) != len("sha256:")+64 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	for _, character := range value[len("sha256:"):] {
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f')) {
			return false
		}
	}
	return true
}

func (p *Provider) assertIdentity(ctx context.Context, instance provider.Instance) (bool, error) {
	if err := validateInstance(instance, true); err != nil {
		return false, err
	}
	items, err := p.inventoryVerified(ctx)
	if err != nil {
		return false, err
	}
	for _, item := range items {
		if item.Instance.Name != instance.Name {
			continue
		}
		if item.Instance.ProviderID != instance.ProviderID {
			return false, fmt.Errorf("docker sandbox identity changed")
		}
		return true, nil
	}
	return false, nil
}

func (p *Provider) run(ctx context.Context, request commandRequest) (provider.ExecResult, error) {
	if err := validateCommandRequest(request); err != nil {
		return provider.ExecResult{}, err
	}
	if !controlPlaneGateHeld(ctx) {
		release, err := p.controlPlaneGate.acquire(ctx)
		if err != nil {
			return provider.ExecResult{}, err
		}
		defer release()
	}
	operationCtx, cancel := contextWithTimeout(ctx, request.timeout)
	defer cancel()
	if request.outputLimit == 0 {
		request.outputLimit = defaultOutputLimit
	}
	if err := operationCtx.Err(); err != nil {
		return provider.ExecResult{}, err
	}
	bufferedStdout, bufferedStderr, flush := provider.BufferSensitiveSinks(request.sensitiveValues, request.stdout, request.stderr)
	request.stdout = bufferedStdout
	request.stderr = bufferedStderr

	var result provider.ExecResult
	var runErr error
	if p.runCommand != nil {
		result, runErr = p.runCommand(operationCtx, request)
	} else {
		result, runErr = p.runRaw(operationCtx, request)
	}
	if len(result.Stdout) > request.outputLimit || len(result.Stderr) > request.outputLimit {
		runErr = errors.Join(runErr, fmt.Errorf("%s exceeded the output limit", request.operation))
		result.Stdout = truncate(result.Stdout, request.outputLimit)
		result.Stderr = truncate(result.Stderr, request.outputLimit)
	}
	if ctxErr := operationCtx.Err(); ctxErr != nil {
		runErr = errors.Join(ctxErr, runErr)
	}
	result, finishErr := provider.FinishSensitiveExecution(result, runErr, flush(), request.sensitiveValues)
	if finishErr != nil {
		detail := strings.TrimSpace(result.Stderr)
		if detail != "" {
			finishErr = fmt.Errorf("%s failed: %w: %s", request.operation, finishErr, detail)
		} else {
			finishErr = fmt.Errorf("%s failed: %w", request.operation, finishErr)
		}
		finishErr = provider.RedactError(finishErr, request.sensitiveValues...)
	}
	return result, finishErr
}

func contextWithTimeout(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return parent, func() {}
	}
	return context.WithTimeout(parent, timeout)
}

type controlPlaneGateContextKey struct{}

func withControlPlaneGate(ctx context.Context) context.Context {
	return context.WithValue(ctx, controlPlaneGateContextKey{}, true)
}

func controlPlaneGateHeld(ctx context.Context) bool {
	held, _ := ctx.Value(controlPlaneGateContextKey{}).(bool)
	return held
}

// controlPlaneCommandGate lets in-flight provider commands drain before a
// daemon recovery begins, then prevents new commands from racing its stop,
// quiescence, and start sequence.
type controlPlaneCommandGate struct {
	initOnce   sync.Once
	mu         sync.Mutex
	active     int
	pending    bool
	recovering bool
	changed    chan struct{}
}

func (gate *controlPlaneCommandGate) initialize() {
	gate.initOnce.Do(func() {
		gate.changed = make(chan struct{})
	})
}

func (gate *controlPlaneCommandGate) signalLocked() {
	close(gate.changed)
	gate.changed = make(chan struct{})
}

func (gate *controlPlaneCommandGate) acquire(ctx context.Context) (func(), error) {
	gate.initialize()
	for {
		gate.mu.Lock()
		if !gate.pending && !gate.recovering {
			gate.active++
			gate.mu.Unlock()
			return func() { gate.releaseOperation() }, nil
		}
		changed := gate.changed
		gate.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-changed:
		}
	}
}

func (gate *controlPlaneCommandGate) releaseOperation() {
	gate.mu.Lock()
	gate.active--
	if gate.active == 0 && gate.pending {
		gate.signalLocked()
	}
	gate.mu.Unlock()
}

func (gate *controlPlaneCommandGate) beginRecovery(ctx context.Context) (func(), error) {
	gate.initialize()
	for {
		gate.mu.Lock()
		if !gate.pending && !gate.recovering {
			gate.pending = true
			gate.signalLocked()
		}
		if gate.pending && gate.active == 0 && !gate.recovering {
			gate.pending = false
			gate.recovering = true
			gate.mu.Unlock()
			return func() { gate.endRecovery() }, nil
		}
		changed := gate.changed
		gate.mu.Unlock()
		select {
		case <-ctx.Done():
			gate.cancelRecovery()
			return nil, ctx.Err()
		case <-changed:
		}
	}
}

func (gate *controlPlaneCommandGate) cancelRecovery() {
	gate.mu.Lock()
	if gate.pending && !gate.recovering {
		gate.pending = false
		gate.signalLocked()
	}
	gate.mu.Unlock()
}

func (gate *controlPlaneCommandGate) endRecovery() {
	gate.mu.Lock()
	gate.recovering = false
	gate.signalLocked()
	gate.mu.Unlock()
}

func (p *Provider) acquireRecoverySlot(ctx context.Context) (func(), error) {
	p.recoverySlotOnce.Do(func() {
		p.recoverySlot = make(chan struct{}, 1)
	})
	select {
	case p.recoverySlot <- struct{}{}:
		return func() { <-p.recoverySlot }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (p *Provider) runRaw(ctx context.Context, request commandRequest) (provider.ExecResult, error) {
	var releaseHostLock func()
	if !provider.ControlPlaneLockHeld(ctx) {
		var err error
		releaseHostLock, err = provider.AcquireControlPlaneCommandLock(ctx)
		if err != nil {
			return provider.ExecResult{}, err
		}
		defer releaseHostLock()
	}
	cmd := exec.CommandContext(ctx, p.Binary, request.args...)
	isolateManagedProcess(cmd)
	cmd.WaitDelay = commandWaitDelay
	var cancellationKilledProcess atomic.Bool
	defaultCancel := cmd.Cancel
	cmd.Cancel = func() error {
		err := killManagedProcess(cmd)
		if err != nil {
			err = defaultCancel()
		}
		if err == nil {
			cancellationKilledProcess.Store(true)
		}
		return err
	}
	cmd.Stdin = request.stdin
	cmd.Env = childEnvironment(request.environment)
	stdout := &boundedBuffer{limit: request.outputLimit}
	stderr := &boundedBuffer{limit: request.outputLimit}
	cmd.Stdout = captureWriter(stdout, request.stdout)
	cmd.Stderr = captureWriter(stderr, request.stderr)
	err := cmd.Start()
	if err == nil {
		cleanup, attachErr := attachManagedProcess(cmd, request.preserveDescendantsOnSuccess)
		if attachErr != nil {
			killErr := killManagedProcess(cmd)
			waitErr := waitForManagedCommandExit(cmd, commandWaitDelay)
			err = errors.Join(fmt.Errorf("attach Docker Sandboxes process containment: %w", attachErr), killErr, waitErr)
		} else {
			defer cleanup()
			err = cmd.Wait()
			if request.preserveDescendantsOnSuccess && err != nil {
				if killErr := killManagedProcess(cmd); killErr != nil && !errors.Is(killErr, os.ErrProcessDone) {
					err = errors.Join(err, fmt.Errorf("clean up failed detached Docker Sandboxes daemon start: %w", killErr))
				}
			}
		}
	}
	if cancellationKilledProcess.Load() {
		if ctxErr := ctx.Err(); ctxErr != nil {
			err = ctxErr
		}
	}
	result := provider.ExecResult{Stdout: stdout.String(), Stderr: stderr.String()}
	if stdout.exceeded || stderr.exceeded {
		err = errors.Join(err, fmt.Errorf("output limit exceeded"))
	}
	return result, err
}

func validateCommandRequest(request commandRequest) error {
	if len(request.args) == 0 {
		return fmt.Errorf("refusing to invoke docker sandboxes without a subcommand")
	}
	if request.operation == "" {
		return fmt.Errorf("docker sandboxes operation label is required")
	}
	switch request.args[0] {
	case "version", "create", "exec", "daemon", "diagnose", "stop", "rm", "ls", "template", "policy", "inspect", "ports":
	default:
		return fmt.Errorf("docker sandboxes command %q is not permitted", request.args[0])
	}
	if request.args[0] == "inspect" && (len(request.args) != 3 || request.args[1] != "--json" || !sandboxNamePattern.MatchString(request.args[2])) {
		return fmt.Errorf("only exact machine-readable sandbox inspection is permitted")
	}
	if request.args[0] == "ports" && (len(request.args) != 3 || !sandboxNamePattern.MatchString(request.args[1]) || request.args[2] != "--json") {
		return fmt.Errorf("only exact machine-readable published-port absence verification is permitted")
	}
	if request.args[0] == "policy" {
		if err := validatePolicyCommand(request.args); err != nil {
			return err
		}
	}
	if request.args[0] == "daemon" {
		exactStop := len(request.args) == 2 && request.args[1] == "stop"
		exactStatus := len(request.args) == 3 && request.args[1] == "status" && request.args[2] == "--json"
		exactDetachedStart := len(request.args) == 3 && request.args[1] == "start" && request.args[2] == "--detach"
		if !exactStop && !exactStatus && !exactDetachedStart {
			return fmt.Errorf("only exact daemon status, cold-stop, or detached-start operations are permitted")
		}
	}
	for _, arg := range request.args {
		if strings.ContainsRune(arg, 0) {
			return fmt.Errorf("docker sandboxes argument contains a null byte")
		}
	}
	for key := range request.environment {
		if key != "DOCKER_SANDBOXES_ROOT_SIZE" && key != "DOCKER_SANDBOXES_DOCKER_SIZE" {
			return fmt.Errorf("docker sandboxes child environment contains a forbidden override")
		}
	}
	return nil
}

func childEnvironment(additions map[string]string) []string {
	environment := make([]string, 0, len(os.Environ())+len(additions))
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		upperKey := strings.ToUpper(key)
		if strings.HasPrefix(upperKey, "DOCKER_SANDBOXES_") || upperKey == "SSH_AUTH_SOCK" || upperKey == "SSH_AUTH_SOCK_GATEWAY" || upperKey == "SSH_AGENT_PID" {
			continue
		}
		environment = append(environment, item)
	}
	for _, key := range []string{"DOCKER_SANDBOXES_ROOT_SIZE", "DOCKER_SANDBOXES_DOCKER_SIZE"} {
		if value := additions[key]; value != "" {
			environment = append(environment, key+"="+value)
		}
	}
	return environment
}

func captureWriter(capture io.Writer, sink io.Writer) io.Writer {
	if sink == nil {
		return capture
	}
	return io.MultiWriter(capture, sink)
}

type boundedBuffer struct {
	buffer   bytes.Buffer
	limit    int
	exceeded bool
}

func (buffer *boundedBuffer) Write(data []byte) (int, error) {
	written := len(data)
	remaining := buffer.limit - buffer.buffer.Len()
	if remaining > 0 {
		if len(data) < remaining {
			remaining = len(data)
		}
		_, _ = buffer.buffer.Write(data[:remaining])
	}
	if len(data) > remaining {
		buffer.exceeded = true
	}
	return written, nil
}

func (buffer *boundedBuffer) String() string { return buffer.buffer.String() }

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}

func isMissingSandbox(text string) bool {
	text = strings.ToLower(text)
	return strings.Contains(text, "sandbox not found") || strings.Contains(text, "no such sandbox") || strings.Contains(text, "status 404")
}

func decodeStrictJSON(data []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if decoder.More() {
		return fmt.Errorf("unexpected trailing json value")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("unexpected trailing json value")
		}
		return err
	}
	return nil
}

var _ provider.Lifecycle = (*Provider)(nil)
var _ provider.ControlPlaneRecoverer = (*Provider)(nil)
var _ provider.AdmissionVerifier = (*Provider)(nil)
var _ provider.InstanceAdmissionVerifier = (*Provider)(nil)
var _ provider.PolicyManager = (*Provider)(nil)
var _ provider.TemplateArtifactRuntime = (*Provider)(nil)
var _ provider.TemplateArtifactActivationController = (*Provider)(nil)
var _ provider.TemplateAdmissionController = (*Provider)(nil)
var _ provider.TemplateArtifactCleaner = (*Provider)(nil)
var _ provider.TemplateArtifactObserver = (*Provider)(nil)
