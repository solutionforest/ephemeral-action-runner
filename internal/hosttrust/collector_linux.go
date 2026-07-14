//go:build linux

package hosttrust

import (
	"context"
	"fmt"
	"os"
	"os/exec"
)

func collectNative(ctx context.Context, scopes []string) (Snapshot, error) {
	if hasScope(scopes, ScopeUser) {
		return Snapshot{}, fmt.Errorf("Linux has no portable user host-trust store")
	}
	if !hasScope(scopes, ScopeSystem) {
		return Snapshot{}, nil
	}
	if path := os.Getenv("EPAR_HOST_TRUST_BUNDLE"); path != "" {
		return collectLinuxBundle(path, "linux-override")
	}
	if _, err := os.Stat("/etc/debian_version"); err == nil {
		if _, err := os.Stat("/etc/ssl/certs/ca-certificates.crt"); err == nil {
			return collectLinuxBundle("/etc/ssl/certs/ca-certificates.crt", "linux-debian-ca-bundle")
		}
	}
	if _, err := exec.LookPath("trust"); err == nil {
		temporary, err := os.CreateTemp("", "epar-host-trust-*.pem")
		if err != nil {
			return Snapshot{}, fmt.Errorf("create p11-kit trust output: %w", err)
		}
		path := temporary.Name()
		if err := temporary.Close(); err != nil {
			_ = os.Remove(path)
			return Snapshot{}, err
		}
		defer os.Remove(path)
		command := exec.CommandContext(ctx, "trust", "extract", "--filter=ca-anchors", "--purpose=server-auth", "--format=pem-bundle", "--overwrite", path)
		if output, err := command.CombinedOutput(); err != nil {
			return Snapshot{}, fmt.Errorf("extract p11-kit CA anchors: %w: %s", err, string(output))
		}
		return collectLinuxBundle(path, "linux-p11-kit")
	}
	for _, path := range []string{
		"/etc/ssl/certs/ca-certificates.crt",
		"/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem",
		"/etc/pki/tls/certs/ca-bundle.crt",
		"/var/lib/ca-certificates/ca-bundle.pem",
		"/etc/ssl/ca-bundle.pem",
	} {
		if _, err := os.Stat(path); err == nil {
			return collectLinuxBundle(path, "linux-ca-bundle")
		} else if !os.IsNotExist(err) {
			return Snapshot{}, fmt.Errorf("inspect Linux CA bundle %s: %w", path, err)
		}
	}
	return Snapshot{}, fmt.Errorf("no supported Linux system CA bundle found; set EPAR_HOST_TRUST_BUNDLE")
}

func collectLinuxBundle(path, kind string) (Snapshot, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return Snapshot{}, fmt.Errorf("read Linux CA bundle %s: %w", path, err)
	}
	certificates, err := CertificatesFromBytes(content)
	if err != nil {
		return Snapshot{}, fmt.Errorf("parse Linux CA bundle %s: %w", path, err)
	}
	return Snapshot{
		Certificates: certificates,
		Sources:      []Source{{Scope: ScopeSystem, Kind: kind, Path: path}},
	}, nil
}
