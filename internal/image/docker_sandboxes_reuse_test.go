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
	"github.com/solutionforest/ephemeral-action-runner/internal/storage"
	storagecatalog "github.com/solutionforest/ephemeral-action-runner/internal/storage/catalog"
)

type reusableTemplateRuntime struct {
	want        provider.TemplateArtifact
	active      provider.TemplateArtifact
	verified    int
	activated   int
	imported    int
	verifyError error
}

type publicationRaceRuntime struct {
	mu                     sync.Mutex
	cached                 provider.TemplateArtifact
	firstActivation        sync.Once
	activationStarted      chan struct{}
	releaseFirstActivation chan struct{}
	imports                int
	active                 provider.TemplateArtifact
}

type generationImportEnvironment struct {
	Environment
	preflights []string
	infos      []string
}

func (environment *generationImportEnvironment) PreflightStorage(operation string, _ storage.OperationPlan) error {
	environment.preflights = append(environment.preflights, operation)
	return nil
}

func (environment *generationImportEnvironment) Infof(format string, args ...any) {
	environment.infos = append(environment.infos, fmt.Sprintf(format, args...))
}

type generationImportRuntime struct {
	cached        map[string]provider.TemplateArtifact
	candidate     provider.TemplateArtifact
	readbackErr   error
	imports       int
	verifications int
	activated     provider.TemplateArtifact
}

func (runtime *generationImportRuntime) ImportTemplate(context.Context, string) error {
	runtime.imports++
	runtime.cached[runtime.candidate.Reference] = runtime.candidate
	return nil
}

func (runtime *generationImportRuntime) VerifyImportedTemplate(_ context.Context, artifact provider.TemplateArtifact) error {
	runtime.verifications++
	cached, found := runtime.cached[artifact.Reference]
	if !found {
		return provider.ErrTemplateNotFound
	}
	if cached != artifact {
		return fmt.Errorf("cached Docker Sandbox template ID %s does not match imported archive identity %s", cached.CacheID, artifact.CacheID)
	}
	if artifact == runtime.candidate && runtime.readbackErr != nil {
		return runtime.readbackErr
	}
	return nil
}

func (runtime *generationImportRuntime) ActivateTemplate(artifact provider.TemplateArtifact) error {
	if err := runtime.VerifyImportedTemplate(context.Background(), artifact); err != nil {
		return err
	}
	runtime.activated = artifact
	return nil
}

func (runtime *generationImportRuntime) WithTemplateActivation(operation func() error) error {
	return operation()
}

func (runtime *generationImportRuntime) ActiveTemplate() (provider.TemplateArtifact, bool) {
	return runtime.activated, runtime.activated.Reference != ""
}

func (runtime *generationImportRuntime) ClearActiveTemplate(expected provider.TemplateArtifact) error {
	if runtime.activated != expected {
		return fmt.Errorf("active template changed")
	}
	runtime.activated = provider.TemplateArtifact{}
	return nil
}

func (runtime *publicationRaceRuntime) ImportTemplate(context.Context, string) error {
	runtime.mu.Lock()
	runtime.imports++
	runtime.mu.Unlock()
	return fmt.Errorf("unexpected template import")
}

func (runtime *publicationRaceRuntime) VerifyImportedTemplate(_ context.Context, artifact provider.TemplateArtifact) error {
	runtime.mu.Lock()
	cached := runtime.cached
	runtime.mu.Unlock()
	if artifact != cached {
		return fmt.Errorf("cached Docker Sandbox template ID %s does not match imported archive identity %s", cached.CacheID, artifact.CacheID)
	}
	return nil
}

func (runtime *publicationRaceRuntime) ActivateTemplate(artifact provider.TemplateArtifact) error {
	if err := runtime.VerifyImportedTemplate(context.Background(), artifact); err != nil {
		return err
	}
	runtime.firstActivation.Do(func() {
		close(runtime.activationStarted)
		<-runtime.releaseFirstActivation
	})
	runtime.mu.Lock()
	runtime.active = artifact
	runtime.mu.Unlock()
	return nil
}

func (runtime *publicationRaceRuntime) WithTemplateActivation(operation func() error) error {
	return operation()
}

func (runtime *publicationRaceRuntime) ActiveTemplate() (provider.TemplateArtifact, bool) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	return runtime.active, runtime.active.Reference != ""
}

func (runtime *publicationRaceRuntime) ClearActiveTemplate(expected provider.TemplateArtifact) error {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.active != expected {
		return fmt.Errorf("active template changed")
	}
	runtime.active = provider.TemplateArtifact{}
	return nil
}

func (runtime *reusableTemplateRuntime) ImportTemplate(context.Context, string) error {
	runtime.imported++
	return nil
}

func (runtime *reusableTemplateRuntime) VerifyImportedTemplate(_ context.Context, artifact provider.TemplateArtifact) error {
	runtime.verified++
	if artifact != runtime.want {
		return provider.ErrTemplateNotFound
	}
	return runtime.verifyError
}

func (runtime *reusableTemplateRuntime) ActivateTemplate(artifact provider.TemplateArtifact) error {
	if artifact != runtime.want {
		return provider.ErrTemplateNotFound
	}
	runtime.activated++
	runtime.active = artifact
	return nil
}

func (runtime *reusableTemplateRuntime) WithTemplateActivation(operation func() error) error {
	return operation()
}

func (runtime *reusableTemplateRuntime) ActiveTemplate() (provider.TemplateArtifact, bool) {
	return runtime.active, runtime.active.Reference != ""
}

func (runtime *reusableTemplateRuntime) ClearActiveTemplate(expected provider.TemplateArtifact) error {
	if runtime.active != expected {
		return fmt.Errorf("active template changed")
	}
	runtime.active = provider.TemplateArtifact{}
	return nil
}

func TestAdoptReusableDockerSandboxesTemplatePublishesExactSecondConfigReceipt(t *testing.T) {
	stateRoot := filepath.Join(t.TempDir(), "host-state")
	t.Setenv("EPAR_STATE_HOME", stateRoot)
	projectRoot := t.TempDir()
	firstConfig := filepath.Join(projectRoot, ".local", "config.first.yml")
	secondConfig := filepath.Join(projectRoot, ".local", "config.second.yml")
	for _, path := range []string{firstConfig, secondConfig} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("provider:\n  type: docker-sandboxes\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	manifest := Manifest{
		SchemaVersion:        ManifestSchemaVersion,
		ProviderType:         "docker-sandboxes",
		ProviderPlatform:     "linux/arm64",
		SourceType:           "docker-image",
		SourceImage:          "ghcr.io/catthehacker/ubuntu:js-latest",
		SourcePlatform:       "linux/arm64",
		SourceDigest:         "sha256:" + strings.Repeat("a", 64),
		SourcePlatformDigest: "sha256:" + strings.Repeat("b", 64),
		RunnerSelector:       "latest",
		RunnerVersion:        "2.336.0",
		RunnerAssetName:      "actions-runner-linux-arm64-2.336.0.tar.gz",
		RunnerAssetURL:       "https://github.com/actions/runner/releases/download/v2.336.0/actions-runner-linux-arm64-2.336.0.tar.gz",
		RunnerAssetDigest:    "sha256:" + strings.Repeat("c", 64),
	}
	manifestHash, err := ManifestHash(manifest)
	if err != nil {
		t.Fatal(err)
	}
	source := ResolvedDockerSource{
		Reference:            manifest.SourceImage,
		ImmutableReference:   "ghcr.io/catthehacker/ubuntu@" + manifest.SourceDigest,
		IndexDigest:          manifest.SourceDigest,
		PlatformDigest:       manifest.SourcePlatformDigest,
		Platform:             manifest.SourcePlatform,
		CompressedLayerBytes: 1024,
	}
	artifact := provider.TemplateArtifact{
		Reference: "docker.io/library/epar-docker-sandboxes-catthehacker-js-latest:" + manifestHash[:16] + "-arm64",
		Digest:    "sha256:" + strings.Repeat("d", 64),
		CacheID:   strings.Repeat("d", 12),
		Platform:  "linux/arm64",
		RootDisk:  "40GiB",
	}
	activatedAt := time.Date(2026, time.August, 2, 1, 2, 3, 0, time.UTC)
	firstReceiptPath, err := DockerSandboxesReceiptPathForConfig(projectRoot, firstConfig)
	if err != nil {
		t.Fatal(err)
	}
	evidence := make(map[string]artifactEvidence)
	for name, filename := range dockerSandboxesCompactEvidenceFiles {
		relative := filepath.ToSlash(filepath.Join("evidence", manifestHash, filename))
		path := filepath.Join(filepath.Dir(firstReceiptPath), filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(name+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		digest, _, err := hashFile(path)
		if err != nil {
			t.Fatal(err)
		}
		evidence[name] = artifactEvidence{Path: relative, SHA256: digest}
		if name == "sbomDescriptor" {
			entry := evidence[name]
			entry.SourceDigest = "sha256:" + strings.Repeat("9", 64)
			evidence[name] = entry
		}
	}
	receipt := dockerSandboxesReceipt{
		SchemaVersion:  dockerSandboxesLegacyReceiptSchema,
		ManifestHash:   manifestHash,
		Manifest:       manifest,
		Source:         source,
		Artifact:       artifact,
		MetadataSHA256: "sha256:" + strings.Repeat("e", 64),
		ArchiveSHA256:  "sha256:" + strings.Repeat("f", 64),
		ArchiveBytes:   4096,
		Evidence:       evidence,
		ActivatedAt:    activatedAt,
	}
	if err := writeJSONFile(firstReceiptPath, receipt); err != nil {
		t.Fatal(err)
	}
	first := &Coordinator{ProjectRoot: projectRoot, ConfigPath: firstConfig, Clock: func() time.Time { return activatedAt }}
	if err := first.recordCurrentSandboxArtifact(context.Background(), artifact, manifestHash, activatedAt); err != nil {
		t.Fatal(err)
	}

	runtime := &reusableTemplateRuntime{want: artifact}
	second := &Coordinator{ProjectRoot: projectRoot, ConfigPath: secondConfig, Clock: func() time.Time { return activatedAt.Add(time.Minute) }}
	adopted, err := second.adoptReusableDockerSandboxesTemplateLocked(context.Background(), manifest, source, manifestHash, artifact.RootDisk, runtime)
	if err != nil {
		t.Fatal(err)
	}
	if !adopted || runtime.verified != 3 || runtime.activated != 1 || runtime.imported != 0 {
		t.Fatalf("adopted=%t verified=%d activated=%d imported=%d", adopted, runtime.verified, runtime.activated, runtime.imported)
	}
	secondReceiptPath, err := DockerSandboxesReceiptPathForConfig(projectRoot, secondConfig)
	if err != nil {
		t.Fatal(err)
	}
	secondReceipt, err := readDockerSandboxesReceiptPath(secondReceiptPath)
	if err != nil {
		t.Fatal(err)
	}
	if secondReceipt.Artifact != artifact || secondReceipt.ManifestHash != manifestHash {
		t.Fatalf("second receipt = %+v", secondReceipt)
	}
	if err := validateDockerSandboxesReceiptEvidence(secondReceiptPath, secondReceipt); err != nil {
		t.Fatal(err)
	}
	thirdConfig := filepath.Join(projectRoot, ".local", "config.third.yml")
	if err := os.WriteFile(thirdConfig, []byte("provider:\n  type: docker-sandboxes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	staleRuntime := &reusableTemplateRuntime{want: artifact, verifyError: provider.ErrTemplateNotFound}
	third := &Coordinator{ProjectRoot: projectRoot, ConfigPath: thirdConfig, Clock: func() time.Time { return activatedAt.Add(2 * time.Minute) }}
	adopted, err = third.adoptReusableDockerSandboxesTemplateLocked(context.Background(), manifest, source, manifestHash, artifact.RootDisk, staleRuntime)
	if err != nil {
		t.Fatal(err)
	}
	if adopted || staleRuntime.verified != 1 || staleRuntime.activated != 0 || staleRuntime.imported != 0 {
		t.Fatalf("stale adopted=%t verified=%d activated=%d imported=%d", adopted, staleRuntime.verified, staleRuntime.activated, staleRuntime.imported)
	}
	thirdReceiptPath, err := DockerSandboxesReceiptPathForConfig(projectRoot, thirdConfig)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(thirdReceiptPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale reusable receipt was cloned before cache preflight: %v", err)
	}

	store, err := storagecatalog.Open(stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	value, err := store.Load(activatedAt.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	for _, resource := range value.Resources {
		if resource.Kind == catalogSandboxTemplateKind && resource.ManifestHash == manifestHash {
			if len(resource.References) != 2 {
				t.Fatalf("shared template references = %+v, want two configs", resource.References)
			}
			return
		}
	}
	t.Fatal("shared template resource was not retained")
}

func TestForcedDockerSandboxesGenerationImportsBesideOldIdentityAndPreservesRollback(t *testing.T) {
	for _, test := range []struct {
		name        string
		readbackErr error
		wantSuccess bool
	}{
		{name: "authoritative readback activates new generation", wantSuccess: true},
		{name: "authoritative readback failure preserves old activation", readbackErr: errors.New("authoritative template readback failed"), wantSuccess: false},
	} {
		t.Run(test.name, func(t *testing.T) {
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
			lockDestination := filepath.Join(projectRoot, "templates", "docker-sandboxes", "sources.lock.json")
			if err := os.MkdirAll(filepath.Dir(lockDestination), 0o700); err != nil {
				t.Fatal(err)
			}
			lockContent, err := os.ReadFile(filepath.Join("..", "..", "templates", "docker-sandboxes", "sources.lock.json"))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(lockDestination, lockContent, 0o600); err != nil {
				t.Fatal(err)
			}

			manifest := Manifest{
				SchemaVersion:        ManifestSchemaVersion,
				ProviderType:         "docker-sandboxes",
				ProviderPlatform:     "linux/amd64",
				SourceType:           "docker-image",
				SourceImage:          "ghcr.io/catthehacker/ubuntu:full-latest",
				SourcePlatform:       "linux/amd64",
				SourceDigest:         "sha256:" + strings.Repeat("a", 64),
				SourcePlatformDigest: "sha256:" + strings.Repeat("b", 64),
				RunnerSelector:       "latest",
				RunnerVersion:        "2.336.0",
				RunnerAssetName:      "actions-runner-linux-x64-2.336.0.tar.gz",
				RunnerAssetURL:       "https://github.com/actions/runner/releases/download/v2.336.0/actions-runner-linux-x64-2.336.0.tar.gz",
				RunnerAssetDigest:    "sha256:" + strings.Repeat("c", 64),
			}
			manifestHash, err := ManifestHash(manifest)
			if err != nil {
				t.Fatal(err)
			}
			source := ResolvedDockerSource{
				Reference:            manifest.SourceImage,
				ImmutableReference:   "ghcr.io/catthehacker/ubuntu@" + manifest.SourceDigest,
				IndexDigest:          manifest.SourceDigest,
				PlatformDigest:       manifest.SourcePlatformDigest,
				Platform:             manifest.SourcePlatform,
				CompressedLayerBytes: 1024,
			}
			activatedAt := time.Date(2026, time.August, 8, 1, 2, 3, 0, time.UTC)
			environment := &generationImportEnvironment{}
			coordinator := &Coordinator{
				Config:      config.Config{Storage: config.StorageConfig{AutomaticHousekeeping: config.StorageHousekeepingDisabled}},
				ProjectRoot: projectRoot,
				ConfigPath:  configPath,
				Clock:       func() time.Time { return activatedAt },
				environment: environment,
			}
			generation, workspace, buildLock, err := coordinator.beginDockerSandboxesBuild(manifestHash)
			if err != nil {
				t.Fatal(err)
			}
			defer buildLock.Close()
			generationRoot, err := coordinator.dockerSandboxesBuildGenerationRoot(manifestHash)
			if err != nil {
				t.Fatal(err)
			}
			candidate := provider.TemplateArtifact{
				Reference: "docker.io/library/" + dockerSandboxesGenerationTag("full-latest", manifestHash, "amd64", generation.ConfigID, generation.Generation),
				Digest:    "sha256:" + strings.Repeat("e", 64),
				CacheID:   strings.Repeat("e", 12),
				Platform:  "linux/amd64",
				RootDisk:  "90GiB",
			}
			previous := provider.TemplateArtifact{
				Reference: "docker.io/library/epar-docker-sandboxes-catthehacker-full-latest:" + manifestHash[:16] + "-amd64",
				Digest:    "sha256:" + strings.Repeat("d", 64),
				CacheID:   strings.Repeat("d", 12),
				Platform:  "linux/amd64",
				RootDisk:  "90GiB",
			}
			if candidate.Reference == previous.Reference || candidate.CacheID == previous.CacheID {
				t.Fatalf("forced candidate did not receive a distinct physical identity: previous=%+v candidate=%+v", previous, candidate)
			}

			metadataPath, metadataSHA, archivePath, archiveSHA, archiveBytes := writeDockerSandboxesPublicationFixture(t, workspace)
			if err := coordinator.recordSandboxWorkspace(context.Background(), workspace, manifestHash, storagecatalog.StateStaging, activatedAt); err != nil {
				t.Fatal(err)
			}
			receiptPath, err := coordinator.dockerSandboxesReceiptPath()
			if err != nil {
				t.Fatal(err)
			}
			oldReceipt := dockerSandboxesReceipt{
				SchemaVersion:  dockerSandboxesLegacyReceiptSchema,
				ManifestHash:   manifestHash,
				Manifest:       manifest,
				Source:         source,
				Artifact:       previous,
				MetadataSHA256: "sha256:" + strings.Repeat("7", 64),
				ArchiveSHA256:  "sha256:" + strings.Repeat("8", 64),
				ArchiveBytes:   4096,
				Evidence:       writeLegacyDockerSandboxesReceiptEvidence(t, receiptPath, manifestHash),
				ActivatedAt:    activatedAt.Add(-time.Hour),
			}
			if err := writeJSONFile(receiptPath, oldReceipt); err != nil {
				t.Fatal(err)
			}
			if err := coordinator.recordCurrentSandboxArtifact(context.Background(), previous, manifestHash, oldReceipt.ActivatedAt); err != nil {
				t.Fatal(err)
			}
			runtime := &generationImportRuntime{
				cached:      map[string]provider.TemplateArtifact{previous.Reference: previous},
				candidate:   candidate,
				readbackErr: test.readbackErr,
			}

			_, publishedAt, publishErr := coordinator.importOrAdoptDockerSandboxesTemplate(context.Background(), manifest, source, manifestHash, candidate.RootDisk, false, candidate, metadataPath, metadataSHA, archivePath, archiveSHA, archiveBytes, runtime)
			if test.wantSuccess {
				if publishErr != nil {
					t.Fatal(publishErr)
				}
				if publishedAt.IsZero() || runtime.activated != candidate {
					t.Fatalf("candidate was not activated after exact readback: publishedAt=%s activated=%+v", publishedAt, runtime.activated)
				}
				if err := coordinator.finishDockerSandboxesTemplateActivation(context.Background(), archivePath, manifestHash, publishedAt); err != nil {
					t.Fatal(err)
				}
				activeReceipt, err := readDockerSandboxesReceiptPath(receiptPath)
				if err != nil {
					t.Fatal(err)
				}
				if activeReceipt.SchemaVersion != dockerSandboxesReceiptSchema || activeReceipt.Artifact != candidate {
					t.Fatalf("active receipt schema=%d artifact=%+v, want schema=%d candidate=%+v", activeReceipt.SchemaVersion, activeReceipt.Artifact, dockerSandboxesReceiptSchema, candidate)
				}
				if err := validateDockerSandboxesReceiptEvidence(receiptPath, activeReceipt); err != nil {
					t.Fatalf("candidate evidence is not exact: %v", err)
				}
				for name, evidence := range activeReceipt.Evidence {
					if !strings.Contains(evidence.Path, "/"+candidate.CacheID+"/") {
						t.Fatalf("candidate evidence %s is not physical-generation scoped: %s", name, evidence.Path)
					}
				}
				if _, err := os.Stat(filepath.Join(generationRoot, "candidate.json")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("completed candidate pointer still exists: %v", err)
				}
			} else {
				if publishErr == nil || !strings.Contains(publishErr.Error(), test.readbackErr.Error()) {
					t.Fatalf("publication error = %v, want authoritative readback failure", publishErr)
				}
				if !publishedAt.IsZero() || runtime.activated != (provider.TemplateArtifact{}) {
					t.Fatalf("failed candidate changed activation: publishedAt=%s activated=%+v", publishedAt, runtime.activated)
				}
				activeReceipt, err := readDockerSandboxesReceiptPath(receiptPath)
				if err != nil {
					t.Fatal(err)
				}
				if activeReceipt.Artifact != previous || activeReceipt.ActivatedAt != oldReceipt.ActivatedAt {
					t.Fatalf("failed candidate replaced old receipt: %+v", activeReceipt)
				}
				if err := validateDockerSandboxesReceiptEvidence(receiptPath, activeReceipt); err != nil {
					t.Fatalf("failed candidate changed old receipt evidence: %v", err)
				}
				if _, err := os.Stat(filepath.Join(generationRoot, "candidate.json")); err != nil {
					t.Fatalf("failed candidate is not resumable: %v", err)
				}
			}
			if runtime.imports != 1 || runtime.verifications < 2 {
				t.Fatalf("imports=%d verifications=%d, want one import plus exact pre/post readback", runtime.imports, runtime.verifications)
			}
			if cached := runtime.cached[previous.Reference]; cached != previous {
				t.Fatalf("old exact template was removed or changed: %+v", cached)
			}
			assertDockerSandboxesGenerationCatalog(t, stateRoot, manifestHash, previous, candidate, test.wantSuccess)
		})
	}
}

func writeDockerSandboxesPublicationFixture(t *testing.T, workspace string) (string, string, string, string, uint64) {
	t.Helper()
	for name, filename := range dockerSandboxesCompactEvidenceFiles {
		if name == "sbomDescriptor" {
			continue
		}
		if err := os.WriteFile(filepath.Join(workspace, filename), []byte(name+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(workspace, "sbom.intoto.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	archivePath := filepath.Join(workspace, "runner-template.tar")
	if err := os.WriteFile(archivePath, []byte("verified archive\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	metadataPath := filepath.Join(workspace, "template-metadata.json")
	metadataSHA, _, err := hashFile(metadataPath)
	if err != nil {
		t.Fatal(err)
	}
	archiveSHA, archiveBytes, err := hashFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	return metadataPath, metadataSHA, archivePath, archiveSHA, archiveBytes
}

func writeLegacyDockerSandboxesReceiptEvidence(t *testing.T, receiptPath, manifestHash string) map[string]artifactEvidence {
	t.Helper()
	evidence := make(map[string]artifactEvidence, len(dockerSandboxesCompactEvidenceFiles))
	for name, filename := range dockerSandboxesCompactEvidenceFiles {
		relative := filepath.ToSlash(filepath.Join("evidence", manifestHash, filename))
		path := filepath.Join(filepath.Dir(receiptPath), filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("old "+name+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		digest, _, err := hashFile(path)
		if err != nil {
			t.Fatal(err)
		}
		evidence[name] = artifactEvidence{Path: relative, SHA256: digest}
		if name == "sbomDescriptor" {
			entry := evidence[name]
			entry.SourceDigest = "sha256:" + strings.Repeat("9", 64)
			evidence[name] = entry
		}
	}
	return evidence
}

func assertDockerSandboxesGenerationCatalog(t *testing.T, stateRoot, manifestHash string, previous, candidate provider.TemplateArtifact, success bool) {
	t.Helper()
	store, err := storagecatalog.Open(stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	value, err := store.Load(time.Date(2026, time.August, 8, 2, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	var previousResource *storagecatalog.Resource
	var candidateResource *storagecatalog.Resource
	for index := range value.Resources {
		resource := &value.Resources[index]
		if resource.Kind != catalogSandboxTemplateKind || resource.ManifestHash != manifestHash {
			continue
		}
		switch resource.Identity {
		case previous.CacheID:
			previousResource = resource
		case candidate.CacheID:
			candidateResource = resource
		}
	}
	if previousResource == nil {
		t.Fatal("old exact template is missing from the ownership catalog")
	}
	if success {
		if previousResource.State != storagecatalog.StateSuperseded || len(previousResource.References) != 0 {
			t.Fatalf("old exact template retention state = %+v", *previousResource)
		}
		if candidateResource == nil || candidateResource.State != storagecatalog.StateCurrent || len(candidateResource.References) != 1 {
			t.Fatalf("candidate catalog state = %+v", candidateResource)
		}
	} else {
		if previousResource.State != storagecatalog.StateCurrent || len(previousResource.References) != 1 {
			t.Fatalf("failed candidate changed old catalog ownership: %+v", *previousResource)
		}
		if candidateResource != nil {
			t.Fatalf("failed candidate was published into the template catalog: %+v", *candidateResource)
		}
	}
}

func TestReusableDockerSandboxesReceiptRejectsCorruptedEvidence(t *testing.T) {
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
	receiptPath, err := DockerSandboxesReceiptPathForConfig(projectRoot, configPath)
	if err != nil {
		t.Fatal(err)
	}
	receipt := dockerSandboxesReceipt{Evidence: map[string]artifactEvidence{"proof": {Path: "../outside", SHA256: "sha256:" + strings.Repeat("a", 64)}}}
	if err := validateDockerSandboxesReceiptEvidence(receiptPath, receipt); err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("corrupt evidence error = %v", err)
	}
}

func TestConcurrentDockerSandboxesPublicationPrecedesSecondConfigReuse(t *testing.T) {
	stateRoot := filepath.Join(t.TempDir(), "host-state")
	t.Setenv("EPAR_STATE_HOME", stateRoot)
	projectRoot := t.TempDir()
	firstConfig := filepath.Join(projectRoot, ".local", "config.first.yml")
	secondConfig := filepath.Join(projectRoot, ".local", "config.second.yml")
	for _, path := range []string{firstConfig, secondConfig} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("provider:\n  type: docker-sandboxes\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	manifest := Manifest{
		SchemaVersion:        ManifestSchemaVersion,
		ProviderType:         "docker-sandboxes",
		ProviderPlatform:     "linux/arm64",
		SourceType:           "docker-image",
		SourceImage:          "ghcr.io/catthehacker/ubuntu:js-latest",
		SourcePlatform:       "linux/arm64",
		SourceDigest:         "sha256:" + strings.Repeat("a", 64),
		SourcePlatformDigest: "sha256:" + strings.Repeat("b", 64),
		RunnerSelector:       "latest",
		RunnerVersion:        "2.336.0",
		RunnerAssetName:      "actions-runner-linux-arm64-2.336.0.tar.gz",
		RunnerAssetURL:       "https://github.com/actions/runner/releases/download/v2.336.0/actions-runner-linux-arm64-2.336.0.tar.gz",
		RunnerAssetDigest:    "sha256:" + strings.Repeat("c", 64),
	}
	manifestHash, err := ManifestHash(manifest)
	if err != nil {
		t.Fatal(err)
	}
	source := ResolvedDockerSource{
		Reference:            manifest.SourceImage,
		ImmutableReference:   "ghcr.io/catthehacker/ubuntu@" + manifest.SourceDigest,
		IndexDigest:          manifest.SourceDigest,
		PlatformDigest:       manifest.SourcePlatformDigest,
		Platform:             manifest.SourcePlatform,
		CompressedLayerBytes: 1024,
	}
	firstArtifact := provider.TemplateArtifact{
		Reference: "docker.io/library/epar-docker-sandboxes-catthehacker-js-latest:" + manifestHash[:16] + "-arm64",
		Digest:    "sha256:" + strings.Repeat("d", 64),
		CacheID:   strings.Repeat("d", 12),
		Platform:  "linux/arm64",
		RootDisk:  "40GiB",
	}
	secondArtifact := firstArtifact
	secondArtifact.Digest = "sha256:" + strings.Repeat("e", 64)
	secondArtifact.CacheID = strings.Repeat("e", 12)

	artifactRoot := filepath.Join(projectRoot, ".local", "race-artifact")
	if err := os.MkdirAll(artifactRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, filename := range dockerSandboxesCompactEvidenceFiles {
		if name == "sbomDescriptor" {
			continue
		}
		if err := os.WriteFile(filepath.Join(artifactRoot, filename), []byte(name+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(artifactRoot, "sbom.intoto.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	archivePath := filepath.Join(artifactRoot, "template.tar")
	if err := os.WriteFile(archivePath, []byte("verified archive\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	metadataPath := filepath.Join(artifactRoot, "template-metadata.json")
	metadataSHA, _, err := hashFile(metadataPath)
	if err != nil {
		t.Fatal(err)
	}
	archiveSHA, _, err := hashFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	archiveBytes := uint64(len("verified archive\n"))

	runtime := &publicationRaceRuntime{
		cached:                 firstArtifact,
		activationStarted:      make(chan struct{}),
		releaseFirstActivation: make(chan struct{}),
	}
	activatedAt := time.Date(2026, time.August, 2, 4, 5, 6, 0, time.UTC)
	first := &Coordinator{ProjectRoot: projectRoot, ConfigPath: firstConfig, Clock: func() time.Time { return activatedAt }}
	second := &Coordinator{ProjectRoot: projectRoot, ConfigPath: secondConfig, Clock: func() time.Time { return activatedAt.Add(time.Minute) }}
	type result struct {
		adopted bool
		err     error
	}
	firstDone := make(chan result, 1)
	go func() {
		adopted, _, publishErr := first.importOrAdoptDockerSandboxesTemplate(context.Background(), manifest, source, manifestHash, firstArtifact.RootDisk, true, firstArtifact, metadataPath, metadataSHA, archivePath, archiveSHA, archiveBytes, runtime)
		firstDone <- result{adopted: adopted, err: publishErr}
	}()
	select {
	case <-runtime.activationStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("first configuration did not reach locked publication")
	}

	secondStarted := make(chan struct{})
	secondDone := make(chan result, 1)
	go func() {
		close(secondStarted)
		adopted, _, adoptErr := second.importOrAdoptDockerSandboxesTemplate(context.Background(), manifest, source, manifestHash, secondArtifact.RootDisk, true, secondArtifact, metadataPath, metadataSHA, archivePath, archiveSHA, archiveBytes, runtime)
		secondDone <- result{adopted: adopted, err: adoptErr}
	}()
	<-secondStarted
	select {
	case premature := <-secondDone:
		close(runtime.releaseFirstActivation)
		<-firstDone
		t.Fatalf("second configuration escaped the backend lock before publication: %+v", premature)
	case <-time.After(200 * time.Millisecond):
	}
	close(runtime.releaseFirstActivation)

	firstResult := <-firstDone
	if firstResult.err != nil || firstResult.adopted {
		t.Fatalf("first publication result = %+v", firstResult)
	}
	secondResult := <-secondDone
	if secondResult.err != nil || !secondResult.adopted {
		t.Fatalf("second reuse result = %+v", secondResult)
	}
	runtime.mu.Lock()
	imports := runtime.imports
	runtime.mu.Unlock()
	if imports != 0 {
		t.Fatalf("template imports = %d, want zero", imports)
	}
}
