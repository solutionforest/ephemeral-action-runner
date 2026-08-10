package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/storage"
	"github.com/solutionforest/ephemeral-action-runner/internal/storage/inventory"
)

func TestDockerImageProvider(t *testing.T) {
	tests := map[string]string{
		"epar-docker-sandboxes-catthehacker-full": "docker-sandboxes",
		"epar-docker-container-catthehacker-full": "docker-container",
		"epar-docker-dind-act":                    "",
		"epar-dev-toolchain":                      "",
	}
	for repository, want := range tests {
		if got := dockerImageProvider(repository); got != want {
			t.Fatalf("dockerImageProvider(%q) = %q, want %q", repository, got, want)
		}
	}
}

func TestStorageProviderMatches(t *testing.T) {
	tests := []struct {
		filter   string
		provider string
		want     bool
	}{
		{filter: "", provider: "docker-container", want: true},
		{filter: "docker-sandboxes", provider: "", want: true},
		{filter: "docker-sandboxes", provider: "docker-sandboxes", want: true},
		{filter: "docker-sandboxes", provider: "docker-container", want: false},
	}
	for _, test := range tests {
		if got := storageProviderMatches(test.filter, test.provider); got != test.want {
			t.Fatalf("storageProviderMatches(%q, %q) = %t, want %t", test.filter, test.provider, got, test.want)
		}
	}
}

func TestParseDockerSize(t *testing.T) {
	tests := map[string]uint64{
		"0B":      0,
		"972.1MB": 972_100_000,
		"17.44GB": 17_440_000_000,
		"1.5GiB":  1_610_612_736,
		"unknown": 0,
	}
	for input, want := range tests {
		if got := parseDockerSize(input); got != want {
			t.Fatalf("parseDockerSize(%q) = %d, want %d", input, got, want)
		}
	}
}

func TestParseBuildxDiskUsageArray(t *testing.T) {
	got, err := parseBuildxDiskUsage([]byte(`[{"ID":"a","Size":1024},{"ID":"b","Size":"1KiB"}]`))
	if err != nil {
		t.Fatal(err)
	}
	if got != 2048 {
		t.Fatalf("parseBuildxDiskUsage() = %d, want 2048", got)
	}
}

func TestParseBuildxDiskUsageNDJSONSummary(t *testing.T) {
	got, err := parseBuildxDiskUsage([]byte("{\"ID\":\"a\",\"Size\":1024}\n{\"Total\":4096}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got != 4096 {
		t.Fatalf("parseBuildxDiskUsage() = %d, want 4096", got)
	}
}

func TestParseBuildxDiskUsageRejectsInvalidJSON(t *testing.T) {
	if _, err := parseBuildxDiskUsage([]byte("{")); err == nil {
		t.Fatal("parseBuildxDiskUsage() error = nil, want invalid JSON error")
	}
}

func TestParseDockerDiskUsageVolumes(t *testing.T) {
	volumes, err := parseDockerDiskUsageVolumes([]byte(`{"Volumes":[{"Name":"epar-project-gocache","Size":"1.5GiB","Labels":"io.solutionforest.epar.cache=gobuild"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(volumes) != 1 || volumes[0].Name != "epar-project-gocache" || volumes[0].Size != "1.5GiB" {
		t.Fatalf("unexpected Docker volume records: %#v", volumes)
	}
}

func TestParseDockerLabels(t *testing.T) {
	labels := parseDockerLabels("io.solutionforest.epar.project=abc123,io.solutionforest.epar.cache=gomod,missing")
	if labels["io.solutionforest.epar.project"] != "abc123" || labels["io.solutionforest.epar.cache"] != "gomod" {
		t.Fatalf("unexpected labels: %#v", labels)
	}
	if _, found := labels["missing"]; found {
		t.Fatalf("malformed label was accepted: %#v", labels)
	}
}

func TestCollectDockerVolumeRecordsTrustsOnlyExactProjectLabels(t *testing.T) {
	root := t.TempDir()
	projectID := storageProjectID(root)
	snapshot := inventory.Snapshot{ProjectRoot: root, CollectedAt: time.Now().UTC()}
	collectDockerVolumeRecords(&snapshot, "docker-engine", []dockerDiskUsageVolume{
		{
			Name:   "epar-" + projectID + "-gocache",
			Size:   "1GiB",
			Labels: "io.solutionforest.epar.project=" + projectID + ",io.solutionforest.epar.cache=gobuild,io.solutionforest.epar.schema=1,io.solutionforest.epar.root=" + root,
		},
		{Name: "epar-gocache", Size: "4GiB"},
	})
	if len(snapshot.Artifacts) != 2 {
		t.Fatalf("artifact count = %d, want 2", len(snapshot.Artifacts))
	}
	if snapshot.Artifacts[0].Kind != storage.ArtifactGoCache || snapshot.Artifacts[0].Ownership.Kind != storage.OwnershipExact {
		t.Fatalf("exact labelled cache was not trusted: %#v", snapshot.Artifacts[0])
	}
	if snapshot.Artifacts[1].Kind != storage.ArtifactDockerVolume || snapshot.Artifacts[1].Ownership.Kind != storage.OwnershipUnknown {
		t.Fatalf("unlabelled cache was trusted: %#v", snapshot.Artifacts[1])
	}
}

func TestRunEffectiveGoCacheLimitUsesDefaults(t *testing.T) {
	root := t.TempDir()
	output, err := captureStdout(t, func() error {
		return runStorage([]string{"effective-go-cache-limit", "--project-root", root})
	})
	if err != nil {
		t.Fatal(err)
	}
	if output != "10737418240\n" {
		t.Fatalf("effective Go cache limit = %q, want 10GiB in bytes", output)
	}
}

func TestNativeWrappersUseExactBoundedGoCaches(t *testing.T) {
	for _, name := range []string{"build-native-controller.sh", "build-native-controller.ps1"} {
		content, err := os.ReadFile(filepath.Join("..", "..", "scripts", name))
		if err != nil {
			t.Fatal(err)
		}
		text := string(content)
		for _, required := range []string{
			"io.solutionforest.epar.project",
			"io.solutionforest.epar.cache",
			"io.solutionforest.epar.root",
			"effective-go-cache-limit",
			"EPAR_GO_CACHE_LIMIT_BYTES",
		} {
			if !strings.Contains(text, required) {
				t.Fatalf("%s is missing exact bounded Go cache contract %q", name, required)
			}
		}
	}
}
