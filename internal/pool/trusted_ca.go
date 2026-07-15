package pool

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
)

const trustedCAGuestDir = "/usr/local/share/ca-certificates/epar"

type trustedCACertificate struct {
	DestinationName string
	PEM             []byte
}

func (m *Manager) trustedCACertificates() ([]trustedCACertificate, error) {
	byName := make(map[string]trustedCACertificate)
	for _, configuredPath := range m.Config.Image.TrustedCACertificatePaths {
		path := config.ProjectPath(m.ProjectRoot, strings.TrimSpace(configuredPath))
		info, err := os.Stat(path)
		if err != nil {
			return nil, fmt.Errorf("trusted CA certificate %s: %w", path, err)
		}
		if info.IsDir() {
			return nil, fmt.Errorf("trusted CA certificate %s is a directory", path)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read trusted CA certificate %s: %w", path, err)
		}
		certificates, err := parseTrustedCACertificateFile(content)
		if err != nil {
			return nil, fmt.Errorf("trusted CA certificate %s: %w", path, err)
		}
		for _, certificate := range certificates {
			if !certificate.IsCA {
				return nil, fmt.Errorf("trusted CA certificate %s contains non-CA certificate %q", path, certificate.Subject.String())
			}
			sum := sha256.Sum256(certificate.Raw)
			name := "epar-" + hex.EncodeToString(sum[:12]) + ".crt"
			byName[name] = trustedCACertificate{
				DestinationName: name,
				PEM:             pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw}),
			}
		}
	}
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]trustedCACertificate, 0, len(names))
	for _, name := range names {
		out = append(out, byName[name])
	}
	return out, nil
}

func parseTrustedCACertificateFile(content []byte) ([]*x509.Certificate, error) {
	trimmed := bytes.TrimSpace(content)
	if !bytes.HasPrefix(trimmed, []byte("-----BEGIN")) {
		certificates, err := x509.ParseCertificates(content)
		if err != nil || len(certificates) == 0 {
			if err == nil {
				err = fmt.Errorf("no certificates found")
			}
			return nil, fmt.Errorf("parse PEM or DER certificate: %w", err)
		}
		return certificates, nil
	}
	rest := trimmed
	var certificates []*x509.Certificate
	for {
		block, next := pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			return nil, fmt.Errorf("PEM block type %q is not CERTIFICATE", block.Type)
		}
		certificate, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse PEM certificate: %w", err)
		}
		certificates = append(certificates, certificate)
		rest = next
	}
	if len(certificates) > 0 {
		if len(bytes.TrimSpace(rest)) != 0 {
			return nil, fmt.Errorf("unexpected non-certificate content after PEM certificate")
		}
		return certificates, nil
	}
	return nil, fmt.Errorf("no PEM certificates found")
}

func (m *Manager) copyTrustedCACertificatesToDir(destination string) error {
	certificates, err := m.trustedCACertificates()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(destination, 0755); err != nil {
		return err
	}
	for _, certificate := range certificates {
		if err := os.WriteFile(filepath.Join(destination, certificate.DestinationName), certificate.PEM, 0644); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) installTrustedCACertificates(ctx context.Context, vmName string) error {
	certificates, err := m.trustedCACertificates()
	if err != nil {
		return err
	}
	if len(certificates) == 0 {
		return nil
	}
	if _, err := m.execGuest(ctx, vmName, provider.ShellCommand("if command -v sudo >/dev/null 2>&1; then sudo mkdir -p "+shellQuote(trustedCAGuestDir)+"; else mkdir -p "+shellQuote(trustedCAGuestDir)+"; fi"), provider.ExecOptions{}); err != nil {
		return err
	}
	for _, certificate := range certificates {
		guestPath := trustedCAGuestDir + "/" + certificate.DestinationName
		if err := provider.CopyText(ctx, m.Provider, vmName, guestPath, "0644", string(certificate.PEM)); err != nil {
			return err
		}
	}
	command := "if command -v sudo >/dev/null 2>&1; then sudo bash /opt/epar/install-trusted-ca-certificates.sh; else bash /opt/epar/install-trusted-ca-certificates.sh; fi"
	_, err = m.execGuest(ctx, vmName, []string{"bash", "-c", command}, provider.ExecOptions{})
	return err
}
