package provider

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestRedactTextRemovesExactValuesAndSecretAssignments(t *testing.T) {
	const secret = "sentinel-registration-token"
	input := "docker exec -e RUNNER_TOKEN=" + secret + " PASSWORD=hunter2 normal=value exact=" + secret
	got := RedactText(input, secret)
	if strings.Contains(got, secret) || strings.Contains(got, "hunter2") {
		t.Fatalf("RedactText() leaked a secret: %q", got)
	}
	for _, want := range []string{"RUNNER_TOKEN=[REDACTED]", "PASSWORD=[REDACTED]", "exact=[REDACTED]"} {
		if !strings.Contains(got, want) {
			t.Fatalf("RedactText() = %q, want %q", got, want)
		}
	}
}

func TestBufferSensitiveSinksRedactsBeforeWriting(t *testing.T) {
	const secret = "sentinel-registration-token"
	var stdout, stderr bytes.Buffer
	bufferedStdout, bufferedStderr, flush := BufferSensitiveSinks([]string{secret}, &stdout, &stderr)
	_, _ = bufferedStdout.Write([]byte("out " + secret))
	_, _ = bufferedStderr.Write([]byte("RUNNER_TOKEN=" + secret))
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatal("sensitive output streamed before redaction")
	}
	if err := flush(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stdout.String()+stderr.String(), secret) {
		t.Fatalf("sensitive sinks leaked secret: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestBufferSensitiveSinksRedactsOrdinaryOutputAcrossChunks(t *testing.T) {
	var stdout bytes.Buffer
	bufferedStdout, _, flush := BufferSensitiveSinks(nil, &stdout, nil)
	_, _ = bufferedStdout.Write([]byte("ordinary line\nPASS"))
	if got := stdout.String(); got != "ordinary line\n" {
		t.Fatalf("ordinary complete-line streaming = %q", got)
	}
	_, _ = bufferedStdout.Write([]byte("WORD=sentinel\n"))
	if err := flush(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stdout.String(), "sentinel") {
		t.Fatalf("ordinary sink leaked chunked secret assignment: %q", stdout.String())
	}
}

func TestFinishSensitiveExecutionRedactsResultAndError(t *testing.T) {
	const secret = "sentinel-registration-token"
	sentinel := errors.New("sentinel cause")
	result, err := FinishSensitiveExecution(
		ExecResult{Stdout: secret, Stderr: "TOKEN=" + secret},
		fmt.Errorf("command failed with %s: %w", secret, sentinel),
		nil,
		[]string{secret},
	)
	combined := result.Stdout + result.Stderr + err.Error()
	if strings.Contains(combined, secret) {
		t.Fatalf("sensitive execution outcome leaked secret: %q", combined)
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("redacted error no longer wraps original cause: %v", err)
	}
}
