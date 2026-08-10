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
