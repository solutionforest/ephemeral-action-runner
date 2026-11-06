package cli

import (
	"encoding/json"
	"fmt"
	"strings"
)

type DiagnosticSummary struct {
	Passed   int
	Warned   int
	Failed   int
	Skipped  int
	Failures []DiagnosticFailure
}

type DiagnosticFailure struct {
	Name    string
	Message string
	Hint    string
}

// ParseDiagnostics validates the versioned report shared by startup and
// promotion. Descriptive text is optional; check results and their matching
// summary remain mandatory even when a CLI release omits presentation fields.
func ParseDiagnostics(data []byte) (DiagnosticSummary, error) {
	var report map[string]json.RawMessage
	if RejectDuplicateJSONKeys(data) != nil || json.Unmarshal(data, &report) != nil {
		return DiagnosticSummary{}, fmt.Errorf("docker sandboxes diagnostics returned an unsupported json schema")
	}
	var version string
	if json.Unmarshal(report["version"], &version) != nil || version != "1.0" {
		return DiagnosticSummary{}, fmt.Errorf("docker sandboxes diagnostics returned an unsupported schema version")
	}
	var checks []map[string]json.RawMessage
	if json.Unmarshal(report["checks"], &checks) != nil || len(checks) == 0 {
		return DiagnosticSummary{}, fmt.Errorf("docker sandboxes diagnostics omitted its checks")
	}
	counts := map[string]int{"pass": 0, "warn": 0, "fail": 0, "skip": 0}
	var failures []DiagnosticFailure
	for _, check := range checks {
		var name, status string
		if json.Unmarshal(check["name"], &name) != nil || strings.TrimSpace(name) == "" || strings.ContainsRune(name, 0) {
			return DiagnosticSummary{}, fmt.Errorf("docker sandboxes diagnostics returned an unsupported check schema")
		}
		for _, field := range []string{"message", "detail", "hint"} {
			if raw, present := check[field]; present {
				var text *string
				if json.Unmarshal(raw, &text) != nil {
					return DiagnosticSummary{}, fmt.Errorf("docker sandboxes diagnostics returned an unsupported check schema")
				}
			}
		}
		if json.Unmarshal(check["status"], &status) != nil {
			return DiagnosticSummary{}, fmt.Errorf("docker sandboxes diagnostics returned an unsupported check schema")
		}
		status = strings.ToLower(status)
		if _, known := counts[status]; !known {
			return DiagnosticSummary{}, fmt.Errorf("docker sandboxes diagnostics returned an unknown check status")
		}
		counts[status]++
		if status == "fail" {
			failure := DiagnosticFailure{Name: name}
			// The optional presentation fields were validated above. Omitted or
			// null strings remain empty; verbose detail stays in the full report.
			_ = json.Unmarshal(check["message"], &failure.Message)
			_ = json.Unmarshal(check["hint"], &failure.Hint)
			failures = append(failures, failure)
		}
	}
	var summary map[string]json.RawMessage
	if json.Unmarshal(report["summary"], &summary) != nil {
		return DiagnosticSummary{}, fmt.Errorf("docker sandboxes diagnostics summary did not match its checks")
	}
	for key, expected := range counts {
		var count *int
		if json.Unmarshal(summary[key], &count) != nil || count == nil || *count != expected {
			return DiagnosticSummary{}, fmt.Errorf("docker sandboxes diagnostics summary did not match its checks")
		}
	}
	return DiagnosticSummary{Passed: counts["pass"], Warned: counts["warn"], Failed: counts["fail"], Skipped: counts["skip"], Failures: failures}, nil
}
