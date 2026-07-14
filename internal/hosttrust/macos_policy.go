package hosttrust

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

type macTrustDecision struct {
	Allow bool
	Deny  bool
}

type macExternalTrustDocument struct {
	TrustVersion int `json:"trustVersion"`
	TrustList    map[string]struct {
		TrustSettings json.RawMessage `json:"trustSettings"`
	} `json:"trustList"`
}

// macTrustDecisionsFromJSON interprets the external representation produced by
// `security trust-settings-export`. Positive settings are deliberately
// conservative because Ubuntu's global PEM store cannot preserve hostname,
// application, allowed-error, or unknown constraints. Deny always wins.
func macTrustDecisionsFromJSON(content []byte) (map[string]macTrustDecision, error) {
	var document macExternalTrustDocument
	if err := json.Unmarshal(content, &document); err != nil {
		return nil, err
	}
	if document.TrustVersion != 1 {
		return nil, fmt.Errorf("unsupported trustVersion %d", document.TrustVersion)
	}
	if document.TrustList == nil {
		return nil, fmt.Errorf("trustList must be a present object")
	}
	decisions := make(map[string]macTrustDecision, len(document.TrustList))
	for fingerprint, entry := range document.TrustList {
		fingerprint = strings.ToUpper(strings.TrimSpace(fingerprint))
		decoded, err := hex.DecodeString(fingerprint)
		if err != nil || len(decoded) != 20 {
			return nil, fmt.Errorf("unsupported non-certificate/default trust entry %q", fingerprint)
		}
		if len(entry.TrustSettings) == 0 || string(entry.TrustSettings) == "null" {
			return nil, fmt.Errorf("trust entry %s has missing or null trustSettings", fingerprint)
		}
		var settings []map[string]any
		if err := json.Unmarshal(entry.TrustSettings, &settings); err != nil || settings == nil {
			return nil, fmt.Errorf("trust entry %s trustSettings must be an array", fingerprint)
		}
		decision := macTrustDecision{}
		if len(settings) == 0 {
			decision.Allow = true // Apple's documented unconditional trustRoot array.
		}
		for index, setting := range settings {
			if setting == nil {
				return nil, fmt.Errorf("trust entry %s setting %d must be an object", fingerprint, index)
			}
			result, err := macTrustSettingResult(setting)
			if err != nil {
				return nil, fmt.Errorf("trust entry %s: %w", fingerprint, err)
			}
			if result == 3 {
				decision.Deny = true
				continue
			}
			if (result == 1 || result == 2) && macPositiveTrustSettingIsExportable(setting) {
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

func macTrustSettingResult(setting map[string]any) (int, error) {
	value, found := setting["kSecTrustSettingsResult"]
	if !found {
		return 1, nil // documented default is trustRoot
	}
	number, ok := value.(float64)
	if !ok || number != float64(int(number)) || number < 0 || number > 4 {
		return 0, fmt.Errorf("invalid kSecTrustSettingsResult %v", value)
	}
	return int(number), nil
}

func macPositiveTrustSettingIsExportable(setting map[string]any) bool {
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
	if _, hasPolicy := setting["kSecTrustSettingsPolicy"]; hasPolicy {
		name, ok := setting["kSecTrustSettingsPolicyName"].(string)
		if !ok || name != "sslServer" {
			return false
		}
	} else if _, hasName := setting["kSecTrustSettingsPolicyName"]; hasName {
		return false
	}
	if value, found := setting["kSecTrustSettingsKeyUsage"]; found {
		number, ok := value.(float64)
		if !ok || number != float64(int64(number)) {
			return false
		}
		// Apple key-use constants that can safely represent CA/server-chain
		// validation in a global PEM trust store. Require exact constants;
		// other uses and combined bitmasks remain conditional and are skipped.
		if number != -1 && number != 0xffffffff && number != 0x1 && number != 0x8 {
			return false
		}
	}
	return true
}
