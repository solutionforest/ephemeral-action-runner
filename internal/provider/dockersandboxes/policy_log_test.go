package dockersandboxes

import (
	"os"
	"strings"
	"testing"
)

func TestPolicyLogCapturedCurrentSchema(t *testing.T) {
	data, err := os.ReadFile("testdata/sbx-v0.49.1-policy-log.json")
	if err != nil {
		t.Fatal(err)
	}
	document, err := parsePolicyLog(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(document.Allowed) != 1 || len(document.Blocked) != 1 {
		t.Fatalf("captured policy log = %#v", document)
	}
	allowed := document.Allowed[0]
	if allowed.SandboxName != "joe-mac.sbx-bff676-20261021-183206-002" || allowed.Host != "broker.actions.githubusercontent.com:443" || allowed.Count != 1 || allowed.LastSeen.IsZero() || allowed.Rule == nil || *allowed.Rule != "" {
		t.Fatalf("normalized allowed record = %#v", allowed)
	}
	if document.Blocked[0].Rule == nil || *document.Blocked[0].Rule != "<dns proxy policy>" {
		t.Fatalf("blocked rule attribution was lost: %#v", document.Blocked[0])
	}
}

func TestPolicyLogToleratesAdditionsAndRejectsAmbiguousEvidence(t *testing.T) {
	valid := `{"allowed":[{"sandbox_id":"sandbox-one","host":"localhost:43123","proxy_type":"transparent","count":1,"last_seen":"2026-10-21T10:00:00Z"}],"blocked":[]}`
	additive := strings.Replace(valid, `"blocked":[]`, `"blocked":[],"updated_at":"2026-10-21T10:00:01Z","metadata":{"new":true}`, 1)
	additive = strings.Replace(additive, `"count":1`, `"count":1,"new_field":true`, 1)
	for _, fixture := range []string{valid, additive, `{"allowed":[],"blocked":[]}`, `{"allowed_hosts":[],"blocked_hosts":[]}`} {
		if _, err := parsePolicyLog([]byte(fixture)); err != nil {
			t.Fatalf("supported policy log rejected: %v", err)
		}
	}
	for name, fixture := range map[string]string{
		"null":             `null`,
		"missing envelope": `{}`,
		"missing blocked":  `{"allowed":[]}`,
		"null blocked":     strings.Replace(valid, `"blocked":[]`, `"blocked":null`, 1),
		"mixed envelope":   strings.Replace(valid, `"blocked":[]`, `"blocked":[],"blocked_hosts":[]`, 1),
		"duplicate key":    strings.Replace(valid, `"count":1`, `"count":1,"count":2`, 1),
		"trailing output":  valid + `{}`,
		"mixed identity":   strings.Replace(valid, `"count":1`, `"count":1,"vm_name":"sandbox-two"`, 1),
		"mixed count":      strings.Replace(valid, `"count":1`, `"count":1,"count_since":2`, 1),
		"missing identity": strings.Replace(valid, `"sandbox_id":"sandbox-one",`, "", 1),
		"missing time":     strings.Replace(valid, `"last_seen":"2026-10-21T10:00:00Z"`, `"other":"value"`, 1),
		"null count":       strings.Replace(valid, `"count":1`, `"count":null`, 1),
		"fractional count": strings.Replace(valid, `"count":1`, `"count":0.5`, 1),
		"invalid rule":     strings.Replace(valid, `"count":1`, `"count":1,"rule":true`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parsePolicyLog([]byte(fixture)); err == nil {
				t.Fatal("ambiguous policy log accepted")
			}
		})
	}
}
