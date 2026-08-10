package provider

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

func TestDockerSandboxesDomainPackagesStayUnderProviderTree(t *testing.T) {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not locate provider package")
	}
	providerDirectory := filepath.Dir(filename)
	internalDirectory := filepath.Dir(providerDirectory)
	for _, name := range []string{"sandboxcapacity", "sandboxfs", "sandboxledger", "sandboxpolicy", "sandboxpromotion"} {
		path := filepath.Join(internalDirectory, name)
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("Docker Sandboxes domain package must not live directly under internal: %s", path)
		}
	}
	for _, name := range []string{"capacity", "staging", "policy", "promotion"} {
		path := filepath.Join(providerDirectory, "dockersandboxes", name)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("required Docker Sandboxes domain package %s: %v", path, err)
		}
		if !info.IsDir() {
			t.Fatalf("required Docker Sandboxes domain package is not a directory: %s", path)
		}
	}
	if _, err := os.Stat(filepath.Join(providerDirectory, "dockersandboxes", "ledger")); !os.IsNotExist(err) {
		t.Fatalf("Docker Sandboxes must use the provider-neutral pool ledger instead of a provider-local ledger")
	}
}

func TestProviderImplementationsDoNotOwnPoolControllers(t *testing.T) {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not locate provider package")
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
			if value == "github.com/solutionforest/ephemeral-action-runner/internal/pool" || strings.HasPrefix(value, "github.com/solutionforest/ephemeral-action-runner/internal/pool/") {
				t.Errorf("%s imports %q; provider packages implement host integration and must not own a pool controller", path, value)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
