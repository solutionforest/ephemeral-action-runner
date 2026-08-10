package prebuilt

import (
	"strings"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

func TestRunnablePlatformDescriptorsIgnoresBuildKitAttestations(t *testing.T) {
	digest := func(value string) v1.Hash {
		hash, err := v1.NewHash("sha256:" + strings.Repeat(value, 64))
		if err != nil {
			t.Fatal(err)
		}
		return hash
	}
	index := &v1.IndexManifest{Manifests: []v1.Descriptor{
		{Digest: digest("a"), MediaType: types.OCIManifestSchema1, Size: 100, Platform: &v1.Platform{OS: "linux", Architecture: "amd64"}},
		{Digest: digest("b"), MediaType: types.OCIManifestSchema1, Size: 10, Platform: &v1.Platform{OS: "unknown", Architecture: "unknown"}, Annotations: map[string]string{"vnd.docker.reference.type": "attestation-manifest"}},
		{Digest: digest("c"), MediaType: types.OCIManifestSchema1, Size: 200, Platform: &v1.Platform{OS: "linux", Architecture: "arm64"}},
		{Digest: digest("d"), MediaType: types.OCIManifestSchema1, Size: 10, Platform: &v1.Platform{OS: "unknown", Architecture: "unknown"}, Annotations: map[string]string{"vnd.docker.reference.type": "attestation-manifest"}},
	}}

	platforms, err := runnablePlatformDescriptors(index, "ghcr.io/example/package@sha256:test")
	if err != nil {
		t.Fatal(err)
	}
	if len(platforms) != 2 || platforms["linux/amd64"].Digest != "sha256:"+strings.Repeat("a", 64) || platforms["linux/arm64"].Digest != "sha256:"+strings.Repeat("c", 64) {
		t.Fatalf("runnable platforms = %#v", platforms)
	}
}
