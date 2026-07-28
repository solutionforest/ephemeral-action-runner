package pool

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestPoolDoesNotImportProviderImplementations(t *testing.T) {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not locate pool package")
	}
	root := filepath.Dir(filename)
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imported := range parsed.Imports {
			value, err := strconv.Unquote(imported.Path.Value)
			if err != nil {
				return err
			}
			if strings.HasPrefix(value, "github.com/solutionforest/ephemeral-action-runner/internal/provider/") {
				t.Errorf("%s imports provider implementation %q; pool may depend only on provider contracts", path, value)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestImageImplementationsStayOutsidePool(t *testing.T) {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not locate pool package")
	}
	poolRoot := filepath.Dir(filename)
	internalRoot := filepath.Dir(poolRoot)
	for _, name := range []string{"image.go", "artifact_lifecycle.go", "image_acquisition.go", "trusted_ca.go", "docker_pull.go", "image_manifest.go"} {
		path := filepath.Join(poolRoot, name)
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("image implementation must not live in pool: %s", path)
		}
	}
	for _, name := range []string{"build.go", "artifact_lifecycle.go", "acquisition.go", "trusted_ca.go", "docker_pull.go", "manifest.go"} {
		path := filepath.Join(internalRoot, "image", name)
		info, err := os.Stat(path)
		if err != nil {
			t.Errorf("required image implementation %s: %v", path, err)
			continue
		}
		if info.IsDir() {
			t.Errorf("image implementation is not a file: %s", path)
		}
	}
}
