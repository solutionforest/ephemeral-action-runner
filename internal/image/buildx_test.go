package image

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

func TestConfigScopedBuildxWorkersAreStoppedAfterImageBuilds(t *testing.T) {
	dockerContainerBuild, err := os.ReadFile("build.go")
	if err != nil {
		t.Fatal(err)
	}
	dockerContainerText := string(dockerContainerBuild)
	if !strings.Contains(dockerContainerText, `m.stopBuildxBuilder(stopContext, builder, "release resident memory after the Docker Container build")`) || !strings.Contains(dockerContainerText, `m.warnf("EPAR Buildx builder shutdown warning: %v\n", stopErr)`) {
		t.Fatal("Docker Container image build does not non-fatally stop its exact BuildKit worker on exit")
	}
	sandboxBuild, err := os.ReadFile("docker_sandboxes.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(sandboxBuild), `m.stopBuildxBuilder(stopContext, builder, "release resident memory after the Docker Sandboxes build")`) {
		t.Fatal("Docker Sandboxes template build does not stop its exact BuildKit worker on exit")
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
		BackendID:      "docker:daemon-one",
		CacheLimit:     "64GiB",
		ConfigPath:     scope.buildkitConfig,
		CreatedAt:      time.Now().UTC(),
	}
	content, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), `"dockerBackendId":"docker:daemon-one"`) || strings.Contains(string(content), `"backendId"`) {
		t.Fatalf("schema-5 metadata does not use the authoritative dockerBackendId field: %s", content)
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

func TestLoadBuildxMetadataAcceptsSchemaFourForExactMigration(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, ".local", "config.yml")
	scope, err := resolveBuildxScope(root, configPath)
	if err != nil {
		t.Fatal(err)
	}
	metadata := exactBuildxTestMetadata(scope)
	metadata.SchemaVersion = migratableBuildxMetadataSchemaVersion
	metadata.BackendID = ""
	writeBuildxTestMetadata(t, scope.metadataPath, metadata)
	loaded, err := LoadBuildxMetadataForConfig(root, configPath)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.SchemaVersion != migratableBuildxMetadataSchemaVersion || loaded.BackendID != "" {
		t.Fatalf("schema-4 migration evidence changed during load: %#v", loaded)
	}
	metadata.BackendID = "docker:invented-before-schema-five"
	writeBuildxTestMetadata(t, scope.metadataPath, metadata)
	if _, err := LoadBuildxMetadataForConfig(root, configPath); err == nil {
		t.Fatal("schema-4 metadata with invented backend identity was accepted")
	}
}

func TestLoadBuildxMetadataRejectsMalformedSchemaFiveBackendIdentity(t *testing.T) {
	for _, backendID := range []string{"", "daemon-one", "docker:", " docker:daemon-one", "docker:daemon one", "docker:daemon-one\n", "docker:daemon\x00one"} {
		t.Run(strings.ReplaceAll(backendID, " ", "_"), func(t *testing.T) {
			root := t.TempDir()
			configPath := filepath.Join(root, ".local", "config.yml")
			scope, err := resolveBuildxScope(root, configPath)
			if err != nil {
				t.Fatal(err)
			}
			metadata := exactBuildxTestMetadata(scope)
			metadata.BackendID = backendID
			writeBuildxTestMetadata(t, scope.metadataPath, metadata)
			if _, err := LoadBuildxMetadataForConfig(root, configPath); err == nil {
				t.Fatalf("schema-5 metadata accepted malformed backend ID %q", backendID)
			}
		})
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

func TestBuildxSchemaFourMetadataIsOwnedAndCanMigrateInPlace(t *testing.T) {
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
		BackendID:         "docker:daemon-one",
		CacheLimit:        "64GiB",
		ConfigPath:        scope.buildkitConfig,
		ConfigSHA256:      strings.Repeat("a", 64),
		TrustGeneration:   strings.Repeat("b", 64),
		CertificateBundle: filepath.Join(root, ".local", "storage", "buildkit-certs", "g", "ca.pem"),
		CertificateSHA256: strings.Repeat("c", 64),
		RegistryHosts:     []string{"docker.io"},
	}
	legacy := expected
	legacy.SchemaVersion = migratableBuildxMetadataSchemaVersion
	legacy.BackendID = ""
	if !buildxOwnershipMatches(legacy, expected) {
		t.Fatal("previous EPAR metadata lost exact ownership")
	}
	if reason := buildxRecreateReason(legacy, expected, "Status: running\n"); reason != "" {
		t.Fatalf("healthy schema-4 metadata required builder recreation instead of in-place migration: %s", reason)
	}
	if buildxMetadataMatches(legacy, expected) {
		t.Fatal("schema-4 EPAR metadata unexpectedly matched publishable schema 5")
	}
}

func TestBuildxInspectRecognizesZeroExitNodeErrors(t *testing.T) {
	for _, test := range []struct {
		name   string
		output string
		want   bool
	}{
		{name: "node error", output: "Name: epar-example\nNodes:\n  Name: epar-example0\n  Error: connection refused\n", want: true},
		{name: "case insensitive", output: "error: context deadline exceeded\n", want: true},
		{name: "status error is not node error field", output: "Status: error\n", want: false},
		{name: "healthy", output: "Status: running\n", want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := buildxInspectShowsNodeError(test.output); got != test.want {
				t.Fatalf("buildxInspectShowsNodeError() = %t, want %t", got, test.want)
			}
		})
	}
}

func TestBuildxInspectRecognizesOnlyMissingBuilderDiagnostics(t *testing.T) {
	for _, test := range []struct {
		message string
		want    bool
	}{
		{message: "builder not found", want: true},
		{message: `ERROR: no builder "epar-test" found`, want: true},
		{message: "failed to find instance epar-test", want: true},
		{message: "failed to connect to Docker daemon", want: false},
	} {
		if got := buildxInspectMeansMissing("epar-test", errors.New(test.message)); got != test.want {
			t.Fatalf("buildxInspectMeansMissing(%q) = %t, want %t", test.message, got, test.want)
		}
	}
}

func TestBuildxRecreateReasonDetectsBackendMismatchAndNodeError(t *testing.T) {
	expected := BuildxMetadata{
		SchemaVersion:     buildxMetadataSchemaVersion,
		Builder:           "epar-test",
		Driver:            "docker-container",
		ProjectRoot:       "/project",
		ConfigID:          "test",
		EPARConfigPath:    "/project/config.yml",
		BackendID:         "docker:current",
		ConfigPath:        "/project/buildkitd.toml",
		CertificateBundle: "/project/ca.pem",
	}
	actual := expected
	actual.BackendID = "docker:previous"
	if reason := buildxRecreateReason(actual, expected, "Status: running\n"); !strings.Contains(reason, "recorded Docker backend") {
		t.Fatalf("backend mismatch reason = %q", reason)
	}
	actual = expected
	if reason := buildxRecreateReason(actual, expected, "Error: failed to dial old daemon\n"); !strings.Contains(reason, "node Error") {
		t.Fatalf("node error reason = %q", reason)
	}
	actual = expected
	actual.ConfigSHA256 = "outdated"
	if reason := buildxRecreateReason(actual, expected, "Status: running\n"); !strings.Contains(reason, "configuration or image changed") {
		t.Fatalf("configuration drift reason = %q", reason)
	}
	actual = expected
	multiNode := "Nodes:\nName: epar-test0\nStatus: running\nName: epar-test1\nStatus: inactive\n"
	if reason := buildxRecreateReason(actual, expected, multiNode); !strings.Contains(reason, "2 nodes") {
		t.Fatalf("multi-node drift reason = %q", reason)
	}
	for _, status := range []string{"running", "stopped", "inactive"} {
		inspectOutput := "Nodes:\nName: epar-test0\nStatus: " + status + "\n"
		if reason := buildxRecreateReason(actual, expected, inspectOutput); reason != "" {
			t.Fatalf("healthy single-node %s builder required recreation: %s", status, reason)
		}
	}
}

func TestRemoveOwnedBuildxBuilderTrustsAbsentReadbackAfterRMError(t *testing.T) {
	environment := &buildxCommandEnvironment{builderExists: true, rmErr: errors.New("detach command lost connection")}
	coordinator := Coordinator{environment: environment}
	if err := coordinator.removeOwnedBuildxBuilder(context.Background(), "epar-test"); err != nil {
		t.Fatalf("absent readback did not resolve rm error: %v", err)
	}
	if environment.rmCalls != 1 || environment.builderExists {
		t.Fatalf("rm sequence = calls %d, exists %t", environment.rmCalls, environment.builderExists)
	}
}

func TestRemoveOwnedBuildxBuilderBlocksWhenReadbackStillFindsDefinition(t *testing.T) {
	environment := &buildxCommandEnvironment{builderExists: true, rmErr: errors.New("detach failed"), rmLeavesBuilder: true}
	coordinator := Coordinator{environment: environment}
	err := coordinator.removeOwnedBuildxBuilder(context.Background(), "epar-test")
	if err == nil || !strings.Contains(err.Error(), "remains after detach") {
		t.Fatalf("present readback error = %v", err)
	}
	if environment.rmCalls != 1 || !environment.builderExists {
		t.Fatalf("rm sequence = calls %d, exists %t", environment.rmCalls, environment.builderExists)
	}
}

func TestBuildxMissingControlContainerGetsOneFreshRecovery(t *testing.T) {
	coordinator, scope, metadata, environment := newBuildxReconcileFixture(t, buildxMetadataSchemaVersion)
	expected := schemaFiveBuildxTestExpected(metadata)
	environment.verifyFailures = 1
	retry, err := coordinator.reconcileBuildxBuilderAttempt(context.Background(), scope, metadata.Builder, &metadata, expected, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !retry {
		t.Fatal("missing control container did not request the bounded fresh retry")
	}
	retry, err = coordinator.reconcileBuildxBuilderAttempt(context.Background(), scope, metadata.Builder, &metadata, expected, 1)
	if err != nil {
		t.Fatal(err)
	}
	if retry {
		t.Fatal("successful fresh retry requested another attempt")
	}
	if environment.rmCalls != 1 || environment.createCalls != 1 {
		t.Fatalf("recovery commands = rm %d, create %d; want one each", environment.rmCalls, environment.createCalls)
	}
	loaded, err := LoadBuildxMetadataForConfig(coordinator.ProjectRoot, coordinator.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.SchemaVersion != buildxMetadataSchemaVersion || loaded.BackendID != "docker:daemon-one" {
		t.Fatalf("published metadata = %#v", loaded)
	}
}

func TestBuildxRepeatedVerificationFailureRemainsBlocking(t *testing.T) {
	coordinator, scope, metadata, environment := newBuildxReconcileFixture(t, buildxMetadataSchemaVersion)
	expected := schemaFiveBuildxTestExpected(metadata)
	environment.verifyFailures = 2
	retry, err := coordinator.reconcileBuildxBuilderAttempt(context.Background(), scope, metadata.Builder, &metadata, expected, 0)
	if err != nil || !retry {
		t.Fatalf("first failed verification = retry %t, err %v", retry, err)
	}
	retry, err = coordinator.reconcileBuildxBuilderAttempt(context.Background(), scope, metadata.Builder, &metadata, expected, 1)
	if err == nil || retry || !strings.Contains(err.Error(), "after one fresh retry") {
		t.Fatalf("repeated failed verification = retry %t, err %v", retry, err)
	}
	if environment.rmCalls != 1 || environment.createCalls != 1 {
		t.Fatalf("bounded recovery commands = rm %d, create %d", environment.rmCalls, environment.createCalls)
	}
}

func TestBuildxBackendChangeBeforePublishRetriesOnce(t *testing.T) {
	coordinator, scope, metadata, environment := newBuildxReconcileFixture(t, buildxMetadataSchemaVersion)
	environment.backendIDs = []string{"daemon-one", "daemon-one", "daemon-two", "daemon-two", "daemon-two", "daemon-two"}
	expected := schemaFiveBuildxTestExpected(metadata)
	retry, err := coordinator.reconcileBuildxBuilderAttempt(context.Background(), scope, metadata.Builder, &metadata, expected, 0)
	if err != nil || !retry {
		t.Fatalf("backend change = retry %t, err %v", retry, err)
	}
	if _, err := os.Stat(scope.metadataPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("metadata was published before stable backend verification: %v", err)
	}
	retry, err = coordinator.reconcileBuildxBuilderAttempt(context.Background(), scope, metadata.Builder, &metadata, expected, 1)
	if err != nil || retry {
		t.Fatalf("stable retry = retry %t, err %v", retry, err)
	}
	loaded, err := LoadBuildxMetadataForConfig(coordinator.ProjectRoot, coordinator.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.BackendID != "docker:daemon-two" || environment.rmCalls != 2 || environment.createCalls != 1 {
		t.Fatalf("backend recovery metadata=%#v rm=%d create=%d", loaded, environment.rmCalls, environment.createCalls)
	}
}

func TestBuildxRecordedBackendMismatchDetachesBeforeReuse(t *testing.T) {
	coordinator, scope, metadata, environment := newBuildxReconcileFixture(t, buildxMetadataSchemaVersion)
	metadata.BackendID = "docker:previous-daemon"
	retry, err := coordinator.reconcileBuildxBuilderAttempt(context.Background(), scope, metadata.Builder, &metadata, schemaFiveBuildxTestExpected(metadata), 0)
	if err != nil || retry {
		t.Fatalf("backend mismatch recovery = retry %t, err %v", retry, err)
	}
	if environment.rmCalls != 1 || environment.createCalls != 1 || environment.inspectCalls != 1 {
		t.Fatalf("backend mismatch commands = rm %d, create %d, ordinary inspect %d; want detach, absence readback, create", environment.rmCalls, environment.createCalls, environment.inspectCalls)
	}
	loaded, err := LoadBuildxMetadataForConfig(coordinator.ProjectRoot, coordinator.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.BackendID != "docker:daemon-one" {
		t.Fatalf("recovered backend = %q", loaded.BackendID)
	}
}

func TestBuildxExistingBuilderWithoutOwnershipIsRefused(t *testing.T) {
	coordinator, scope, metadata, environment := newBuildxReconcileFixture(t, buildxMetadataSchemaVersion)
	_, err := coordinator.reconcileBuildxBuilderAttempt(context.Background(), scope, metadata.Builder, nil, schemaFiveBuildxTestExpected(metadata), 0)
	if err == nil || !strings.Contains(err.Error(), "refusing to adopt or remove") {
		t.Fatalf("unknown ownership error = %v", err)
	}
	if environment.rmCalls != 0 || environment.createCalls != 0 {
		t.Fatalf("unknown builder was mutated: rm %d, create %d", environment.rmCalls, environment.createCalls)
	}
}

func TestHealthySchemaFourAndFiveBuildersAreReused(t *testing.T) {
	for _, schemaVersion := range []int{migratableBuildxMetadataSchemaVersion, buildxMetadataSchemaVersion} {
		t.Run(fmt.Sprintf("schema-%d", schemaVersion), func(t *testing.T) {
			coordinator, scope, metadata, environment := newBuildxReconcileFixture(t, schemaVersion)
			expected := schemaFiveBuildxTestExpected(metadata)
			if reason := buildxRecreateReason(metadata, expected, environment.inspectOutput); reason != "" {
				t.Fatalf("healthy fixture requires recreation before reconciliation: %s; actual=%#v expected=%#v", reason, metadata, expected)
			}
			retry, err := coordinator.reconcileBuildxBuilderAttempt(context.Background(), scope, metadata.Builder, &metadata, expected, 0)
			if err != nil || retry {
				t.Fatalf("healthy reuse = retry %t, err %v", retry, err)
			}
			if environment.rmCalls != 0 || environment.createCalls != 0 {
				t.Fatalf("healthy builder was recreated: rm %d, create %d", environment.rmCalls, environment.createCalls)
			}
			loaded, err := LoadBuildxMetadataForConfig(coordinator.ProjectRoot, coordinator.ConfigPath)
			if err != nil {
				t.Fatal(err)
			}
			if loaded.SchemaVersion != buildxMetadataSchemaVersion || loaded.BackendID != "docker:daemon-one" {
				t.Fatalf("healthy reuse did not publish schema 5: %#v", loaded)
			}
		})
	}
}

func TestBuildxCacheHousekeepingSkipsNodeErrorWithoutDU(t *testing.T) {
	coordinator, scope, metadata, environment := newBuildxReconcileFixture(t, buildxMetadataSchemaVersion)
	writeBuildxTestMetadata(t, scope.metadataPath, metadata)
	environment.inspectOutput = "Status: error\nError: failed to dial stale endpoint\n"
	if err := coordinator.enforceDedicatedBuildxCache(context.Background()); err != nil {
		t.Fatal(err)
	}
	if environment.duCalls != 0 {
		t.Fatalf("housekeeping ran buildx du %d times for a stale builder", environment.duCalls)
	}
	if !strings.Contains(strings.Join(environment.infos, "\n"), "reconciliation will occur when a build is required") {
		t.Fatalf("housekeeping did not explain stale-builder deferral: %v", environment.infos)
	}
}

func TestBuildxCacheHousekeepingSkipsUnattributedOrDifferentBackend(t *testing.T) {
	for _, schemaVersion := range []int{migratableBuildxMetadataSchemaVersion, buildxMetadataSchemaVersion} {
		t.Run(fmt.Sprintf("schema-%d", schemaVersion), func(t *testing.T) {
			coordinator, scope, metadata, environment := newBuildxReconcileFixture(t, schemaVersion)
			if schemaVersion == buildxMetadataSchemaVersion {
				metadata.BackendID = "docker:different-daemon"
			}
			writeBuildxTestMetadata(t, scope.metadataPath, metadata)
			if err := coordinator.enforceDedicatedBuildxCache(context.Background()); err != nil {
				t.Fatal(err)
			}
			if environment.inspectCalls != 0 || environment.duCalls != 0 {
				t.Fatalf("housekeeping probed stale cache: inspect %d, du %d", environment.inspectCalls, environment.duCalls)
			}
			if !strings.Contains(strings.Join(environment.infos, "\n"), "reconciliation will occur when a build is required") {
				t.Fatalf("housekeeping did not explain backend-attribution deferral: %v", environment.infos)
			}
		})
	}
}

func TestStopBuildxBuilderReturnsShutdownFailureWithoutRemovingState(t *testing.T) {
	stopErr := errors.New("stop failed")
	environment := &buildxCommandEnvironment{stopErr: stopErr}
	coordinator := Coordinator{environment: environment}
	err := coordinator.stopBuildxBuilder(context.Background(), "epar-test", "release memory")
	if !errors.Is(err, stopErr) || environment.stopCalls != 1 || environment.rmCalls != 0 {
		t.Fatalf("stop result = %v, stop calls %d, rm calls %d", err, environment.stopCalls, environment.rmCalls)
	}
}

func exactBuildxTestMetadata(scope buildxScope) BuildxMetadata {
	return BuildxMetadata{
		SchemaVersion:     buildxMetadataSchemaVersion,
		Builder:           "epar-" + scope.configID,
		Driver:            "docker-container",
		ProjectRoot:       scope.projectRoot,
		ConfigID:          scope.configID,
		EPARConfigPath:    scope.configPath,
		BackendID:         "docker:daemon-one",
		CacheLimit:        "20GiB",
		ConfigPath:        scope.buildkitConfig,
		ConfigSHA256:      strings.Repeat("a", 64),
		TrustGeneration:   strings.Repeat("b", 64),
		CertificateBundle: filepath.Join(scope.certificateDir, "generation", "ca.pem"),
		CertificateSHA256: strings.Repeat("c", 64),
		BuildKitImageID:   "sha256:buildkit",
		CreatedAt:         time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
	}
}

func schemaFiveBuildxTestExpected(metadata BuildxMetadata) BuildxMetadata {
	metadata.SchemaVersion = buildxMetadataSchemaVersion
	metadata.BackendID = "docker:daemon-one"
	return metadata
}

func writeBuildxTestMetadata(t *testing.T, path string, metadata BuildxMetadata) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	content, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
}

func newBuildxReconcileFixture(t *testing.T, schemaVersion int) (Coordinator, buildxScope, BuildxMetadata, *buildxCommandEnvironment) {
	t.Helper()
	t.Setenv("EPAR_STATE_HOME", filepath.Join(t.TempDir(), "host-state"))
	root := t.TempDir()
	configPath := filepath.Join(root, ".local", "config.yml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte("provider:\n  type: docker-container\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	scope, err := resolveBuildxScope(root, configPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(scope.metadataPath), 0o700); err != nil {
		t.Fatal(err)
	}
	metadata := exactBuildxTestMetadata(scope)
	metadata.SchemaVersion = schemaVersion
	if schemaVersion == migratableBuildxMetadataSchemaVersion {
		metadata.BackendID = ""
	}
	environment := &buildxCommandEnvironment{builderExists: true, inspectOutput: "Status: running\n"}
	coordinator := Coordinator{ProjectRoot: root, ConfigPath: configPath, environment: environment}
	return coordinator, scope, metadata, environment
}

type buildxCommandEnvironment struct {
	Environment
	builderExists     bool
	inspectOutput     string
	backendIDs        []string
	rmLeavesBuilder   bool
	rmErr             error
	stopErr           error
	verifyFailures    int
	rmCalls           int
	createCalls       int
	stopCalls         int
	inspectCalls      int
	imageInspectCalls int
	dockerPSCalls     int
	duCalls           int
	infos             []string
	warnings          []string
}

func (environment *buildxCommandEnvironment) Infof(format string, args ...any) {
	environment.infos = append(environment.infos, fmt.Sprintf(format, args...))
}

func (environment *buildxCommandEnvironment) Warnf(format string, args ...any) {
	environment.warnings = append(environment.warnings, fmt.Sprintf(format, args...))
}

func (environment *buildxCommandEnvironment) RunHost(_ context.Context, name string, args ...string) error {
	command := strings.Join(append([]string{name}, args...), " ")
	switch {
	case command == "docker pull "+buildkitImageReference:
		return nil
	case strings.HasPrefix(command, "docker buildx create "):
		environment.createCalls++
		environment.builderExists = true
		return nil
	case strings.HasPrefix(command, "docker buildx rm --keep-state --force "):
		environment.rmCalls++
		if !environment.rmLeavesBuilder {
			environment.builderExists = false
		}
		return environment.rmErr
	case strings.HasPrefix(command, "docker buildx stop "):
		environment.stopCalls++
		return environment.stopErr
	default:
		return fmt.Errorf("unexpected host command: %s", command)
	}
}

func (environment *buildxCommandEnvironment) RunHostOutput(_ context.Context, name string, args ...string) (string, error) {
	command := strings.Join(append([]string{name}, args...), " ")
	switch {
	case command == "docker info --format {{.ID}}":
		if len(environment.backendIDs) > 0 {
			backendID := environment.backendIDs[0]
			environment.backendIDs = environment.backendIDs[1:]
			return backendID + "\n", nil
		}
		return "daemon-one\n", nil
	case strings.HasPrefix(command, "docker image inspect --format {{.Id}} "):
		environment.imageInspectCalls++
		if args[len(args)-1] != buildkitImageReference {
			return args[len(args)-1] + "\n", nil
		}
		return "sha256:buildkit\n", nil
	case strings.HasPrefix(command, "docker ps -a "):
		environment.dockerPSCalls++
		return "", nil
	case strings.HasPrefix(command, "docker buildx inspect --bootstrap "):
		if !environment.builderExists {
			return "", fmt.Errorf("ERROR: no builder %q found", args[len(args)-1])
		}
		return environment.inspectOutput, nil
	case strings.HasPrefix(command, "docker buildx inspect "):
		environment.inspectCalls++
		if !environment.builderExists {
			return "", fmt.Errorf("ERROR: no builder %q found", args[len(args)-1])
		}
		return environment.inspectOutput, nil
	case strings.HasPrefix(command, "docker inspect --format {{.Image}} buildx_buildkit_"):
		if environment.verifyFailures > 0 {
			environment.verifyFailures--
			return "", errors.New("No such container: missing BuildKit control container")
		}
		return "sha256:buildkit\n", nil
	case strings.HasPrefix(command, "docker exec buildx_buildkit_") && strings.HasSuffix(command, " cat /etc/buildkit/buildkitd.toml"):
		return "[worker.oci]\n", nil
	case strings.HasPrefix(command, "docker buildx du "):
		environment.duCalls++
		return `[{"Total":0}]`, nil
	default:
		return "", fmt.Errorf("unexpected host output command: %s", command)
	}
}

func (environment *buildxCommandEnvironment) RunHostQuiet(_ context.Context, name string, args ...string) error {
	return fmt.Errorf("unexpected quiet host command: %s", strings.Join(append([]string{name}, args...), " "))
}
