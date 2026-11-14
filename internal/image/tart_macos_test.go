package image

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/solutionforest/ephemeral-action-runner/internal/config"
)

func TestTartMacOSRunnerUsesVerifiedOSXPackage(t *testing.T) {
	manifest := Manifest{ProviderType: "tart", ProviderPlatform: config.TartMacOSPlatform}
	platform, err := actionsRunnerPlatform(manifest, "tart")
	if err != nil || platform != config.TartMacOSPlatform {
		t.Fatalf("platform=%s err=%v", platform, err)
	}
	resolved, err := resolveActionsRunnerFallback(filepath.Join("..", ".."), manifest, "latest", "arm64")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.RunnerAssetName != "actions-runner-osx-arm64-2.336.0.tar.gz" || resolved.RunnerAssetDigest != "sha256:8e8839c49b7060b6b2154f4931f815df330c27f167d53ef2239ee3dfce28b079" {
		t.Fatalf("wrong native runner: %+v", resolved)
	}
	_, err = applyActionsRunnerRelease(manifest, "latest", "arm64", actionsRunnerRelease{TagName: "v1.0.0", Assets: []actionsRunnerAsset{{Name: "actions-runner-linux-arm64-1.0.0.tar.gz", Digest: "sha256:" + strings.Repeat("a", 64)}}})
	if err == nil {
		t.Fatal("macOS accepted a Linux-only runner release")
	}
}

func TestTartSourceRejectsWrongPlatformAndNonVMArtifacts(t *testing.T) {
	manifest := v1.Manifest{Annotations: map[string]string{"org.cirruslabs.tart.uncompressed-disk-size": "140000000000"}, Layers: []v1.Descriptor{{MediaType: types.MediaType("application/vnd.cirruslabs.tart.disk.v2"), Size: 1234}}}
	guest := v1.ConfigFile{OS: "darwin", Architecture: "arm64"}
	source, err := parseTartSource("image:tag", "image@sha256:digest", config.TartMacOSPlatform, &manifest, &guest)
	if err != nil {
		t.Fatal(err)
	}
	if source.ExpandedBytes != 140000000000 || source.CompressedBytes != 1234 || source.ImmutableReference != "image@sha256:digest" {
		t.Fatalf("wrong Tart source metadata: %+v", source)
	}
	for _, test := range []struct {
		name   string
		change func(*v1.Manifest, *v1.ConfigFile)
	}{
		{"Linux guest", func(_ *v1.Manifest, c *v1.ConfigFile) { c.OS = "linux" }},
		{"x64 guest", func(_ *v1.Manifest, c *v1.ConfigFile) { c.Architecture = "amd64" }},
		{"missing expansion size", func(m *v1.Manifest, _ *v1.ConfigFile) { m.Annotations = nil }},
		{"negative layer size", func(m *v1.Manifest, _ *v1.ConfigFile) { m.Layers = []v1.Descriptor{{Size: -1}} }},
		{"Docker layer", func(m *v1.Manifest, _ *v1.ConfigFile) {
			m.Layers = []v1.Descriptor{{Size: 10, MediaType: types.DockerLayer}}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			m := manifest
			c := guest
			test.change(&m, &c)
			if _, err := parseTartSource("image:tag", "image@sha256:digest", config.TartMacOSPlatform, &m, &c); err == nil {
				t.Fatal("invalid native VM metadata accepted")
			}
		})
	}
}

func TestTartMacOSChangesScriptIdentityAndSchedulesMutableSource(t *testing.T) {
	coordinator := Coordinator{ProjectRoot: filepath.Join("..", ".."), Config: config.Config{Provider: config.ProviderConfig{Type: "tart", Platform: config.TartMacOSPlatform}}}
	scripts, err := coordinator.eparScriptDigests()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, script := range scripts {
		if strings.Contains(script.Path, "guest/ubuntu/") {
			t.Fatalf("macOS image includes Ubuntu script: %s", script.Path)
		}
		if strings.HasSuffix(script.Path, "macos/run-runner.sh") {
			found = true
		}
	}
	if !found {
		t.Fatal("macOS runtime is absent from image identity")
	}
	if !manifestHasMutableRemoteInputs(Manifest{SourceType: config.ImageSourceTartImage, SourceImage: "ghcr.io/example/image:tag", RunnerSelector: "2.336.0"}) {
		t.Fatal("mutable Tart tags bypass the remote update schedule")
	}
}
