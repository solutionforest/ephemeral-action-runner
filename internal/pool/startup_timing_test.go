package pool

import (
	"errors"
	"strings"
	"testing"
)

func TestSanitizeTimingErrorRedactsSecretAssignments(t *testing.T) {
	const sentinel = "timing-secret-sentinel"
	got := sanitizeTimingError(errors.New("configure failed RUNNER_TOKEN=" + sentinel + " PASSWORD=" + sentinel))
	if strings.Contains(got, sentinel) {
		t.Fatalf("sanitizeTimingError leaked sentinel: %q", got)
	}
	if !strings.Contains(got, "RUNNER_TOKEN=[REDACTED]") || !strings.Contains(got, "PASSWORD=[REDACTED]") {
		t.Fatalf("sanitizeTimingError did not retain sanitized keys: %q", got)
	}
}
