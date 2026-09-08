package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/solutionforest/ephemeral-action-runner/internal/prebuilt"
)

func TestAtomicWriteReplacesExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog.json")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := atomicWrite(path, []byte("new")); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "new" {
		t.Fatalf("rewritten file = %q", data)
	}
	if matches, _ := filepath.Glob(path + ".previous"); len(matches) != 0 {
		t.Fatalf("replacement backup was left behind: %v", matches)
	}
}

func TestReadEntryOrPlanAcceptsBothJSONShapes(t *testing.T) {
	entry := prebuilt.Entry{PackageIndexDigest: "sha256:" + strings.Repeat("a", 64)}
	for name, value := range map[string]any{"entry": entry, "plan": prebuilt.PublicationPlan{Entry: entry}} {
		path := filepath.Join(t.TempDir(), name+".json")
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		var got prebuilt.Entry
		if err := readEntryOrPlan(path, &got); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got.PackageIndexDigest != entry.PackageIndexDigest {
			t.Fatalf("%s digest = %s", name, got.PackageIndexDigest)
		}
	}
}

func TestVerifyRebuildSourceCommandExecutesExactReferenceAndPlatformChecks(t *testing.T) {
	packageDigest := "sha256:" + strings.Repeat("a", 64)
	indexDigest := "sha256:" + strings.Repeat("d", 64)
	amd64Digest := "sha256:" + strings.Repeat("e", 64)
	arm64Digest := "sha256:" + strings.Repeat("f", 64)
	selection := prebuilt.RebuildSourceSelection{
		Profile:                  prebuilt.ProfileFull,
		SourcePackageIndexDigest: packageDigest,
		Source: prebuilt.SourceDescriptor{
			Repository:      "ghcr.io/catthehacker/ubuntu",
			SourceTag:       "full-latest",
			Reference:       "ghcr.io/catthehacker/ubuntu@" + indexDigest,
			IndexDigest:     indexDigest,
			PlatformDigests: map[string]string{"linux/amd64": amd64Digest, "linux/arm64": arm64Digest},
		},
	}
	path := filepath.Join(t.TempDir(), "selection.json")
	data, err := json.Marshal(selection)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"--selection", path, "--reference", selection.Source.Reference, "--index-digest", indexDigest, "--amd64-digest", amd64Digest, "--arm64-digest", arm64Digest}
	if err := verifyRebuildSourceCommand(args); err != nil {
		t.Fatal(err)
	}
	args[3] = "ghcr.io/catthehacker/ubuntu:full-latest"
	if err := verifyRebuildSourceCommand(args); err == nil || !strings.Contains(err.Error(), "reference expected") {
		t.Fatalf("mutable source reference error = %v", err)
	}
}
