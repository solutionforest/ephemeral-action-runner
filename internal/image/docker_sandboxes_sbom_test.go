package image

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/solutionforest/ephemeral-action-runner/internal/storage"
)

const scannerStatement = `{"_type":"https://in-toto.io/Statement/v0.1","subject":null,"predicateType":"https://spdx.dev/Document","predicate":{"SPDXID":"SPDXRef-DOCUMENT","packages":[{"name":"example"}],"files":[],"relationships":[]}}`

func TestDockerSandboxesSBOMStatementPreservesAllBytes(t *testing.T) {
	root := t.TempDir()
	source, destination := filepath.Join(root, "scanner.json"), filepath.Join(root, "statement.json")
	content := []byte(scannerStatement + "\n")
	if err := os.WriteFile(source, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := copyDockerSandboxesSBOMStatement(source, destination, uint64(len(content))); err != nil {
		t.Fatal(err)
	}
	actual, err := os.ReadFile(destination)
	if err != nil || !bytes.Equal(actual, content) {
		t.Fatalf("scanner bytes changed: %v", err)
	}
}

func TestDockerSandboxesSBOMStatementRejectsInvalidEvidenceAtomically(t *testing.T) {
	for name, statement := range map[string]string{
		"empty": "", "malformed": "{", "raw-predicate": `{"SPDXID":"SPDXRef-DOCUMENT"}`,
		"array": `[]`, "trailing": scannerStatement + ` {}`,
		"wrong-type":               strings.Replace(scannerStatement, "https://spdx.dev/Document", "wrong", 1),
		"wrong-document":           strings.Replace(scannerStatement, "SPDXRef-DOCUMENT", "wrong", 1),
		"duplicate-type":           strings.Replace(scannerStatement, `"_type":`, `"_type":"wrong","_type":`, 1),
		"duplicate-predicate":      strings.Replace(scannerStatement, `"predicate":`, `"predicate":{},"predicate":`, 1),
		"duplicate-predicate-type": strings.Replace(scannerStatement, `"predicateType":`, `"predicateType":"wrong","predicateType":`, 1),
		"duplicate-document":       strings.Replace(scannerStatement, `"SPDXID":`, `"SPDXID":"wrong","SPDXID":`, 1),
		"invalid-utf8":             strings.Replace(scannerStatement, "example", "bad\xff", 1),
		"excessive-depth":          strings.Replace(scannerStatement, `"files":[]`, `"files":`+strings.Repeat("[", 257)+"0"+strings.Repeat("]", 257), 1),
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			source, destination := filepath.Join(root, "scanner.json"), filepath.Join(root, "statement.json")
			if err := os.WriteFile(source, []byte(statement), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(destination, []byte("previous"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := copyDockerSandboxesSBOMStatement(source, destination, storage.GiB); err == nil {
				t.Fatal("invalid evidence accepted")
			}
			content, err := os.ReadFile(destination)
			if err != nil || string(content) != "previous" {
				t.Fatalf("previous evidence changed: %q %v", content, err)
			}
			leftovers, err := filepath.Glob(filepath.Join(root, ".sbom-statement-*"))
			if err != nil || len(leftovers) != 0 {
				t.Fatalf("temporary evidence retained: %v %v", leftovers, err)
			}
		})
	}
}

func TestDockerSandboxesSBOMStatementRejectsSingleOversizedToken(t *testing.T) {
	root := t.TempDir()
	source, destination := filepath.Join(root, "scanner.json"), filepath.Join(root, "statement.json")
	file, err := os.Create(source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = file.WriteString(`{"_type":"https://in-toto.io/Statement/v0.1","predicateType":"https://spdx.dev/Document","predicate":{"SPDXID":"SPDXRef-DOCUMENT","comment":"`); err != nil {
		t.Fatal(err)
	}
	chunk := strings.Repeat("x", 1024)
	for written := uint64(0); written <= sbomMaximumJSONTokenBytes; written += uint64(len(chunk)) {
		if _, err = io.WriteString(file, chunk); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = file.WriteString(`"}}`); err != nil {
		t.Fatal(err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destination, []byte("previous"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := copyDockerSandboxesSBOMStatement(source, destination, storage.GiB); err == nil || !strings.Contains(err.Error(), "token exceeds") {
		t.Fatalf("oversized token accepted or wrong error: %v", err)
	}
	content, err := os.ReadFile(destination)
	if err != nil || string(content) != "previous" {
		t.Fatalf("previous evidence changed: %q %v", content, err)
	}
}

func TestDockerSandboxesSBOMStatementRejectsRedirectAndLimit(t *testing.T) {
	root := t.TempDir()
	source, destination := filepath.Join(root, "scanner.json"), filepath.Join(root, "statement.json")
	if err := os.WriteFile(source, []byte(scannerStatement), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, limit := range []uint64{0, 1, uint64(len(scannerStatement) - 1), storage.GiB + 1} {
		if err := copyDockerSandboxesSBOMStatement(source, destination, limit); err == nil {
			t.Fatalf("limit %d accepted", limit)
		}
	}
	link := filepath.Join(root, "link.json")
	if err := os.Symlink(source, link); err == nil {
		if err := copyDockerSandboxesSBOMStatement(link, destination, storage.GiB); err == nil {
			t.Fatal("symlink accepted")
		}
	}
}

func TestDockerSandboxesSBOMStatementExceedsBuildKitAttestationLimit(t *testing.T) {
	if testing.Short() {
		t.Skip("large evidence regression")
	}
	root := t.TempDir()
	source, destination := filepath.Join(root, "scanner.json"), filepath.Join(root, "statement.json")
	file, err := os.Create(source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = file.WriteString(`{"_type":"https://in-toto.io/Statement/v0.1","predicateType":"https://spdx.dev/Document","predicate":{"SPDXID":"SPDXRef-DOCUMENT","packages":[`); err != nil {
		t.Fatal(err)
	}
	chunk := strings.Repeat(`{"name":"component","versionInfo":"1"},`, 1024)
	for written := uint64(0); written <= 81*storage.MiB; written += uint64(len(chunk)) {
		if _, err = io.WriteString(file, chunk); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = file.WriteString(`{"name":"last"}]}}`); err != nil {
		t.Fatal(err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := copyDockerSandboxesSBOMStatement(source, destination, storage.GiB); err != nil {
		t.Fatal(err)
	}
	want, _, err := hashFile(source)
	if err != nil {
		t.Fatal(err)
	}
	got, size, err := hashFile(destination)
	if err != nil || got != want || size <= 80*storage.MiB {
		t.Fatalf("large evidence changed: %s %s %d %v", want, got, size, err)
	}
}
