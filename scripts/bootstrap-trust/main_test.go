package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMaterializeBundleValidatesAndWritesCanonicalCA(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	certificatePEM, certificateHash := testCA(t, now)
	document := testFeed(now, certificatePEM, certificateHash)
	root := t.TempDir()
	feedPath := writeTestFeed(t, root, document)
	outputPath := filepath.Join(root, "out", "ca.pem")

	summary, err := materializeBundle(feedPath, outputPath, "windows", now)
	if err != nil {
		t.Fatalf("materializeBundle: %v", err)
	}
	if summary.HostOS != "windows" || strings.Join(summary.Scopes, ",") != "system,user" || summary.Certificates != 1 {
		t.Fatalf("unexpected summary: %+v", summary)
	}
	content, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	block, rest := pem.Decode(content)
	if block == nil || block.Type != "CERTIFICATE" || len(strings.TrimSpace(string(rest))) != 0 {
		t.Fatalf("output is not one canonical certificate")
	}
	actual := sha256.Sum256(block.Bytes)
	if hex.EncodeToString(actual[:]) != certificateHash {
		t.Fatalf("output hash mismatch")
	}
}

func TestMaterializeBundleFailsClosed(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	certificatePEM, certificateHash := testCA(t, now)
	tests := []struct {
		name       string
		mutate     func(*feedDocument)
		expectedOS string
		want       string
	}{
		{
			name: "expired",
			mutate: func(document *feedDocument) {
				document.GeneratedAt = now.Add(-time.Minute)
				document.ExpiresAt = now.Add(-30 * time.Second)
			},
			expectedOS: "windows",
			want:       "older than",
		},
		{
			name: "wrong host",
			mutate: func(document *feedDocument) {
				document.HostOS = "darwin"
			},
			expectedOS: "windows",
			want:       "does not match",
		},
		{
			name: "hash mismatch",
			mutate: func(document *feedDocument) {
				document.Certificates[0].SHA256 = strings.Repeat("0", 64)
			},
			expectedOS: "windows",
			want:       "SHA-256 mismatch",
		},
		{
			name: "distrusted certificate",
			mutate: func(document *feedDocument) {
				document.DistrustSHA256 = []string{certificateHash}
			},
			expectedOS: "windows",
			want:       "also present in distrustSHA256",
		},
		{
			name: "missing system scope",
			mutate: func(document *feedDocument) {
				document.Scopes = []string{"user"}
			},
			expectedOS: "windows",
			want:       "must include system scope",
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			document := testFeed(now, certificatePEM, certificateHash)
			test.mutate(&document)
			root := t.TempDir()
			feedPath := writeTestFeed(t, root, document)
			_, err := materializeBundle(feedPath, filepath.Join(root, "ca.pem"), test.expectedOS, now)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestMaterializeBundleRejectsSymlinkPaths(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	certificatePEM, certificateHash := testCA(t, now)
	root := t.TempDir()
	feedPath := writeTestFeed(t, root, testFeed(now, certificatePEM, certificateHash))
	feedLink := filepath.Join(root, "feed-link.json")
	if err := os.Symlink(feedPath, feedLink); err != nil {
		t.Skipf("symlink creation is unavailable: %v", err)
	}
	if _, err := materializeBundle(feedLink, filepath.Join(root, "ca.pem"), "windows", now); err == nil || !strings.Contains(err.Error(), "non-symlink") {
		t.Fatalf("symlink feed error = %v", err)
	}

	outputTarget := filepath.Join(root, "target.pem")
	if err := os.WriteFile(outputTarget, []byte("target"), 0o600); err != nil {
		t.Fatalf("write output target: %v", err)
	}
	outputLink := filepath.Join(root, "output-link.pem")
	if err := os.Symlink(outputTarget, outputLink); err != nil {
		t.Fatalf("create output symlink: %v", err)
	}
	if _, err := materializeBundle(feedPath, outputLink, "windows", now); err == nil || !strings.Contains(err.Error(), "non-symlink") {
		t.Fatalf("symlink output error = %v", err)
	}
}

func testFeed(now time.Time, certificatePEM []byte, certificateHash string) feedDocument {
	return feedDocument{
		SchemaVersion: 1,
		HostOS:        "windows",
		Scopes:        []string{"system", "user"},
		GeneratedAt:   now.Add(-time.Second),
		ExpiresAt:     now.Add(29 * time.Second),
		Certificates: []feedCertificate{{
			SHA256: certificateHash,
			PEM:    string(certificatePEM),
		}},
		DistrustSHA256: []string{},
	}
}

func writeTestFeed(t *testing.T, root string, document feedDocument) string {
	t.Helper()
	content, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("marshal feed: %v", err)
	}
	path := filepath.Join(root, "feed.json")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("write feed: %v", err)
	}
	return path
}

func testCA(t *testing.T, now time.Time) ([]byte, string) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, publicKey, privateKey)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	hash := sha256.Sum256(der)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), hex.EncodeToString(hash[:])
}
