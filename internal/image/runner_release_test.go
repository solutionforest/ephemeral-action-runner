package image

import (
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
	for _, required := range []string{"<verified-package-path>", "sha256sum --check", "Runner.Listener --version"} {
		if !strings.Contains(text, required) {
			t.Fatalf("install-runner.sh omitted %q", required)
		}
	}
}
