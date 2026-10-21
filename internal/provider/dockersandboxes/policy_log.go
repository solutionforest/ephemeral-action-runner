package dockersandboxes

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// policyLogDocument is independent of the CLI's legacy and v0.48 wire names.
type policyLogDocument struct {
	Blocked []policyLogRecord
	Allowed []policyLogRecord
}

type policyLogRecord struct {
	Host        string
	SandboxName string
	ProxyType   string
	Rule        *string
	LastSeen    time.Time
	Count       int
}

func parsePolicyLog(data []byte) (policyLogDocument, error) {
	invalid := func() (policyLogDocument, error) {
		return policyLogDocument{}, fmt.Errorf("Docker Sandboxes policy log returned an unsupported json schema")
	}
	if rejectDuplicateJSONKeys(data) != nil {
		return invalid()
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(data, &root) != nil || root == nil {
		return invalid()
	}
	_, hasAllowed := root["allowed"]
	_, hasBlocked := root["blocked"]
	_, hasLegacyAllowed := root["allowed_hosts"]
	_, hasLegacyBlocked := root["blocked_hosts"]
	modern := hasAllowed || hasBlocked
	if modern && (hasLegacyAllowed || hasLegacyBlocked) {
		return invalid()
	}
	allowedKey, blockedKey := "allowed_hosts", "blocked_hosts"
	if modern {
		allowedKey, blockedKey = "allowed", "blocked"
	}
	allowed, err := parsePolicyLogRecords(root[allowedKey], modern)
	if err != nil {
		return invalid()
	}
	blocked, err := parsePolicyLogRecords(root[blockedKey], modern)
	if err != nil {
		return invalid()
	}
	return policyLogDocument{Allowed: allowed, Blocked: blocked}, nil
}

func parsePolicyLogRecords(data []byte, modern bool) ([]policyLogRecord, error) {
	var rows []map[string]json.RawMessage
	if json.Unmarshal(data, &rows) != nil || rows == nil {
		return nil, fmt.Errorf("missing policy log array")
	}
	nameKey, countKey := "vm_name", "count_since"
	otherNameKey, otherCountKey := "sandbox_id", "count"
	if modern {
		nameKey, countKey, otherNameKey, otherCountKey = otherNameKey, otherCountKey, nameKey, countKey
	}
	records := make([]policyLogRecord, 0, len(rows))
	for _, row := range rows {
		if _, mixed := row[otherNameKey]; mixed {
			return nil, fmt.Errorf("mixed policy log identity fields")
		}
		if _, mixed := row[otherCountKey]; mixed {
			return nil, fmt.Errorf("mixed policy log count fields")
		}
		var record policyLogRecord
		for key, target := range map[string]*string{"host": &record.Host, nameKey: &record.SandboxName, "proxy_type": &record.ProxyType} {
			value, err := requiredJSONString(row, key)
			if err != nil || strings.TrimSpace(value) != value || strings.ContainsRune(value, 0) {
				return nil, fmt.Errorf("invalid policy log identity or route")
			}
			*target = value
		}
		if json.Unmarshal(row["last_seen"], &record.LastSeen) != nil || record.LastSeen.IsZero() {
			return nil, fmt.Errorf("invalid policy log timestamp")
		}
		var count *int
		if json.Unmarshal(row[countKey], &count) != nil || count == nil {
			return nil, fmt.Errorf("invalid policy log count")
		}
		record.Count = *count
		if raw, present := row["rule"]; present {
			if json.Unmarshal(raw, &record.Rule) != nil {
				return nil, fmt.Errorf("invalid policy log rule")
			}
		} else if modern {
			// v0.48+ omits an empty matched rule. This is unattributed
			// evidence, accepted only with the existing bound causal proof
			// and unchanged exact policy inventory. Null stays unknown.
			empty := ""
			record.Rule = &empty
		}
		// Local v0.48+ sandbox_id contains the sandbox NAME, as captured
		// from v0.49.1. Never treat it as an interchangeable immutable ID.
		records = append(records, record)
	}
	return records, nil
}
