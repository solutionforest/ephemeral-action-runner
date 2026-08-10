package image

import "testing"

func TestWSLArtifactPaths(t *testing.T) {
	tests := map[string]string{
		"runner.tar":    "runner.source.rootfs.tar",
		"runner.tar.gz": "runner.source.rootfs.tar",
		"runner.tgz":    "runner.source.rootfs.tar",
		"runner":        "runner.source.rootfs.tar",
	}
	for output, want := range tests {
		if got := WSLSourceRootfsPath(output); got != want {
			t.Errorf("WSLSourceRootfsPath(%q) = %q, want %q", output, got, want)
		}
	}
	if got, want := WSLImageManifestPath("runner.tar"), "runner.tar.epar-manifest.json"; got != want {
		t.Fatalf("WSLImageManifestPath() = %q, want %q", got, want)
	}
}
