package hosttrust

import (
	"fmt"
	"strings"
	"testing"
)

func TestMacTrustDecisionsAllowCFDataPolicyAndDenyWins(t *testing.T) {
	fingerprint := strings.Repeat("A", 40)
	content := macTrustSettingsPlist(fingerprint, `<array><dict>`+
		`<key>kSecTrustSettingsPolicy</key><data>AQID</data>`+
		`<key>kSecTrustSettingsPolicyName</key><string>sslServer</string>`+
		`<key>kSecTrustSettingsResult</key><integer>2</integer>`+
		`</dict><dict>`+
		`<key>kSecTrustSettingsPolicy</key><data>BAUG</data>`+
		`<key>kSecTrustSettingsPolicyName</key><string>SMIME</string>`+
		`<key>kSecTrustSettingsResult</key><integer>1</integer>`+
		`</dict></array>`)
	decisions, err := macTrustDecisionsFromPlist(content)
	if err != nil {
		t.Fatal(err)
	}
	if !decisions[fingerprint].Allow || decisions[fingerprint].Deny {
		t.Fatalf("TLS trust decision = %+v", decisions[fingerprint])
	}

	content = macTrustSettingsPlist(fingerprint, `<array><dict>`+
		`<key>kSecTrustSettingsResult</key><integer>1</integer>`+
		`</dict><dict>`+
		`<key>kSecTrustSettingsPolicyString</key><string>restricted.example</string>`+
		`<key>kSecTrustSettingsResult</key><integer>3</integer>`+
		`</dict></array>`)
	decisions, err = macTrustDecisionsFromPlist(content)
	if err != nil {
		t.Fatal(err)
	}
	if decisions[fingerprint].Allow || !decisions[fingerprint].Deny {
		t.Fatalf("deny precedence decision = %+v", decisions[fingerprint])
	}
}

func TestMacTrustDecisionsRejectsUnexportableConstraints(t *testing.T) {
	fingerprint := strings.Repeat("B", 40)
	for _, setting := range []string{
		`<dict><key>kSecTrustSettingsAllowedError</key><integer>-1</integer><key>kSecTrustSettingsPolicy</key><data>AQID</data><key>kSecTrustSettingsPolicyName</key><string>sslServer</string><key>kSecTrustSettingsResult</key><integer>2</integer></dict>`,
		`<dict><key>kSecTrustSettingsPolicyString</key><string>restricted.example</string><key>kSecTrustSettingsPolicy</key><data>AQID</data><key>kSecTrustSettingsPolicyName</key><string>sslServer</string><key>kSecTrustSettingsResult</key><integer>2</integer></dict>`,
		`<dict><key>kSecTrustSettingsPolicy</key><data>AQID</data><key>kSecTrustSettingsResult</key><integer>2</integer></dict>`,
		`<dict><key>kSecTrustSettingsPolicy</key><data>AQID</data><key>kSecTrustSettingsPolicyName</key><string>SMIME</string><key>kSecTrustSettingsResult</key><integer>2</integer></dict>`,
	} {
		decisions, err := macTrustDecisionsFromPlist(macTrustSettingsPlist(fingerprint, `<array>`+setting+`</array>`))
		if err != nil {
			t.Fatal(err)
		}
		if decisions[fingerprint].Allow {
			t.Fatalf("constrained or non-TLS trust setting was exported: %s", setting)
		}
	}
	if _, err := macTrustDecisionsFromPlist(macTrustSettingsPlist("default-root", `<array/>`)); err == nil {
		t.Fatal("domain-wide/default trust entry was accepted")
	}
}

func TestMacTrustDecisionsAcceptsCASafeKeyUsageConstants(t *testing.T) {
	fingerprint := strings.Repeat("D", 40)
	for _, usage := range []string{"-1", "4294967295", "1", "8"} {
		content := macTrustSettingsPlist(fingerprint, `<array><dict>`+
			`<key>kSecTrustSettingsPolicy</key><data>AQID</data>`+
			`<key>kSecTrustSettingsPolicyName</key><string>sslServer</string>`+
			`<key>kSecTrustSettingsKeyUsage</key><integer>`+usage+`</integer>`+
			`<key>kSecTrustSettingsResult</key><integer>2</integer>`+
			`</dict></array>`)
		decisions, err := macTrustDecisionsFromPlist(content)
		if err != nil || !decisions[fingerprint].Allow {
			t.Fatalf("CA-safe key usage %s rejected: decisions=%v error=%v", usage, decisions, err)
		}
	}
	for _, value := range []string{
		`<integer>2</integer>`,
		`<integer>9</integer>`,
		`<integer>16</integer>`,
		`<string>8</string>`,
	} {
		content := macTrustSettingsPlist(fingerprint, `<array><dict>`+
			`<key>kSecTrustSettingsKeyUsage</key>`+value+
			`<key>kSecTrustSettingsResult</key><integer>1</integer>`+
			`</dict></array>`)
		decisions, err := macTrustDecisionsFromPlist(content)
		if err != nil {
			t.Fatal(err)
		}
		if decisions[fingerprint].Allow {
			t.Fatalf("conditional key usage %s exported globally", value)
		}
	}
}

func TestMacTrustDocumentRejectsMalformedPlistAndData(t *testing.T) {
	fingerprint := strings.Repeat("E", 40)
	for _, content := range [][]byte{
		[]byte(`<plist version="1.0"><dict>`),
		macTrustSettingsPlist(fingerprint, `<array><dict><key>kSecTrustSettingsPolicy</key><data>%%%not-base64%%%</data><key>kSecTrustSettingsPolicyName</key><string>sslServer</string></dict></array>`),
		macTrustSettingsPlist(fingerprint, `<array><dict><key>kSecTrustSettingsPolicy</key><data>AQID</string></dict></array>`),
	} {
		if _, err := macTrustDecisionsFromPlist(content); err == nil {
			t.Fatalf("malformed plist was accepted: %s", content)
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

func TestMacTrustDocumentAllowsOmittedTrustSettingsAndRejectsInvalidPresentValue(t *testing.T) {
	fingerprint := strings.Repeat("C", 40)
	for _, value := range []string{`<data>AQID</data>`, `<dict/>`, `<string>not-an-array</string>`} {
		content := macTrustSettingsPlist(fingerprint, value)
		if _, err := macTrustDecisionsFromPlist(content); err == nil {
			t.Fatalf("trustSettings=%s was accepted", value)
		}
	}
	omitted := macTrustEntryPlist(fingerprint, `<key>issuerName</key><data>AQID</data><key>serialNumber</key><data>BAUG</data>`)
	decisions, err := macTrustDecisionsFromPlist(omitted)
	if err != nil || !decisions[fingerprint].Allow || decisions[fingerprint].Deny {
		t.Fatalf("omitted trustSettings should be unconditional trust: decisions=%v error=%v", decisions, err)
	}
	empty := macTrustSettingsPlist(fingerprint, `<array/>`)
	decisions, err = macTrustDecisionsFromPlist(empty)
	if err != nil || !decisions[fingerprint].Allow {
		t.Fatalf("present empty trustSettings should be unconditional trust: decisions=%v error=%v", decisions, err)
	}
	nonDictionarySetting := macTrustSettingsPlist(fingerprint, `<array><string>invalid</string></array>`)
	if _, err := macTrustDecisionsFromPlist(nonDictionarySetting); err == nil {
		t.Fatal("non-dictionary trust setting was accepted")
	}
}

func TestMacTrustDocumentRejectsMissingOrInvalidTrustList(t *testing.T) {
	for _, content := range [][]byte{
		macPlistDocument(`<key>trustVersion</key><integer>1</integer>`),
		macPlistDocument(`<key>trustVersion</key><integer>1</integer><key>trustList</key><string>invalid</string>`),
	} {
		if _, err := macTrustDecisionsFromPlist(content); err == nil {
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

func macTrustSettingsPlist(fingerprint, trustSettings string) []byte {
	return macTrustEntryPlist(fingerprint, `<key>trustSettings</key>`+trustSettings)
}

func macTrustEntryPlist(fingerprint, entry string) []byte {
	return macPlistDocument(`<key>trustVersion</key><integer>1</integer><key>trustList</key><dict><key>` + fingerprint + `</key><dict>` + entry + `</dict></dict>`)
}

func macPlistDocument(dictionaryContent string) []byte {
	return []byte(`<?xml version="1.0" encoding="UTF-8"?><!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd"><plist version="1.0"><dict>` + dictionaryContent + `</dict></plist>`)
}

func mustTestCertificate(t *testing.T, commonName string) Certificate {
	t.Helper()
	certificates, err := CertificatesFromBytes(testCertificatePEM(t, true, commonName))
	if err != nil || len(certificates) != 1 {
		t.Fatalf("create test certificate: count=%d error=%v", len(certificates), err)
	}
	return certificates[0]
}
