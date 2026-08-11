package image

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/solutionforest/ephemeral-action-runner/internal/config"
	"github.com/solutionforest/ephemeral-action-runner/internal/hosttrust"
	"github.com/solutionforest/ephemeral-action-runner/internal/prebuilt"
	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
	dockersandboxesprovider "github.com/solutionforest/ephemeral-action-runner/internal/provider/dockersandboxes"
)

type candidateOnlyPrebuiltResolver struct {
	stableCalls    int
	candidateCalls int
	err            error
}

func (r *candidateOnlyPrebuiltResolver) ResolveAndVerify(context.Context, string, string, string) (VerifiedDockerSandboxesPrebuilt, error) {
	r.stableCalls++
	return VerifiedDockerSandboxesPrebuilt{}, errors.New("stable resolver must not be used")
}

func (r *candidateOnlyPrebuiltResolver) ResolveCandidate(context.Context, string, string, string, string, string) (VerifiedDockerSandboxesPrebuilt, error) {
	r.candidateCalls++
	return VerifiedDockerSandboxesPrebuilt{}, r.err
}

type prebuiltAcquisitionEnvironment struct{ Environment }

func (*prebuiltAcquisitionEnvironment) ResolveBuildTrust(context.Context) (hosttrust.Snapshot, error) {
	return hosttrust.Snapshot{}, nil
}

func (*prebuiltAcquisitionEnvironment) Infof(string, ...any) {}

func TestDockerSandboxesPrebuiltLocalIdentityExcludesRuntimeCAOverlay(t *testing.T) {
	base := config.Config{
		Provider: config.ProviderConfig{Type: "docker-sandboxes", Platform: "linux/amd64"},
		Image: config.ImageConfig{
			Distribution:      config.ImageDistributionPrebuilt,
			SourceImage:       "ghcr.io/catthehacker/ubuntu:act-latest",
			SourcePlatform:    "linux/amd64",
			PrebuiltReference: config.DockerSandboxesPrebuiltActReference,
		},
	}
	first := &Coordinator{Config: base, ProjectRoot: t.TempDir()}
	firstManifest, err := first.dockerSandboxesPrebuiltLocalManifest()
	if err != nil {
		t.Fatal(err)
	}
	changed := base
	changed.Image.HostTrustMode = config.HostTrustModeOverlay
	changed.Image.HostTrustScopes = []string{"system", "user"}
	changed.Image.TrustedCACertificatePaths = []string{"a.pem", "b.pem"}
	second := &Coordinator{Config: changed, ProjectRoot: t.TempDir()}
	secondManifest, err := second.dockerSandboxesPrebuiltLocalManifest()
	if err != nil {
		t.Fatal(err)
	}
	firstHash, err := ManifestHash(firstManifest)
	if err != nil {
		t.Fatal(err)
	}
	secondHash, err := ManifestHash(secondManifest)
	if err != nil {
		t.Fatal(err)
	}
	if firstHash != secondHash || len(firstManifest.TrustedCACertificates) != 0 || len(secondManifest.TrustedCACertificates) != 0 {
		t.Fatalf("prebuilt base identity changed with runtime CA overlay: %s != %s", firstHash, secondHash)
	}
}

func TestDockerSandboxesCandidateAcceptanceNeverFallsBackToStableAlias(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	resolver := &candidateOnlyPrebuiltResolver{err: errors.New("candidate evidence unavailable")}
	coordinator := &Coordinator{
		Config: config.Config{Provider: config.ProviderConfig{Type: "docker-sandboxes", Platform: "linux/amd64"}, Image: config.ImageConfig{
			Distribution: config.ImageDistributionPrebuilt, SourcePlatform: "linux/amd64", PrebuiltReference: config.DockerSandboxesPrebuiltActReference,
			PrebuiltDigest: digest, PrebuiltAcceptance: true, PrebuiltCatalogReference: prebuilt.DefaultPackageRepository + ":catalog-v1-pkg-" + strings.Repeat("b", 64), PrebuiltEvidenceRef: "refs/heads/feature/prebuilt_img",
		}},
		PrebuiltResolver: resolver,
	}
	_, err := coordinator.resolveVerifiedDockerSandboxesPrebuilt(context.Background())
	if err == nil || !strings.Contains(err.Error(), "candidate evidence unavailable") {
		t.Fatalf("candidate resolution error = %v", err)
	}
	if resolver.candidateCalls != 1 || resolver.stableCalls != 0 {
		t.Fatalf("candidate/stable calls = %d/%d, want 1/0", resolver.candidateCalls, resolver.stableCalls)
	}
}

func TestDockerSandboxesCandidateReceiptRequiresAcceptanceEvidence(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	catalogDigest := "sha256:" + strings.Repeat("b", 64)
	platformDigest := "sha256:" + strings.Repeat("c", 64)
	catalogReference := prebuilt.DefaultPackageRepository + ":catalog-v1-pkg-" + strings.Repeat("d", 64)
	receipt := dockerSandboxesReceipt{
		SchemaVersion: dockerSandboxesReceiptSchema, Distribution: dockerSandboxesDistributionPrebuilt, ManifestHash: strings.Repeat("e", 64),
		Manifest:       Manifest{SchemaVersion: ManifestSchemaVersion, Distribution: dockerSandboxesDistributionPrebuilt, Prebuilt: &PrebuiltManifestMetadata{PackageIndexDigest: digest, PackagePlatformDigest: platformDigest, CatalogDigest: catalogDigest, Acceptance: true, CatalogReference: catalogReference, EvidenceRef: "refs/heads/feature/prebuilt_img"}},
		Artifact:       provider.TemplateArtifact{Reference: "docker.io/library/epar-candidate:test", Digest: platformDigest, CacheID: strings.Repeat("c", 12), Platform: "linux/amd64", RootDisk: "20GiB"},
		MetadataSHA256: digest, ArchiveSHA256: digest, ArchiveBytes: 1, Evidence: map[string]artifactEvidence{"proof": {Path: "proof.json", SHA256: digest}}, ActivatedAt: time.Unix(100, 0).UTC(),
		Prebuilt: &dockerSandboxesPrebuiltReceiptEvidence{CatalogDigest: catalogDigest, CatalogReference: catalogReference, EvidenceRef: "refs/heads/feature/prebuilt_img", Acceptance: true, Entry: prebuilt.Entry{PackageIndexDigest: digest, PackageReference: prebuilt.DefaultPackageRepository + "@" + digest}, PackageReference: prebuilt.DefaultPackageRepository + "@" + digest, PackageIndexDigest: digest, PackagePlatformDigest: platformDigest, EffectiveStatus: prebuilt.StatusCandidate, VerifiedAt: time.Unix(90, 0).UTC(), BaseArchiveSHA256: digest, BaseArchiveBytes: 1},
	}
	path := filepath.Join(t.TempDir(), "active.json")
	if err := writeJSONFile(path, receipt); err != nil {
		t.Fatal(err)
	}
	if _, err := readDockerSandboxesReceiptPath(path); err != nil {
		t.Fatalf("candidate acceptance receipt rejected: %v", err)
	}
	receipt.Prebuilt.Acceptance = false
	if err := writeJSONFile(path, receipt); err != nil {
		t.Fatal(err)
	}
	if _, err := readDockerSandboxesReceiptPath(path); err == nil || !strings.Contains(err.Error(), "prebuilt receipt evidence") {
		t.Fatalf("candidate receipt without acceptance error = %v", err)
	}
}

func TestDockerSandboxesPrebuiltPinRetainsScheduledSecurityStatusChecks(t *testing.T) {
	unpinned := Manifest{Distribution: dockerSandboxesDistributionPrebuilt, Prebuilt: &PrebuiltManifestMetadata{Reference: "ghcr.io/example/package@sha256:one"}}
	pinned := unpinned
	pinned.Prebuilt = &PrebuiltManifestMetadata{Reference: "ghcr.io/example/package@sha256:one", Pinned: true}
	if !manifestHasMutableRemoteInputs(unpinned) {
		t.Fatal("moving prebuilt selector was treated as immutable")
	}
	if !manifestHasMutableRemoteInputs(pinned) {
		t.Fatal("exact prebuilt digest pin disabled signed revocation checks")
	}
}

func TestDockerSandboxesPrebuiltAliasProfileSupportsFullAndActOnly(t *testing.T) {
	for _, test := range []struct {
		reference string
		want      string
	}{
		{reference: config.DockerSandboxesPrebuiltFullReference, want: prebuilt.ProfileFull},
		{reference: config.DockerSandboxesPrebuiltActReference, want: prebuilt.ProfileAct},
	} {
		got, err := dockerSandboxesPrebuiltProfileForAlias(test.reference)
		if err != nil || got != test.want {
			t.Fatalf("profile for %q = %q, %v; want %q", test.reference, got, err, test.want)
		}
	}
	if _, err := dockerSandboxesPrebuiltProfileForAlias(prebuilt.DefaultPackageRepository + ":full-preview"); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("unsupported alias error = %v", err)
	}
}

func TestDockerSandboxesPrebuiltAcquisitionDownloadsOnceThenReusesExactArchive(t *testing.T) {
	root := t.TempDir()
	t.Setenv("EPAR_STATE_HOME", filepath.Join(root, "host-state"))
	verified := dockerSandboxesPrebuiltFixture()
	fixturePath, _, _ := writeDockerArchiveWithTagAndLabels(t, "fixture:source", dockerSandboxesPrebuiltBaseLabels(verified))
	fixtureImage, err := tarball.ImageFromPath(fixturePath, nil)
	if err != nil {
		t.Fatal(err)
	}
	platformDigest, err := fixtureImage.Digest()
	if err != nil {
		t.Fatal(err)
	}
	verified.Platform.Digest = platformDigest.String()
	verified.Entry.Platforms[0].PackageManifestDigest = platformDigest.String()
	verified.CatalogDigest = "sha256:" + strings.Repeat("b", 64)

	fetches := 0
	coordinator := &Coordinator{
		Config:      config.Config{Provider: config.ProviderConfig{Type: "docker-sandboxes", Platform: "linux/amd64"}},
		ProjectRoot: root,
		ConfigPath:  filepath.Join(root, "config.yml"),
		Clock:       func() time.Time { return time.Unix(100, 0).UTC() },
		environment: &prebuiltAcquisitionEnvironment{},
		dockerSandboxesPrebuiltImageFetcher: func(context.Context, name.Digest, http.RoundTripper) (v1.Image, error) {
			fetches++
			return fixtureImage, nil
		},
	}
	archivePath, acquisition, err := coordinator.acquireDockerSandboxesPrebuiltArchive(context.Background(), verified)
	if err != nil {
		t.Fatal(err)
	}
	if fetches != 1 {
		t.Fatalf("cold acquisition fetched %d times, want exactly once", fetches)
	}
	verified.CatalogDigest = "sha256:" + strings.Repeat("c", 64)
	reusedPath, reused, err := coordinator.acquireDockerSandboxesPrebuiltArchive(context.Background(), verified)
	if err != nil {
		t.Fatal(err)
	}
	if fetches != 1 {
		t.Fatalf("verified archive reuse performed another package fetch: %d", fetches)
	}
	if reusedPath != archivePath || reused.ArchiveSHA256 != acquisition.ArchiveSHA256 || reused.ImageConfigDigest != acquisition.ImageConfigDigest {
		t.Fatalf("archive identity changed during catalog-only refresh: first=%+v second=%+v", acquisition, reused)
	}
	if reused.CatalogDigest != verified.CatalogDigest {
		t.Fatalf("catalog evidence was not refreshed on no-download reuse: got %s", reused.CatalogDigest)
	}
	var stored dockerSandboxesPrebuiltAcquisition
	if err := readJSONFile(filepath.Join(filepath.Dir(archivePath), "acquisition.json"), &stored); err != nil {
		t.Fatal(err)
	}
	if stored.CatalogDigest != verified.CatalogDigest {
		t.Fatalf("persisted acquisition catalog digest = %s, want %s", stored.CatalogDigest, verified.CatalogDigest)
	}
}

func TestDockerSandboxesPrebuiltArchiveRejectsWrongRunnerAndRecipeLabels(t *testing.T) {
	verified := dockerSandboxesPrebuiltFixture()
	archivePath, configDigest, _ := writeDockerArchiveWithTagAndLabels(t, dockerSandboxesPrebuiltBaseTag(verified), dockerSandboxesPrebuiltBaseLabels(verified))
	archiveSHA, archiveBytes, err := hashFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	acquisition := dockerSandboxesPrebuiltAcquisition{ImageConfigDigest: configDigest, ArchiveSHA256: archiveSHA, ArchiveBytes: archiveBytes}
	coordinator := &Coordinator{}
	if err := coordinator.verifyDockerSandboxesPrebuiltBaseArchive(archivePath, dockerSandboxesPrebuiltBaseTag(verified), verified, acquisition); err != nil {
		t.Fatalf("exact base archive rejected: %v", err)
	}

	t.Run("runner", func(t *testing.T) {
		changed := verified
		changed.Entry.Runner.Version = "9.9.9"
		if err := coordinator.verifyDockerSandboxesPrebuiltBaseArchive(archivePath, dockerSandboxesPrebuiltBaseTag(changed), changed, acquisition); err == nil || !strings.Contains(err.Error(), "runner.version") {
			t.Fatalf("wrong runner label was not rejected exactly: %v", err)
		}
	})
	t.Run("recipe", func(t *testing.T) {
		changed := verified
		changed.Entry.Recipe.Digest = "sha256:" + strings.Repeat("f", 64)
		if err := coordinator.verifyDockerSandboxesPrebuiltBaseArchive(archivePath, dockerSandboxesPrebuiltBaseTag(changed), changed, acquisition); err == nil || !strings.Contains(err.Error(), "recipe.digest") {
			t.Fatalf("wrong recipe label was not rejected exactly: %v", err)
		}
	})
}

func TestDockerSandboxesPrebuiltDerivativeDoesNotAlsoAcquireBaseArchive(t *testing.T) {
	verified := dockerSandboxesPrebuiltFixture()
	acquisitions := 0
	derivativeBuilds := 0
	materialized, err := selectDockerSandboxesPrebuiltMaterialization(
		true,
		verified,
		"docker.io/library/epar-derivative:test",
		time.Unix(100, 0),
		func() (string, dockerSandboxesPrebuiltAcquisition, error) {
			acquisitions++
			return "base.tar", dockerSandboxesPrebuiltAcquisition{ImageConfigDigest: "sha256:" + strings.Repeat("6", 64)}, nil
		},
		func() (string, string, error) {
			derivativeBuilds++
			return "derivative.tar", "sha256:" + strings.Repeat("7", 64), nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if acquisitions != 0 || derivativeBuilds != 1 {
		t.Fatalf("derivative materialization made acquisition/build calls %d/%d, want 0/1", acquisitions, derivativeBuilds)
	}
	if materialized.ArchivePath != "derivative.tar" || materialized.Acquisition.ArchiveSHA256 != "" || materialized.ArtifactTag != "docker.io/library/epar-derivative:test" {
		t.Fatalf("unexpected derivative materialization: %+v", materialized)
	}

	acquisitions = 0
	derivativeBuilds = 0
	if _, err := selectDockerSandboxesPrebuiltMaterialization(
		false,
		verified,
		"unused",
		time.Unix(100, 0),
		func() (string, dockerSandboxesPrebuiltAcquisition, error) {
			acquisitions++
			return "base.tar", dockerSandboxesPrebuiltAcquisition{ImageConfigDigest: "sha256:" + strings.Repeat("6", 64)}, nil
		},
		func() (string, string, error) {
			derivativeBuilds++
			return "derivative.tar", "sha256:" + strings.Repeat("7", 64), nil
		},
	); err != nil {
		t.Fatal(err)
	}
	if acquisitions != 1 || derivativeBuilds != 0 {
		t.Fatalf("base materialization made acquisition/build calls %d/%d, want 1/0", acquisitions, derivativeBuilds)
	}
}

func TestDockerSandboxesCriticalRevocationAdmissionBlockPersistsButOrdinaryRevocationDoesNot(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "config.yml")
	firstProvider := &dockersandboxesprovider.Provider{}
	coordinator := &Coordinator{ProjectRoot: root, ConfigPath: configPath, Lifecycle: firstProvider}
	criticalDigest := "sha256:" + strings.Repeat("8", 64)
	state := UpdatePolicyState{SchemaVersion: updatePolicyStateSchemaVersion}
	if !coordinator.recordDockerSandboxesPrebuiltResolutionFailure(&state, &DockerSandboxesPrebuiltStatusError{Digest: criticalDigest, Status: prebuilt.StatusCriticalRevoked, Reason: "security incident"}) {
		t.Fatal("critical revocation was not recognized")
	}
	if err := coordinator.writeUpdatePolicyState(state); err != nil {
		t.Fatal(err)
	}
	if reason, blocked := firstProvider.TemplateAdmissionBlock(); !blocked || !strings.Contains(reason, criticalDigest) {
		t.Fatalf("critical revocation did not block current provider admission: %q %v", reason, blocked)
	}

	loaded, err := coordinator.readUpdatePolicyState()
	if err != nil {
		t.Fatal(err)
	}
	restartedProvider := &dockersandboxesprovider.Provider{}
	restarted := &Coordinator{ProjectRoot: root, ConfigPath: configPath, Lifecycle: restartedProvider}
	restarted.restoreDockerSandboxesPrebuiltAdmissionBlock(loaded)
	if reason, blocked := restartedProvider.TemplateAdmissionBlock(); !blocked || !strings.Contains(reason, criticalDigest) {
		t.Fatalf("persisted critical revocation was not restored after restart: %q %v", reason, blocked)
	}

	ordinary := UpdatePolicyState{SchemaVersion: updatePolicyStateSchemaVersion}
	if coordinator.recordDockerSandboxesPrebuiltResolutionFailure(&ordinary, &DockerSandboxesPrebuiltStatusError{Digest: criticalDigest, Status: prebuilt.StatusRevoked, Reason: "retired"}) {
		t.Fatal("ordinary revocation was promoted to a critical admission block")
	}
	if ordinary.AdmissionBlockedReason != "" {
		t.Fatalf("ordinary revocation persisted an admission block: %+v", ordinary)
	}
}

func TestDockerSandboxesPrebuiltCompatibilityGateRejectsUnsupportedContractSchemaAndRecipe(t *testing.T) {
	missingSourceTree := &Coordinator{ProjectRoot: t.TempDir()}
	for name, recipe := range map[string]prebuilt.RecipeDescriptor{
		"runtime-contract": {RuntimeContract: "docker-sandboxes-v2", TemplateSchema: 2},
		"template-schema":  {RuntimeContract: "docker-sandboxes-v1", TemplateSchema: 3},
	} {
		t.Run(name, func(t *testing.T) {
			if err := missingSourceTree.validateDockerSandboxesPrebuiltRecipe(prebuilt.Entry{Recipe: recipe}); err == nil || !strings.Contains(err.Error(), "unsupported runtime contract") {
				t.Fatalf("unsupported compatibility was not rejected before source/package materialization: %v", err)
			}
		})
	}
	repositoryRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	unknownRecipe := dockerSandboxesPrebuiltFixture().Entry
	unknownRecipe.Recipe.RuntimeContract = "docker-sandboxes-v1"
	unknownRecipe.Recipe.TemplateSchema = 2
	unknownRecipe.Recipe.Digest = "sha256:" + strings.Repeat("9", 64)
	unknownRecipe.Recipe.SourceLockDigest = "sha256:" + strings.Repeat("a", 64)
	unknownRecipe.Recipe.ToolDigest = "sha256:" + strings.Repeat("b", 64)
	if err := (&Coordinator{ProjectRoot: repositoryRoot}).validateDockerSandboxesPrebuiltRecipe(unknownRecipe); err == nil || !strings.Contains(err.Error(), "not supported by this controller") {
		t.Fatalf("unknown prebuilt recipe was not rejected: %v", err)
	}
}

func TestDockerSandboxesSupportedPrebuiltToolDigestMatchesPublisherCanonicalJSON(t *testing.T) {
	lock := []byte(`{
		"dockerfileFrontend":{"reference":"frontend"},
		"sbomGenerator":{"reference":"sbom"},
		"goBuilder":{"version":"1.25"},
		"emulation":{"backend":"qemu"},
		"tini":{"version":"0.19.0"},
		"ignored":{"changes":"do not affect the tool identity"}
	}`)
	got, err := dockerSandboxesSupportedPrebuiltToolDigest(lock)
	if err != nil {
		t.Fatal(err)
	}
	const want = "sha256:6ddef591a74f68147acca7a9534bc19a3f1c21c2c70ce4d4dafa0692d379754c"
	if got != want {
		t.Fatalf("tool digest = %s, want publisher-compatible %s", got, want)
	}
}

func dockerSandboxesPrebuiltFixture() VerifiedDockerSandboxesPrebuilt {
	indexDigest := "sha256:" + strings.Repeat("a", 64)
	platformDigest := "sha256:" + strings.Repeat("d", 64)
	sourceIndex := "sha256:" + strings.Repeat("1", 64)
	sourcePlatform := "sha256:" + strings.Repeat("2", 64)
	runnerDigest := "sha256:" + strings.Repeat("3", 64)
	recipeDigest := "sha256:" + strings.Repeat("4", 64)
	repository := prebuilt.DefaultPackageRepository
	return VerifiedDockerSandboxesPrebuilt{
		CatalogDigest: "sha256:" + strings.Repeat("5", 64),
		Entry: prebuilt.Entry{
			Profile: "act", PackageRepository: repository, PackageReference: repository + "@" + indexDigest, PackageIndexDigest: indexDigest,
			Source:    prebuilt.SourceDescriptor{Repository: "ghcr.io/catthehacker/ubuntu", SourceTag: "act-latest", Reference: "ghcr.io/catthehacker/ubuntu:act-latest", IndexDigest: sourceIndex, PlatformDigests: map[string]string{"linux/amd64": sourcePlatform}},
			Recipe:    prebuilt.RecipeDescriptor{Digest: recipeDigest, RuntimeContract: "docker-sandboxes-v1", TemplateSchema: 2},
			Runner:    prebuilt.RunnerDescriptor{Selector: "latest", Version: "2.999.0", AssetDigests: map[string]string{"linux/amd64": runnerDigest}},
			Platforms: []prebuilt.PlatformPublication{{Platform: "linux/amd64", PackageManifestDigest: platformDigest, SourceManifestDigest: sourcePlatform, Validated: true}},
		},
		Package:         prebuilt.ResolvedReference{Reference: repository + "@" + indexDigest, Digest: indexDigest},
		Platform:        prebuilt.PlatformDescriptor{Platform: "linux/amd64", Digest: platformDigest, Size: 1024},
		EffectiveStatus: prebuilt.StatusActive,
		VerifiedAt:      time.Unix(50, 0).UTC(),
	}
}

func writeDockerArchiveWithTagAndLabels(t *testing.T, tag string, labels map[string]string) (string, string, map[string]string) {
	t.Helper()
	layer := []byte("verified prebuilt layer")
	layerDigest := digestBytes(layer)
	imageConfig := dockerImageConfig{Architecture: "amd64", OS: "linux"}
	imageConfig.Config.Labels = labels
	imageConfig.RootFS.DiffIDs = []string{layerDigest}
	configBytes, err := json.Marshal(imageConfig)
	if err != nil {
		t.Fatal(err)
	}
	configDigest := digestBytes(configBytes)
	configName := strings.TrimPrefix(configDigest, "sha256:") + ".json"
	manifestBytes, err := json.Marshal([]dockerSaveManifestEntry{{Config: configName, RepoTags: []string{tag}, Layers: []string{"layer/layer.tar"}}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "prebuilt.tar")
	writeTarFixture(t, path, []tarFixtureEntry{{name: "manifest.json", content: manifestBytes}, {name: configName, content: configBytes}, {name: "layer/layer.tar", content: layer}})
	return path, configDigest, labels
}
