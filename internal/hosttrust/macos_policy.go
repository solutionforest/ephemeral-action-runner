package hosttrust

import (
	"bytes"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

type macTrustDecision struct {
	Allow bool
	Deny  bool
}

func parseMacKeychainCertificates(content []byte, required bool) ([]*x509.Certificate, error) {
	if len(bytes.TrimSpace(content)) == 0 {
		if required {
			return nil, fmt.Errorf("no certificates found")
		}
		return nil, nil
	}
	return parseCertificates(content)
}

func macSelectedCertificates(baseline, explicitlyAllowed map[string]Certificate, denied map[string]struct{}) []Certificate {
	selected := make(map[string]Certificate, len(baseline)+len(explicitlyAllowed))
	for hash, certificate := range baseline {
		selected[hash] = certificate
	}
	for hash, certificate := range explicitlyAllowed {
		selected[hash] = certificate
	}
	for hash := range denied {
		delete(selected, hash)
	}
	out := make([]Certificate, 0, len(selected))
	for _, certificate := range selected {
		out = append(out, certificate)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SHA256 < out[j].SHA256 })
	return out
}

// macTrustDecisionsFromPlist interprets the external representation produced by
// `security trust-settings-export`. Positive settings are deliberately
// conservative because Ubuntu's global PEM store cannot preserve hostname,
// application, allowed-error, or unknown constraints. Deny always wins.
func macTrustDecisionsFromPlist(content []byte) (map[string]macTrustDecision, error) {
	document, err := parseMacXMLPlist(content)
	if err != nil {
		return nil, err
	}
	if document.kind != macPlistDictionary {
		return nil, fmt.Errorf("macOS trust settings document must be a dictionary")
	}
	version, found := document.dictionary["trustVersion"]
	if !found || version.kind != macPlistInteger || version.integer != 1 {
		return nil, fmt.Errorf("unsupported trustVersion")
	}
	trustList, found := document.dictionary["trustList"]
	if !found || trustList.kind != macPlistDictionary {
		return nil, fmt.Errorf("trustList must be a present object")
	}
	decisions := make(map[string]macTrustDecision, len(trustList.dictionary))
	for fingerprint, entry := range trustList.dictionary {
		fingerprint = strings.ToUpper(strings.TrimSpace(fingerprint))
		decoded, err := hex.DecodeString(fingerprint)
		if err != nil || len(decoded) != 20 {
			return nil, fmt.Errorf("unsupported non-certificate/default trust entry %q", fingerprint)
		}
		if entry.kind != macPlistDictionary {
			return nil, fmt.Errorf("trust entry %s must be a dictionary", fingerprint)
		}
		decision := macTrustDecision{}
		settings, found := entry.dictionary["trustSettings"]
		if !found {
			decision.Allow = true // Apple's persisted schema omits zero usage constraints.
			decisions[fingerprint] = decision
			continue
		}
		if settings.kind != macPlistArray {
			return nil, fmt.Errorf("trust entry %s trustSettings must be an array", fingerprint)
		}
		if len(settings.array) == 0 {
			decision.Allow = true // Apple's documented unconditional trustRoot array.
		}
		for index, setting := range settings.array {
			if setting.kind != macPlistDictionary {
				return nil, fmt.Errorf("trust entry %s setting %d must be an object", fingerprint, index)
			}
			result, err := macTrustSettingResult(setting.dictionary)
			if err != nil {
				return nil, fmt.Errorf("trust entry %s: %w", fingerprint, err)
			}
			if result == 3 {
				decision.Deny = true
				continue
			}
			if (result == 1 || result == 2) && macPositiveTrustSettingIsExportable(setting.dictionary) {
				decision.Allow = true
			}
		}
		if decision.Deny {
			decision.Allow = false
		}
		decisions[fingerprint] = decision
	}
	return decisions, nil
}

func macTrustSettingsExportWasAbsent(err error) bool {
	return err != nil && strings.Contains(err.Error(), "SecTrustSettingsCreateExternalRepresentation: No Trust Settings were found.")
}

func macUserTrustSettingsEnabledFromOutput(content []byte) (bool, error) {
	state := strings.ToLower(strings.TrimSpace(string(content)))
	switch state {
	case "user-level trust settings are enabled":
		return true, nil
	case "user-level trust settings are disabled":
		return false, nil
	default:
		return false, fmt.Errorf("unrecognized macOS user trust settings state %q", strings.TrimSpace(string(content)))
	}
}

func macTrustSettingResult(setting map[string]macPlistValue) (int, error) {
	value, found := setting["kSecTrustSettingsResult"]
	if !found {
		return 1, nil // documented default is trustRoot
	}
	if value.kind != macPlistInteger || value.integer < 0 || value.integer > 4 {
		return 0, fmt.Errorf("invalid kSecTrustSettingsResult")
	}
	return int(value.integer), nil
}

func macPositiveTrustSettingIsExportable(setting map[string]macPlistValue) bool {
	allowedKeys := map[string]struct{}{
		"kSecTrustSettingsPolicy":     {},
		"kSecTrustSettingsPolicyName": {},
		"kSecTrustSettingsKeyUsage":   {},
		"kSecTrustSettingsResult":     {},
	}
	for key := range setting {
		if _, allowed := allowedKeys[key]; !allowed {
			return false
		}
	}
	if policy, hasPolicy := setting["kSecTrustSettingsPolicy"]; hasPolicy {
		name, found := setting["kSecTrustSettingsPolicyName"]
		if policy.kind != macPlistData || !found || name.kind != macPlistString || name.text != "sslServer" {
			return false
		}
	} else if _, hasName := setting["kSecTrustSettingsPolicyName"]; hasName {
		return false
	}
	if value, found := setting["kSecTrustSettingsKeyUsage"]; found {
		if value.kind != macPlistInteger {
			return false
		}
		// Apple key-use constants that can safely represent CA/server-chain
		// validation in a global PEM trust store. Require exact constants;
		// other uses and combined bitmasks remain conditional and are skipped.
		if value.integer != -1 && value.integer != 0xffffffff && value.integer != 0x1 && value.integer != 0x8 {
			return false
		}
	}
	return true
}
