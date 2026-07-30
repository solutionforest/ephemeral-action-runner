package image

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
)

const defaultActionsRunnerReleaseAPI = "https://api.github.com/repos/actions/runner/releases"

type actionsRunnerRelease struct {
	TagName string               `json:"tag_name"`
	Assets  []actionsRunnerAsset `json:"assets"`
}

type actionsRunnerAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Digest             string `json:"digest"`
}

func normalizedRunnerSelector(value string) string {
	value = strings.TrimSpace(strings.TrimPrefix(value, "v"))
	if value == "" {
		return "latest"
	}
	return value
}

func (m *Coordinator) resolveActionsRunner(ctx context.Context, manifest Manifest) (Manifest, error) {
	selector := normalizedRunnerSelector(manifest.RunnerSelector)
	platform, err := actionsRunnerPlatform(manifest, m.Config.Provider.Type)
	if err != nil {
		return manifest, err
	}
	architecture := ""
	switch platform {
	case "linux/amd64":
		architecture = "x64"
	case "linux/arm64":
		architecture = "arm64"
	default:
		return manifest, fmt.Errorf("GitHub Actions runner packages are not configured for %s", platform)
	}
	endpoint := actionsRunnerReleaseAPI()
	if selector == "latest" {
		endpoint += "/latest"
	} else {
		endpoint += "/tags/v" + selector
	}
	buildTrust, err := m.resolveBuildTrust(ctx)
	if err != nil {
		return manifest, err
	}
	client, err := buildTrustHTTPClient(buildTrust)
	if err != nil {
		return manifest, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return manifest, err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	request.Header.Set("User-Agent", "ephemeral-action-runner")
	response, err := client.Do(request)
	if err != nil {
		return manifest, fmt.Errorf("resolve GitHub Actions runner %s: %w", selector, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return manifest, fmt.Errorf("resolve GitHub Actions runner %s: GitHub returned HTTP %d", selector, response.StatusCode)
	}
	var release actionsRunnerRelease
	if err := json.NewDecoder(response.Body).Decode(&release); err != nil {
		return manifest, fmt.Errorf("parse GitHub Actions runner release: %w", err)
	}
	return applyActionsRunnerRelease(manifest, selector, architecture, release)
}

func applyActionsRunnerRelease(manifest Manifest, selector, architecture string, release actionsRunnerRelease) (Manifest, error) {
	version := strings.TrimPrefix(strings.TrimSpace(release.TagName), "v")
	if version == "" {
		return manifest, fmt.Errorf("GitHub Actions runner release omitted tag_name")
	}
	if selector != "latest" && selector != version {
		return manifest, fmt.Errorf("GitHub Actions runner release returned version %q for selector %q", version, selector)
	}
	assetName := fmt.Sprintf("actions-runner-linux-%s-%s.tar.gz", architecture, version)
	var selected *actionsRunnerAsset
	for index := range release.Assets {
		if release.Assets[index].Name == assetName {
			selected = &release.Assets[index]
			break
		}
	}
	if selected == nil {
		return manifest, fmt.Errorf("GitHub Actions runner %s does not provide %s", version, assetName)
	}
	digest := strings.ToLower(strings.TrimSpace(selected.Digest))
	if !validSHA256(digest) {
		return manifest, fmt.Errorf("GitHub Actions runner asset %s omitted a valid SHA-256 digest", assetName)
	}
	if strings.TrimSpace(selected.BrowserDownloadURL) == "" {
		return manifest, fmt.Errorf("GitHub Actions runner asset %s omitted its download URL", assetName)
	}
	manifest.RunnerSelector = selector
	manifest.RunnerVersion = version
	manifest.RunnerAssetName = assetName
	manifest.RunnerAssetURL = selected.BrowserDownloadURL
	manifest.RunnerAssetDigest = digest
	return manifest, nil
}

func actionsRunnerPlatform(manifest Manifest, providerType string) (string, error) {
	for _, candidate := range []string{manifest.SourcePlatform, manifest.ProviderPlatform} {
		if platform, ok := NormalizedDockerPlatform(candidate, "linux"); ok {
			return platform.OS + "/" + platform.Architecture, nil
		}
	}
	if providerType == "tart" {
		return "linux/arm64", nil
	}
	architecture := runtime.GOARCH
	if architecture == "386" {
		architecture = "amd64"
	}
	if architecture != "amd64" && architecture != "arm64" {
		return "", fmt.Errorf("cannot select a GitHub Actions runner package for host architecture %s", runtime.GOARCH)
	}
	return "linux/" + architecture, nil
}

func actionsRunnerReleaseAPI() string {
	if value := strings.TrimSpace(os.Getenv("EPAR_TEST_ACTIONS_RUNNER_RELEASE_API")); value != "" {
		return strings.TrimRight(value, "/")
	}
	return defaultActionsRunnerReleaseAPI
}

func (m *Coordinator) acquireActionsRunner(ctx context.Context, manifest Manifest) (string, error) {
	if manifest.RunnerVersion == "" || manifest.RunnerAssetURL == "" || !validSHA256(manifest.RunnerAssetDigest) {
		return "", fmt.Errorf("resolved Actions runner identity is incomplete")
	}
	digest := strings.TrimPrefix(manifest.RunnerAssetDigest, "sha256:")
	cachePath := filepath.Join(m.ProjectRoot, ".local", "state", "image", "downloads", "actions-runner", digest, manifest.RunnerAssetName)
	buildTrust, err := m.resolveBuildTrust(ctx)
	if err != nil {
		return "", err
	}
	client, err := buildTrustHTTPClient(buildTrust)
	if err != nil {
		return "", err
	}
	if err := verifiedDownload(ctx, client, manifest.RunnerAssetURL, cachePath, manifest.RunnerAssetDigest, 0o600); err != nil {
		return "", fmt.Errorf("acquire GitHub Actions runner %s: %w", manifest.RunnerVersion, err)
	}
	return cachePath, nil
}

func (m *Coordinator) installActionsRunnerPackage(ctx context.Context, instance string, manifest Manifest) error {
	path, err := m.acquireActionsRunner(ctx, manifest)
	if err != nil {
		return err
	}
	const guestPath = "/opt/epar/actions-runner.tar.gz"
	if err := provider.CopyFile(ctx, m.Provider, instance, path, guestPath, "0600"); err != nil {
		return fmt.Errorf("copy verified Actions runner package into build guest: %w", err)
	}
	_, err = m.execBuildGuest(ctx, instance, []string{"sudo", "bash", "/opt/epar/install-runner.sh", manifest.RunnerVersion, guestPath, manifest.RunnerAssetDigest}, provider.ExecOptions{})
	return err
}
