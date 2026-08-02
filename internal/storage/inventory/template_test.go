package inventory

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/storage"
)

type templateFixture struct {
	Profile        string
	Platform       string
	Tag            string
	TemplateDigest string
	ArchiveSHA256  string
	MetadataSHA256 string
	Directory      string
	Archive        string
}

func TestCollectTemplatesStrictIntegrityValidation(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	valid := writeTemplateFixture(t, root, "valid", "act-22.04", "linux/amd64", "valid", now.Add(-10*24*time.Hour), nil)
	writeTemplateFixture(t, root, "bad-hash", "act-22.04", "linux/amd64", "bad-hash", now.Add(-10*24*time.Hour), func(metadata map[string]any) {
		template := metadata["template"].(map[string]any)
		template["archiveSha256"] = "sha256:" + strings.Repeat("0", 64)
	})
	duplicateDirectory := filepath.Join(root, "duplicate-json")
	mustMkdirAll(t, duplicateDirectory)
	mustWriteFile(t, filepath.Join(duplicateDirectory, "template-metadata.json"), []byte(`{"schemaVersion":2,"schemaVersion":2}`))

	artifacts, warnings, err := collectTemplates(templateOptions{Root: root})
	if err != nil {
		t.Fatalf("collectTemplates() error = %v", err)
	}
	if len(artifacts) != 3 || len(warnings) != 2 {
		t.Fatalf("collectTemplates() artifacts=%+v warnings=%v", artifacts, warnings)
	}
	exact := findArtifactByArchiveDigest(t, artifacts, valid.ArchiveSHA256)
	if exact.Ownership.Kind != storage.OwnershipExact || exact.SizeBytes == 0 {
		t.Fatalf("valid archive = %+v", exact)
	}
	unknownCount := 0
	for _, artifact := range artifacts {
		if artifact.Ownership.Kind == storage.OwnershipUnknown {
			unknownCount++
		}
	}
	if unknownCount != 2 {
		t.Fatalf("ownership-unknown artifacts = %d, want 2", unknownCount)
	}
}

func TestCollectTemplatesDoesNotRetainArchiveForImportedTemplate(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	old := writeTemplateFixture(t, root, "old", "full", "linux/amd64", "old", now.Add(-20*24*time.Hour), nil)
	current := writeTemplateFixture(t, root, "current", "full", "linux/amd64", "current", now.Add(-10*24*time.Hour), nil)
	artifacts, warnings, err := collectTemplates(templateOptions{
		Root: root,
		Selections: []TemplateSelection{{
			Platform:       current.Platform,
			Tag:            current.Tag,
			TemplateDigest: current.TemplateDigest,
			MetadataSHA256: current.MetadataSHA256,
			ActivatedAt:    now.Add(-9 * 24 * time.Hour),
		}},
		Protections: []TemplateProtection{{
			ArchiveSHA256: old.ArchiveSHA256,
			Kind:          storage.ProtectionPromotion,
			Detail:        "promoted bytes",
		}},
	})
	if err != nil || len(warnings) != 0 {
		t.Fatalf("collectTemplates() error=%v warnings=%v", err, warnings)
	}
	currentArtifact := findArtifactByArchiveDigest(t, artifacts, current.ArchiveSHA256)
	oldArtifact := findArtifactByArchiveDigest(t, artifacts, old.ArchiveSHA256)
	if currentArtifact.Current || hasProtection(currentArtifact, storage.ProtectionConfiguration) {
		t.Fatalf("imported-template selection retained its transient archive = %+v", currentArtifact)
	}
	if oldArtifact.SupersededAt != nil || !hasProtection(oldArtifact, storage.ProtectionPromotion) {
		t.Fatalf("old artifact = %+v", oldArtifact)
	}
}

func TestCollectTemplatesRejectsSymlinkArchive(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	directory := filepath.Join(root, "symlink")
	mustMkdirAll(t, directory)
	outside := filepath.Join(t.TempDir(), "archive.tar")
	mustWriteFile(t, outside, []byte("archive"))
	link := filepath.Join(directory, "archive.tar")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink creation unavailable on %s: %v", runtime.GOOS, err)
	}
	digest := sha256.Sum256([]byte("archive"))
	metadata := validTemplateMetadata("act-22.04", "linux/amd64", "symlink", "archive.tar", "sha256:"+hex.EncodeToString(digest[:]), uint64(len("archive")))
	encoded, _ := json.Marshal(metadata)
	mustWriteFile(t, filepath.Join(directory, "template-metadata.json"), encoded)

	artifacts, warnings, err := collectTemplates(templateOptions{Root: root})
	if err != nil {
		t.Fatalf("collectTemplates() error = %v", err)
	}
	if len(artifacts) != 1 || len(warnings) != 1 || artifacts[0].Ownership.Kind != storage.OwnershipUnknown {
		t.Fatalf("symlink archive artifacts=%+v warnings=%v", artifacts, warnings)
	}
}

func TestValidateTemplateInputsRejectsAmbiguity(t *testing.T) {
	t.Parallel()
	selection := TemplateSelection{
		Profile:        "full",
		Platform:       "linux/amd64",
		Tag:            "epar-docker-sandboxes-full:test-amd64",
		TemplateDigest: "sha256:" + strings.Repeat("a", 64),
	}
	if err := validateTemplateInputs(templateOptions{Selections: []TemplateSelection{selection, selection}}); err == nil {
		t.Fatal("validateTemplateInputs() accepted duplicate configured group")
	}
	if err := validateTemplateInputs(templateOptions{Protections: []TemplateProtection{{ArchiveSHA256: "sha256:short", Kind: storage.ProtectionCertification}}}); err == nil {
		t.Fatal("validateTemplateInputs() accepted invalid protected digest")
	}
}

func TestTemplateSelectionAcceptsCanonicalDockerHubName(t *testing.T) {
	selection := TemplateSelection{
		Platform:       "linux/amd64",
		Tag:            "docker.io/library/epar-docker-sandboxes-catthehacker-full:20260723-r2-amd64",
		TemplateDigest: "sha256:" + strings.Repeat("a", 64),
	}
	if err := validateTemplateInputs(templateOptions{Selections: []TemplateSelection{selection}}); err != nil {
		t.Fatalf("canonical Docker Hub template name rejected: %v", err)
	}
	if got, want := normalizedTemplateTag(selection.Tag), "epar-docker-sandboxes-catthehacker-full:20260723-r2-amd64"; got != want {
		t.Fatalf("normalized tag = %q, want %q", got, want)
	}
}

func writeTemplateFixture(t *testing.T, root, name, profile, platform, suffix string, created time.Time, mutate func(map[string]any)) templateFixture {
	t.Helper()
	directory := filepath.Join(root, name)
	mustMkdirAll(t, directory)
	archiveName := "epar-docker-sandboxes-" + profile + ".tar"
	archive := filepath.Join(directory, archiveName)
	archiveBytes := []byte("archive:" + name + ":" + suffix)
	mustWriteFile(t, archive, archiveBytes)
	if err := os.Chtimes(archive, created, created); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(archiveBytes)
	archiveSHA := "sha256:" + hex.EncodeToString(sum[:])
	metadata := validTemplateMetadata(profile, platform, suffix, archiveName, archiveSHA, uint64(len(archiveBytes)))
	if mutate != nil {
		mutate(metadata)
	}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	metadataPath := filepath.Join(directory, "template-metadata.json")
	mustWriteFile(t, metadataPath, encoded)
	metadataSum := sha256.Sum256(encoded)
	template := metadata["template"].(map[string]any)
	return templateFixture{
		Profile:        profile,
		Platform:       platform,
		Tag:            template["tag"].(string),
		TemplateDigest: template["digest"].(string),
		ArchiveSHA256:  archiveSHA,
		MetadataSHA256: "sha256:" + hex.EncodeToString(metadataSum[:]),
		Directory:      directory,
		Archive:        archive,
	}
}

func validTemplateMetadata(profile, platform, suffix, archive string, archiveSHA string, archiveBytes uint64) map[string]any {
	templateDigest := "sha256:" + strings.Repeat(digestCharacter(suffix), 64)
	return map[string]any{
		"schemaVersion": templateMetadataSchema,
		"profile":       profile,
		"platform":      platform,
		"template": map[string]any{
			"tag":           "epar-docker-sandboxes-" + profile + ":" + suffix + "-amd64",
			"digest":        templateDigest,
			"cacheID":       strings.TrimPrefix(templateDigest, "sha256:")[:12],
			"rootDisk":      "30GiB",
			"archive":       archive,
			"archiveSha256": archiveSHA,
			"archiveBytes":  archiveBytes,
		},
		"compatibility": map[string]any{
			"templateSchemaVersion":     1,
			"runnerExecution":           "direct-actions-listener",
			"dockerDaemonOwner":         "docker-sandboxes-runtime",
			"expectedDockerDaemonCount": 1,
		},
	}
}

func digestCharacter(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:1]
}
