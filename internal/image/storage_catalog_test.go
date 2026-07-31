package image

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
	"github.com/solutionforest/ephemeral-action-runner/internal/storage"
	storagecatalog "github.com/solutionforest/ephemeral-action-runner/internal/storage/catalog"
)

func TestBeginDockerAcquisitionPersistsPreexistingIdentityBeforePull(t *testing.T) {
	stateRoot := filepath.Join(t.TempDir(), "host-state")
	t.Setenv("EPAR_STATE_HOME", stateRoot)
	projectRoot := t.TempDir()
	configPath := filepath.Join(projectRoot, "config.yml")
	if err := os.WriteFile(configPath, []byte("provider:\n  type: docker-container\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	coordinator := Coordinator{Config: config.Default(), ProjectRoot: projectRoot, ConfigPath: configPath}
	now := time.Date(2026, 7, 29, 1, 2, 3, 0, time.UTC)
	if err := coordinator.beginDockerRoleAcquisition("docker:daemon", "build-source", "example/source:latest", "sha256:before", now); err != nil {
		t.Fatal(err)
	}
	store, err := storagecatalog.Open("")
	if err != nil {
		t.Fatal(err)
	}
	value, err := store.Load(now)
	if err != nil {
		t.Fatal(err)
	}
	if len(value.Journals) != 1 {
		t.Fatalf("journals = %#v, want one acquisition journal", value.Journals)
	}
	journal := value.Journals[0]
	if journal.Operation != "docker-image-acquisition" || journal.Phase != "acquiring" || journal.BackendID != "docker:daemon" || journal.Locator != "example/source:latest" || journal.PreviousIdentity != "sha256:before" {
		t.Fatalf("acquisition journal omitted pre-pull evidence: %#v", journal)
	}
	if len(value.Configs) != 1 || value.Configs[0].InstallationID == "" {
		t.Fatalf("acquisition journal did not register its exact EPAR installation: %#v", value.Configs)
	}
}

func TestNonTartControllerPreservesTartCatalogEvidence(t *testing.T) {
	coordinator := Coordinator{Config: config.Default()}
	coordinator.Config.Provider.Type = "docker-container"
	exists, err := coordinator.catalogResourceExists(context.Background(), storagecatalog.Resource{
		Kind:     catalogTartImageKind,
		Locator:  "epar-tart-image",
		Identity: "tart-mac:001122334455",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatal("non-Tart controller discarded Tart catalog evidence")
	}
}

func TestCurrentReferenceUpdateWaitsForBackendLock(t *testing.T) {
	t.Setenv("EPAR_STATE_HOME", filepath.Join(t.TempDir(), "host-state"))
	projectRoot := t.TempDir()
	configPath := filepath.Join(projectRoot, "config.yml")
	if err := os.WriteFile(configPath, []byte("provider:\n  type: wsl\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	coordinator := Coordinator{Config: config.Default(), ProjectRoot: projectRoot, ConfigPath: configPath}
	store, err := storagecatalog.Open("")
	if err != nil {
		t.Fatal(err)
	}
	backendID := "filesystem:test"
	backendLock, err := store.AcquireBackendLock(context.Background(), backendID)
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		result <- coordinator.registerCurrentCatalogResource(context.Background(), storagecatalog.Resource{
			BackendID: backendID,
			Kind:      catalogWSLArtifactKind,
			Provider:  "wsl",
			Role:      "runtime-rootfs",
			Locator:   filepath.Join(projectRoot, "runner.tar"),
			Identity:  "filesystem-id",
			Custody:   storagecatalog.CustodyGenerated,
			State:     storagecatalog.StateCurrent,
		}, "manifest", time.Now().UTC())
	}()
	select {
	case err := <-result:
		t.Fatalf("reference update bypassed the held backend lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := backendLock.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("reference update did not proceed after backend lock release")
	}
}

func TestSandboxWorkspaceIsCatalogedBeforeBuildAndSupersededExactly(t *testing.T) {
	t.Setenv("EPAR_STATE_HOME", filepath.Join(t.TempDir(), "host-state"))
	projectRoot := t.TempDir()
	configPath := filepath.Join(projectRoot, "config.yml")
	if err := os.WriteFile(configPath, []byte("provider:\n  type: docker-sandboxes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	first := filepath.Join(projectRoot, "work", "template-builds", "docker-sandboxes", "first")
	second := filepath.Join(projectRoot, "work", "template-builds", "docker-sandboxes", "second")
	for _, path := range []string{first, second} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	coordinator := Coordinator{Config: config.Default(), ProjectRoot: projectRoot, ConfigPath: configPath}
	now := time.Date(2026, 7, 30, 1, 2, 3, 0, time.UTC)
	if err := coordinator.recordSandboxWorkspace(context.Background(), first, "manifest-first", storagecatalog.StateStaging, now); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.recordSandboxWorkspace(context.Background(), second, "manifest-second", storagecatalog.StateStaging, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	store, err := storagecatalog.Open("")
	if err != nil {
		t.Fatal(err)
	}
	value, err := store.Load(now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	firstTarget, err := storage.SnapshotFilesystemTarget(first)
	if err != nil {
		t.Fatal(err)
	}
	secondTarget, err := storage.SnapshotFilesystemTarget(second)
	if err != nil {
		t.Fatal(err)
	}
	firstResource := findCatalogResourceByLocator(t, value.Resources, firstTarget.Locator)
	secondResource := findCatalogResourceByLocator(t, value.Resources, secondTarget.Locator)
	if firstResource.State != storagecatalog.StateSuperseded || len(firstResource.References) != 0 || firstResource.SupersededAt == nil {
		t.Fatalf("replaced staging workspace = %#v", firstResource)
	}
	if secondResource.State != storagecatalog.StateStaging || len(secondResource.References) != 1 || secondResource.References[0].Role != "template-staging" {
		t.Fatalf("current staging workspace = %#v", secondResource)
	}
	if err := coordinator.recordSandboxWorkspace(context.Background(), second, "manifest-second", storagecatalog.StateSuperseded, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	value, err = store.Load(now.Add(2 * time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	secondResource = findCatalogResourceByLocator(t, value.Resources, secondTarget.Locator)
	if secondResource.State != storagecatalog.StateSuperseded || len(secondResource.References) != 0 || secondResource.SupersededAt == nil {
		t.Fatalf("completed staging workspace = %#v", secondResource)
	}
}

func findCatalogResourceByLocator(t *testing.T, resources []storagecatalog.Resource, locator string) storagecatalog.Resource {
	t.Helper()
	for _, resource := range resources {
		if filepath.Clean(resource.Locator) == filepath.Clean(locator) {
			return resource
		}
	}
	t.Fatalf("catalog resource %q not found in %#v", locator, resources)
	return storagecatalog.Resource{}
}

func TestTartCurrentStateRequiresExactCatalogManifestReference(t *testing.T) {
	value := storagecatalog.Catalog{Resources: []storagecatalog.Resource{{
		Kind:     catalogTartImageKind,
		Locator:  "epar-tart-image",
		Identity: "tart-mac:001122334455",
		References: []storagecatalog.Reference{{
			ConfigID:     "config-one",
			Role:         "provider-artifact",
			ManifestHash: "manifest-one",
		}},
	}}}
	if !tartCatalogReferenceMatches(value, "config-one", "epar-tart-image", "tart-mac:001122334455", "manifest-one") {
		t.Fatal("exact Tart catalog reference was not recognized")
	}
	for _, mismatch := range []struct {
		configID     string
		locator      string
		identity     string
		manifestHash string
	}{
		{configID: "config-two", locator: "epar-tart-image", identity: "tart-mac:001122334455", manifestHash: "manifest-one"},
		{configID: "config-one", locator: "other-image", identity: "tart-mac:001122334455", manifestHash: "manifest-one"},
		{configID: "config-one", locator: "epar-tart-image", identity: "tart-mac:aabbccddeeff", manifestHash: "manifest-one"},
		{configID: "config-one", locator: "epar-tart-image", identity: "tart-mac:001122334455", manifestHash: "manifest-two"},
	} {
		if tartCatalogReferenceMatches(value, mismatch.configID, mismatch.locator, mismatch.identity, mismatch.manifestHash) {
			t.Fatalf("mismatched Tart catalog reference was accepted: %+v", mismatch)
		}
	}
}
