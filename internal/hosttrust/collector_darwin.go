//go:build darwin

package hosttrust

import (
	"context"
	"crypto/sha1" // macOS external trust settings are keyed by SHA-1 fingerprints.
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

const (
	macSystemRootsKeychain = "/System/Library/Keychains/SystemRootCertificates.keychain"
	macAdminKeychain       = "/Library/Keychains/System.keychain"
)

type macCandidate struct {
	certificate Certificate
}

func collectNative(ctx context.Context, scopes []string) (Snapshot, error) {
	candidates := make(map[string]macCandidate)
	baseline := make(map[string]Certificate)
	allowed := make(map[string]Certificate)
	denied := make(map[string]struct{})
	var snapshot Snapshot
	userTrustEnabled := false
	if hasScope(scopes, ScopeUser) {
		output, err := runMacSecurity(ctx, "user-trust-settings-enable")
		if err != nil {
			return Snapshot{}, err
		}
		userTrustEnabled, err = macUserTrustSettingsEnabledFromOutput(output)
		if err != nil {
			return Snapshot{}, err
		}
		snapshot.Sources = append(snapshot.Sources, Source{Scope: ScopeUser, Kind: fmt.Sprintf("macos-user-trust-settings-enabled-%t", userTrustEnabled), Path: "security user-trust-settings-enable"})
	}

	addKeychain := func(scope, kind, path string, required, baselineAllowed bool) error {
		output, err := runMacSecurity(ctx, "find-certificate", "-a", "-p", path)
		if err != nil {
			return err
		}
		parsed, err := parseMacKeychainCertificates(output, required)
		if err != nil {
			return fmt.Errorf("parse certificates from macOS keychain %s: %w", path, err)
		}
		for _, certificate := range parsed {
			if !certificate.IsCA {
				continue
			}
			canonical := certificatesFromParsed([]*x509.Certificate{certificate})[0]
			fingerprint := sha1.Sum(certificate.Raw)
			key := strings.ToUpper(hex.EncodeToString(fingerprint[:]))
			candidates[key] = macCandidate{certificate: canonical}
			if baselineAllowed {
				baseline[canonical.SHA256] = canonical
			}
		}
		snapshot.Sources = append(snapshot.Sources, Source{Scope: scope, Kind: kind, Path: path})
		return nil
	}

	if hasScope(scopes, ScopeSystem) {
		if err := addKeychain(ScopeSystem, "macos-system-roots", macSystemRootsKeychain, true, true); err != nil {
			return Snapshot{}, err
		}
		if err := addKeychain(ScopeSystem, "macos-admin-keychain", macAdminKeychain, false, false); err != nil {
			return Snapshot{}, err
		}
	}
	if hasScope(scopes, ScopeUser) && userTrustEnabled {
		keychains, err := macUserKeychains(ctx)
		if err != nil {
			return Snapshot{}, err
		}
		for _, keychain := range keychains {
			if err := addKeychain(ScopeUser, "macos-user-keychain", keychain, false, false); err != nil {
				return Snapshot{}, err
			}
		}
	}

	applyDomain := func(scope, domain string, required bool) error {
		content, present, err := exportMacTrustSettingsPlist(ctx, domain, required)
		if err != nil {
			return err
		}
		if !present {
			return nil
		}
		decisions, err := macTrustDecisionsFromPlist(content)
		if err != nil {
			return fmt.Errorf("parse macOS %s trust settings: %w", domain, err)
		}
		for fingerprint, decision := range decisions {
			candidate, found := candidates[fingerprint]
			if decision.Deny {
				if found {
					denied[candidate.certificate.SHA256] = struct{}{}
					snapshot.Policy = append(snapshot.Policy, PolicyEntry{Scope: scope, Kind: "macos-explicit-deny", SHA256: candidate.certificate.SHA256})
				}
				continue
			}
			if decision.Allow {
				if !found {
					return fmt.Errorf("macOS %s trust settings reference certificate %s which is absent from the selected keychains", domain, fingerprint)
				}
				allowed[candidate.certificate.SHA256] = candidate.certificate
			}
		}
		snapshot.Sources = append(snapshot.Sources, Source{Scope: scope, Kind: "macos-trust-settings-" + domain, Path: "security trust-settings-export"})
		return nil
	}
	if hasScope(scopes, ScopeSystem) {
		if err := applyDomain(ScopeSystem, "system", true); err != nil {
			return Snapshot{}, err
		}
		if err := applyDomain(ScopeSystem, "admin", false); err != nil {
			return Snapshot{}, err
		}
	}
	if hasScope(scopes, ScopeUser) && userTrustEnabled {
		if err := applyDomain(ScopeUser, "user", false); err != nil {
			return Snapshot{}, err
		}
	}
	snapshot.Certificates = macSelectedCertificates(baseline, allowed, denied)
	return snapshot, nil
}

func macUserKeychains(ctx context.Context) ([]string, error) {
	output, err := runMacSecurity(ctx, "list-keychains", "-d", "user")
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, line := range strings.Split(string(output), "\n") {
		path := strings.Trim(strings.TrimSpace(line), `"`)
		if path != "" {
			paths = append(paths, path)
		}
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("security list-keychains -d user returned no keychains")
	}
	return paths, nil
}

func exportMacTrustSettingsPlist(ctx context.Context, domain string, required bool) ([]byte, bool, error) {
	directory, err := os.MkdirTemp("", "epar-macos-trust-settings-")
	if err != nil {
		return nil, false, err
	}
	defer os.RemoveAll(directory)
	path := directory + "/settings.plist"
	args := []string{"trust-settings-export"}
	if domain == "system" {
		args = append(args, "-s")
	}
	if domain == "admin" {
		args = append(args, "-d")
	}
	args = append(args, path)
	if _, err := runMacSecurity(ctx, args...); err != nil {
		if !required && macTrustSettingsExportWasAbsent(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	command := exec.CommandContext(ctx, "plutil", "-convert", "xml1", "-o", "-", path)
	output, err := command.CombinedOutput()
	if err != nil {
		return nil, false, fmt.Errorf("plutil convert macOS %s trust settings: %w: %s", domain, err, strings.TrimSpace(string(output)))
	}
	return output, true, nil
}

func runMacSecurity(ctx context.Context, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, "security", args...)
	output, err := command.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("security %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return output, nil
}
