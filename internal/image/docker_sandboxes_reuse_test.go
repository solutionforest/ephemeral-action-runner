package image

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
	storagecatalog "github.com/solutionforest/ephemeral-action-runner/internal/storage/catalog"
)

type reusableTemplateRuntime struct {
	want        provider.TemplateArtifact
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
		SchemaVersion:  dockerSandboxesReceiptSchema,
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
	if !adopted || runtime.verified != 1 || runtime.activated != 1 || runtime.imported != 0 {
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
		adopted, _, publishErr := first.importOrAdoptDockerSandboxesTemplate(context.Background(), manifest, source, manifestHash, firstArtifact.RootDisk, true, firstArtifact, metadataPath, metadataSHA, archivePath, archiveSHA, runtime)
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
		adopted, _, adoptErr := second.importOrAdoptDockerSandboxesTemplate(context.Background(), manifest, source, manifestHash, secondArtifact.RootDisk, true, secondArtifact, metadataPath, metadataSHA, archivePath, archiveSHA, runtime)
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
