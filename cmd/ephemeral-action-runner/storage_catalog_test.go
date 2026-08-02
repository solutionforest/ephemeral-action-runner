package main

import (
	"strings"
	"testing"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/storage"
	storagecatalog "github.com/solutionforest/ephemeral-action-runner/internal/storage/catalog"
	"github.com/solutionforest/ephemeral-action-runner/internal/storage/inventory"
)

func TestStorageLegacyExecutionRequiresPreviewPlanHash(t *testing.T) {
	err := runStorage([]string{"prune", "--legacy", "--execute"})
	if err == nil || !strings.Contains(err.Error(), "--plan <hash>") {
		t.Fatalf("runStorage() error = %v, want legacy plan-hash requirement", err)
	}
	if err := runStorage([]string{"status", "--legacy"}); err == nil || !strings.Contains(err.Error(), "does not support") {
		t.Fatalf("storage status --legacy error = %v, want rejection", err)
	}
}

func TestLegacyArtifactSelectionNeverIncludesShellDockerOrUnknownVolumes(t *testing.T) {
	now := time.Now().UTC()
	snapshot := inventory.Snapshot{CollectedAt: now, Artifacts: []storage.Artifact{
		{
			ID: "template", Kind: storage.ArtifactSandboxTemplate,
			Target:    storage.Target{Kind: storage.TargetSandboxTemplate, Locator: "docker.io/docker/sandbox-templates:shell-docker", Identity: "39cf20eca861", Match: storage.MatchExact},
			Ownership: storage.Ownership{Kind: storage.OwnershipUnknown},
		},
		{
			ID: "volume", Kind: storage.ArtifactDockerVolume,
			Target:      storage.Target{Kind: storage.TargetDockerVolume, Locator: "epar-old-cache", Identity: "epar-old-cache", Match: storage.MatchExact},
			Ownership:   storage.Ownership{Kind: storage.OwnershipUnknown},
			Protections: []storage.Protection{{Kind: storage.ProtectionUncertain, Detail: "prefix only"}},
		},
	}}
	selectLegacyStorage(&snapshot, storagecatalog.Catalog{}, now)
	if snapshot.Artifacts[0].Ownership.Kind != storage.OwnershipUnknown {
		t.Fatal("shell-docker became a legacy cleanup candidate")
	}
	if snapshot.Artifacts[1].Ownership.Kind != storage.OwnershipExact {
		t.Fatal("legacy volume was not represented in the approved exact preview")
	}
	if len(snapshot.Artifacts[1].Protections) == 0 {
		t.Fatal("prefix-era volume lost its fail-closed protection")
	}
}

func TestConfiguredSandboxTemplateProtectionMatchesFullReceiptDigest(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	snapshot := inventory.Snapshot{Artifacts: []storage.Artifact{{
		Kind:   storage.ArtifactSandboxTemplate,
		Target: storage.Target{Kind: storage.TargetSandboxTemplate, Locator: "docker.io/library/epar-docker-sandboxes-test:current", Identity: strings.Repeat("a", 12), Match: storage.MatchExact},
	}}}

	protectConfiguredSandboxTemplates(&snapshot, []inventory.TemplateSelection{{
		Tag:            "epar-docker-sandboxes-test:current",
		TemplateDigest: digest,
	}})

	if !snapshot.Artifacts[0].Current {
		t.Fatal("configured Sandbox template was not marked current")
	}
	if !hasStorageProtection(snapshot.Artifacts[0].Protections, storage.ProtectionConfiguration) {
		t.Fatalf("configured Sandbox template protections = %#v", snapshot.Artifacts[0].Protections)
	}
}

func hasStorageProtection(values []storage.Protection, want storage.ProtectionKind) bool {
	for _, value := range values {
		if value.Kind == want {
			return true
		}
	}
	return false
}

func TestCatalogArtifactExposesReferencesCustodyAndCleanupState(t *testing.T) {
	now := time.Now().UTC()
	supersededAt := now.Add(-time.Minute)
	value := storagecatalog.Catalog{InstallationID: "installation"}
	resource := storagecatalog.Resource{
		Key: "resource", BackendID: "docker:one", Kind: "docker-image", Provider: "docker-container",
		Role: "runtime-image", Locator: "epar-image:old", Identity: "sha256:old",
		Custody: storagecatalog.CustodyGenerated, State: storagecatalog.StateCleanupPending,
		References: []storagecatalog.Reference{{ConfigID: "config-one"}}, SupersededAt: &supersededAt,
		CleanupError: "in use",
	}
	artifact, ok := catalogStorageArtifact(value, resource, now)
	if !ok {
		t.Fatal("catalog Docker image was not exposed")
	}
	if artifact.BackendID != "docker:one" || artifact.Custody != "generated" || artifact.LifecycleState != "cleanup-pending" || artifact.CleanupError != "in use" || len(artifact.ConfigRefs) != 1 {
		t.Fatalf("catalog artifact omitted lifecycle evidence: %#v", artifact)
	}
}

func TestCatalogSandboxTemplateReplacesMatchingExternalInventory(t *testing.T) {
	cacheID := strings.Repeat("a", 12)
	snapshot := inventory.Snapshot{Artifacts: []storage.Artifact{{
		ID:        "external",
		Provider:  "docker-sandboxes",
		SurfaceID: "docker-sandboxes-template-cache",
		Kind:      storage.ArtifactSandboxTemplate,
		Target: storage.Target{
			Kind: storage.TargetSandboxTemplate, Locator: "epar-docker-sandboxes-test:current", Identity: cacheID, Match: storage.MatchExact,
		},
		Ownership: storage.Ownership{Kind: storage.OwnershipUnknown},
		SizeBytes: 1234,
		Protections: []storage.Protection{{
			Kind: storage.ProtectionUncertain, Detail: "external inventory",
		}},
	}}}
	catalogArtifact := storage.Artifact{
		ID:        "catalog",
		Provider:  "docker-sandboxes",
		SurfaceID: "docker-sandboxes-template-cache",
		Kind:      storage.ArtifactSandboxTemplate,
		Target: storage.Target{
			Kind: storage.TargetSandboxTemplate, Locator: "docker.io/library/epar-docker-sandboxes-test:current", Identity: cacheID, Match: storage.MatchExact,
		},
		Ownership: storage.Ownership{Kind: storage.OwnershipExact},
		Current:   true,
	}

	mergeCatalogStorageArtifact(&snapshot, catalogArtifact)

	if len(snapshot.Artifacts) != 1 {
		t.Fatalf("merged artifacts = %#v, want one authoritative catalog artifact", snapshot.Artifacts)
	}
	if snapshot.Artifacts[0].ID != "catalog" || snapshot.Artifacts[0].Ownership.Kind != storage.OwnershipExact || snapshot.Artifacts[0].SizeBytes != 1234 {
		t.Fatalf("merged catalog artifact = %#v", snapshot.Artifacts[0])
	}
	if hasStorageProtection(snapshot.Artifacts[0].Protections, storage.ProtectionUncertain) {
		t.Fatalf("merged catalog artifact retained external uncertainty: %#v", snapshot.Artifacts[0].Protections)
	}
}

func TestCatalogPreexistingAcquiredDockerTagRemainsReportOnly(t *testing.T) {
	now := time.Now().UTC()
	resource := storagecatalog.Resource{
		Key: "source", BackendID: "docker:one", Kind: "docker-image", Role: "build-source",
		Locator: "golang:latest", Identity: "sha256:source", Custody: storagecatalog.CustodyAcquired,
		State: storagecatalog.StateSuperseded, SupersededAt: &now,
	}
	artifact, ok := catalogStorageArtifact(storagecatalog.Catalog{InstallationID: "host"}, resource, now)
	if !ok {
		t.Fatal("catalog Docker source was not exposed")
	}
	if !hasStorageProtection(artifact.Protections, storage.ProtectionUncertain) {
		t.Fatalf("preexisting acquired Docker tag protections = %#v", artifact.Protections)
	}
}

func TestCatalogTartImageUsesExactExternalIdentity(t *testing.T) {
	now := time.Now().UTC()
	value := storagecatalog.Catalog{InstallationID: "host", Resources: nil}
	resource := storagecatalog.Resource{
		Key: "tart-resource", BackendID: "tart:one", InstallationIDs: []string{"installation"},
		Kind: "tart-image", Provider: "tart", Role: "runtime-image", Locator: "epar-current",
		Identity: "tart-mac:02:00:00:00:00:01", Custody: storagecatalog.CustodyGenerated, State: storagecatalog.StateCurrent,
	}
	artifact, ok := catalogStorageArtifact(value, resource, now)
	if !ok {
		t.Fatal("catalog Tart image was not exposed")
	}
	if artifact.Target.Kind != storage.TargetExternal || artifact.Target.Locator != "tart-image:epar-current" || artifact.Target.Identity != resource.Identity {
		t.Fatalf("Tart artifact lost exact backend identity: %#v", artifact)
	}
	if artifact.Ownership.OwnerID != "installation" {
		t.Fatalf("Tart artifact owner = %q, want installation identity", artifact.Ownership.OwnerID)
	}
}
