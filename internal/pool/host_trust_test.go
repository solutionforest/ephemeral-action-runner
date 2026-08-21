package pool

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
	gh "github.com/solutionforest/ephemeral-action-runner/internal/github"
	"github.com/solutionforest/ephemeral-action-runner/internal/hosttrust"
	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
)

func TestDockerContainerBuildContextKeepsHostAndExplicitTrustSeparate(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{
		filepath.Join(root, "scripts", "guest", "ubuntu"),
		filepath.Join(root, "scripts", "container", "ubuntu"),
	} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	explicitPath := filepath.Join(root, "explicit.pem")
	hostPath := filepath.Join(root, "host.pem")
	writeTestCACertificate(t, explicitPath, "Explicit Root")
	writeTestCACertificate(t, hostPath, "Host Root")
	snapshot := hostTrustSnapshotFromFile(t, hostPath, "windows", []string{"system", "user"})
	manager := Manager{
		Config: config.Config{Image: config.ImageConfig{
			TrustedCACertificatePaths: []string{"explicit.pem"},
			HostTrustMode:             config.HostTrustModeOverlay,
			HostTrustScopes:           []string{"system", "user"},
		}},
		ProjectRoot: root,
	}
	buildContext := t.TempDir()
	if err := manager.prepareDockerContainerBuildContextWithHostTrust(buildContext, t.TempDir(), `{"hash":"test"}`+"\n", snapshot); err != nil {
		t.Fatal(err)
	}
	assertSingleCertificateFile(t, filepath.Join(buildContext, "trusted-ca-certificates"))
	assertSingleCertificateFile(t, filepath.Join(buildContext, "host-trust-certificates"))
	marker, err := os.ReadFile(filepath.Join(buildContext, "host-trust-metadata", "host-trust-generation.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(marker), snapshot.Generation) || !strings.Contains(string(marker), `"hostOS": "windows"`) {
		t.Fatalf("host trust marker = %s", marker)
	}
	dockerfile, err := os.ReadFile(filepath.Join(buildContext, "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(dockerfile)
	for _, want := range []string{
		"RUN rm -rf " + trustedCAGuestDir + " " + hostTrustGuestDir + " " + hostTrustMarkerGuest,
		"COPY trusted-ca-certificates/ " + trustedCAGuestDir + "/",
		"COPY host-trust-certificates/ " + hostTrustGuestDir + "/",
		"COPY host-trust-metadata/ /opt/epar/",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("Dockerfile missing %q:\n%s", want, text)
		}
	}
}

func TestHostTrustLeaseMatchesMarkerAndExpires(t *testing.T) {
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	snapshot := hosttrust.Snapshot{
		Generation:   "generation-one",
		HostOS:       "linux",
		Scopes:       []string{"system"},
		Certificates: []hosttrust.Certificate{{Name: "root.crt", PEM: []byte("pem")}},
		CollectedAt:  now,
	}
	markerBytes, err := hostTrustMarkerJSON(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	leaseBytes, err := hostTrustLeaseJSON(snapshot, now)
	if err != nil {
		t.Fatal(err)
	}
	var marker hostTrustMarker
	var lease hostTrustLease
	if err := json.Unmarshal(markerBytes, &marker); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(leaseBytes, &lease); err != nil {
		t.Fatal(err)
	}
	if marker.Generation != lease.Generation || marker.HostOS != lease.HostOS || strings.Join(marker.Scopes, ",") != strings.Join(lease.Scopes, ",") {
		t.Fatalf("marker/lease mismatch: %+v %+v", marker, lease)
	}
	expires, err := time.Parse(time.RFC3339Nano, lease.ExpiresAt)
	if err != nil {
		t.Fatal(err)
	}
	if got := expires.Sub(now); got != hostTrustLeaseLifetime {
		t.Fatalf("lease lifetime = %s, want %s", got, hostTrustLeaseLifetime)
	}
}

func TestHostTrustCertificateArchiveContainsOnlyExactCertificates(t *testing.T) {
	snapshot := hosttrust.Snapshot{
		Generation: "generation-one",
		HostOS:     "windows",
		Scopes:     []string{"system", "user"},
		Certificates: []hosttrust.Certificate{
			{Name: "epar-system-a.crt", PEM: []byte("system")},
			{Name: "epar-user-b.crt", PEM: []byte("user")},
		},
		CollectedAt: time.Now(),
	}
	archive, err := hostTrustCertificateArchive(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	reader := tar.NewReader(bytes.NewBufferString(archive))
	var names []string
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, header.Name)
		if header.Mode != 0644 {
			t.Fatalf("certificate mode = %o, want 0644", header.Mode)
		}
	}
	if len(names) != len(snapshot.Certificates) {
		t.Fatalf("archive entries = %d, want %d", len(names), len(snapshot.Certificates))
	}
	for index, certificate := range snapshot.Certificates {
		if names[index] != certificate.Name {
			t.Fatalf("archive entry %d = %q, want %q", index, names[index], certificate.Name)
		}
	}
}

func TestValidateHostTrustMarkerAgainstSnapshotRejectsCloningRace(t *testing.T) {
	snapshot := hosttrust.Snapshot{
		Generation: "g2", HostOS: "windows", Scopes: []string{"system", "user"},
		Certificates: []hosttrust.Certificate{{Name: "root.crt", PEM: []byte("pem")}},
	}
	current := hostTrustMarker{SchemaVersion: 1, Generation: "g2", HostOS: "windows", Mode: hosttrust.ModeOverlay, Scopes: []string{"system", "user"}, CertificateCount: 1}
	if err := validateHostTrustMarkerAgainstSnapshot(current, snapshot); err != nil {
		t.Fatalf("current image marker rejected: %v", err)
	}
	stale := current
	stale.Generation = "g1"
	if err := validateHostTrustMarkerAgainstSnapshot(stale, snapshot); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("stale image marker error = %v", err)
	}
	wrongCount := current
	wrongCount.CertificateCount = 2
	if err := validateHostTrustMarkerAgainstSnapshot(wrongCount, snapshot); err == nil || !strings.Contains(err.Error(), "certificateCount") {
		t.Fatalf("wrong certificate count error = %v", err)
	}
}

func TestLinuxContainerEvidence(t *testing.T) {
	tests := []struct {
		name    string
		exists  map[string]bool
		environ map[string]string
		files   map[string]string
		want    bool
	}{
		{name: "docker marker", exists: map[string]bool{"/.dockerenv": true}, want: true},
		{name: "podman marker", exists: map[string]bool{"/run/.containerenv": true}, want: true},
		{name: "container environment", environ: map[string]string{"container": "podman"}, want: true},
		{name: "kubernetes environment", environ: map[string]string{"KUBERNETES_SERVICE_HOST": "10.0.0.1"}, want: true},
		{name: "cgroup", files: map[string]string{"/proc/self/cgroup": "0::/kubepods/burstable/pod/containerd/id"}, want: true},
		{name: "overlay root", files: map[string]string{"/proc/self/mountinfo": "1 0 0:1 / / rw - overlay overlay rw,upperdir=/var/lib/docker/overlay2/id/diff"}, want: true},
		{name: "native host", files: map[string]string{"/proc/self/cgroup": "0::/user.slice/user-1000.slice", "/proc/self/mountinfo": "1 0 8:1 / / rw - ext4 /dev/sda1 rw"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := linuxContainerEvidence(
				func(path string) bool { return test.exists[path] },
				func(key string) string { return test.environ[key] },
				func(path string) []byte { return []byte(test.files[path]) },
			)
			if got != test.want {
				t.Fatalf("linuxContainerEvidence() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestResolveHostTrustRejectsContainerWithoutFeed(t *testing.T) {
	oldDetector := hostTrustControllerInContainer
	oldOS := hostTrustControllerOS
	hostTrustControllerInContainer = func() bool { return true }
	hostTrustControllerOS = "linux"
	t.Cleanup(func() {
		hostTrustControllerInContainer = oldDetector
		hostTrustControllerOS = oldOS
	})
	t.Setenv("EPAR_HOST_TRUST_FEED", "")
	manager := Manager{Config: config.Config{Image: config.ImageConfig{
		HostTrustMode: config.HostTrustModeOverlay, HostTrustScopes: []string{"system"},
	}}}
	_, err := manager.resolveHostTrust(context.Background())
	if err == nil || !strings.Contains(err.Error(), "requires EPAR_HOST_TRUST_FEED") {
		t.Fatalf("container without feed error = %v", err)
	}
}

func TestInitialLeaseKeeperRevokesRunnerWhenGenerationChanges(t *testing.T) {
	oldInterval := hostTrustRefreshInterval
	hostTrustRefreshInterval = 5 * time.Millisecond
	t.Cleanup(func() { hostTrustRefreshInterval = oldInterval })

	provider := &fakeProvider{}
	github := &fakeGitHub{runner: gh.Runner{Name: "runner-1", ID: 42, Status: "online", Busy: false}, found: true}
	manager := Manager{
		Config:   config.Config{Image: config.ImageConfig{HostTrustMode: config.HostTrustModeOverlay, HostTrustScopes: []string{"system"}}},
		Provider: provider,
		GitHub:   github,
	}
	manager.hostTrustResolver = func(context.Context) (hosttrust.Snapshot, error) {
		return hosttrust.Snapshot{
			Generation: "g2", HostOS: "linux", Scopes: []string{"system"},
			Certificates: []hosttrust.Certificate{{Name: "root.crt", PEM: []byte("pem")}},
			CollectedAt:  time.Now().UTC(),
		}, nil
	}
	add, stop := manager.startHostTrustLeaseKeeper(context.Background())
	add(ProvisionedInstance{Name: "runner-1", RunnerID: 42, HostTrustGeneration: "g1"})
	deadline := time.Now().Add(time.Second)
	for {
		provider.mu.Lock()
		found := false
		for _, options := range provider.execOptions {
			if strings.Contains(options.Stdin, `"generation": "g2"`) {
				found = true
				break
			}
		}
		provider.mu.Unlock()
		if found {
			break
		}
		if time.Now().After(deadline) {
			stop()
			t.Fatal("initial lease keeper did not revoke G1 after observing G2")
		}
		time.Sleep(5 * time.Millisecond)
	}
	stop()
}

func TestHostTrustReconciliationRevokesAndRetiresIdleOldGeneration(t *testing.T) {
	provider := &fakeProvider{}
	github := &fakeGitHub{runner: gh.Runner{Name: "runner-1", ID: 42, Status: "online", Busy: false}, found: true}
	manager := Manager{
		Config: config.Config{
			Image:   config.ImageConfig{HostTrustMode: config.HostTrustModeOverlay, HostTrustScopes: []string{"system"}},
			Logging: config.LoggingConfig{Directory: t.TempDir()},
		},
		Provider: provider,
		GitHub:   github,
	}
	current := hosttrust.Snapshot{
		Generation:   "g2",
		HostOS:       "linux",
		Scopes:       []string{"system"},
		Certificates: []hosttrust.Certificate{{Name: "root.crt", PEM: []byte("pem")}},
		CollectedAt:  time.Now().UTC(),
	}
	active := map[string]ProvisionedInstance{"runner-1": {Name: "runner-1", RunnerID: 42, HostTrustGeneration: "g1", ProviderOwned: true, Phase: LifecycleReady}}
	manager.reconcileHostTrustRunners(context.Background(), active, current, make(map[string]bool))
	if len(active) != 0 {
		t.Fatalf("active runners = %#v, want old idle runner retired", active)
	}
	if atomic.LoadInt32(&provider.stopCalls) != 1 || atomic.LoadInt32(&provider.deleteCalls) != 1 {
		t.Fatalf("stop/delete calls = %d/%d, want 1/1", provider.stopCalls, provider.deleteCalls)
	}
	foundRevocation := false
	for _, options := range provider.execOptions {
		if strings.Contains(options.Stdin, `"generation": "g2"`) {
			foundRevocation = true
		}
	}
	if !foundRevocation {
		t.Fatal("old runner did not receive a mismatching G2 revocation lease before retirement")
	}
}

func TestHostTrustReconciliationRevokesButDoesNotRetireBusyOldGeneration(t *testing.T) {
	provider := &fakeProvider{}
	github := &fakeGitHub{runner: gh.Runner{Name: "runner-1", ID: 42, Status: "online", Busy: true}, found: true}
	manager := Manager{
		Config:   config.Config{Image: config.ImageConfig{HostTrustMode: config.HostTrustModeOverlay, HostTrustScopes: []string{"system"}}},
		Provider: provider,
		GitHub:   github,
	}
	current := hosttrust.Snapshot{
		Generation:   "g2",
		HostOS:       "linux",
		Scopes:       []string{"system"},
		Certificates: []hosttrust.Certificate{{Name: "root.crt", PEM: []byte("pem")}},
		CollectedAt:  time.Now().UTC(),
	}
	active := map[string]ProvisionedInstance{"runner-1": {Name: "runner-1", RunnerID: 42, HostTrustGeneration: "g1", ProviderOwned: true, Phase: LifecycleReady}}
	manager.reconcileHostTrustRunners(context.Background(), active, current, make(map[string]bool))
	if len(active) != 1 {
		t.Fatal("busy old-generation runner was retired before its job completed")
	}
	if atomic.LoadInt32(&provider.stopCalls) != 0 || atomic.LoadInt32(&provider.deleteCalls) != 0 {
		t.Fatalf("busy old runner was stopped/deleted: %d/%d", provider.stopCalls, provider.deleteCalls)
	}
	foundRevocation := false
	for _, options := range provider.execOptions {
		if strings.Contains(options.Stdin, `"generation": "g2"`) {
			foundRevocation = true
		}
	}
	if !foundRevocation {
		t.Fatal("busy old runner did not receive a mismatching lease to block a subsequent assignment")
	}
}

func TestHostTrustReconciliationPreservesRunnerDuringGitHub503(t *testing.T) {
	fake := &fakeProvider{}
	github := &fakeGitHub{runnerErr: &gh.HTTPError{StatusCode: http.StatusServiceUnavailable}}
	manager := Manager{
		Config:   config.Config{Image: config.ImageConfig{HostTrustMode: config.HostTrustModeOverlay, HostTrustScopes: []string{"system"}}},
		Provider: fake,
		GitHub:   github,
	}
	current := hosttrust.Snapshot{
		Generation:   "g1",
		HostOS:       "linux",
		Scopes:       []string{"system"},
		Certificates: []hosttrust.Certificate{{Name: "root.crt", PEM: []byte("pem")}},
		CollectedAt:  time.Now().UTC(),
	}
	active := map[string]ProvisionedInstance{"runner-1": {Name: "runner-1", RunnerID: 42, HostTrustGeneration: "g1", ProviderOwned: true, Phase: LifecycleReady}}
	if retired := manager.reconcileHostTrustRunners(context.Background(), active, current, make(map[string]bool)); retired != 0 {
		t.Fatalf("retired runners = %d, want 0 during GitHub 503", retired)
	}
	if len(active) != 1 {
		t.Fatal("GitHub 503 removed the active runner")
	}
	if got := atomic.LoadInt32(&fake.execCalls); got != 0 {
		t.Fatalf("guest lease commands = %d, want 0 when GitHub status is unknown", got)
	}
	if got := atomic.LoadInt32(&fake.deleteCalls); got != 0 {
		t.Fatalf("provider delete calls = %d, want 0 during GitHub 503", got)
	}
}

func TestHostTrustReconciliationFencesWhenTransportVerificationFails(t *testing.T) {
	fake := &fakeProvider{instances: []provider.Instance{{Name: "runner-1", ProviderID: "fake:runner-1", State: "running"}}}
	activator := &activatingLifecycle{
		Lifecycle: provider.AdaptLegacy(fake, false),
		err:       errors.New("relay unavailable"),
		verifyErr: errors.New("relay marker unavailable"),
	}
	github := &fakeGitHub{runner: gh.Runner{Name: "runner-1", ID: 42, Status: "online"}, found: true}
	manager := Manager{
		Config: config.Config{
			Provider: config.ProviderConfig{Type: "docker-sandboxes"},
			Image:    config.ImageConfig{HostTrustMode: config.HostTrustModeOverlay, HostTrustScopes: []string{"system"}},
			Timeouts: config.TimeoutConfig{CommandSeconds: 5},
		},
		Provider:  fake,
		Lifecycle: activator,
		GitHub:    github,
	}
	current := hosttrust.Snapshot{Generation: "g1", HostOS: "windows", Scopes: []string{"system"}, Certificates: []hosttrust.Certificate{{Name: "root.crt", PEM: []byte("pem")}}, CollectedAt: time.Now().UTC()}
	active := map[string]ProvisionedInstance{"runner-1": {Name: "runner-1", ProviderID: "fake:runner-1", RunnerID: 42, HostTrustGeneration: "g1", ProviderOwned: true, Phase: LifecycleReady}}

	if retired := manager.reconcileHostTrustRunners(context.Background(), active, current, make(map[string]bool)); retired != 0 {
		t.Fatalf("retired runners = %d, want fenced preservation", retired)
	}
	if activator.calls != 0 {
		t.Fatalf("activation calls = %d, want zero while fencing the unhealthy registered runner", activator.calls)
	}
	if activator.verifyCalls != 1 {
		t.Fatalf("runtime verification calls = %d, want one before fencing the unhealthy registered runner", activator.verifyCalls)
	}
	if got := atomic.LoadInt32(&github.runnerByNameCalls); got != 2 {
		t.Fatalf("GitHub status calls = %d, want status lookup plus exact registration fence lookup", got)
	}
	if got := atomic.LoadInt32(&github.deleteCalls); got != 1 {
		t.Fatalf("GitHub registration fence calls = %d, want 1", got)
	}
	if got := active["runner-1"].Phase; got != LifecycleQuarantined {
		t.Fatalf("runner phase = %s, want %s", got, LifecycleQuarantined)
	}
	if fake.commandCount("rm -f '/run/epar/host-trust-lease.json'") != 0 {
		t.Fatalf("guest commands = %v, want immediate exact GitHub fence without a second blocked transport attempt", fake.commands)
	}
}

func TestHostTrustReconciliationDoesNotReactivateHealthyTransport(t *testing.T) {
	fake := &fakeProvider{instances: []provider.Instance{{Name: "runner-1", ProviderID: "fake:runner-1", State: "running"}}}
	activator := &activatingLifecycle{Lifecycle: provider.AdaptLegacy(fake, false)}
	github := &fakeGitHub{runner: gh.Runner{Name: "runner-1", ID: 42, Status: "online", Busy: false}, found: true}
	manager := Manager{
		Config: config.Config{
			Provider: config.ProviderConfig{Type: "docker-sandboxes"},
			Image:    config.ImageConfig{HostTrustMode: config.HostTrustModeOverlay, HostTrustScopes: []string{"system"}},
		},
		Provider:  fake,
		Lifecycle: activator,
		GitHub:    github,
	}
	current := hosttrust.Snapshot{Generation: "g1", HostOS: "windows", Scopes: []string{"system"}, Certificates: []hosttrust.Certificate{{Name: "root.crt", PEM: []byte("pem")}}, CollectedAt: time.Now().UTC()}
	active := map[string]ProvisionedInstance{"runner-1": {Name: "runner-1", ProviderID: "fake:runner-1", RunnerID: 42, HostTrustGeneration: "g1", ProviderOwned: true, Phase: LifecycleReady}}

	if retired := manager.reconcileHostTrustRunners(context.Background(), active, current, make(map[string]bool)); retired != 0 {
		t.Fatalf("retired runners = %d, want 0", retired)
	}
	if activator.calls != 0 {
		t.Fatalf("host trust transport activations = %d, want zero for healthy current-generation transport", activator.calls)
	}
	if activator.verifyCalls != 1 {
		t.Fatalf("runtime verification calls = %d, want one for healthy current-generation transport", activator.verifyCalls)
	}
	if got := len(hostTrustLeaseInputs(fake)); got != 1 {
		t.Fatalf("host trust lease writes = %d, want one refresh without relay reactivation", got)
	}
	if got := fake.commandCount("configure-egress-relay.sh"); got != 0 {
		t.Fatalf("relay activation commands = %d, want zero for healthy current-generation transport", got)
	}
}

func TestHostTrustReconciliationFencesBusyRunnerWhenTransportVerificationFails(t *testing.T) {
	fake := &fakeProvider{instances: []provider.Instance{{Name: "runner-1", ProviderID: "fake:runner-1", State: "running"}}}
	activator := &activatingLifecycle{
		Lifecycle: provider.AdaptLegacy(fake, false),
		verifyErr: errors.New("relay marker unavailable"),
	}
	github := &fakeGitHub{runner: gh.Runner{Name: "runner-1", ID: 42, Status: "online", Busy: true}, found: true}
	manager := Manager{
		Config: config.Config{
			Provider: config.ProviderConfig{Type: "docker-sandboxes"},
			Image:    config.ImageConfig{HostTrustMode: config.HostTrustModeOverlay, HostTrustScopes: []string{"system"}},
		},
		Provider:  fake,
		Lifecycle: activator,
		GitHub:    github,
	}
	current := hosttrust.Snapshot{Generation: "g1", HostOS: "windows", Scopes: []string{"system"}, Certificates: []hosttrust.Certificate{{Name: "root.crt", PEM: []byte("pem")}}, CollectedAt: time.Now().UTC()}
	active := map[string]ProvisionedInstance{"runner-1": {Name: "runner-1", ProviderID: "fake:runner-1", RunnerID: 42, HostTrustGeneration: "g1", ProviderOwned: true, Phase: LifecycleReady}}

	manager.reconcileHostTrustRunners(context.Background(), active, current, make(map[string]bool))
	if got := active["runner-1"].Phase; got != LifecycleQuarantined {
		t.Fatalf("runner phase = %s, want %s", got, LifecycleQuarantined)
	}
	if activator.calls != 0 {
		t.Fatalf("host trust transport activations = %d, want zero for failed verification", activator.calls)
	}
	if activator.verifyCalls != 1 {
		t.Fatalf("runtime verification calls = %d, want one before fencing the busy runner", activator.verifyCalls)
	}
	if got := atomic.LoadInt32(&github.deleteCalls); got != 1 {
		t.Fatalf("GitHub registration fence calls = %d, want 1", got)
	}
	if got := len(hostTrustLeaseInputs(fake)); got != 0 {
		t.Fatalf("host trust lease writes = %d, want zero after failed transport verification", got)
	}
}

func TestHostTrustReconciliationFencesRegistrationWhenIdleLeaseRefreshFails(t *testing.T) {
	fake := &fakeProvider{execErrs: []error{errors.New("lease transport unavailable"), nil}}
	github := &fakeGitHub{runner: gh.Runner{Name: "runner-1", ID: 42, Status: "online"}, found: true}
	manager := Manager{
		Config: config.Config{
			Image:    config.ImageConfig{HostTrustMode: config.HostTrustModeOverlay, HostTrustScopes: []string{"system"}},
			Timeouts: config.TimeoutConfig{CommandSeconds: 5},
		},
		Provider: fake,
		GitHub:   github,
	}
	current := hosttrust.Snapshot{Generation: "g1", HostOS: "linux", Scopes: []string{"system"}, Certificates: []hosttrust.Certificate{{Name: "root.crt", PEM: []byte("pem")}}, CollectedAt: time.Now().UTC()}
	active := map[string]ProvisionedInstance{"runner-1": {Name: "runner-1", RunnerID: 42, HostTrustGeneration: "g1", ProviderOwned: true, Phase: LifecycleReady}}

	manager.reconcileHostTrustRunners(context.Background(), active, current, make(map[string]bool))

	if got := atomic.LoadInt32(&github.deleteCalls); got != 1 {
		t.Fatalf("GitHub registration fence calls = %d, want 1", got)
	}
	github.mu.Lock()
	deletedIDs := append([]int64(nil), github.deletedIDs...)
	github.mu.Unlock()
	if len(deletedIDs) != 1 || deletedIDs[0] != 42 {
		t.Fatalf("deleted runner ids = %v, want [42]", deletedIDs)
	}
	if got := active["runner-1"].Phase; got != LifecycleQuarantined {
		t.Fatalf("runner phase = %s, want %s", got, LifecycleQuarantined)
	}
}

func TestHostTrustRegistrationFenceRejectsSameNameDifferentRunnerID(t *testing.T) {
	github := &fakeGitHub{runner: gh.Runner{Name: "runner-1", ID: 43, Status: "online"}, found: true}
	manager := Manager{GitHub: github}
	err := manager.fenceHostTrustRunnerRegistration(context.Background(), ProvisionedInstance{Name: "runner-1", RunnerID: 42}, errors.New("lease unavailable"))
	if err == nil || !strings.Contains(err.Error(), "does not match expected id=42") {
		t.Fatalf("fence error = %v, want exact identity mismatch", err)
	}
	if got := atomic.LoadInt32(&github.deleteCalls); got != 0 {
		t.Fatalf("GitHub delete calls = %d, want 0 for same-name identity mismatch", got)
	}
}

func TestHostTrustReconciliationDoesNotTouchUnownedQuarantinedSandbox(t *testing.T) {
	fake := &fakeProvider{instances: []provider.Instance{{Name: "runner-1", ProviderID: "foreign", State: "running"}}}
	activator := &activatingLifecycle{Lifecycle: provider.AdaptLegacy(fake, false)}
	manager := Manager{
		Config:    config.Config{Image: config.ImageConfig{HostTrustMode: config.HostTrustModeOverlay, HostTrustScopes: []string{"system"}}},
		Provider:  fake,
		Lifecycle: activator,
		GitHub:    &fakeGitHub{},
	}
	active := map[string]ProvisionedInstance{"runner-1": {Name: "runner-1", ProviderID: "foreign", ProviderOwned: false, Phase: LifecycleQuarantined}}
	current := hosttrust.Snapshot{Generation: "g1"}

	manager.reconcileHostTrustRunners(context.Background(), active, current, make(map[string]bool))
	if activator.calls != 0 || atomic.LoadInt32(&fake.execCalls) != 0 {
		t.Fatalf("unowned sandbox mutations = activations %d guest execs %d, want zero", activator.calls, fake.execCalls)
	}
}

func TestHostTrustReconciliationIssuesOneBoundedBusyHandoffLease(t *testing.T) {
	provider := &fakeProvider{}
	github := &fakeGitHub{runner: gh.Runner{Name: "runner-1", ID: 42, Status: "online", Busy: true}, found: true}
	manager := Manager{
		Config:   config.Config{Image: config.ImageConfig{HostTrustMode: config.HostTrustModeOverlay, HostTrustScopes: []string{"system"}}},
		Provider: provider,
		GitHub:   github,
	}
	current := hosttrust.Snapshot{
		Generation:   "g1",
		HostOS:       "linux",
		Scopes:       []string{"system"},
		Certificates: []hosttrust.Certificate{{Name: "root.crt", PEM: []byte("pem")}},
		CollectedAt:  time.Now().UTC(),
	}
	active := map[string]ProvisionedInstance{"runner-1": {Name: "runner-1", RunnerID: 42, HostTrustGeneration: "g1", ProviderOwned: true, Phase: LifecycleReady}}
	busyHandoff := make(map[string]bool)

	manager.reconcileHostTrustRunners(context.Background(), active, current, busyHandoff)
	manager.reconcileHostTrustRunners(context.Background(), active, current, busyHandoff)

	leases := hostTrustLeaseInputs(provider)
	if len(leases) != 1 {
		t.Fatalf("busy handoff lease writes = %d, want 1", len(leases))
	}
	var lease hostTrustLease
	if err := json.Unmarshal([]byte(leases[0]), &lease); err != nil {
		t.Fatal(err)
	}
	expires, err := time.Parse(time.RFC3339Nano, lease.ExpiresAt)
	if err != nil {
		t.Fatal(err)
	}
	if remaining := time.Until(expires); remaining < hostTrustHandoffLease-5*time.Second || remaining > hostTrustHandoffLease+time.Second {
		t.Fatalf("busy handoff lease remaining lifetime = %s, want about %s", remaining, hostTrustHandoffLease)
	}

	github.runner.Busy = false
	manager.reconcileHostTrustRunners(context.Background(), active, current, busyHandoff)
	github.runner.Busy = true
	manager.reconcileHostTrustRunners(context.Background(), active, current, busyHandoff)
	if leases = hostTrustLeaseInputs(provider); len(leases) != 3 {
		t.Fatalf("lease writes after idle and second busy transition = %d, want 3", len(leases))
	}
}

func TestHostTrustLeaseUsesOneBoundedAtomicGuestCommand(t *testing.T) {
	fake := &fakeProvider{}
	manager := Manager{
		Config:   config.Config{Image: config.ImageConfig{HostTrustMode: config.HostTrustModeOverlay, HostTrustScopes: []string{"system"}}},
		Provider: fake,
	}
	snapshot := hosttrust.Snapshot{
		Generation:   "g1",
		HostOS:       "linux",
		Scopes:       []string{"system"},
		Certificates: []hosttrust.Certificate{{Name: "root.crt", PEM: []byte("pem")}},
		CollectedAt:  time.Now().UTC(),
	}
	if err := manager.issueHostTrustLease(context.Background(), "runner-1", snapshot); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&fake.execCalls); got != 1 {
		t.Fatalf("guest commands = %d, want one atomic lease write", got)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.commands) != 1 || !strings.Contains(fake.commands[0], "install -d -m 0755 /run/epar") || !strings.Contains(fake.commands[0], "mv -f") {
		t.Fatalf("lease command = %q, want directory creation and atomic rename in one command", strings.Join(fake.commands, "\n"))
	}
	if len(fake.execOptions) != 1 || !strings.Contains(fake.execOptions[0].Stdin, `"expiresAt"`) {
		t.Fatal("lease payload was not supplied to the atomic guest command")
	}
}

func TestHostTrustLeaseWriteFailureAttemptsExactFence(t *testing.T) {
	fake := &fakeProvider{execErrs: []error{errors.New("lease write failed"), nil}}
	manager := Manager{
		Config:   config.Config{Image: config.ImageConfig{HostTrustMode: config.HostTrustModeOverlay, HostTrustScopes: []string{"system"}}, Timeouts: config.TimeoutConfig{CommandSeconds: 5}},
		Provider: fake,
	}
	snapshot := hosttrust.Snapshot{Generation: "g1", HostOS: "windows", Scopes: []string{"system"}, Certificates: []hosttrust.Certificate{{Name: "root.crt", PEM: []byte("pem")}}, CollectedAt: time.Now().UTC()}

	if err := manager.issueHostTrustLease(context.Background(), "runner-1", snapshot); err == nil || !strings.Contains(err.Error(), "lease write failed") {
		t.Fatalf("issueHostTrustLease() error = %v, want original write failure", err)
	}
	if got := atomic.LoadInt32(&fake.execCalls); got != 2 {
		t.Fatalf("guest calls = %d, want failed write plus fence", got)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.commands) != 2 || !strings.Contains(fake.commands[0], "rm -f") || !strings.Contains(fake.commands[0], "mv -f") || !strings.Contains(fake.commands[1], "rm -f") || strings.Contains(fake.commands[1], "cat >") {
		t.Fatalf("lease replacement/fence commands = %v", fake.commands)
	}
}

func TestHostTrustLeaseWriteTimeoutIsReportedWithoutBlocking(t *testing.T) {
	oldTimeout := hostTrustWriteTimeout
	hostTrustWriteTimeout = 5 * time.Millisecond
	t.Cleanup(func() { hostTrustWriteTimeout = oldTimeout })
	fake := &fakeProvider{execFunc: func(ctx context.Context, _ string, _ []string, _ provider.ExecOptions) (provider.ExecResult, error) {
		<-ctx.Done()
		return provider.ExecResult{}, ctx.Err()
	}}
	manager := Manager{
		Config:   config.Config{Image: config.ImageConfig{HostTrustMode: config.HostTrustModeOverlay, HostTrustScopes: []string{"system"}}},
		Provider: fake,
	}
	snapshot := hosttrust.Snapshot{
		Generation:   "g1",
		HostOS:       "linux",
		Scopes:       []string{"system"},
		Certificates: []hosttrust.Certificate{{Name: "root.crt", PEM: []byte("pem")}},
		CollectedAt:  time.Now().UTC(),
	}
	started := time.Now()
	err := manager.issueHostTrustLease(context.Background(), "runner-1", snapshot)
	if err == nil || !strings.Contains(err.Error(), "host trust lease write exceeded") {
		t.Fatalf("issueHostTrustLease() error = %v, want bounded-timeout detail", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("lease timeout took %s, want less than one second", elapsed)
	}
}

func hostTrustLeaseInputs(provider *fakeProvider) []string {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	var leases []string
	for _, options := range provider.execOptions {
		if strings.Contains(options.Stdin, `"expiresAt"`) {
			leases = append(leases, options.Stdin)
		}
	}
	return leases
}

func TestHostTrustImageBuildRetriesChangedGenerationBeforePublishing(t *testing.T) {
	t.Setenv("EPAR_STATE_HOME", filepath.Join(t.TempDir(), "host-state"))
	root := t.TempDir()
	for _, dir := range []string{
		filepath.Join(root, "scripts", "guest", "ubuntu"),
		filepath.Join(root, "scripts", "container", "ubuntu"),
	} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	firstPath := filepath.Join(root, "first.pem")
	secondPath := filepath.Join(root, "second.pem")
	writeTestCACertificate(t, firstPath, "Host Root G1")
	writeTestCACertificate(t, secondPath, "Host Root G2")
	g1 := hostTrustSnapshotFromFile(t, firstPath, "windows", []string{"system", "user"})
	g2 := hostTrustSnapshotFromFile(t, secondPath, "windows", []string{"system", "user"})
	var buildTrustBundleContent strings.Builder
	for _, certificate := range g1.Certificates {
		buildTrustBundleContent.Write(certificate.PEM)
	}
	sequence := []hosttrust.Snapshot{g1, g2, g2, g2}
	manager := Manager{
		Config: config.Config{
			Image: config.ImageConfig{
				SourceType:      config.ImageSourceDockerImage,
				SourceImage:     "source:latest",
				OutputImage:     "runner:latest",
				RunnerVersion:   "latest",
				UpstreamLock:    "missing.lock",
				HostTrustMode:   config.HostTrustModeOverlay,
				HostTrustScopes: []string{"system", "user"},
			},
			Provider: config.ProviderConfig{Type: "docker-container"},
			Runner:   config.RunnerConfig{Ephemeral: true},
			Logging:  config.LoggingConfig{Directory: "work/logs"},
		},
		ProjectRoot: root,
	}
	index := 0
	manager.hostTrustResolver = func(context.Context) (hosttrust.Snapshot, error) {
		if index >= len(sequence) {
			return g2, nil
		}
		value := sequence[index]
		index++
		value.CollectedAt = time.Now().UTC()
		return value, nil
	}
	manager.buildTrustResolver = func(context.Context) (hosttrust.Snapshot, error) {
		value := g1
		value.CollectedAt = time.Now().UTC()
		return value, nil
	}
	oldLogged := runHostLoggedCommand
	oldOutput := runHostOutputCommand
	oldQuiet := runHostQuietCommand
	oldRun := runHostCommand
	oldPull := pullDockerSourceCommand
	t.Cleanup(func() {
		runHostLoggedCommand = oldLogged
		runHostOutputCommand = oldOutput
		runHostQuietCommand = oldQuiet
		runHostCommand = oldRun
		pullDockerSourceCommand = oldPull
	})
	builds := 0
	tagged := false
	runHostLoggedCommand = func(_ context.Context, _ string, _, _ io.Writer, name string, args ...string) error {
		if name == "docker" && len(args) > 1 && args[0] == "buildx" && args[1] == "build" {
			builds++
		}
		return nil
	}
	runHostOutputCommand = func(_ context.Context, _ string, args ...string) (string, error) {
		if len(args) > 1 && args[0] == "buildx" && args[1] == "inspect" {
			if len(args) > 2 && args[2] == "--bootstrap" {
				return "Status: running\n", nil
			}
			return "", errors.New("builder not found")
		}
		if len(args) > 3 && args[0] == "exec" && strings.Contains(args[3], "/certs/") {
			return buildTrustBundleContent.String(), nil
		}
		if len(args) > 1 && args[0] == "exec" {
			return "# epar-build-trust-generation=" + g1.Generation + "\n[registry.\"docker.io\"]\n", nil
		}
		return `["source@sha256:1234"]`, nil
	}
	runHostQuietCommand = func(context.Context, string, ...string) error { return nil }
	pullDockerSourceCommand = func(*Manager, context.Context, dockerSourcePullOptions) error { return nil }
	runHostCommand = func(_ context.Context, name string, args ...string) error {
		if name == "docker" && len(args) >= 4 && args[0] == "image" && args[1] == "tag" {
			tagged = true
		}
		return nil
	}
	runnerPackage := []byte("test-actions-runner-package")
	runnerServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		_, _ = response.Write(runnerPackage)
	}))
	defer runnerServer.Close()
	runnerDigest := sha256.Sum256(runnerPackage)
	manifest := ImageManifest{
		SchemaVersion:     imageManifestSchemaVersion,
		ProviderType:      "docker-container",
		SourceType:        config.ImageSourceDockerImage,
		SourceImage:       "source:latest",
		OutputImage:       "output:latest",
		RunnerSelector:    "latest",
		RunnerVersion:     "2.332.0",
		RunnerAssetName:   "actions-runner-linux-x64-2.332.0.tar.gz",
		RunnerAssetURL:    runnerServer.URL,
		RunnerAssetDigest: fmt.Sprintf("sha256:%x", runnerDigest),
	}
	if err := manager.buildDockerContainerImage(context.Background(), ImageBuildOptions{Replace: true, Manifest: &manifest}, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if builds != 2 {
		t.Fatalf("docker builds = %d, want 2", builds)
	}
	if !tagged {
		t.Fatal("stable generation was not published to the configured image tag")
	}
}

func TestBuildTrustScopesAreIndependentFromDisabledRunnerOverlay(t *testing.T) {
	if got := buildTrustScopes(config.HostTrustModeDisabled, []string{hosttrust.ScopeSystem, hosttrust.ScopeUser}); !slices.Equal(got, []string{hosttrust.ScopeSystem}) {
		t.Fatalf("disabled runner build scopes = %v, want system only", got)
	}
	if got := buildTrustScopes(config.HostTrustModeOverlay, []string{hosttrust.ScopeUser}); !slices.Equal(got, []string{hosttrust.ScopeSystem, hosttrust.ScopeUser}) {
		t.Fatalf("user-overlay build scopes = %v, want mandatory system plus opted-in user", got)
	}
}

func hostTrustSnapshotFromFile(t *testing.T, path, hostOS string, scopes []string) hosttrust.Snapshot {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	certificates, err := hosttrust.CertificatesFromBytes(content)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := hosttrust.Canonicalize(hosttrust.Snapshot{
		HostOS:       hostOS,
		Scopes:       scopes,
		Certificates: certificates,
		CollectedAt:  time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func assertSingleCertificateFile(t *testing.T, path string) {
	t.Helper()
	entries, err := os.ReadDir(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || !strings.HasSuffix(entries[0].Name(), ".crt") {
		t.Fatalf("certificate directory %s entries = %#v", path, entries)
	}
}
