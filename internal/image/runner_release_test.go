package image

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestApplyActionsRunnerReleaseSelectsExactArchitectureAndDigest(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	release := actionsRunnerRelease{
		TagName: "v2.332.0",
		Assets: []actionsRunnerAsset{
			{Name: "actions-runner-linux-x64-2.332.0.tar.gz", BrowserDownloadURL: "https://example.invalid/x64", Digest: digest},
			{Name: "actions-runner-linux-arm64-2.332.0.tar.gz", BrowserDownloadURL: "https://example.invalid/arm64", Digest: "sha256:" + strings.Repeat("b", 64)},
		},
	}
	manifest, err := applyActionsRunnerRelease(Manifest{}, "latest", "x64", release)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.RunnerSelector != "latest" || manifest.RunnerVersion != "2.332.0" {
		t.Fatalf("runner identity = selector %q version %q", manifest.RunnerSelector, manifest.RunnerVersion)
	}
	if manifest.RunnerAssetName != "actions-runner-linux-x64-2.332.0.tar.gz" || manifest.RunnerAssetURL != "https://example.invalid/x64" || manifest.RunnerAssetDigest != digest {
		t.Fatalf("selected asset = %+v", manifest)
	}
}

func TestResolveActionsRunnerFromAPISucceeds(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/latest" {
			t.Fatalf("request path = %q", request.URL.Path)
		}
		if request.Header.Get("X-GitHub-Api-Version") != "2022-11-28" {
			t.Fatalf("GitHub API version = %q", request.Header.Get("X-GitHub-Api-Version"))
		}
		_ = json.NewEncoder(response).Encode(actionsRunnerRelease{TagName: "v2.332.0", Assets: []actionsRunnerAsset{{Name: "actions-runner-linux-x64-2.332.0.tar.gz", BrowserDownloadURL: "https://example.invalid/x64", Digest: digest}}})
	}))
	defer server.Close()

	manifest, err := resolveActionsRunnerFromAPI(context.Background(), Manifest{}, "latest", "x64", server.Client(), server.URL+"/latest")
	if err != nil {
		t.Fatal(err)
	}
	if manifest.RunnerVersion != "2.332.0" || manifest.RunnerAssetDigest != digest {
		t.Fatalf("resolved manifest = %+v", manifest)
	}
}

func TestActionsRunnerReleaseRateLimitFailureUsesExactFallback(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("X-RateLimit-Remaining", "0")
		response.Header().Set("X-RateLimit-Reset", "1785600000")
		response.Header().Set("X-GitHub-Request-Id", "test-request-id")
		response.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()

	_, err := resolveActionsRunnerFromAPI(context.Background(), Manifest{}, "latest", "x64", server.Client(), server.URL+"/latest")
	var failure *actionsRunnerReleaseFailure
	if !errors.As(err, &failure) || !failure.fallbackEligible {
		t.Fatalf("failure = %#v, want fallback-eligible rate-limit failure", err)
	}
	for _, part := range []string{"HTTP 403", "x-ratelimit-remaining=0", "x-ratelimit-reset=1785600000", "x-github-request-id=test-request-id"} {
		if !strings.Contains(failure.diagnostic, part) {
			t.Fatalf("diagnostic %q omitted %q", failure.diagnostic, part)
		}
	}

	manifest, fallbackErr := resolveActionsRunnerFallback(filepath.Join("..", ".."), Manifest{}, "latest", "x64")
	if fallbackErr != nil {
		t.Fatal(fallbackErr)
	}
	if manifest.RunnerVersion != "2.336.0" || manifest.RunnerAssetName != "actions-runner-linux-x64-2.336.0.tar.gz" || manifest.RunnerAssetDigest != "sha256:04cf0be1aff4c3ec3554466c39124ca250e3effd8873bb7e8d68535aa9505d5d" {
		t.Fatalf("fallback manifest = %+v", manifest)
	}
}

func TestActionsRunnerFallbackRejectsUnusableLockAndSelectorMismatch(t *testing.T) {
	projectRoot := t.TempDir()
	lockPath := filepath.Join(projectRoot, actionsRunnerFallbackReleasePath)
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lockPath, []byte(`{"tag_name":`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveActionsRunnerFallback(projectRoot, Manifest{}, "latest", "x64"); err == nil || !strings.Contains(err.Error(), "parse") {
		t.Fatalf("malformed-lock error = %v", err)
	}
	if err := os.WriteFile(lockPath, []byte(`{"tag_name":"v2.336.0","assets":[{"name":"actions-runner-linux-x64-2.336.0.tar.gz","browser_download_url":"https://example.invalid/runner","digest":"sha256:04cf0be1aff4c3ec3554466c39124ca250e3effd8873bb7e8d68535aa9505d5d"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveActionsRunnerFallback(projectRoot, Manifest{}, "latest", "x64"); err == nil || !strings.Contains(err.Error(), "canonical GitHub release URL") {
		t.Fatalf("unusable-lock error = %v", err)
	}

	writeTestActionsRunnerFallbackLock(t, lockPath)
	if _, err := resolveActionsRunnerFallback(projectRoot, Manifest{}, "2.335.0", "x64"); err == nil || !strings.Contains(err.Error(), "returned version") {
		t.Fatalf("selector-mismatch error = %v", err)
	}
}

func TestActionsRunnerReleaseUnauthorizedFailureDoesNotUseFallback(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	_, err := resolveActionsRunnerFromAPI(context.Background(), Manifest{}, "latest", "x64", server.Client(), server.URL+"/latest")
	var failure *actionsRunnerReleaseFailure
	if !errors.As(err, &failure) {
		t.Fatalf("failure = %v, want classified HTTP failure", err)
	}
	if failure.fallbackEligible {
		t.Fatalf("unauthorized failure unexpectedly allows fallback: %+v", failure)
	}
}

func writeTestActionsRunnerFallbackLock(t *testing.T, lockPath string) {
	t.Helper()
	content := `{"tag_name":"v2.336.0","assets":[{"name":"actions-runner-linux-x64-2.336.0.tar.gz","browser_download_url":"https://github.com/actions/runner/releases/download/v2.336.0/actions-runner-linux-x64-2.336.0.tar.gz","digest":"sha256:04cf0be1aff4c3ec3554466c39124ca250e3effd8873bb7e8d68535aa9505d5d"}]}`
	if err := os.WriteFile(lockPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestApplyActionsRunnerReleaseRejectsWrongVersionAndMissingIntegrity(t *testing.T) {
	release := actionsRunnerRelease{
		TagName: "v2.332.0",
		Assets: []actionsRunnerAsset{{
			Name:               "actions-runner-linux-x64-2.332.0.tar.gz",
			BrowserDownloadURL: "https://example.invalid/x64",
		}},
	}
	if _, err := applyActionsRunnerRelease(Manifest{}, "2.331.0", "x64", release); err == nil || !strings.Contains(err.Error(), "returned version") {
		t.Fatalf("wrong-version error = %v", err)
	}
	if _, err := applyActionsRunnerRelease(Manifest{}, "latest", "x64", release); err == nil || !strings.Contains(err.Error(), "valid SHA-256") {
		t.Fatalf("missing-integrity error = %v", err)
	}
}

func TestActionsRunnerPlatformUsesConfiguredSourceArchitecture(t *testing.T) {
	for _, test := range []struct {
		platform string
		want     string
	}{
		{"linux/amd64", "linux/amd64"},
		{"linux/arm64", "linux/arm64"},
	} {
		got, err := actionsRunnerPlatform(Manifest{SourcePlatform: test.platform}, "docker-container")
		if err != nil {
			t.Fatal(err)
		}
		if got != test.want {
			t.Fatalf("actionsRunnerPlatform(%q) = %q, want %q", test.platform, got, test.want)
		}
	}
}

func TestRunnerInstallScriptRequiresVerifiedLocalPackage(t *testing.T) {
	content, err := os.ReadFile(filepath.Join("..", "..", "scripts", "guest", "ubuntu", "install-runner.sh"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(content)
	for _, forbidden := range []string{"releases/latest", "api.github.com", "curl ", "wget "} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("install-runner.sh still performs guest-side remote resolution: found %q", forbidden)
		}
	}
	for _, required := range []string{"<verified-package-path>", "sha256sum --check", "sudo -u runner -H ./bin/Runner.Listener --version"} {
		if !strings.Contains(text, required) {
			t.Fatalf("install-runner.sh omitted %q", required)
		}
	}
}
