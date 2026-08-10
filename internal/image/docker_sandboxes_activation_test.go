package image

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
	storagecatalog "github.com/solutionforest/ephemeral-action-runner/internal/storage/catalog"
)

type activationJournalRuntime struct {
	gate   sync.Mutex
	mu     sync.Mutex
	cached map[string]provider.TemplateArtifact
	active provider.TemplateArtifact
}

func (runtime *activationJournalRuntime) ImportTemplate(context.Context, string) error {
	return errors.New("unexpected import of an already cached activation-test artifact")
}

func (runtime *activationJournalRuntime) VerifyImportedTemplate(_ context.Context, artifact provider.TemplateArtifact) error {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.cached[artifact.Reference] != artifact {
		return provider.ErrTemplateNotFound
	}
	return nil
}

func (runtime *activationJournalRuntime) ActivateTemplate(artifact provider.TemplateArtifact) error {
	if err := runtime.VerifyImportedTemplate(context.Background(), artifact); err != nil {
		return err
	}
	runtime.mu.Lock()
	runtime.active = artifact
	runtime.mu.Unlock()
	return nil
}

func (runtime *activationJournalRuntime) WithTemplateActivation(operation func() error) error {
	runtime.gate.Lock()
	defer runtime.gate.Unlock()
	return operation()
}

func (runtime *activationJournalRuntime) ActiveTemplate() (provider.TemplateArtifact, bool) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	return runtime.active, runtime.active.Reference != ""
}

func (runtime *activationJournalRuntime) ClearActiveTemplate(expected provider.TemplateArtifact) error {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.active != expected {
		return fmt.Errorf("active template changed")
	}
	runtime.active = provider.TemplateArtifact{}
	return nil
}

func (runtime *activationJournalRuntime) ResolveTemplateCacheID(_ context.Context, reference string) (string, bool, error) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	artifact, found := runtime.cached[reference]
	return artifact.CacheID, found, nil
}

func TestDockerSandboxesActivationFaultsPreservePreviousReceipt(t *testing.T) {
	phases := []string{
		dockerSandboxesActivationPrepared,
		dockerSandboxesActivationImported,
		dockerSandboxesActivationVerified,
		dockerSandboxesActivationAdmissionBlocked,
		dockerSandboxesActivationActivated,
		dockerSandboxesActivationReadBack,
		dockerSandboxesActivationReceiptPublished,
		dockerSandboxesActivationCommitted,
	}
	for _, phase := range phases {
		t.Run(phase, func(t *testing.T) {
			coordinator, previous, candidate := activationJournalFixture(t, true)
			runtime := &activationJournalRuntime{
				cached: map[string]provider.TemplateArtifact{previous.Artifact.Reference: previous.Artifact, candidate.Artifact.Reference: candidate.Artifact},
				active: previous.Artifact,
			}
			coordinator.DockerSandboxesActivationFault = func(current string) error {
				if current == phase {
					return errors.New("stop at " + phase)
				}
				return nil
			}
			var activationErr error
			if err := coordinator.withSandboxBackendLock(context.Background(), func() error {
				_, activationErr = coordinator.activateDockerSandboxesCandidateLocked(context.Background(), candidate, "", false, runtime)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if activationErr == nil || !strings.Contains(activationErr.Error(), "stop at "+phase) {
				t.Fatalf("activation error = %v", activationErr)
			}
			coordinator.DockerSandboxesActivationFault = nil
			if err := coordinator.recoverDockerSandboxesActivation(context.Background(), runtime); err != nil {
				t.Fatal(err)
			}
			active, found := runtime.ActiveTemplate()
			if !found || active != previous.Artifact {
				t.Fatalf("active template = %+v, found=%t; want previous %+v", active, found, previous.Artifact)
			}
			receipt, err := coordinator.readDockerSandboxesReceipt()
			if err != nil {
				t.Fatal(err)
			}
			if !sameDockerSandboxesReceipt(receipt, previous) {
				t.Fatalf("active receipt = %+v; want previous %+v", receipt, previous)
			}
			journalPath, err := coordinator.dockerSandboxesActivationJournalPath()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(journalPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("activation journal remains after recovery: %v", err)
			}
			assertActivationCatalogCurrent(t, previous)
		})
	}
}

func TestDockerSandboxesInitialActivationRollbackClearsAdmissionArtifact(t *testing.T) {
	coordinator, _, candidate := activationJournalFixture(t, false)
	runtime := &activationJournalRuntime{cached: map[string]provider.TemplateArtifact{candidate.Artifact.Reference: candidate.Artifact}}
	coordinator.DockerSandboxesActivationFault = func(phase string) error {
		if phase == dockerSandboxesActivationActivated {
			return errors.New("activation crash")
		}
		return nil
	}
	var activationErr error
	if err := coordinator.withSandboxBackendLock(context.Background(), func() error {
		_, activationErr = coordinator.activateDockerSandboxesCandidateLocked(context.Background(), candidate, "", false, runtime)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if activationErr == nil {
		t.Fatal("initial activation fault succeeded")
	}
	if active, found := runtime.ActiveTemplate(); found || active != (provider.TemplateArtifact{}) {
		t.Fatalf("uncommitted initial template remained active: %+v", active)
	}
	if _, err := coordinator.readDockerSandboxesReceipt(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("uncommitted initial receipt exists: %v", err)
	}
	store, err := storagecatalog.Open("")
	if err != nil {
		t.Fatal(err)
	}
	value, err := store.Load(time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	configID, err := storagecatalog.ConfigID(coordinator.ProjectRoot, coordinator.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, resource := range value.Resources {
		for _, reference := range resource.References {
			if reference.ConfigID == configID && reference.Role == "provider-artifact" {
				t.Fatalf("no-previous rollback retained provider-artifact role on %+v", resource)
			}
		}
	}
}

func TestDockerSandboxesImportedCandidateRollbackRetainsExactGeneratedCustody(t *testing.T) {
	coordinator, previous, candidate := activationJournalFixture(t, true)
	coordinator.environment = &generationImportEnvironment{}
	sourceLock, err := os.ReadFile(filepath.Join("..", "..", "templates", "docker-sandboxes", "sources.lock.json"))
	if err != nil {
		t.Fatal(err)
	}
	testSourceLock := filepath.Join(coordinator.ProjectRoot, "templates", "docker-sandboxes", "sources.lock.json")
	if err := os.MkdirAll(filepath.Dir(testSourceLock), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(testSourceLock, sourceLock, 0o600); err != nil {
		t.Fatal(err)
	}
	archivePath := filepath.Join(t.TempDir(), "candidate.tar")
	if err := os.WriteFile(archivePath, []byte("candidate archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	archiveSHA, archiveBytes, err := hashFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	candidate.ArchiveSHA256 = archiveSHA
	candidate.ArchiveBytes = archiveBytes
	runtime := &generationImportRuntime{
		cached:    map[string]provider.TemplateArtifact{previous.Artifact.Reference: previous.Artifact},
		candidate: candidate.Artifact,
		activated: previous.Artifact,
	}
	coordinator.DockerSandboxesActivationFault = func(phase string) error {
		if phase == dockerSandboxesActivationActivated {
			return errors.New("post-import activation fault")
		}
		return nil
	}
	var activationErr error
	if err := coordinator.withSandboxBackendLock(context.Background(), func() error {
		_, activationErr = coordinator.activateDockerSandboxesCandidateLocked(context.Background(), candidate, archivePath, false, runtime)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if activationErr == nil || runtime.imports != 1 {
		t.Fatalf("activation error/imports = %v/%d, want fault after one import", activationErr, runtime.imports)
	}
	if runtime.activated != previous.Artifact {
		t.Fatalf("rollback active template = %+v, want previous %+v", runtime.activated, previous.Artifact)
	}

	store, err := storagecatalog.Open("")
	if err != nil {
		t.Fatal(err)
	}
	value, err := store.Load(time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	foundCandidate := false
	for _, resource := range value.Resources {
		if resource.Kind != catalogSandboxTemplateKind || resource.Identity != candidate.Artifact.CacheID {
			continue
		}
		foundCandidate = true
		if resource.Custody != storagecatalog.CustodyGenerated || resource.State != storagecatalog.StateSuperseded || len(resource.References) != 0 || resource.Fingerprint != candidate.Artifact.Digest {
			t.Fatalf("rolled-back imported candidate custody = %+v", resource)
		}
	}
	if !foundCandidate {
		t.Fatal("rolled-back imported candidate cache was left outside the storage catalog")
	}
	assertActivationCatalogCurrent(t, previous)
}

func TestDockerSandboxesRecoveryReconcilesOpaqueImportedCacheID(t *testing.T) {
	coordinator, _, candidate := activationJournalFixture(t, false)
	oldPredictedID := candidate.Artifact.CacheID
	actualArtifact := candidate.Artifact
	actualArtifact.CacheID = "ec2006fea720"
	runtime := &activationJournalRuntime{cached: map[string]provider.TemplateArtifact{actualArtifact.Reference: actualArtifact}}

	journal, err := coordinator.prepareDockerSandboxesActivationJournal(candidate, "", false)
	if err != nil {
		t.Fatal(err)
	}
	journal.Imported = true
	if err := coordinator.updateDockerSandboxesActivationJournal(&journal, dockerSandboxesActivationImported, nil); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.recoverDockerSandboxesActivation(context.Background(), runtime); err != nil {
		t.Fatal(err)
	}

	store, err := storagecatalog.Open("")
	if err != nil {
		t.Fatal(err)
	}
	value, err := store.Load(time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	foundActual := false
	for _, resource := range value.Resources {
		if resource.Kind != catalogSandboxTemplateKind {
			continue
		}
		if resource.Identity == oldPredictedID {
			t.Fatalf("recovery retained predicted digest-prefix cache identity: %+v", resource)
		}
		if resource.Identity == actualArtifact.CacheID {
			foundActual = true
			if resource.Fingerprint != actualArtifact.Digest || resource.Custody != storagecatalog.CustodyGenerated || resource.State != storagecatalog.StateSuperseded {
				t.Fatalf("reconciled candidate custody = %+v", resource)
			}
		}
	}
	if !foundActual {
		t.Fatal("recovery did not retain the provider-assigned cache identity")
	}
}

func TestDockerSandboxesPrebuiltWarmActivationResolvesOpaqueCacheWithoutImport(t *testing.T) {
	coordinator, _, candidate := activationJournalFixture(t, false)
	candidate.Distribution = dockerSandboxesDistributionPrebuilt
	candidate.Artifact.CacheID = ""
	candidate.MetadataSHA256 = ""
	candidate.Evidence = nil
	actualArtifact := candidate.Artifact
	actualArtifact.CacheID = "ec2006fea720"
	runtime := &activationJournalRuntime{cached: map[string]provider.TemplateArtifact{actualArtifact.Reference: actualArtifact}}
	receiptPath := mustDockerSandboxesReceiptPath(t, coordinator)

	if err := coordinator.withSandboxBackendLock(context.Background(), func() error {
		_, err := coordinator.activateDockerSandboxesCandidateWithFinalizerLocked(context.Background(), candidate, "", false, false, runtime, func(finalized *dockerSandboxesReceipt) error {
			finalized.MetadataSHA256 = "sha256:" + strings.Repeat("1", 64)
			finalized.Evidence = writeActivationReceiptEvidence(t, receiptPath, *finalized)
			return nil
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	active, found := runtime.ActiveTemplate()
	if !found || active != actualArtifact {
		t.Fatalf("active template = %+v, found=%t; want provider-assigned artifact %+v", active, found, actualArtifact)
	}
	journalPath, err := coordinator.dockerSandboxesActivationJournalPath()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(journalPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("warm activation journal remains: %v", err)
	}
}

func TestDockerSandboxesRecoveryCommitsPublishedReceipt(t *testing.T) {
	coordinator, previous, candidate := activationJournalFixture(t, true)
	runtime := &activationJournalRuntime{
		cached: map[string]provider.TemplateArtifact{previous.Artifact.Reference: previous.Artifact, candidate.Artifact.Reference: candidate.Artifact},
	}
	journal, err := coordinator.prepareDockerSandboxesActivationJournal(candidate, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeJSONFile(mustDockerSandboxesReceiptPath(t, coordinator), candidate); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.updateDockerSandboxesActivationJournal(&journal, dockerSandboxesActivationReceiptPublished, nil); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.recoverDockerSandboxesActivation(context.Background(), runtime); err != nil {
		t.Fatal(err)
	}
	active, found := runtime.ActiveTemplate()
	if !found || active != candidate.Artifact {
		t.Fatalf("recovered active template = %+v, found=%t", active, found)
	}
	receipt, err := coordinator.readDockerSandboxesReceipt()
	if err != nil {
		t.Fatal(err)
	}
	if !sameDockerSandboxesReceipt(receipt, candidate) {
		t.Fatalf("recovered receipt = %+v; want candidate %+v", receipt, candidate)
	}
	assertActivationCatalogCurrent(t, candidate)
}

func activationJournalFixture(t *testing.T, withPrevious bool) (*Coordinator, dockerSandboxesReceipt, dockerSandboxesReceipt) {
	t.Helper()
	stateRoot := filepath.Join(t.TempDir(), "host-state")
	t.Setenv("EPAR_STATE_HOME", stateRoot)
	projectRoot := t.TempDir()
	configPath := filepath.Join(projectRoot, ".local", "config.yml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte("provider:\n  type: docker-sandboxes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := Manifest{SchemaVersion: ManifestSchemaVersion, ProviderType: "docker-sandboxes", ProviderPlatform: "linux/amd64", SourceType: config.ImageSourceDockerImage, SourceImage: "ghcr.io/catthehacker/ubuntu:act-latest", SourcePlatform: "linux/amd64", SourceDigest: "sha256:" + strings.Repeat("a", 64), SourcePlatformDigest: "sha256:" + strings.Repeat("b", 64), RunnerSelector: "latest", RunnerVersion: "2.336.0"}
	manifestHash, err := ManifestHash(manifest)
	if err != nil {
		t.Fatal(err)
	}
	source := ResolvedDockerSource{Reference: manifest.SourceImage, ImmutableReference: "ghcr.io/catthehacker/ubuntu@" + manifest.SourceDigest, IndexDigest: manifest.SourceDigest, PlatformDigest: manifest.SourcePlatformDigest, Platform: "linux/amd64", CompressedLayerBytes: 1024}
	previousArtifact := provider.TemplateArtifact{Reference: "docker.io/library/epar-previous:one", Digest: "sha256:" + strings.Repeat("c", 64), CacheID: strings.Repeat("c", 12), Platform: "linux/amd64", RootDisk: "40GiB"}
	candidateArtifact := provider.TemplateArtifact{Reference: "docker.io/library/epar-candidate:two", Digest: "sha256:" + strings.Repeat("d", 64), CacheID: strings.Repeat("d", 12), Platform: "linux/amd64", RootDisk: "40GiB"}
	now := time.Date(2026, time.August, 9, 1, 2, 3, 0, time.UTC)
	coordinator := &Coordinator{Config: config.Config{Storage: config.StorageConfig{AutomaticHousekeeping: config.StorageHousekeepingDisabled}}, ProjectRoot: projectRoot, ConfigPath: configPath, Clock: func() time.Time { return now }}
	receiptPath := mustDockerSandboxesReceiptPath(t, coordinator)
	previous := dockerSandboxesReceipt{SchemaVersion: dockerSandboxesReceiptSchema, Distribution: dockerSandboxesDistributionLocal, ManifestHash: manifestHash, Manifest: manifest, Source: source, Artifact: previousArtifact, MetadataSHA256: "sha256:" + strings.Repeat("e", 64), ArchiveSHA256: "sha256:" + strings.Repeat("f", 64), ArchiveBytes: 1, ActivatedAt: now.Add(-time.Hour)}
	previous.Evidence = writeActivationReceiptEvidence(t, receiptPath, previous)
	candidate := dockerSandboxesReceipt{SchemaVersion: dockerSandboxesReceiptSchema, Distribution: dockerSandboxesDistributionLocal, ManifestHash: manifestHash, Manifest: manifest, Source: source, Artifact: candidateArtifact, MetadataSHA256: "sha256:" + strings.Repeat("1", 64), ArchiveSHA256: "sha256:" + strings.Repeat("2", 64), ArchiveBytes: 2, ActivatedAt: now}
	candidate.Evidence = writeActivationReceiptEvidence(t, receiptPath, candidate)
	if withPrevious {
		if err := writeJSONFile(receiptPath, previous); err != nil {
			t.Fatal(err)
		}
		if err := coordinator.recordCurrentSandboxArtifact(context.Background(), previous.Artifact, previous.ManifestHash, previous.ActivatedAt); err != nil {
			t.Fatal(err)
		}
	}
	return coordinator, previous, candidate
}

func writeActivationReceiptEvidence(t *testing.T, receiptPath string, receipt dockerSandboxesReceipt) map[string]artifactEvidence {
	t.Helper()
	result := make(map[string]artifactEvidence, len(dockerSandboxesCompactEvidenceFiles))
	for name, filename := range dockerSandboxesCompactEvidenceFiles {
		relative := filepath.ToSlash(filepath.Join("evidence", receipt.ManifestHash, receipt.Artifact.CacheID, filename))
		path := filepath.Join(filepath.Dir(receiptPath), filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(receipt.Artifact.CacheID+" "+name+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		digest, _, err := hashFile(path)
		if err != nil {
			t.Fatal(err)
		}
		result[name] = artifactEvidence{Path: relative, SHA256: digest}
		if name == "sbomDescriptor" {
			evidence := result[name]
			evidence.SourceDigest = "sha256:" + strings.Repeat("9", 64)
			result[name] = evidence
		}
	}
	return result
}

func mustDockerSandboxesReceiptPath(t *testing.T, coordinator *Coordinator) string {
	t.Helper()
	path, err := coordinator.dockerSandboxesReceiptPath()
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func assertActivationCatalogCurrent(t *testing.T, receipt dockerSandboxesReceipt) {
	t.Helper()
	store, err := storagecatalog.Open("")
	if err != nil {
		t.Fatal(err)
	}
	value, err := store.Load(time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	for _, resource := range value.Resources {
		if resource.Kind == catalogSandboxTemplateKind && resource.Identity == receipt.Artifact.CacheID {
			if resource.State != storagecatalog.StateCurrent || resource.Custody != storagecatalog.CustodyGenerated || resource.ManifestHash != receipt.ManifestHash {
				t.Fatalf("catalog resource = %+v", resource)
			}
			return
		}
	}
	t.Fatalf("current catalog resource for %s not found", receipt.Artifact.CacheID)
}
