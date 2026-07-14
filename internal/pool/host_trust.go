package pool

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/hosttrust"
	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
)

const (
	hostTrustGuestDir      = "/usr/local/share/ca-certificates/epar-host"
	hostTrustMarkerGuest   = "/opt/epar/host-trust-generation.json"
	hostTrustLeaseGuest    = "/run/epar/host-trust-lease.json"
	hostTrustLeaseLifetime = 20 * time.Second
	hostTrustMaximumAge    = 30 * time.Second
	hostTrustNativePoll    = 15 * time.Second
)

var hostTrustRefreshInterval = 5 * time.Second
var hostTrustControllerInContainer = linuxControllerInContainer
var hostTrustControllerOS = runtime.GOOS

type hostTrustImageMetadata struct {
	Mode             string   `json:"mode"`
	HostOS           string   `json:"hostOS"`
	Scopes           []string `json:"scopes"`
	Generation       string   `json:"generation"`
	CertificateCount int      `json:"certificateCount"`
}

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

func (m *Manager) hostTrustEnabled() bool {
	return hosttrust.Enabled(m.Config.Image.HostTrustMode)
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
	if m.hostTrustResolver != nil {
		snapshot, err := m.hostTrustResolver(ctx)
		if err != nil {
			return hosttrust.Snapshot{}, err
		}
		return validateHostTrustSnapshot(snapshot, time.Now().UTC())
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
	return validateHostTrustSnapshot(snapshot, time.Now().UTC())
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
	result, err := m.Provider.Exec(ctx, instanceName, []string{"cat", hostTrustMarkerGuest}, provider.ExecOptions{})
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
	if _, err := validateHostTrustSnapshot(snapshot, now); err != nil {
		return nil, err
	}
	return json.MarshalIndent(hostTrustLease{
		SchemaVersion: 1,
		Generation:    snapshot.Generation,
		HostOS:        snapshot.HostOS,
		Mode:          hosttrust.ModeOverlay,
		Scopes:        append([]string(nil), snapshot.Scopes...),
		ExpiresAt:     now.Add(hostTrustLeaseLifetime).UTC().Format(time.RFC3339Nano),
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
	if !m.hostTrustEnabled() {
		return nil
	}
	now := time.Now().UTC()
	content, err := hostTrustLeaseJSON(snapshot, now)
	if err != nil {
		return err
	}
	if _, err := m.Provider.Exec(ctx, instanceName, provider.ShellCommand("if command -v sudo >/dev/null 2>&1; then sudo install -d -m 0755 /run/epar; else install -d -m 0755 /run/epar; fi"), provider.ExecOptions{}); err != nil {
		return err
	}
	return provider.CopyTextAtomic(ctx, m.Provider, instanceName, hostTrustLeaseGuest, "0644", string(content)+"\n")
}

func (m *Manager) reconcileHostTrustRunners(ctx context.Context, active map[string]ProvisionedInstance, current hosttrust.Snapshot) int {
	if m.GitHub == nil {
		return 0
	}
	retired := 0
	for name, instance := range active {
		if instance.HostTrustGeneration != current.Generation {
			// Revoke the old generation before any remote status query. This is
			// safe for an already-running job (its hook already ran) and closes
			// the assignment window even when GitHub status is unavailable.
			if err := m.issueHostTrustLease(ctx, name, current); err != nil {
				fmt.Printf("[%s] old-generation revocation warning: %v\n", name, err)
			}
		}
		runner, found, err := m.GitHub.RunnerByName(ctx, name)
		if err != nil {
			fmt.Printf("[%s] host trust reconciliation warning; lease not refreshed: %v\n", name, err)
			continue
		}
		if instance.HostTrustGeneration == current.Generation {
			if !found || runner.Busy {
				continue
			}
			if err := m.issueHostTrustLease(ctx, name, current); err != nil {
				fmt.Printf("[%s] host trust lease refresh warning: %v\n", name, err)
			}
			continue
		}
		if found && runner.Busy {
			fmt.Printf("[%s] draining busy runner on old host trust generation %s\n", name, instance.HostTrustGeneration)
			continue
		}
		reason := fmt.Sprintf("host trust generation changed from %s to %s", instance.HostTrustGeneration, current.Generation)
		if err := m.retireInstance(context.Background(), instance, reason); err != nil {
			fmt.Printf("[%s] old-generation retirement warning: %v\n", name, err)
			continue
		}
		delete(active, name)
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
						fmt.Printf("host trust initial lease refresh warning: %v\n", err)
						continue
					}
					current = snapshot
				}
				for name, instance := range active {
					if instance.HostTrustGeneration != current.Generation {
						if err := m.issueHostTrustLease(ctx, name, current); err != nil {
							fmt.Printf("[%s] host trust initial stale-generation revocation warning: %v\n", name, err)
						}
						continue
					}
					runner, found, err := m.GitHub.RunnerByName(ctx, name)
					if err != nil || !found || runner.Busy {
						continue
					}
					if err := m.issueHostTrustLease(ctx, name, current); err != nil {
						fmt.Printf("[%s] host trust initial lease refresh warning: %v\n", name, err)
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
