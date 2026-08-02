package image

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
	storagecatalog "github.com/solutionforest/ephemeral-action-runner/internal/storage/catalog"
)

func TestDockerOutputTagClaimAllowsSameManifestAndRejectsDivergence(t *testing.T) {
	t.Setenv("EPAR_STATE_HOME", filepath.Join(t.TempDir(), "host-state"))
	project := t.TempDir()
	firstPath := filepath.Join(project, ".local", "config.yml")
	secondPath := filepath.Join(project, ".local", "config.docker-container.yml")
	if err := os.MkdirAll(filepath.Dir(firstPath), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{firstPath, secondPath} {
		if err := os.WriteFile(path, []byte("provider:\n  type: docker-container\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	first := Coordinator{Config: config.Default(), ProjectRoot: project, ConfigPath: firstPath}
	second := Coordinator{Config: config.Default(), ProjectRoot: project, ConfigPath: secondPath}
	now := time.Date(2026, 7, 31, 2, 3, 4, 0, time.UTC)
	const backendID = "docker:test-daemon"
	const tag = "example/runner:current"
	const firstManifest = "manifest-one"
	if err := first.claimDockerOutputTagLocked(backendID, tag, firstManifest, now); err != nil {
		t.Fatal(err)
	}
	if err := first.claimDockerOutputTagLocked(backendID, tag, firstManifest, now.Add(time.Second)); err != nil {
		t.Fatalf("same configuration could not renew its stable tag claim: %v", err)
	}
	if err := second.claimDockerOutputTagLocked(backendID, tag, firstManifest, now.Add(2*time.Second)); err != nil {
		t.Fatalf("identical manifests could not share output tag intent: %v", err)
	}
	err := second.claimDockerOutputTagLocked(backendID, tag, "manifest-two", now.Add(3*time.Second))
	if err == nil {
		t.Fatal("divergent configuration claimed a shared mutable Docker output tag")
	}
	if !strings.Contains(err.Error(), secondPath) && !strings.Contains(err.Error(), firstPath) {
		t.Fatalf("conflict error omitted actionable configuration path: %v", err)
	}
	if !strings.Contains(err.Error(), tag) || !strings.Contains(err.Error(), firstManifest) || !strings.Contains(err.Error(), "manifest-two") {
		t.Fatalf("conflict error omitted tag or manifest evidence: %v", err)
	}

	store, err := storagecatalog.Open("")
	if err != nil {
		t.Fatal(err)
	}
	value, err := store.Load(now.Add(4 * time.Second))
	if err != nil {
		t.Fatal(err)
	}
	var claim storagecatalog.Resource
	for _, resource := range value.Resources {
		if resource.Kind == catalogDockerOutputTagClaimKind {
			claim = resource
			break
		}
	}
	if claim.Key == "" || len(claim.References) != 2 || claim.ManifestHash != firstManifest {
		t.Fatalf("shared exact tag claim = %#v, want one stable claim with both references", claim)
	}
	if err := first.releaseDockerOutputTagClaim(backendID, tag, now.Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := second.releaseDockerOutputTagClaim(backendID, tag, now.Add(6*time.Second)); err != nil {
		t.Fatal(err)
	}
	value, err = store.Load(now.Add(7 * time.Second))
	if err != nil {
		t.Fatal(err)
	}
	claim = storagecatalog.Resource{}
	for _, resource := range value.Resources {
		if resource.Kind == catalogDockerOutputTagClaimKind {
			claim = resource
			break
		}
	}
	if claim.State != storagecatalog.StateSuperseded || len(claim.References) != 0 {
		t.Fatalf("released tag claim was not retained only as exact superseded cleanup evidence: %#v", claim)
	}
}

func TestDockerOutputTagClaimRejectsOtherActiveArtifactManifest(t *testing.T) {
	t.Setenv("EPAR_STATE_HOME", filepath.Join(t.TempDir(), "host-state"))
	project := t.TempDir()
	firstPath := filepath.Join(project, ".local", "config.yml")
	secondPath := filepath.Join(project, ".local", "config.docker-container.yml")
	if err := os.MkdirAll(filepath.Dir(firstPath), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{firstPath, secondPath} {
		if err := os.WriteFile(path, []byte("provider:\n  type: docker-container\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	second := Coordinator{Config: config.Default(), ProjectRoot: project, ConfigPath: secondPath}
	now := time.Date(2026, 7, 31, 3, 4, 5, 0, time.UTC)
	const backendID = "docker:test-daemon"
	const tag = "example/runner:current"
	store, err := storagecatalog.Open("")
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.WithLock(now, func(value *storagecatalog.Catalog) error {
		record, err := storagecatalog.RegisterConfig(value, project, firstPath, now)
		if err != nil {
			return err
		}
		resource := storagecatalog.Resource{
			BackendID: backendID, Kind: catalogDockerImageKind, Provider: "docker-container", Role: "runtime-image",
			Locator: tag, Identity: "sha256:active", Custody: storagecatalog.CustodyGenerated, ManifestHash: "manifest-one",
			State: storagecatalog.StateCurrent, CreatedAt: now, LastSeenAt: now,
		}
		if err := storagecatalog.UpsertResource(value, resource); err != nil {
			return err
		}
		storagecatalog.ReplaceConfigRoleReferences(value, record.ID, "provider-artifact", map[string]storagecatalog.Reference{
			storagecatalog.ResourceKey(backendID, catalogDockerImageKind, resource.Identity): {ManifestHash: "manifest-one"},
		}, now)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	err = second.claimDockerOutputTagLocked(backendID, tag, "manifest-two", now.Add(time.Second))
	if err == nil || !strings.Contains(err.Error(), firstPath) {
		t.Fatalf("active artifact conflict = %v, want first configuration path", err)
	}
}

func TestDockerOutputTagConflictFallsBackToCanonicalPath(t *testing.T) {
	value := &storagecatalog.Catalog{Configs: []storagecatalog.Config{{ID: "legacy-config", Path: "canonical-config.yml"}}}
	err := dockerOutputTagConflict(value, "legacy-config", "docker.io/example/runner:current", "manifest-one", "manifest-two")
	if !strings.Contains(err.Error(), "canonical-config.yml") {
		t.Fatalf("legacy catalog conflict omitted canonical configuration path: %v", err)
	}
}

func TestDockerOutputTagClaimExpiresAfterPublisherCrash(t *testing.T) {
	t.Setenv("EPAR_STATE_HOME", filepath.Join(t.TempDir(), "host-state"))
	project := t.TempDir()
	firstPath := filepath.Join(project, ".local", "config.yml")
	secondPath := filepath.Join(project, ".local", "config.other.yml")
	if err := os.MkdirAll(filepath.Dir(firstPath), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{firstPath, secondPath} {
		if err := os.WriteFile(path, []byte("provider:\n  type: docker-container\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	first := Coordinator{Config: config.Default(), ProjectRoot: project, ConfigPath: firstPath}
	second := Coordinator{Config: config.Default(), ProjectRoot: project, ConfigPath: secondPath}
	now := time.Date(2026, 7, 31, 4, 5, 6, 0, time.UTC)
	const backendID = "docker:test-daemon"
	const tag = "example/runner:current"
	if err := first.claimDockerOutputTagLocked(backendID, tag, "manifest-one", now); err != nil {
		t.Fatal(err)
	}
	if err := second.claimDockerOutputTagLocked(backendID, tag, "manifest-two", now.Add(dockerOutputTagClaimLifetime-time.Second)); err == nil {
		t.Fatal("fresh claim did not block a divergent manifest")
	}
	if err := second.claimDockerOutputTagLocked(backendID, tag, "manifest-two", now.Add(dockerOutputTagClaimLifetime)); err != nil {
		t.Fatalf("expired claim still blocked a divergent manifest after publisher crash: %v", err)
	}
}

func TestNormalizedDockerTagCollapsesEquivalentNames(t *testing.T) {
	tests := map[string]string{
		"epar-runner":                           "docker.io/library/epar-runner:latest",
		"epar-runner:latest":                    "docker.io/library/epar-runner:latest",
		"solutionforest/epar-runner":            "docker.io/solutionforest/epar-runner:latest",
		"index.docker.io/epar-runner:Current":   "docker.io/library/epar-runner:Current",
		"registry-1.docker.io/epar-runner:edge": "docker.io/library/epar-runner:edge",
		"ghcr.io/SolutionForest/EPAR:edge":      "ghcr.io/solutionforest/epar:edge",
		"localhost:5000/epar-runner":            "localhost:5000/epar-runner:latest",
	}
	for input, want := range tests {
		if got := normalizedDockerTag(input); got != want {
			t.Fatalf("normalizedDockerTag(%q) = %q, want %q", input, got, want)
		}
	}
}
