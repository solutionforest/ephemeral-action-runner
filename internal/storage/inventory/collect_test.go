package inventory

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/storage"
)

func TestCollectDeterministicInventoryAndPreview(t *testing.T) {
	t.Parallel()
	project := t.TempDir()
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	logs := filepath.Join(project, "work", "logs")
	mustMkdirAll(t, logs)
	mustWriteFile(t, filepath.Join(logs, "epar.log"), []byte("log"))

	nativeRoot := filepath.Join(project, ".local", "bin")
	oldKey := repeatedHex("1")
	currentKey := repeatedHex("2")
	writeNativeRevision(t, nativeRoot, oldKey, now.Add(-16*24*time.Hour), false)
	currentExecutable := writeNativeRevision(t, nativeRoot, currentKey, now.Add(-8*24*time.Hour), false)
	ambiguousKey := repeatedHex("3")
	mustMkdirAll(t, filepath.Join(nativeRoot, ambiguousKey))
	mustWriteFile(t, filepath.Join(nativeRoot, ambiguousKey, "ephemeral-action-runner"), []byte("unlabelled"))

	templateRoot := filepath.Join(project, "work", "template-builds", "docker-sandboxes")
	oldTemplate := writeTemplateFixture(t, templateRoot, "old", "act-22.04", "linux/amd64", "old", now.Add(-16*24*time.Hour), nil)
	currentTemplate := writeTemplateFixture(t, templateRoot, "current", "act-22.04", "linux/amd64", "current", now.Add(-8*24*time.Hour), nil)

	options := Options{
		ProjectRoot:       project,
		Now:               now,
		CurrentExecutable: currentExecutable,
		CurrentRevision:   "sha256:" + currentKey,
		ConfiguredTemplates: []TemplateSelection{{
			Profile:        currentTemplate.Profile,
			Platform:       currentTemplate.Platform,
			Tag:            currentTemplate.Tag,
			TemplateDigest: currentTemplate.TemplateDigest,
			ActivatedAt:    now.Add(-8 * 24 * time.Hour),
		}},
		TemplateProtections: []TemplateProtection{{
			ArchiveSHA256: oldTemplate.ArchiveSHA256,
			Kind:          storage.ProtectionCertification,
			Detail:        "release evidence",
		}},
	}
	first, err := Collect(options)
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	second, err := Collect(options)
	if err != nil {
		t.Fatalf("second Collect() error = %v", err)
	}
	if first.CollectedAt != second.CollectedAt || first.ProjectRoot != second.ProjectRoot || first.ProviderFilter != second.ProviderFilter || !reflect.DeepEqual(first.Warnings, second.Warnings) {
		t.Fatalf("Collect() stable metadata is nondeterministic:\nfirst=%+v\nsecond=%+v", first, second)
	}
	assertArtifactsEqual(t, first.Artifacts, second.Artifacts)
	if len(first.Surfaces) != 1 || first.Surfaces[0].ID != ProjectSurfaceID || !first.Surfaces[0].Capacity.Known {
		t.Fatalf("Collect() surfaces = %+v", first.Surfaces)
	}

	currentNative := findArtifact(t, first.Artifacts, "native-controller:"+currentKey)
	if !currentNative.Current || currentNative.Ownership.Kind != storage.OwnershipExact {
		t.Fatalf("current native artifact = %+v", currentNative)
	}
	oldNative := findArtifact(t, first.Artifacts, "native-controller:"+oldKey)
	if oldNative.SupersededAt == nil || !oldNative.SupersededAt.Equal(now.Add(-8*24*time.Hour)) {
		t.Fatalf("old native supersededAt = %v", oldNative.SupersededAt)
	}
	unknownNative := findArtifactByLocatorSuffix(t, first.Artifacts, ambiguousKey)
	if unknownNative.Ownership.Kind != storage.OwnershipUnknown {
		t.Fatalf("unlabelled native ownership = %s", unknownNative.Ownership.Kind)
	}
	logArtifact := findArtifactByLocatorSuffix(t, first.Artifacts, "logs")
	if !hasProtection(logArtifact, storage.ProtectionOperator) {
		t.Fatalf("logs artifact protections = %+v", logArtifact.Protections)
	}
	currentArchive := findArtifactByArchiveDigest(t, first.Artifacts, currentTemplate.ArchiveSHA256)
	if currentArchive.Current || hasProtection(currentArchive, storage.ProtectionConfiguration) {
		t.Fatalf("imported-template selection retained its transient archive = %+v", currentArchive)
	}
	oldArchive := findArtifactByArchiveDigest(t, first.Artifacts, oldTemplate.ArchiveSHA256)
	if !hasProtection(oldArchive, storage.ProtectionCertification) || oldArchive.SupersededAt != nil {
		t.Fatalf("old archive = %+v", oldArchive)
	}

	plan, err := storage.Preview(first.PreviewRequest(storage.DefaultPolicy(), nil))
	if err != nil {
		t.Fatalf("Preview() inventory error = %v", err)
	}
	if actionForArtifact(plan, oldNative.ID) != storage.ActionRemove {
		t.Fatalf("old native action = %s", actionForArtifact(plan, oldNative.ID))
	}
	if actionForArtifact(plan, oldArchive.ID) != storage.ActionProtected {
		t.Fatalf("certified archive action = %s", actionForArtifact(plan, oldArchive.ID))
	}
}

func TestCollectProviderFilter(t *testing.T) {
	t.Parallel()
	project := t.TempDir()
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	mustMkdirAll(t, filepath.Join(project, "work", "logs"))
	writeNativeRevision(t, filepath.Join(project, ".local", "bin"), repeatedHex("a"), now.Add(-time.Hour), false)
	writeTemplateFixture(t, filepath.Join(project, "work", "template-builds", "docker-sandboxes"), "template", "full", "linux/amd64", "current", now.Add(-time.Hour), nil)

	snapshot, err := Collect(Options{ProjectRoot: project, Provider: ProviderDockerSandboxes, Now: now})
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	if len(snapshot.Artifacts) != 3 {
		t.Fatalf("provider-filtered artifacts = %+v", snapshot.Artifacts)
	}
	var providerSpecific int
	for _, artifact := range snapshot.Artifacts {
		if artifact.Provider == ProviderDockerSandboxes {
			providerSpecific++
			if artifact.Kind != storage.ArtifactTemplateArchive {
				t.Fatalf("provider-specific artifact = %+v", artifact)
			}
		} else if artifact.Provider != "" {
			t.Fatalf("unrelated provider artifact = %+v", artifact)
		}
	}
	if providerSpecific != 1 {
		t.Fatalf("provider-specific artifacts = %d", providerSpecific)
	}
	if len(snapshot.Surfaces) != 1 {
		t.Fatalf("provider-filtered surfaces = %+v", snapshot.Surfaces)
	}
}

func TestCollectAssignsExternalConfiguredRootToSeparateCapacitySurface(t *testing.T) {
	t.Parallel()
	project := t.TempDir()
	externalLogs := t.TempDir()
	mustWriteFile(t, filepath.Join(externalLogs, "epar.log"), []byte("log"))
	snapshot, err := Collect(Options{
		ProjectRoot: project,
		LogsRoot:    externalLogs,
		Now:         time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	if len(snapshot.Surfaces) != 2 {
		t.Fatalf("Collect() surfaces = %+v, want project and external", snapshot.Surfaces)
	}
	logArtifact := findArtifactByLocatorSuffix(t, snapshot.Artifacts, filepath.Base(externalLogs))
	if logArtifact.SurfaceID == ProjectSurfaceID || !hasProtection(logArtifact, storage.ProtectionCustomRoot) {
		t.Fatalf("external logs artifact = %+v", logArtifact)
	}
	var found bool
	for _, surface := range snapshot.Surfaces {
		if surface.ID == logArtifact.SurfaceID && samePath(surface.Location, externalLogs) && surface.Capacity.Known {
			found = true
		}
	}
	if !found {
		t.Fatalf("external logs surface %q not found in %+v", logArtifact.SurfaceID, snapshot.Surfaces)
	}
}

func TestCollectConfiguredProviderFilesUsesExactIdentityAndProtection(t *testing.T) {
	t.Parallel()
	project := t.TempDir()
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	imagePath := filepath.Join(project, "work", "images", "runner.tar")
	mustMkdirAll(t, filepath.Dir(imagePath))
	mustWriteFile(t, imagePath, []byte("reusable-wsl-image"))

	snapshot, err := Collect(Options{
		ProjectRoot: project,
		Provider:    "wsl",
		Now:         now,
		ConfiguredFiles: []ConfiguredFile{{
			Provider:         "wsl",
			Role:             "reusable-image",
			Path:             imagePath,
			Kind:             storage.ArtifactProviderImage,
			Current:          true,
			ConfiguredAt:     now.Add(-time.Hour),
			ProtectionKind:   storage.ProtectionConfiguration,
			ProtectionDetail: "current reusable WSL image",
		}},
	})
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	artifact := findArtifactByLocatorSuffix(t, snapshot.Artifacts, "runner.tar")
	if artifact.Provider != "wsl" || artifact.Kind != storage.ArtifactProviderImage || artifact.SurfaceID != ProjectSurfaceID {
		t.Fatalf("configured artifact = %+v", artifact)
	}
	if artifact.Ownership.Kind != storage.OwnershipExact || artifact.Target.Match != storage.MatchExact || artifact.Target.Identity == "" || artifact.Target.Fingerprint == "" {
		t.Fatalf("configured artifact identity = %+v", artifact)
	}
	if artifact.SizeBytes != uint64(len("reusable-wsl-image")) || !artifact.Current || !hasProtection(artifact, storage.ProtectionConfiguration) || !hasProtection(artifact, storage.ProtectionCurrent) {
		t.Fatalf("configured artifact protection = %+v", artifact)
	}
	plan, err := storage.Preview(snapshot.PreviewRequest(storage.DefaultPolicy(), nil))
	if err != nil {
		t.Fatalf("Preview() error = %v", err)
	}
	if actionForArtifact(plan, artifact.ID) != storage.ActionProtected {
		t.Fatalf("configured artifact action = %s", actionForArtifact(plan, artifact.ID))
	}
}

func TestSnapshotPreviewRequestCopiesTopLevelSlices(t *testing.T) {
	t.Parallel()
	snapshot := Snapshot{
		CollectedAt: time.Now().UTC(),
		Surfaces:    []storage.Surface{{ID: ProjectSurfaceID}},
		Artifacts:   []storage.Artifact{{ID: "artifact"}},
	}
	request := snapshot.PreviewRequest(storage.DefaultPolicy(), []storage.Requirement{{ID: "requirement"}})
	request.Surfaces[0].ID = "changed"
	request.Artifacts[0].ID = "changed"
	if snapshot.Surfaces[0].ID != ProjectSurfaceID || snapshot.Artifacts[0].ID != "artifact" {
		t.Fatal("PreviewRequest() aliased snapshot top-level slices")
	}
}

func findArtifact(t *testing.T, artifacts []storage.Artifact, id string) storage.Artifact {
	t.Helper()
	for _, artifact := range artifacts {
		if artifact.ID == id {
			return artifact
		}
	}
	t.Fatalf("artifact %q not found in %+v", id, artifacts)
	return storage.Artifact{}
}

func findArtifactByLocatorSuffix(t *testing.T, artifacts []storage.Artifact, suffix string) storage.Artifact {
	t.Helper()
	for _, artifact := range artifacts {
		if filepath.Base(artifact.Target.Locator) == suffix {
			return artifact
		}
	}
	t.Fatalf("artifact with locator suffix %q not found", suffix)
	return storage.Artifact{}
}

func findArtifactByKind(t *testing.T, artifacts []storage.Artifact, kind storage.ArtifactKind, provider string) storage.Artifact {
	t.Helper()
	for _, artifact := range artifacts {
		if artifact.Kind == kind && artifact.Provider == provider {
			return artifact
		}
	}
	t.Fatalf("artifact kind=%q provider=%q not found", kind, provider)
	return storage.Artifact{}
}

func findArtifactByArchiveDigest(t *testing.T, artifacts []storage.Artifact, digest string) storage.Artifact {
	t.Helper()
	for _, artifact := range artifacts {
		if artifact.Kind == storage.ArtifactTemplateArchive && artifact.Ownership.OwnerID == "template-archive:"+digest {
			return artifact
		}
	}
	t.Fatalf("archive artifact %q not found", digest)
	return storage.Artifact{}
}

func actionForArtifact(plan storage.Plan, id string) storage.Action {
	for _, decision := range plan.Decisions {
		if decision.Artifact.ID == id {
			return decision.Action
		}
	}
	return ""
}

func hasProtection(artifact storage.Artifact, kind storage.ProtectionKind) bool {
	for _, protection := range artifact.Protections {
		if protection.Kind == kind {
			return true
		}
	}
	return false
}

func repeatedHex(value string) string {
	return value + value + value + value + value + value + value + value +
		value + value + value + value + value + value + value + value +
		value + value + value + value + value + value + value + value +
		value + value + value + value + value + value + value + value +
		value + value + value + value + value + value + value + value +
		value + value + value + value + value + value + value + value +
		value + value + value + value + value + value + value + value +
		value + value + value + value + value + value + value + value
}

func mustMkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
}

func mustWriteFile(t *testing.T, path string, data []byte) {
	t.Helper()
	mustMkdirAll(t, filepath.Dir(path))
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertArtifactsEqual(t *testing.T, left, right []storage.Artifact) {
	t.Helper()
	if !reflect.DeepEqual(left, right) {
		t.Fatalf("artifacts differ:\nleft=%+v\nright=%+v", left, right)
	}
}
