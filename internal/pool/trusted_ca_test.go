package pool

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
)

func TestDockerDindBuildContextInstallsTrustedCABeforeNetworkSteps(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{
		filepath.Join(root, "scripts", "guest", "ubuntu"),
		filepath.Join(root, "scripts", "container", "ubuntu"),
	} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	certificatePath := filepath.Join(root, "enterprise-root.pem")
	writeTestCACertificate(t, certificatePath, "Enterprise Root One")
	manager := Manager{
		Config: config.Config{Image: config.ImageConfig{
			UpstreamLock:              "missing.lock",
			TrustedCACertificatePaths: []string{"enterprise-root.pem"},
		}},
		ProjectRoot: root,
	}
	buildContext := t.TempDir()
	if err := manager.prepareDockerDindBuildContext(buildContext, t.TempDir(), `{"hash":"test"}`+"\n"); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(filepath.Join(buildContext, "trusted-ca-certificates"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || !strings.HasPrefix(entries[0].Name(), "epar-") || !strings.HasSuffix(entries[0].Name(), ".crt") {
		t.Fatalf("normalized trusted CA files = %#v", entries)
	}
	normalized, err := os.ReadFile(filepath.Join(buildContext, "trusted-ca-certificates", entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(normalized)
	if block == nil || block.Type != "CERTIFICATE" {
		t.Fatalf("normalized trusted CA is not PEM: %q", normalized)
	}

	dockerfileBytes, err := os.ReadFile(filepath.Join(buildContext, "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	dockerfile := string(dockerfileBytes)
	trustIndex := strings.Index(dockerfile, "RUN bash /opt/epar/install-trusted-ca-certificates.sh")
	baseIndex := strings.Index(dockerfile, "RUN bash /opt/epar/install-base.sh")
	runnerIndex := strings.Index(dockerfile, "RUN bash /opt/epar/install-runner.sh")
	if trustIndex < 0 || baseIndex < 0 || runnerIndex < 0 || !(trustIndex < baseIndex && baseIndex < runnerIndex) {
		t.Fatalf("trusted CA installation must precede all network install steps:\n%s", dockerfile)
	}
}

func TestTrustedCACertificateValidationRejectsInvalidInputs(t *testing.T) {
	root := t.TempDir()
	invalidPath := filepath.Join(root, "invalid.crt")
	if err := os.WriteFile(invalidPath, []byte("not a certificate\n"), 0644); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		path string
		want string
	}{
		{name: "missing", path: "missing.pem", want: "missing.pem"},
		{name: "directory", path: ".", want: "is a directory"},
		{name: "invalid content", path: "invalid.crt", want: "parse pem or der certificate"},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager := Manager{
				Config:      config.Config{Image: config.ImageConfig{TrustedCACertificatePaths: []string{test.path}}},
				ProjectRoot: root,
			}
			if _, err := manager.trustedCACertificates(); err == nil || !strings.Contains(strings.ToLower(err.Error()), test.want) {
				t.Fatalf("trustedCACertificates() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestBuildImageRejectsInvalidTrustedCABeforePreparingProviderBuild(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "invalid.pem"), []byte("invalid\n"), 0644); err != nil {
		t.Fatal(err)
	}
	manager := Manager{
		Config: config.Config{
			Image: config.ImageConfig{TrustedCACertificatePaths: []string{"invalid.pem"}},
			Provider: config.ProviderConfig{
				Type: "unsupported-provider-that-must-not-be-reached",
			},
		},
		ProjectRoot: root,
		DryRun:      true,
	}
	err := manager.BuildImage(context.Background(), ImageBuildOptions{})
	if err == nil || !strings.Contains(err.Error(), "invalid.pem") {
		t.Fatalf("BuildImage() error = %v, want trusted CA validation error", err)
	}
}

func TestTrustedCACertificateAcceptsDERAndNormalizesToPEM(t *testing.T) {
	root := t.TempDir()
	pemPath := filepath.Join(root, "root.pem")
	writeTestCACertificate(t, pemPath, "DER Enterprise Root")
	content, err := os.ReadFile(pemPath)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(content)
	if block == nil {
		t.Fatal("test certificate is not PEM")
	}
	derPath := filepath.Join(root, "root.crt")
	if err := os.WriteFile(derPath, block.Bytes, 0644); err != nil {
		t.Fatal(err)
	}
	manager := Manager{
		Config:      config.Config{Image: config.ImageConfig{TrustedCACertificatePaths: []string{"root.crt"}}},
		ProjectRoot: root,
	}
	certificates, err := manager.trustedCACertificates()
	if err != nil {
		t.Fatal(err)
	}
	if len(certificates) != 1 || !bytes.HasPrefix(certificates[0].PEM, []byte("-----BEGIN CERTIFICATE-----")) {
		t.Fatalf("normalized certificates = %+v", certificates)
	}
}

func TestTrustedCACertificateDigestInvalidatesImageManifest(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "scripts", "guest", "ubuntu"), 0755); err != nil {
		t.Fatal(err)
	}
	sourcePath := filepath.Join(root, "source.tar")
	if err := os.WriteFile(sourcePath, []byte("source"), 0644); err != nil {
		t.Fatal(err)
	}
	certificatePath := filepath.Join(root, "enterprise-root.pem")
	writeTestCACertificate(t, certificatePath, "Enterprise Root One")
	manager := Manager{
		Config: config.Config{
			Image: config.ImageConfig{
				SourceType:                config.ImageSourceRootFSTar,
				SourceImage:               "source.tar",
				OutputImage:               "output.tar",
				RunnerVersion:             "latest",
				TrustedCACertificatePaths: []string{"enterprise-root.pem"},
			},
			Provider: config.ProviderConfig{Type: "wsl"},
		},
		ProjectRoot: root,
	}
	manifest, err := manager.desiredImageManifest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.TrustedCACertificates) != 1 || manifest.TrustedCACertificates[0].Path != "enterprise-root.pem" {
		t.Fatalf("trusted CA manifest entries = %+v", manifest.TrustedCACertificates)
	}
	before, err := imageManifestHash(manifest)
	if err != nil {
		t.Fatal(err)
	}
	writeTestCACertificate(t, certificatePath, "Enterprise Root Two")
	manifest, err = manager.desiredImageManifest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	after, err := imageManifestHash(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Fatal("image manifest hash did not change after trusted CA certificate rotation")
	}
}

func writeTestCACertificate(t *testing.T, path, commonName string) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, publicKey, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	content := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatal(err)
	}
}
