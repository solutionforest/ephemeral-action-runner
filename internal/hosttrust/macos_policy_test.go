package hosttrust

import (
	"fmt"
	"strings"
	"testing"
)

func TestMacTrustDecisionsAllowOnlyUnconstrainedTLSAndDenyWins(t *testing.T) {
	fingerprint := strings.Repeat("A", 40)
	content := `{"trustVersion":1,"trustList":{"` + fingerprint + `":{"trustSettings":[` +
		`{"kSecTrustSettingsPolicy":"opaque","kSecTrustSettingsPolicyName":"sslServer","kSecTrustSettingsResult":2},` +
		`{"kSecTrustSettingsPolicy":"opaque","kSecTrustSettingsPolicyName":"SMIME","kSecTrustSettingsResult":1}` +
		`]}}}`
	decisions, err := macTrustDecisionsFromJSON([]byte(content))
	if err != nil {
		t.Fatal(err)
	}
	if !decisions[fingerprint].Allow || decisions[fingerprint].Deny {
		t.Fatalf("TLS trust decision = %+v", decisions[fingerprint])
	}

	content = `{"trustVersion":1,"trustList":{"` + fingerprint + `":{"trustSettings":[` +
		`{"kSecTrustSettingsResult":1},` +
		`{"kSecTrustSettingsPolicyString":"restricted.example","kSecTrustSettingsResult":3}` +
		`]}}}`
	decisions, err = macTrustDecisionsFromJSON([]byte(content))
	if err != nil {
		t.Fatal(err)
	}
	if decisions[fingerprint].Allow || !decisions[fingerprint].Deny {
		t.Fatalf("deny precedence decision = %+v", decisions[fingerprint])
	}
}

func TestMacTrustDecisionsRejectsUnexportableConstraints(t *testing.T) {
	fingerprint := strings.Repeat("B", 40)
	content := `{"trustVersion":1,"trustList":{"` + fingerprint + `":{"trustSettings":[` +
		`{"kSecTrustSettingsAllowedError":-1,"kSecTrustSettingsPolicy":"opaque","kSecTrustSettingsPolicyName":"sslServer","kSecTrustSettingsResult":2}` +
		`]}}}`
	decisions, err := macTrustDecisionsFromJSON([]byte(content))
	if err != nil {
		t.Fatal(err)
	}
	if decisions[fingerprint].Allow {
		t.Fatalf("allowed-error trust setting was exported globally: %+v", decisions[fingerprint])
	}
	content = `{"trustVersion":1,"trustList":{"` + fingerprint + `":{"trustSettings":[` +
		`{"kSecTrustSettingsPolicyString":"restricted.example","kSecTrustSettingsPolicy":"opaque","kSecTrustSettingsPolicyName":"sslServer","kSecTrustSettingsResult":2}` +
		`]}}}`
	decisions, err = macTrustDecisionsFromJSON([]byte(content))
	if err != nil {
		t.Fatal(err)
	}
	if decisions[fingerprint].Allow {
		t.Fatalf("hostname-constrained positive setting was exported: %+v", decisions[fingerprint])
	}
	if _, err := macTrustDecisionsFromJSON([]byte(`{"trustVersion":1,"trustList":{"default-root":{}}}`)); err == nil {
		t.Fatal("domain-wide/default trust entry was accepted")
	}
}

func TestMacTrustDecisionsAcceptsCASafeKeyUsageConstants(t *testing.T) {
	fingerprint := strings.Repeat("D", 40)
	for _, usage := range []string{"-1", "4294967295", "1", "8"} {
		content := `{"trustVersion":1,"trustList":{"` + fingerprint + `":{"trustSettings":[` +
			`{"kSecTrustSettingsPolicy":"opaque","kSecTrustSettingsPolicyName":"sslServer","kSecTrustSettingsKeyUsage":` + usage + `,"kSecTrustSettingsResult":2}` +
			`]}}}`
		decisions, err := macTrustDecisionsFromJSON([]byte(content))
		if err != nil || !decisions[fingerprint].Allow {
			t.Fatalf("CA-safe key usage %s rejected: decisions=%v error=%v", usage, decisions, err)
		}
	}
	for _, usage := range []string{"2", "9", "16", `"8"`} {
		content := `{"trustVersion":1,"trustList":{"` + fingerprint + `":{"trustSettings":[` +
			`{"kSecTrustSettingsKeyUsage":` + usage + `,"kSecTrustSettingsResult":1}` +
			`]}}}`
		decisions, err := macTrustDecisionsFromJSON([]byte(content))
		if err != nil {
			t.Fatal(err)
		}
		if decisions[fingerprint].Allow {
			t.Fatalf("conditional key usage %s exported globally", usage)
		}
	}
}

func TestMacUserTrustSettingsEnabledFromOutput(t *testing.T) {
	for _, test := range []struct {
		name    string
		output  string
		enabled bool
		wantErr bool
	}{
		{name: "enabled", output: "User-level Trust Settings are Enabled\n", enabled: true},
		{name: "disabled", output: "User-level Trust Settings are Disabled\n"},
		{name: "unknown", output: "enabled", wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			enabled, err := macUserTrustSettingsEnabledFromOutput([]byte(test.output))
			if (err != nil) != test.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, test.wantErr)
			}
			if enabled != test.enabled {
				t.Fatalf("enabled = %v, want %v", enabled, test.enabled)
			}
		})
	}
}

func TestMacTrustDocumentRejectsMissingOrInvalidTrustSettings(t *testing.T) {
	fingerprint := strings.Repeat("C", 40)
	for _, value := range []string{"null", `{}`, `"not-an-array"`} {
		content := `{"trustVersion":1,"trustList":{"` + fingerprint + `":{"trustSettings":` + value + `}}}`
		if _, err := macTrustDecisionsFromJSON([]byte(content)); err == nil {
			t.Fatalf("trustSettings=%s was accepted", value)
		}
	}
	missing := `{"trustVersion":1,"trustList":{"` + fingerprint + `":{}}}`
	if _, err := macTrustDecisionsFromJSON([]byte(missing)); err == nil {
		t.Fatal("missing trustSettings was accepted")
	}
	empty := `{"trustVersion":1,"trustList":{"` + fingerprint + `":{"trustSettings":[]}}}`
	decisions, err := macTrustDecisionsFromJSON([]byte(empty))
	if err != nil || !decisions[fingerprint].Allow {
		t.Fatalf("present empty trustSettings should be unconditional trust: decisions=%v error=%v", decisions, err)
	}
	nullSetting := `{"trustVersion":1,"trustList":{"` + fingerprint + `":{"trustSettings":[null]}}}`
	if _, err := macTrustDecisionsFromJSON([]byte(nullSetting)); err == nil {
		t.Fatal("null trust setting was accepted")
	}
}

func TestMacTrustDocumentRejectsMissingOrNullTrustList(t *testing.T) {
	for _, content := range []string{
		`{"trustVersion":1}`,
		`{"trustVersion":1,"trustList":null}`,
	} {
		if _, err := macTrustDecisionsFromJSON([]byte(content)); err == nil {
			t.Fatalf("invalid trustList was accepted: %s", content)
		}
	}
}

func TestMacTrustSettingsExportAbsentClassification(t *testing.T) {
	absent := fmt.Errorf("security trust-settings-export: exit status 1: SecTrustSettingsCreateExternalRepresentation: No Trust Settings were found.")
	if !macTrustSettingsExportWasAbsent(absent) {
		t.Fatal("documented no-trust-settings error was not classified as absent")
	}
	for _, err := range []error{
		fmt.Errorf("security trust-settings-export: permission denied"),
		fmt.Errorf("security trust-settings-export: transient failure"),
	} {
		if macTrustSettingsExportWasAbsent(err) {
			t.Fatalf("unexpected error classified as absent: %v", err)
		}
	}
}

func TestParseMacKeychainCertificatesRequiredAndOptionalEmptyOutput(t *testing.T) {
	for _, content := range [][]byte{nil, []byte(" \r\n\t")} {
		parsed, err := parseMacKeychainCertificates(content, false)
		if err != nil || len(parsed) != 0 {
			t.Fatalf("optional empty keychain output parsed=%v error=%v", parsed, err)
		}
		if _, err := parseMacKeychainCertificates(content, true); err == nil {
			t.Fatal("required empty keychain output was accepted")
		}
	}
	if _, err := parseMacKeychainCertificates([]byte("not a certificate"), false); err == nil {
		t.Fatal("optional nonempty malformed keychain output was accepted")
	}
	parsed, err := parseMacKeychainCertificates(testCertificatePEM(t, true, "macOS Keychain Root"), true)
	if err != nil || len(parsed) != 1 {
		t.Fatalf("valid required keychain output parsed=%d error=%v", len(parsed), err)
	}
}

func TestMacSelectedCertificatesUsesSystemRootsBaselineAndDenyWins(t *testing.T) {
	root := mustTestCertificate(t, "macOS System Root")
	explicit := mustTestCertificate(t, "macOS Explicit Root")
	selected := macSelectedCertificates(
		map[string]Certificate{root.SHA256: root},
		map[string]Certificate{explicit.SHA256: explicit},
		map[string]struct{}{explicit.SHA256: {}},
	)
	if len(selected) != 1 || selected[0].SHA256 != root.SHA256 {
		t.Fatalf("selected certificates = %#v, want baseline root only", selected)
	}
	selected = macSelectedCertificates(
		map[string]Certificate{root.SHA256: root},
		map[string]Certificate{explicit.SHA256: explicit},
		map[string]struct{}{root.SHA256: {}},
	)
	if len(selected) != 1 || selected[0].SHA256 != explicit.SHA256 {
		t.Fatalf("selected certificates = %#v, want explicit root only", selected)
	}
}

func mustTestCertificate(t *testing.T, commonName string) Certificate {
	t.Helper()
	certificates, err := CertificatesFromBytes(testCertificatePEM(t, true, commonName))
	if err != nil || len(certificates) != 1 {
		t.Fatalf("create test certificate: count=%d error=%v", len(certificates), err)
	}
	return certificates[0]
}
