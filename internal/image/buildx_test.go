package image

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBuildxBuilderNameIsStableAndProjectScoped(t *testing.T) {
	first := buildxBuilderName(filepath.Join("one", "project"))
	second := buildxBuilderName(filepath.Join("one", "project"))
	other := buildxBuilderName(filepath.Join("two", "project"))
	if first != second {
		t.Fatalf("builder names are not stable: %q != %q", first, second)
	}
	if first == other {
		t.Fatalf("different projects share builder name %q", first)
	}
	if !strings.HasPrefix(first, "epar-") || len(first) != len("epar-")+12 {
		t.Fatalf("builder name %q does not use the bounded EPAR identity", first)
	}
}

func TestLoadBuildxMetadataRequiresExactOwnershipFields(t *testing.T) {
	root := t.TempDir()
	path := BuildxMetadataPath(root)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	metadata := BuildxMetadata{
		SchemaVersion: buildxMetadataSchemaVersion,
		Builder:       buildxBuilderName(root),
		Driver:        "docker-container",
		ProjectRoot:   root,
		CacheLimit:    "64GiB",
		ConfigPath:    filepath.Join(root, ".local", "storage", "buildkitd.toml"),
		CreatedAt:     time.Now().UTC(),
	}
	content, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadBuildxMetadata(root)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Builder != metadata.Builder || loaded.CacheLimit != "64GiB" {
		t.Fatalf("loaded metadata = %+v, want %+v", loaded, metadata)
	}

	metadata.Driver = "docker"
	content, _ = json.Marshal(metadata)
	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadBuildxMetadata(root); err == nil {
		t.Fatal("LoadBuildxMetadata accepted a shared Docker driver")
	}
}

func TestBuildRegistryHostsUsesDockerHubForUnqualifiedImages(t *testing.T) {
	got, err := buildRegistryHosts([]string{
		"source:latest",
		"docker.io/docker/dockerfile:1",
		"ghcr.io/catthehacker/ubuntu@sha256:abc",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"docker.io", "ghcr.io"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("registry hosts = %v, want %v", got, want)
	}
}

func TestBuildkitConfigIsDeterministicAndEscapesPaths(t *testing.T) {
	first := buildkitConfig(20<<30, "generation", `C:\repo path\.local\ca.pem`, []string{"docker.io", "ghcr.io"})
	second := buildkitConfig(20<<30, "generation", `C:\repo path\.local\ca.pem`, []string{"docker.io", "ghcr.io"})
	if string(first) != string(second) {
		t.Fatal("BuildKit configuration is not deterministic")
	}
	text := string(first)
	for _, want := range []string{
		"# epar-build-trust-generation=generation",
		`[registry."docker.io"]`,
		`[registry."ghcr.io"]`,
		`"C:/repo path/.local/ca.pem"`,
		`maxUsedSpace = "21474836480B"`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("BuildKit configuration omitted %q:\n%s", want, text)
		}
	}
}

func TestParseBuildxUsageBytesUsesSummaryOrSumsRecords(t *testing.T) {
	for _, test := range []struct {
		content string
		want    uint64
	}{
		{content: `[{"ID":"a","Size":1024},{"ID":"b","Size":2048}]`, want: 3072},
		{content: "{\"ID\":\"a\",\"Size\":1024}\n{\"Total\":4096}\n", want: 4096},
		{content: `[{"ID":"a","Size":"4.128kB"},{"ID":"b","Size":"310.8MB"},{"ID":"c","Size":"15.22GB"}]`, want: 15_530_804_128},
	} {
		got, err := parseBuildxUsageBytes([]byte(test.content))
		if err != nil {
			t.Fatal(err)
		}
		if got != test.want {
			t.Fatalf("parseBuildxUsageBytes() = %d, want %d", got, test.want)
		}
	}
}

func TestBuildxSchemaOneMetadataIsOwnedButRequiresUpgrade(t *testing.T) {
	root := t.TempDir()
	expected := BuildxMetadata{
		SchemaVersion:     buildxMetadataSchemaVersion,
		Builder:           buildxBuilderName(root),
		Driver:            "docker-container",
		ProjectRoot:       root,
		CacheLimit:        "64GiB",
		ConfigPath:        filepath.Join(root, ".local", "storage", "buildkitd.toml"),
		ConfigSHA256:      strings.Repeat("a", 64),
		TrustGeneration:   strings.Repeat("b", 64),
		CertificateBundle: filepath.Join(root, ".local", "storage", "buildkit-certs", "g", "ca.pem"),
		CertificateSHA256: strings.Repeat("c", 64),
		RegistryHosts:     []string{"docker.io"},
	}
	legacy := expected
	legacy.SchemaVersion = 1
	legacy.ConfigSHA256 = ""
	legacy.TrustGeneration = ""
	legacy.CertificateBundle = ""
	legacy.CertificateSHA256 = ""
	legacy.RegistryHosts = nil
	if !buildxOwnershipMatches(legacy, expected) {
		t.Fatal("schema-one EPAR metadata lost exact ownership")
	}
	if buildxMetadataMatches(legacy, expected) {
		t.Fatal("schema-one EPAR metadata did not require an owned builder upgrade")
	}
}
