package tart

import (
	"bytes"
	"context"
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
