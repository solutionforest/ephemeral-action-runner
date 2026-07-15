package pool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
	gh "github.com/solutionforest/ephemeral-action-runner/internal/github"
	"github.com/solutionforest/ephemeral-action-runner/internal/hosttrust"
)

func TestDockerDindBuildContextKeepsHostAndExplicitTrustSeparate(t *testing.T) {
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
	if err := manager.prepareDockerDindBuildContextWithHostTrust(buildContext, t.TempDir(), `{"hash":"test"}`+"\n", snapshot); err != nil {
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
			Image: config.ImageConfig{HostTrustMode: config.HostTrustModeOverlay, HostTrustScopes: []string{"system"}},
			Pool:  config.PoolConfig{LogDir: t.TempDir()},
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
	active := map[string]ProvisionedInstance{"runner-1": {Name: "runner-1", RunnerID: 42, HostTrustGeneration: "g1"}}
	manager.reconcileHostTrustRunners(context.Background(), active, current)
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
	active := map[string]ProvisionedInstance{"runner-1": {Name: "runner-1", RunnerID: 42, HostTrustGeneration: "g1"}}
	manager.reconcileHostTrustRunners(context.Background(), active, current)
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

func TestHostTrustImageBuildRetriesChangedGenerationBeforePublishing(t *testing.T) {
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
			Provider: config.ProviderConfig{Type: "docker-dind"},
			Runner:   config.RunnerConfig{Ephemeral: true},
			Pool:     config.PoolConfig{LogDir: "work/logs"},
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
	runHostLoggedCommand = func(_ context.Context, _ string, name string, args ...string) error {
		if name == "docker" && len(args) > 0 && args[0] == "build" {
			builds++
		}
		return nil
	}
	runHostOutputCommand = func(context.Context, string, ...string) (string, error) {
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
	if err := manager.buildDockerDindImage(context.Background(), ImageBuildOptions{Replace: true}, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if builds != 2 {
		t.Fatalf("docker builds = %d, want 2", builds)
	}
	if !tagged {
		t.Fatal("stable generation was not published to the configured image tag")
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
