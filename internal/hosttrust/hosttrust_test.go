package hosttrust

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCertificatesFromBytesCanonicalizesAndRejectsLeaf(t *testing.T) {
	first := testCertificatePEM(t, true, "First CA")
	second := testCertificatePEM(t, true, "Second CA")
	certificates, err := CertificatesFromBytes(append(append([]byte(nil), second...), append(first, second...)...))
	if err != nil {
		t.Fatal(err)
	}
	if len(certificates) != 2 {
		t.Fatalf("certificate count = %d, want 2", len(certificates))
	}
	if certificates[0].SHA256 >= certificates[1].SHA256 {
		t.Fatalf("certificates are not sorted by SHA-256: %#v", certificates)
	}
	if !strings.HasPrefix(certificates[0].Name, "epar-") || !strings.HasSuffix(certificates[0].Name, ".crt") {
		t.Fatalf("certificate name = %q", certificates[0].Name)
	}
	if _, err := CertificatesFromBytes(testCertificatePEM(t, false, "Leaf")); err == nil {
		t.Fatal("leaf certificate accepted as host-trust CA")
	}
}

func TestCanonicalGenerationCoversHostScopesAndPolicy(t *testing.T) {
	certificates, err := CertificatesFromBytes(testCertificatePEM(t, true, "Root"))
	if err != nil {
		t.Fatal(err)
	}
	base, err := Canonicalize(Snapshot{
		HostOS:       "windows",
		Scopes:       []string{ScopeUser, ScopeSystem},
		Certificates: certificates,
		Policy:       []PolicyEntry{{Scope: "windows", Kind: "disallowed", SHA256: strings.Repeat("a", 64)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	reordered, err := Canonicalize(Snapshot{
		HostOS:       "windows",
		Scopes:       []string{ScopeSystem, ScopeUser},
		Certificates: certificates,
		Policy:       []PolicyEntry{{Scope: "windows", Kind: "disallowed", SHA256: strings.Repeat("a", 64)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if base.Generation != reordered.Generation {
		t.Fatalf("canonical generation changed after ordering-only change: %s != %s", base.Generation, reordered.Generation)
	}
	changedHost, err := Canonicalize(Snapshot{HostOS: "darwin", Scopes: base.Scopes, Certificates: certificates, Policy: base.Policy})
	if err != nil {
		t.Fatal(err)
	}
	if changedHost.Generation == base.Generation {
		t.Fatal("generation omitted hostOS")
	}
	changedPolicy, err := Canonicalize(Snapshot{HostOS: base.HostOS, Scopes: base.Scopes, Certificates: certificates})
	if err != nil {
		t.Fatal(err)
	}
	if changedPolicy.Generation == base.Generation {
		t.Fatal("generation omitted policy entries")
	}
	marker, err := MarkerJSON(base)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(marker), `"hostOS":"windows"`) {
		t.Fatalf("marker JSON did not use hostOS: %s", marker)
	}
}

func TestParseFeedVerifiesCertificateHashAndFreshness(t *testing.T) {
	certificates, err := CertificatesFromBytes(testCertificatePEM(t, true, "Feed Root"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 13, 3, 4, 5, 0, time.UTC)
	feed := FeedDocument{
		SchemaVersion: feedSchemaVersion,
		HostOS:        "darwin",
		Scopes:        []string{ScopeSystem, ScopeUser},
		GeneratedAt:   now.Add(-29 * time.Second),
		ExpiresAt:     now.Add(time.Minute),
		Certificates:  []FeedCertificate{{SHA256: certificates[0].SHA256, PEM: string(certificates[0].PEM)}},
	}
	content, err := json.Marshal(feed)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := ParseFeed(content, "", now, "feed.json")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := snapshot.Scopes, []string{ScopeSystem, ScopeUser}; !sameStrings(got, want) {
		t.Fatalf("feed scopes = %#v, want %#v", got, want)
	}
	if snapshot.HostOS != "darwin" {
		t.Fatalf("feed hostOS = %q", snapshot.HostOS)
	}
	feed.Certificates[0].SHA256 = strings.Repeat("0", 64)
	content, err = json.Marshal(feed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseFeed(content, "", now, "feed.json"); err == nil || !strings.Contains(err.Error(), "does not match PEM") {
		t.Fatalf("bad per-certificate hash error = %v", err)
	}
	feed.Certificates[0].SHA256 = certificates[0].SHA256
	feed.GeneratedAt = now.Add(-31 * time.Second)
	content, err = json.Marshal(feed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseFeed(content, "", now, "feed.json"); err == nil || !strings.Contains(err.Error(), "older") {
		t.Fatalf("stale feed error = %v", err)
	}
}

func TestResolveExternalFeedDoesNotRequireNativeHostOS(t *testing.T) {
	certificates, err := CertificatesFromBytes(testCertificatePEM(t, true, "External Root"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 13, 3, 4, 5, 0, time.UTC)
	content, err := json.Marshal(FeedDocument{
		SchemaVersion: feedSchemaVersion,
		HostOS:        "darwin",
		Scopes:        []string{ScopeSystem, ScopeUser},
		GeneratedAt:   now,
		ExpiresAt:     now.Add(time.Minute),
		Certificates:  []FeedCertificate{{SHA256: certificates[0].SHA256, PEM: string(certificates[0].PEM)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "host-trust.json")
	if err := os.WriteFile(path, content, 0600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := Resolve(context.Background(), Options{
		Mode:             ModeOverlay,
		Scopes:           []string{ScopeUser, ScopeSystem},
		FeedPath:         path,
		ControllerHostOS: "darwin",
		Now:              func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.HostOS != "darwin" || !sameStrings(snapshot.Scopes, []string{ScopeSystem, ScopeUser}) {
		t.Fatalf("external snapshot = %+v", snapshot)
	}
}

func testCertificatePEM(t *testing.T, isCA bool, commonName string) []byte {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		BasicConstraintsValid: true,
		IsCA:                  isCA,
	}
	if isCA {
		template.KeyUsage = x509.KeyUsageCertSign
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, publicKey, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}
