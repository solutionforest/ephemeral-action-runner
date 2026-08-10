package image

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRunProgressOperationReportsHeartbeatAndCompletion(t *testing.T) {
	var mu sync.Mutex
	var messages []string
	heartbeatObserved := make(chan struct{})
	var heartbeatOnce sync.Once
	logf := func(format string, args ...any) {
		message := fmt.Sprintf(format, args...)
		mu.Lock()
		messages = append(messages, message)
		mu.Unlock()
		if strings.Contains(message, "still working") {
			heartbeatOnce.Do(func() { close(heartbeatObserved) })
		}
	}

	if err := runProgressOperation("Template archive export", 5*time.Millisecond, logf, func() string {
		return "archive written 12.00 GiB"
	}, func() error {
		<-heartbeatObserved
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	output := strings.Join(messages, "")
	mu.Unlock()
	for _, wanted := range []string{
		"Template archive export started",
		"Template archive export: still working; elapsed",
		"archive written 12.00 GiB",
		"Template archive export complete; elapsed",
	} {
		if !strings.Contains(output, wanted) {
			t.Fatalf("progress output omitted %q:\n%s", wanted, output)
		}
	}
}

func TestRunProgressOperationDoesNotReportFailedOperationAsComplete(t *testing.T) {
	var mu sync.Mutex
	var output strings.Builder
	logf := func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		fmt.Fprintf(&output, format, args...)
	}
	expected := errors.New("failed")
	err := runProgressOperation("Template import", 0, logf, nil, func() error {
		return expected
	})
	if !errors.Is(err, expected) {
		t.Fatalf("error = %v, want %v", err, expected)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Contains(output.String(), "Template import complete") {
		t.Fatalf("failed operation was reported complete:\n%s", output.String())
	}
}

func TestImageOperationHeartbeatDefaultIsFiveSeconds(t *testing.T) {
	if got, want := imageOperationHeartbeatInterval, 5*time.Second; got != want {
		t.Fatalf("image-operation progress heartbeat = %s, want %s", got, want)
	}
}

func TestRegularFileSizeDetailUsesHumanReadableSize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.tar")
	if err := os.WriteFile(path, make([]byte, 1536), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, want := regularFileSizeDetail(path, "archive written")(), "archive written 1.50 KiB"; got != want {
		t.Fatalf("detail = %q, want %q", got, want)
	}
}
