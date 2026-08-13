package pool

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
	"github.com/solutionforest/ephemeral-action-runner/internal/hosttrust"
	artifactimage "github.com/solutionforest/ephemeral-action-runner/internal/image"
	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
)

const (
	hostTrustGuestDir      = "/usr/local/share/ca-certificates/epar-host"
	hostTrustMarkerGuest   = "/opt/epar/host-trust-generation.json"
	hostTrustLeaseGuest    = "/run/epar/host-trust-lease.json"
	hostTrustLeaseLifetime = 90 * time.Second
	hostTrustHandoffLease  = 2 * time.Minute
	hostTrustMaximumAge    = 30 * time.Second
	hostTrustNativePoll    = 15 * time.Second
)

// HostTrustMarkerGuest is the stable guest path for verified host-trust metadata.
const HostTrustMarkerGuest = hostTrustMarkerGuest

// HostTrustLeaseLifetime is the default shared guest lease duration.
const HostTrustLeaseLifetime = hostTrustLeaseLifetime

var hostTrustRefreshInterval = 30 * time.Second
var hostTrustWriteTimeout = 10 * time.Second
var hostTrustControllerInContainer = linuxControllerInContainer
var hostTrustControllerOS = runtime.GOOS

type hostTrustImageMetadata = artifactimage.HostTrustMetadata

type hostTrustMarker struct {
	SchemaVersion    int      `json:"schemaVersion"`
	Generation       string   `json:"generation"`
	HostOS           string   `json:"hostOS"`
	Mode             string   `json:"mode"`
	Scopes           []string `json:"scopes"`
	CertificateCount int      `json:"certificateCount"`
}

type hostTrustLease struct {
	SchemaVersion int      `json:"schemaVersion"`
	Generation    string   `json:"generation"`
	HostOS        string   `json:"hostOS"`
	Mode          string   `json:"mode"`
	Scopes        []string `json:"scopes"`
	ExpiresAt     string   `json:"expiresAt"`
}

// HostTrustMarker and HostTrustLease are shared payloads for specialized
// providers that install the same verified host-trust contract by another
// transport.
type HostTrustMarker = hostTrustMarker
type HostTrustLease = hostTrustLease

func (m *Manager) hostTrustEnabled() bool {
	return hosttrust.Enabled(m.Config.Image.HostTrustMode) || (m.Config.Provider.Type == "docker-sandboxes" && strings.EqualFold(strings.TrimSpace(m.Config.Image.Distribution), config.ImageDistributionPrebuilt) && len(m.Config.Image.TrustedCACertificatePaths) != 0)
}

func (m *Manager) hostTrustCollectionInterval() time.Duration {
	if strings.TrimSpace(os.Getenv("EPAR_HOST_TRUST_FEED")) != "" {
		return hostTrustRefreshInterval
	}
	return hostTrustNativePoll
}

func (m *Manager) acquireHostTrustControllerLock() (io.Closer, error) {
	if !m.hostTrustEnabled() || strings.TrimSpace(os.Getenv("EPAR_HOST_TRUST_FEED")) != "" {
		return nil, nil
	}
	configPath := strings.TrimSpace(m.ConfigPath)
	if configPath == "" {
		configPath = filepath.Join(m.ProjectRoot, ".local", "config.yml")
	}
	lock, err := hosttrust.AcquireConfigLock(configPath)
	if err != nil {
		return nil, fmt.Errorf("acquire host-trust controller lock: %w", err)
	}
	return lock, nil
}

// AcquireHostTrustControllerLock excludes another native controller or
// official host-feed wrapper for the same canonical configuration. Callers
// spanning image ensure plus pool startup should hold it across both phases.
func (m *Manager) AcquireHostTrustControllerLock() (io.Closer, error) {
	return m.acquireHostTrustControllerLock()
}

func (m *Manager) ensureHostTrustImage(ctx context.Context) error {
	m.hostTrustImageMu.Lock()
	defer m.hostTrustImageMu.Unlock()
	if m.hostTrustImageEnsurer != nil {
		return m.hostTrustImageEnsurer(ctx)
	}
	return m.EnsureImage(ctx)
}

func (m *Manager) resolveHostTrust(ctx context.Context) (hosttrust.Snapshot, error) {
	if !m.hostTrustEnabled() {
		return hosttrust.Snapshot{}, nil
	}
	hostOverlay := hosttrust.Enabled(m.Config.Image.HostTrustMode)
	if m.hostTrustResolver != nil {
		snapshot, err := m.hostTrustResolver(ctx)
		if err != nil {
			return hosttrust.Snapshot{}, err
		}
		return m.mergePrebuiltRuntimeTrustedCAs(snapshot, time.Now().UTC())
	}
	if !hostOverlay {
		return m.mergePrebuiltRuntimeTrustedCAs(hosttrust.Snapshot{HostOS: hostTrustControllerOS, CollectedAt: time.Now().UTC()}, time.Now().UTC())
	}
	feedPath := strings.TrimSpace(os.Getenv("EPAR_HOST_TRUST_FEED"))
	controllerHostOS := strings.TrimSpace(os.Getenv("EPAR_CONTROLLER_HOST_OS"))
	if feedPath == "" && hostTrustControllerOS == "linux" && hostTrustControllerInContainer() {
		return hosttrust.Snapshot{}, fmt.Errorf("image.hostTrustMode=overlay requires EPAR_HOST_TRUST_FEED when the EPAR controller runs in a container; use an official no-Go wrapper")
	}
	snapshot, err := hosttrust.Resolve(ctx, hosttrust.Options{
		Mode:             m.Config.Image.HostTrustMode,
		Scopes:           m.Config.Image.HostTrustScopes,
		FeedPath:         feedPath,
		ControllerHostOS: controllerHostOS,
	})
	if err != nil {
		return hosttrust.Snapshot{}, err
	}
	return m.mergePrebuiltRuntimeTrustedCAs(snapshot, time.Now().UTC())
}

func (m *Manager) mergePrebuiltRuntimeTrustedCAs(snapshot hosttrust.Snapshot, now time.Time) (hosttrust.Snapshot, error) {
	if m.Config.Provider.Type == "docker-sandboxes" && strings.EqualFold(strings.TrimSpace(m.Config.Image.Distribution), config.ImageDistributionPrebuilt) {
		explicit, err := m.trustedCACertificates()
		if err != nil {
			return hosttrust.Snapshot{}, err
		}
		for _, certificate := range explicit {
			parsed, err := hosttrust.CertificatesFromBytes(certificate.PEM)
			if err != nil {
				return hosttrust.Snapshot{}, fmt.Errorf("canonicalize explicit runtime CA %s: %w", certificate.DestinationName, err)
			}
			snapshot.Certificates = append(snapshot.Certificates, parsed...)
		}
		if len(explicit) != 0 && len(snapshot.Scopes) == 0 {
			snapshot.Scopes = []string{"explicit-ca"}
		}
		if snapshot.CollectedAt.IsZero() {
			snapshot.CollectedAt = now
		}
		canonical, err := hosttrust.Canonicalize(snapshot)
		if err != nil {
			return hosttrust.Snapshot{}, err
		}
		snapshot = canonical
	}
	return validateHostTrustSnapshot(snapshot, now)
}

func (m *Manager) resolveBuildTrust(ctx context.Context) (hosttrust.Snapshot, error) {
	if m.buildTrustResolver != nil {
		snapshot, err := m.buildTrustResolver(ctx)
		if err != nil {
			return hosttrust.Snapshot{}, err
		}
		return validateHostTrustSnapshot(snapshot, time.Now().UTC())
	}
	if m.hostTrustResolver != nil {
		snapshot, err := m.hostTrustResolver(ctx)
		if err != nil {
			return hosttrust.Snapshot{}, err
		}
		return validateHostTrustSnapshot(snapshot, time.Now().UTC())
	}
	scopes := buildTrustScopes(m.Config.Image.HostTrustMode, m.Config.Image.HostTrustScopes)
	feedPath := strings.TrimSpace(os.Getenv("EPAR_BUILD_TRUST_FEED"))
	controllerHostOS := strings.TrimSpace(os.Getenv("EPAR_CONTROLLER_HOST_OS"))
	if feedPath == "" && hostTrustControllerOS == "linux" && hostTrustControllerInContainer() {
		return hosttrust.Snapshot{}, fmt.Errorf("operational BuildKit trust requires EPAR_BUILD_TRUST_FEED when the EPAR controller runs in a container; use an official no-Go wrapper")
	}
	snapshot, err := hosttrust.Resolve(ctx, hosttrust.Options{
		Mode:             hosttrust.ModeOverlay,
		Scopes:           scopes,
		FeedPath:         feedPath,
		ControllerHostOS: controllerHostOS,
	})
	if err != nil {
		return hosttrust.Snapshot{}, fmt.Errorf("resolve operational BuildKit trust: %w", err)
	}
	return validateHostTrustSnapshot(snapshot, time.Now().UTC())
}

func buildTrustScopes(runnerMode string, runnerScopes []string) []string {
	scopes := []string{hosttrust.ScopeSystem}
	if hosttrust.Enabled(runnerMode) {
		for _, scope := range runnerScopes {
			if strings.EqualFold(strings.TrimSpace(scope), hosttrust.ScopeUser) {
				scopes = append(scopes, hosttrust.ScopeUser)
				break
			}
		}
	}
	return scopes
}

func linuxControllerInContainer() bool {
	return linuxContainerEvidence(
		func(path string) bool { _, err := os.Stat(path); return err == nil },
		os.Getenv,
		func(path string) []byte { content, _ := os.ReadFile(path); return content },
	)
}

func linuxContainerEvidence(exists func(string) bool, getenv func(string) string, read func(string) []byte) bool {
	if exists("/.dockerenv") || exists("/run/.containerenv") {
		return true
	}
	if strings.TrimSpace(getenv("container")) != "" || strings.TrimSpace(getenv("KUBERNETES_SERVICE_HOST")) != "" {
		return true
	}
	for _, path := range []string{"/proc/1/cgroup", "/proc/self/cgroup"} {
		content := strings.ToLower(string(read(path)))
		for _, marker := range []string{"/docker/", "/kubepods/", "/libpod-", "/containerd/", "/lxc/"} {
			if strings.Contains(content, marker) {
				return true
			}
		}
	}
	for _, line := range strings.Split(strings.ToLower(string(read("/proc/self/mountinfo"))), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 7 || fields[4] != "/" || !strings.Contains(line, " - overlay ") {
			continue
		}
		for _, marker := range []string{"/docker/", "/overlay2/", "/containers/storage/", "/containerd/", "/kubepods/"} {
			if strings.Contains(line, marker) {
				return true
			}
		}
	}
	return false
}

func validateHostTrustSnapshot(snapshot hosttrust.Snapshot, now time.Time) (hosttrust.Snapshot, error) {
	if snapshot.Generation == "" {
		return hosttrust.Snapshot{}, fmt.Errorf("host trust collector returned an empty generation")
	}
	if snapshot.HostOS == "" {
		return hosttrust.Snapshot{}, fmt.Errorf("host trust collector returned an empty host OS")
	}
	if len(snapshot.Scopes) == 0 {
		return hosttrust.Snapshot{}, fmt.Errorf("host trust collector returned no scopes")
	}
	if len(snapshot.Certificates) == 0 {
		return hosttrust.Snapshot{}, fmt.Errorf("host trust collector returned no CA certificates")
	}
	if snapshot.CollectedAt.IsZero() || now.Sub(snapshot.CollectedAt) > hostTrustMaximumAge {
		return hosttrust.Snapshot{}, fmt.Errorf("host trust snapshot is older than %s", hostTrustMaximumAge)
	}
	if snapshot.CollectedAt.After(now.Add(time.Minute)) {
		return hosttrust.Snapshot{}, fmt.Errorf("host trust snapshot collection time is in the future")
	}
	return snapshot, nil
}

// ValidateHostTrustSnapshot verifies the shared host-trust freshness contract.
func ValidateHostTrustSnapshot(snapshot hosttrust.Snapshot, now time.Time) (hosttrust.Snapshot, error) {
	return validateHostTrustSnapshot(snapshot, now)
}

func hostTrustMetadata(snapshot hosttrust.Snapshot) *hostTrustImageMetadata {
	if snapshot.Generation == "" {
		return nil
	}
	return &hostTrustImageMetadata{
		Mode:             hosttrust.ModeOverlay,
		HostOS:           snapshot.HostOS,
		Scopes:           append([]string(nil), snapshot.Scopes...),
		Generation:       snapshot.Generation,
		CertificateCount: len(snapshot.Certificates),
	}
}

func hostTrustMarkerJSON(snapshot hosttrust.Snapshot) ([]byte, error) {
	return json.MarshalIndent(hostTrustMarker{
		SchemaVersion:    1,
		Generation:       snapshot.Generation,
		HostOS:           snapshot.HostOS,
		Mode:             hosttrust.ModeOverlay,
		Scopes:           append([]string(nil), snapshot.Scopes...),
		CertificateCount: len(snapshot.Certificates),
	}, "", "  ")
}

func (m *Manager) readInstanceHostTrustMarker(ctx context.Context, instanceName string) (hostTrustMarker, error) {
	result, err := m.execGuest(ctx, instanceName, []string{"cat", hostTrustMarkerGuest}, provider.ExecOptions{})
	if err != nil {
		return hostTrustMarker{}, fmt.Errorf("read image host trust marker: %w", err)
	}
	var marker hostTrustMarker
	if err := json.Unmarshal([]byte(result.Stdout), &marker); err != nil {
		return hostTrustMarker{}, fmt.Errorf("parse image host trust marker: %w", err)
	}
	return marker, nil
}

func validateHostTrustMarkerAgainstSnapshot(marker hostTrustMarker, snapshot hosttrust.Snapshot) error {
	if marker.SchemaVersion != 1 {
		return fmt.Errorf("image host trust marker schemaVersion=%d, want 1", marker.SchemaVersion)
	}
	if marker.Generation != snapshot.Generation {
		return fmt.Errorf("image generation %q does not match current generation %q", marker.Generation, snapshot.Generation)
	}
	if marker.HostOS != snapshot.HostOS {
		return fmt.Errorf("image hostOS %q does not match current hostOS %q", marker.HostOS, snapshot.HostOS)
	}
	if marker.Mode != hosttrust.ModeOverlay {
		return fmt.Errorf("image host trust mode %q is not overlay", marker.Mode)
	}
	if strings.Join(marker.Scopes, "\x00") != strings.Join(snapshot.Scopes, "\x00") {
		return fmt.Errorf("image host trust scopes %v do not match current scopes %v", marker.Scopes, snapshot.Scopes)
	}
	if marker.CertificateCount != len(snapshot.Certificates) {
		return fmt.Errorf("image host trust certificateCount=%d does not match current count %d", marker.CertificateCount, len(snapshot.Certificates))
	}
	return nil
}

func hostTrustLeaseJSON(snapshot hosttrust.Snapshot, now time.Time) ([]byte, error) {
	return hostTrustLeaseJSONWithLifetime(snapshot, now, hostTrustLeaseLifetime)
}

func hostTrustLeaseJSONWithLifetime(snapshot hosttrust.Snapshot, now time.Time, lifetime time.Duration) ([]byte, error) {
	if _, err := validateHostTrustSnapshot(snapshot, now); err != nil {
		return nil, err
	}
	if lifetime <= 0 {
		return nil, fmt.Errorf("host trust lease lifetime must be positive")
	}
	return json.MarshalIndent(hostTrustLease{
		SchemaVersion: 1,
		Generation:    snapshot.Generation,
		HostOS:        snapshot.HostOS,
		Mode:          hosttrust.ModeOverlay,
		Scopes:        append([]string(nil), snapshot.Scopes...),
		ExpiresAt:     now.Add(lifetime).UTC().Format(time.RFC3339Nano),
	}, "", "  ")
}

func copyHostTrustCertificatesToDir(destination string, snapshot hosttrust.Snapshot) error {
	if err := os.MkdirAll(destination, 0755); err != nil {
		return err
	}
	for _, certificate := range snapshot.Certificates {
		if err := os.WriteFile(filepath.Join(destination, certificate.Name), certificate.PEM, 0644); err != nil {
			return err
		}
	}
	return nil
}

func hostTrustCertificateArchive(snapshot hosttrust.Snapshot) (string, error) {
	var buffer bytes.Buffer
	writer := tar.NewWriter(&buffer)
	for _, certificate := range snapshot.Certificates {
		header := &tar.Header{
			Name: certificate.Name,
			Mode: 0644,
			Size: int64(len(certificate.PEM)),
		}
		if err := writer.WriteHeader(header); err != nil {
			return "", err
		}
		if _, err := writer.Write(certificate.PEM); err != nil {
			return "", err
		}
	}
	if err := writer.Close(); err != nil {
		return "", err
	}
	return buffer.String(), nil
}

func (m *Manager) installHostTrustRuntime(ctx context.Context, instanceName string, snapshot hosttrust.Snapshot) error {
	if !m.hostTrustEnabled() {
		return nil
	}
	if _, err := validateHostTrustSnapshot(snapshot, time.Now().UTC()); err != nil {
		return err
	}
	archive, err := hostTrustCertificateArchive(snapshot)
	if err != nil {
		return fmt.Errorf("archive host trust certificates: %w", err)
	}
	script := fmt.Sprintf("sudo install -d -m 0755 %s && sudo find %s -maxdepth 1 -type f -name 'epar-*.crt' -delete && sudo tar -x -f - --no-same-owner --no-same-permissions -C %s && sudo update-ca-certificates && sudo install -d -m 0755 -o root -g root /opt/epar/trust && sudo install -m 0444 -o root -g root /etc/ssl/certs/ca-certificates.crt /opt/epar/trust/ca-bundle.pem.tmp && sudo mv -f /opt/epar/trust/ca-bundle.pem.tmp /opt/epar/trust/ca-bundle.pem && sudo test -s /opt/epar/trust/ca-bundle.pem && sudo test ! -L /opt/epar/trust/ca-bundle.pem", shellQuote(hostTrustGuestDir), shellQuote(hostTrustGuestDir), shellQuote(hostTrustGuestDir))
	if _, err := m.execGuest(ctx, instanceName, provider.ShellCommand(script), provider.ExecOptions{Stdin: archive}); err != nil {
		return fmt.Errorf("install host trust certificates in runtime: %w", err)
	}
	content, err := hostTrustMarkerJSON(snapshot)
	if err != nil {
		return err
	}
	if err := m.copyTextGuest(ctx, instanceName, hostTrustMarkerGuest, "0644", string(content)+"\n", true); err != nil {
		return fmt.Errorf("install host trust generation marker: %w", err)
	}
	return nil
}

func (m *Manager) writeHostTrustBuildInputs(buildContext string, snapshot hosttrust.Snapshot) error {
	if err := copyHostTrustCertificatesToDir(filepath.Join(buildContext, "host-trust-certificates"), snapshot); err != nil {
		return err
	}
	metadataDir := filepath.Join(buildContext, "host-trust-metadata")
	if err := os.MkdirAll(metadataDir, 0755); err != nil {
		return err
	}
	if snapshot.Generation == "" {
		return nil
	}
	content, err := hostTrustMarkerJSON(snapshot)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(metadataDir, filepath.Base(hostTrustMarkerGuest)), append(content, '\n'), 0644)
}

func (m *Manager) issueHostTrustLease(ctx context.Context, instanceName string, snapshot hosttrust.Snapshot) error {
	return m.issueHostTrustLeaseWithLifetime(ctx, instanceName, snapshot, hostTrustLeaseLifetime)
}

func (m *Manager) issueHostTrustLeaseWithLifetime(ctx context.Context, instanceName string, snapshot hosttrust.Snapshot, lifetime time.Duration) error {
	if !m.hostTrustEnabled() {
		return nil
	}
	content, err := hostTrustLeaseJSONWithLifetime(snapshot, time.Now().UTC(), lifetime)
	if err != nil {
		return err
	}
	writeCtx, cancel := context.WithTimeout(ctx, hostTrustWriteTimeout)
	defer cancel()
	staging := hostTrustLeaseGuest + ".tmp"
	script := fmt.Sprintf("if command -v sudo >/dev/null 2>&1; then sudo rm -f %s %s; else rm -f %s %s; fi && cat > /tmp/epar-host-trust-lease && if command -v sudo >/dev/null 2>&1; then sudo install -d -m 0755 /run/epar && sudo install -m 0644 /tmp/epar-host-trust-lease %s && sudo mv -f %s %s; else install -d -m 0755 /run/epar && install -m 0644 /tmp/epar-host-trust-lease %s && mv -f %s %s; fi && rm -f /tmp/epar-host-trust-lease", shellQuote(hostTrustLeaseGuest), shellQuote(staging), shellQuote(hostTrustLeaseGuest), shellQuote(staging), shellQuote(staging), shellQuote(staging), shellQuote(hostTrustLeaseGuest), shellQuote(staging), shellQuote(staging), shellQuote(hostTrustLeaseGuest))
	if _, err := m.execGuest(writeCtx, instanceName, provider.ShellCommand(script), provider.ExecOptions{Stdin: string(content) + "\n"}); err != nil {
		revokeCtx, revokeCancel := context.WithTimeout(context.WithoutCancel(ctx), hostTrustWriteTimeout)
		revokeErr := m.revokeHostTrustLease(revokeCtx, instanceName)
		revokeCancel()
		if errors.Is(err, context.DeadlineExceeded) {
			return errors.Join(fmt.Errorf("host trust lease write exceeded %s: %w", hostTrustWriteTimeout, err), revokeErr)
		}
		return errors.Join(err, revokeErr)
	}
	return nil
}

func (m *Manager) revokeHostTrustLease(ctx context.Context, instanceName string) error {
	script := fmt.Sprintf("if command -v sudo >/dev/null 2>&1; then sudo rm -f %s %s; else rm -f %s %s; fi", shellQuote(hostTrustLeaseGuest), shellQuote(hostTrustLeaseGuest+".tmp"), shellQuote(hostTrustLeaseGuest), shellQuote(hostTrustLeaseGuest+".tmp"))
	_, err := m.execGuest(ctx, instanceName, provider.ShellCommand(script), provider.ExecOptions{})
	return err
}

func (m *Manager) fenceHostTrustRunnerRegistration(ctx context.Context, instance ProvisionedInstance, cause error) error {
	// Admission uncertainty must be durable even when the exact remote fence
	// cannot be completed. The physical instance remains capacity-counting and
	// reconciliation must not advertise it as Ready again by name alone.
	fenceCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), hostTrustWriteTimeout)
	defer cancel()
	m.quarantineLifecycle(fenceCtx, instance.Name, cause)
	if m.GitHub == nil || instance.RunnerID == 0 {
		return fmt.Errorf("exact GitHub runner identity is unavailable; cannot fence registration")
	}
	runner, found, err := m.GitHub.RunnerByName(fenceCtx, instance.Name)
	if err != nil {
		return fmt.Errorf("verify exact GitHub runner before host-trust fence: %w", err)
	}
	if !found {
		return nil
	}
	if runner.ID != instance.RunnerID {
		return fmt.Errorf("same-name GitHub runner id=%d does not match expected id=%d; refusing registration fence", runner.ID, instance.RunnerID)
	}
	if err := m.GitHub.DeleteRunnerIfExists(fenceCtx, runner.ID); err != nil {
		return fmt.Errorf("delete exact GitHub runner id=%d: %w", instance.RunnerID, err)
	}
	return nil
}

func (m *Manager) reconcileHostTrustRunners(ctx context.Context, active map[string]ProvisionedInstance, current hosttrust.Snapshot, busyHandoff map[string]bool) int {
	if m.GitHub == nil {
		return 0
	}
	for name := range busyHandoff {
		if _, found := active[name]; !found {
			delete(busyHandoff, name)
		}
	}
	retired := 0
	for name, instance := range active {
		if !instance.ProviderOwned || (instance.Phase != LifecycleReady && instance.Phase != LifecycleDraining) {
			continue
		}
		if _, requiresTransport := m.providerLifecycle().(provider.HostTrustRuntimeActivator); requiresTransport {
			providerInstance, providerErr := m.providerInstance(ctx, name)
			if providerErr != nil {
				m.warnf("[%s] host trust transport identity warning; lease not refreshed: %v\n", name, providerErr)
				if fenceErr := m.fenceHostTrustRunnerRegistration(ctx, instance, providerErr); fenceErr != nil {
					m.warnf("[%s] host trust registration fencing warning: %v\n", name, fenceErr)
				}
				instance.Phase = LifecycleQuarantined
				active[name] = instance
				continue
			}
			if err := m.activateProviderHostTrustRuntime(ctx, providerInstance); err != nil {
				m.warnf("[%s] host trust transport refresh warning; lease not refreshed: %v\n", name, err)
				if fenceErr := m.fenceHostTrustRunnerRegistration(ctx, instance, err); fenceErr != nil {
					m.warnf("[%s] host trust registration fencing warning: %v\n", name, fenceErr)
				}
				instance.Phase = LifecycleQuarantined
				active[name] = instance
				continue
			}
		}
		if instance.HostTrustGeneration != current.Generation {
			// Revoke the old generation before any remote status query. This is
			// safe for an already-running job (its hook already ran) and closes
			// the assignment window even when GitHub status is unavailable.
			if err := m.issueHostTrustLease(ctx, name, current); err != nil {
				m.warnf("[%s] old-generation revocation warning: %v\n", name, err)
				if fenceErr := m.fenceHostTrustRunnerRegistration(ctx, instance, err); fenceErr != nil {
					m.warnf("[%s] host trust registration fencing warning: %v\n", name, fenceErr)
				}
				instance.Phase = LifecycleQuarantined
				active[name] = instance
				continue
			}
		}
		runner, found, err := m.GitHub.RunnerByName(ctx, name)
		if err != nil {
			if isTransientGitHubLivenessError(err) {
				m.warnf("[%s] GitHub API is temporarily unavailable during host trust refresh; the existing lease will expire closed and EPAR will retry: %v\n", name, err)
			} else {
				m.warnf("[%s] host trust reconciliation warning; lease not refreshed: %v\n", name, err)
			}
			continue
		}
		if instance.HostTrustGeneration == current.Generation {
			if !found {
				delete(busyHandoff, name)
				continue
			}
			if runner.Busy {
				if busyHandoff[name] {
					continue
				}
				// GitHub can mark a runner busy before the job-start hook executes.
				// Issue one bounded handoff lease for that transition, but never
				// renew it while the job remains busy.
				if err := m.issueHostTrustLeaseWithLifetime(ctx, name, current, hostTrustHandoffLease); err != nil {
					m.warnf("[%s] host trust job handoff lease warning: %v\n", name, err)
					if fenceErr := m.fenceHostTrustRunnerRegistration(ctx, instance, err); fenceErr != nil {
						m.warnf("[%s] host trust registration fencing warning: %v\n", name, fenceErr)
					}
					instance.Phase = LifecycleQuarantined
					active[name] = instance
					continue
				}
				busyHandoff[name] = true
				continue
			}
			delete(busyHandoff, name)
			if err := m.issueHostTrustLease(ctx, name, current); err != nil {
				m.warnf("[%s] host trust lease refresh warning: %v\n", name, err)
				if fenceErr := m.fenceHostTrustRunnerRegistration(ctx, instance, err); fenceErr != nil {
					m.warnf("[%s] host trust registration fencing warning: %v\n", name, fenceErr)
				}
				instance.Phase = LifecycleQuarantined
				active[name] = instance
			}
			continue
		}
		if found && runner.Busy {
			delete(busyHandoff, name)
			m.infof("[%s] draining busy runner on old host trust generation %s\n", name, instance.HostTrustGeneration)
			instance.Phase = LifecycleDraining
			active[name] = instance
			continue
		}
		reason := fmt.Sprintf("host trust generation changed from %s to %s", instance.HostTrustGeneration, current.Generation)
		if err := m.retireInstance(ctx, instance, reason); err != nil {
			m.warnf("[%s] old-generation retirement warning: %v\n", name, err)
			continue
		}
		delete(active, name)
		delete(busyHandoff, name)
		retired++
	}
	return retired
}

// startHostTrustLeaseKeeper preserves already-ready idle capacity while a
// controller is still provisioning the rest of the initial pool (or waiting
// for parallel verification instances). It never refreshes a busy runner and
// never renews an old generation after host trust changes.
func (m *Manager) startHostTrustLeaseKeeper(parent context.Context) (func(ProvisionedInstance), func()) {
	if !m.hostTrustEnabled() || m.GitHub == nil {
		return func(ProvisionedInstance) {}, func() {}
	}
	ctx, cancel := context.WithCancel(parent)
	additions := make(chan ProvisionedInstance, 64)
	done := make(chan struct{})
	var once sync.Once
	go func() {
		defer close(done)
		active := make(map[string]ProvisionedInstance)
		ticker := time.NewTicker(hostTrustRefreshInterval)
		defer ticker.Stop()
		var current hosttrust.Snapshot
		nextCollection := time.Time{}
		for {
			select {
			case <-ctx.Done():
				return
			case instance := <-additions:
				active[instance.Name] = instance
			case now := <-ticker.C:
				if current.Generation == "" || !now.Before(nextCollection) {
					snapshot, err := m.resolveHostTrust(ctx)
					nextCollection = now.Add(m.hostTrustCollectionInterval())
					if err != nil {
						current = hosttrust.Snapshot{}
						m.warnf("host trust initial lease refresh warning: %v\n", err)
						continue
					}
					current = snapshot
				}
				for name, instance := range active {
					if instance.HostTrustGeneration != current.Generation {
						if err := m.issueHostTrustLease(ctx, name, current); err != nil {
							m.warnf("[%s] host trust initial stale-generation revocation warning: %v\n", name, err)
							if fenceErr := m.fenceHostTrustRunnerRegistration(ctx, instance, err); fenceErr != nil {
								m.warnf("[%s] host trust initial registration fencing warning: %v\n", name, fenceErr)
							}
							instance.Phase = LifecycleQuarantined
							active[name] = instance
						}
						continue
					}
					runner, found, err := m.GitHub.RunnerByName(ctx, name)
					if err != nil || !found || runner.Busy {
						continue
					}
					if err := m.issueHostTrustLease(ctx, name, current); err != nil {
						m.warnf("[%s] host trust initial lease refresh warning: %v\n", name, err)
						if fenceErr := m.fenceHostTrustRunnerRegistration(ctx, instance, err); fenceErr != nil {
							m.warnf("[%s] host trust initial registration fencing warning: %v\n", name, fenceErr)
						}
						instance.Phase = LifecycleQuarantined
						active[name] = instance
					}
				}
			}
		}
	}()
	add := func(instance ProvisionedInstance) {
		select {
		case additions <- instance:
		case <-ctx.Done():
		}
	}
	stop := func() {
		once.Do(func() {
			cancel()
			<-done
		})
	}
	return add, stop
}
