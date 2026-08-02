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
	project := filepath.Join("one", "project")
	config := filepath.Join(project, ".local", "config.yml")
	first, err := buildxBuilderNameForConfig(project, config)
	if err != nil {
		t.Fatal(err)
	}
	second, err := buildxBuilderNameForConfig(project, config)
	if err != nil {
		t.Fatal(err)
	}
	other, err := buildxBuilderNameForConfig(filepath.Join("two", "project"), filepath.Join("two", "project", ".local", "config.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("builder names are not stable: %q != %q", first, second)
	}
	if first == other {
		t.Fatalf("different projects share builder name %q", first)
	}
	if !strings.HasPrefix(first, "epar-") || len(first) != len("epar-")+24 {
		t.Fatalf("builder name %q does not use the bounded EPAR identity", first)
	}
}

func TestBuildxScopeSeparatesConfigurationsInOneProject(t *testing.T) {
	root := t.TempDir()
	firstConfig := filepath.Join(root, ".local", "config.yml")
	secondConfig := filepath.Join(root, ".local", "config.docker-container.yml")
	first, err := resolveBuildxScope(root, firstConfig)
	if err != nil {
		t.Fatal(err)
	}
	second, err := resolveBuildxScope(root, secondConfig)
	if err != nil {
		t.Fatal(err)
	}
	firstAgain, err := resolveBuildxScope(root, firstConfig)
	if err != nil {
		t.Fatal(err)
	}
	if first.configID != firstAgain.configID || first.metadataPath != firstAgain.metadataPath || first.lockPath != firstAgain.lockPath {
		t.Fatalf("same configuration scope is not stable: first=%+v again=%+v", first, firstAgain)
	}
	for _, pair := range [][2]string{{first.configID, second.configID}, {first.metadataPath, second.metadataPath}, {first.lockPath, second.lockPath}, {first.buildkitConfig, second.buildkitConfig}, {first.certificateDir, second.certificateDir}} {
		if pair[0] == pair[1] {
			t.Fatalf("distinct configuration scopes unexpectedly share %q", pair[0])
		}
	}
}

func TestBuildxScopeUsesCanonicalConfigurationIdentity(t *testing.T) {
	root := t.TempDir()
	realConfig := filepath.Join(root, ".local", "config.yml")
	linkConfig := filepath.Join(root, ".local", "config-link.yml")
	if err := os.MkdirAll(filepath.Dir(realConfig), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(realConfig, []byte("provider: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realConfig, linkConfig); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	realScope, err := resolveBuildxScope(root, realConfig)
	if err != nil {
		t.Fatal(err)
	}
	linkScope, err := resolveBuildxScope(root, linkConfig)
	if err != nil {
		t.Fatal(err)
	}
	if realScope != linkScope {
		t.Fatalf("Buildx scope split through a config symlink: real=%+v link=%+v", realScope, linkScope)
	}
}

func TestLoadBuildxMetadataRequiresExactOwnershipFields(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, ".local", "config.yml")
	path, err := BuildxMetadataPathForConfig(root, configPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	scope, err := resolveBuildxScope(root, configPath)
	if err != nil {
		t.Fatal(err)
	}
	builder, err := buildxBuilderNameForConfig(root, configPath)
	if err != nil {
		t.Fatal(err)
	}
	metadata := BuildxMetadata{
		SchemaVersion:  buildxMetadataSchemaVersion,
		Builder:        builder,
		Driver:         "docker-container",
		ProjectRoot:    scope.projectRoot,
		ConfigID:       scope.configID,
		EPARConfigPath: scope.configPath,
		CacheLimit:     "64GiB",
		ConfigPath:     scope.buildkitConfig,
		CreatedAt:      time.Now().UTC(),
	}
	content, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadBuildxMetadataForConfig(root, configPath)
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
	if _, err := LoadBuildxMetadataForConfig(root, configPath); err == nil {
		t.Fatal("LoadBuildxMetadata accepted a shared Docker driver")
	}
}

func TestLoadLegacyBuildxMetadataRetainsExactInventoryEvidence(t *testing.T) {
	root := t.TempDir()
	metadata := BuildxMetadata{
		SchemaVersion: legacyBuildxMaxSchemaVersion,
		Builder:       legacyBuildxBuilderName(root),
		Driver:        "docker-container",
		ProjectRoot:   root,
		CacheLimit:    "20GiB",
		ConfigPath:    filepath.Join(root, ".local", "storage", "buildkitd.toml"),
		CreatedAt:     time.Now().UTC(),
	}
	content, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	path := LegacyBuildxMetadataPath(root)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadLegacyBuildxMetadata(root)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Builder != metadata.Builder || loaded.ProjectRoot != root {
		t.Fatalf("legacy metadata = %+v, want %+v", loaded, metadata)
	}
	metadata.Builder = "epar-unowned"
	content, _ = json.Marshal(metadata)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadLegacyBuildxMetadata(root); err == nil {
		t.Fatal("legacy metadata accepted a builder not derived from its project root")
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
	configPath := filepath.Join(root, ".local", "config.yml")
	scope, err := resolveBuildxScope(root, configPath)
	if err != nil {
		t.Fatal(err)
	}
	builder, err := buildxBuilderNameForConfig(root, configPath)
	if err != nil {
		t.Fatal(err)
	}
	expected := BuildxMetadata{
		SchemaVersion:     buildxMetadataSchemaVersion,
		Builder:           builder,
		Driver:            "docker-container",
		ProjectRoot:       scope.projectRoot,
		ConfigID:          scope.configID,
		EPARConfigPath:    scope.configPath,
		CacheLimit:        "64GiB",
		ConfigPath:        scope.buildkitConfig,
		ConfigSHA256:      strings.Repeat("a", 64),
		TrustGeneration:   strings.Repeat("b", 64),
		CertificateBundle: filepath.Join(root, ".local", "storage", "buildkit-certs", "g", "ca.pem"),
		CertificateSHA256: strings.Repeat("c", 64),
		RegistryHosts:     []string{"docker.io"},
	}
	legacy := expected
	legacy.SchemaVersion = buildxMetadataSchemaVersion - 1
	legacy.ConfigSHA256 = ""
	legacy.TrustGeneration = ""
	legacy.CertificateBundle = ""
	legacy.CertificateSHA256 = ""
	legacy.RegistryHosts = nil
	if !buildxOwnershipMatches(legacy, expected) {
		t.Fatal("previous EPAR metadata lost exact ownership")
	}
	if buildxMetadataMatches(legacy, expected) {
		t.Fatal("schema-one EPAR metadata did not require an owned builder upgrade")
	}
}
