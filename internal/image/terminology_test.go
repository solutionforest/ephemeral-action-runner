package image

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestLegacyDockerSandboxesDevelopmentLabelsDoNotReturn(t *testing.T) {
	projectRoot := filepath.Clean(filepath.Join("..", ".."))
	pattern := regexp.MustCompile(`(?i)\bcandidate[\s_-]*[ab]\b`)
	allowedExtensions := map[string]bool{
		".go": true, ".md": true, ".json": true, ".yml": true, ".yaml": true, ".ps1": true, ".sh": true,
	}
	err := filepath.WalkDir(projectRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", ".local", "work":
				if path != projectRoot {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !allowedExtensions[strings.ToLower(filepath.Ext(path))] {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if pattern.Match(content) {
			t.Errorf("legacy Docker Sandboxes development label found in %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
