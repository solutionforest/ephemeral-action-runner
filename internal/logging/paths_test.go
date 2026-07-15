package logging

import (
	"path/filepath"
	"testing"
	"time"
)

func TestPathHelpersAndRecognition(t *testing.T) {
	root := t.TempDir()
	timestamp := time.Date(2026, 7, 15, 1, 2, 3, 4, time.UTC)
	tests := []struct {
		path     string
		category Category
	}{
		{mustPath(InstancePath(root, "runner-1", "docker-dind")), CategoryInstances},
		{mustPath(InstancePath(root, "runner-1", "guest")), CategoryInstances},
		{mustPath(BuildPath(root, "ubuntu-24.04", "docker-build")), CategoryBuilds},
		{mustPath(BuildPath(root, "ubuntu-24.04", "guest")), CategoryBuilds},
		{ErrorPath(root, timestamp), CategoryErrors},
		{mustPath(BenchmarkPath(root, timestamp, "docker-dind")), CategoryBenchmarks},
	}
	for _, test := range tests {
		recognized, ok := recognizePath(root, test.path)
		if !ok || recognized.category != test.category || recognized.current || recognized.backup {
			t.Errorf("recognizePath(%s) = %#v, %v", test.path, recognized, ok)
		}
		canonicalRoot, _ := canonicalPath(root)
		canonicalTestPath, _ := canonicalPath(test.path)
		if canonicalRecognized, canonicalOK := recognizePath(canonicalRoot, canonicalTestPath); !canonicalOK || canonicalRecognized.category != test.category {
			t.Errorf("canonical recognizePath(%s) = %#v, %v", canonicalTestPath, canonicalRecognized, canonicalOK)
		}
		name := filepath.Base(test.path)
		extension := filepath.Ext(name)
		rotated := test.path[:len(test.path)-len(extension)] + "-2026-07-15T01-02-03.004" + extension + ".gz"
		recognized, ok = recognizePath(root, rotated)
		if !ok || recognized.category != test.category || !recognized.backup {
			t.Errorf("rotated recognizePath(%s) = %#v, %v", rotated, recognized, ok)
		}
	}
	if _, err := InstancePath(root, "../escape", "guest"); err == nil {
		t.Fatal("unsafe instance path accepted")
	}
	if _, err := BuildPath(root, "image", "unknown"); err == nil {
		t.Fatal("unknown build component accepted")
	}
	if _, err := InstancePath(root, "runner", "unknown"); err == nil {
		t.Fatal("unknown instance component accepted")
	}
}

func TestLegacyFlatRecognitionIsConstrained(t *testing.T) {
	root := t.TempDir()
	tests := []struct {
		name     string
		category Category
	}{
		{"epar-pool-20260715-010203-007.guest.log", CategoryInstances},
		{"epar-pool-20260715-010203-007.docker-dind.log", CategoryInstances},
		{"ubuntu.docker-build.log", CategoryBuilds},
		{"ubuntu.source.log", CategoryBuilds},
		{"epar-20260715-010203-error.log", CategoryErrors},
		{"20260715T010203.000000123Z-docker-dind.jsonl", CategoryBenchmarks},
		{"epar-2026-07-15T01-02-03.004.log.gz", CategoryManager},
	}
	for _, test := range tests {
		recognized, ok := recognizePath(root, filepath.Join(root, test.name))
		if !ok || recognized.category != test.category {
			t.Errorf("legacy %s = %#v, %v", test.name, recognized, ok)
		}
	}
	for _, name := range []string{"notes.user.log", "arbitrary.guest.log", "runner.foo.log", "almost-20260715-010203-01.guest.log"} {
		if recognized, ok := recognizePath(root, filepath.Join(root, name)); ok {
			t.Errorf("unknown legacy name %s recognized as %#v", name, recognized)
		}
	}
}

func mustPath(path string, err error) string {
	if err != nil {
		panic(err)
	}
	return path
}
