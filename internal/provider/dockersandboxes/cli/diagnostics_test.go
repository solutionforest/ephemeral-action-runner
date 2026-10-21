package cli

import (
	"os"
	"strings"
	"testing"
)

func TestParseDiagnosticsAcceptsOptionalTextAndValidatesResults(t *testing.T) {
	minimal := `{"version":"1.0","checks":[{"name":"daemon","status":"PASS"},{"name":"update","status":"warn"},{"name":"optional","status":"skip"},{"name":"runtime","status":"fail"}],"summary":{"pass":1,"warn":1,"fail":1,"skip":1}}`
	for _, fixture := range []string{
		minimal,
		strings.Replace(minimal, `"name":"daemon"`, `"name":"daemon","message":"healthy","detail":null,"hint":"","extra":true`, 1),
		strings.Replace(minimal, `"summary":{`, `"metadata":{"source":"sbx"},"summary":{"metadata":{"source":"sbx"},"description":"ok",`, 1),
	} {
		report, err := ParseDiagnostics([]byte(fixture))
		if err != nil || report != (DiagnosticSummary{Passed: 1, Warned: 1, Failed: 1, Skipped: 1}) {
			t.Fatalf("diagnostic results = %#v, %v", report, err)
		}
	}
	for name, fixture := range map[string]string{
		"future version": strings.Replace(minimal, `"1.0"`, `"2.0"`, 1),
		"unknown status": strings.Replace(minimal, `"PASS"`, `"healthy"`, 1),
		"missing status": strings.Replace(minimal, `,"status":"PASS"`, "", 1),
		"blank name":     strings.Replace(minimal, `"daemon"`, `" "`, 1),
		"summary drift":  strings.Replace(minimal, `"pass":1`, `"pass":2`, 1),
		"null count":     strings.Replace(minimal, `"pass":1`, `"pass":null`, 1),
		"missing count":  strings.Replace(minimal, `"pass":1,`, "", 1),
		"negative count": strings.Replace(minimal, `"pass":1`, `"pass":-1`, 1),
		"duplicate key":  strings.Replace(minimal, `"status":"fail"`, `"status":"fail","status":"pass"`, 1),
		"case alias":     strings.Replace(minimal, `"status":"PASS"`, `"status":"fail","Status":"pass"`, 1),
		"invalid text":   strings.Replace(minimal, `"name":"daemon"`, `"name":"daemon","detail":[]`, 1),
		"trailing data":  minimal + `{}`,
		"empty checks":   `{"version":"1.0","checks":[],"summary":{"pass":0,"warn":0,"fail":0,"skip":0}}`,
		"null check":     `{"version":"1.0","checks":[null],"summary":{"pass":0,"warn":0,"fail":0,"skip":0}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseDiagnostics([]byte(fixture)); err == nil {
				t.Fatal("invalid diagnostic evidence accepted")
			}
		})
	}
}

func TestParseCapturedDiagnostics(t *testing.T) {
	data, err := os.ReadFile("testdata/sbx-v0.49.1-diagnose.json")
	if err != nil {
		t.Fatal(err)
	}
	report, err := ParseDiagnostics(data)
	if err != nil || report != (DiagnosticSummary{Passed: 15, Warned: 1, Skipped: 1}) {
		t.Fatalf("captured diagnostic results = %#v, %v", report, err)
	}
}
