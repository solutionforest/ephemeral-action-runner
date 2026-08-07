package image

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/solutionforest/ephemeral-action-runner/internal/dependency"
)

func TestVerifiedDownloadResumesAndPublishesOnlyLockedContent(t *testing.T) {
	content := []byte(strings.Repeat("verified-actions-runner-content\n", 128))
	sum := sha256.Sum256(content)
	var requestedRange string
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requestedRange = request.Header.Get("Range")
		start := 0
		if requestedRange != "" {
			if _, err := fmt.Sscanf(requestedRange, "bytes=%d-", &start); err != nil {
				t.Errorf("invalid Range header %q: %v", requestedRange, err)
				response.WriteHeader(http.StatusBadRequest)
				return
			}
			response.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(content)-1, len(content)))
			response.WriteHeader(http.StatusPartialContent)
		}
		_, _ = response.Write(content[start:])
	}))
	defer server.Close()

	destination := filepath.Join(t.TempDir(), "inputs", "actions-runner.tar.gz")
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		t.Fatal(err)
	}
	partialBytes := 197
	if err := os.WriteFile(destination+".partial", content[:partialBytes], 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifiedDownload(context.Background(), server.Client(), server.URL, destination, hex.EncodeToString(sum[:]), 0o600); err != nil {
		t.Fatal(err)
	}
	if requestedRange != "bytes="+strconv.Itoa(partialBytes)+"-" {
		t.Fatalf("Range = %q, want resume from %d", requestedRange, partialBytes)
	}
	got, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(content) {
		t.Fatal("verified download content does not match")
	}
	if _, err := os.Stat(destination + ".partial"); !os.IsNotExist(err) {
		t.Fatalf("partial download still exists after publication: %v", err)
	}
}

func TestVerifiedDownloadReturnsTypedTransientHTTPFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	destination := filepath.Join(t.TempDir(), "asset")
	err := verifiedDownload(context.Background(), server.Client(), server.URL, destination, strings.Repeat("0", 64), 0o600)
	if !dependency.IsRetryable(err) {
		t.Fatalf("HTTP 503 download error was not a typed retryable failure: %v", err)
	}
	if _, statErr := os.Stat(destination); !os.IsNotExist(statErr) {
		t.Fatalf("failed download published a destination: %v", statErr)
	}
}

func TestVerifiedDownloadRetainsFailedPartialAndDoesNotPublish(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		_, _ = response.Write([]byte("wrong content"))
	}))
	defer server.Close()
	destination := filepath.Join(t.TempDir(), "tini")
	err := verifiedDownload(context.Background(), server.Client(), server.URL, destination, strings.Repeat("0", 64), 0o700)
	if err == nil || !strings.Contains(err.Error(), "failed locked SHA-256 verification") {
		t.Fatalf("checksum failure = %v", err)
	}
	if _, err := os.Stat(destination); !os.IsNotExist(err) {
		t.Fatalf("unverified destination was published: %v", err)
	}
	if _, err := os.Stat(destination + ".partial"); err != nil {
		t.Fatalf("failed partial was not retained for diagnosis/resume: %v", err)
	}
}
