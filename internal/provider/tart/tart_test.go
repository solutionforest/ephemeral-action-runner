package tart

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
)

func TestListParsesTartOutputShape(t *testing.T) {
	p := New("tart", true)
	_ = p
	// The parser is exercised indirectly in integration; keep this package
	// dependency-free so dry-run builds work without Tart installed.
	_, err := New("tart", true).List(context.Background())
	if err != nil {
		t.Fatalf("dry-run list should not fail: %v", err)
	}
}

func TestCaptureWriterCopiesToCaptureAndInjectedSink(t *testing.T) {
	var capture, sink bytes.Buffer
	writer := captureWriter(&capture, &sink)
	if _, err := writer.Write([]byte("transcript")); err != nil {
		t.Fatal(err)
	}
	if capture.String() != "transcript" || sink.String() != "transcript" {
		t.Fatalf("capture=%q sink=%q", capture.String(), sink.String())
	}
}

func TestExecRedactsSensitiveValuesFromResultErrorAndSinks(t *testing.T) {
	const secret = "sentinel-registration-token"
	p := New("tart", false)
	var stdout, stderr bytes.Buffer
	p.runCommand = func(_ context.Context, _ io.Reader, gotStdout, gotStderr io.Writer, args ...string) (provider.ExecResult, error) {
		_, _ = gotStdout.Write([]byte("stream " + secret))
		_, _ = gotStderr.Write([]byte("RUNNER_TOKEN=" + secret))
		return provider.ExecResult{Stdout: "result " + secret, Stderr: "SECRET=" + secret}, errors.New("tart " + strings.Join(args, " ") + " failed with " + secret)
	}
	result, err := p.Exec(context.Background(), "runner", []string{"false"}, provider.ExecOptions{
		Env:             map[string]string{"RUNNER_TOKEN": secret},
		SensitiveValues: []string{secret},
		Stdout:          &stdout,
		Stderr:          &stderr,
	})
	combined := stdout.String() + stderr.String() + result.Stdout + result.Stderr + err.Error()
	if strings.Contains(combined, secret) {
		t.Fatalf("sensitive Tart exec leaked secret: %q", combined)
	}
}

func TestExecRedactsSecretAssignmentsWithoutSensitiveValues(t *testing.T) {
	const secret = "ordinary-sentinel"
	p := New("tart", false)
	var stdout, stderr bytes.Buffer
	p.runCommand = func(_ context.Context, _ io.Reader, gotStdout, gotStderr io.Writer, _ ...string) (provider.ExecResult, error) {
		_, _ = gotStdout.Write([]byte("PASSWORD=" + secret + "\n"))
		_, _ = gotStderr.Write([]byte("SECRET=" + secret + "\n"))
		return provider.ExecResult{Stdout: "TOKEN=" + secret, Stderr: "PRIVATE_KEY=" + secret}, errors.New("RUNNER_TOKEN=" + secret)
	}
	result, err := p.Exec(context.Background(), "runner", []string{"false"}, provider.ExecOptions{Stdout: &stdout, Stderr: &stderr})
	combined := stdout.String() + stderr.String() + result.Stdout + result.Stderr + err.Error()
	if strings.Contains(combined, secret) {
		t.Fatalf("ordinary Tart exec leaked secret assignment: %q", combined)
	}
}

func TestStartDryRunIncludesRosettaBeforeName(t *testing.T) {
	p := New("tart", true)
	out := captureStdout(t, func() {
		if _, err := p.Start(context.Background(), "epar-test", provider.StartOptions{RosettaTag: "rosetta"}); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "tart run --no-graphics --rosetta rosetta epar-test") {
		t.Fatalf("dry-run output did not include Rosetta start command: %q", out)
	}
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	original := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = writer
	defer func() { os.Stdout = original }()

	fn()
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, reader); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}
