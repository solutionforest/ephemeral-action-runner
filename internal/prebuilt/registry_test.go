package prebuilt

import "testing"

func TestIsSigstoreBundleMediaType(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		mediaType string
		want      bool
	}{
		{name: "GitHub artifact attestation v0.3", mediaType: "application/vnd.dev.sigstore.bundle.v0.3+json", want: true},
		{name: "parameterized v0.3", mediaType: ReferrerBundleMediaType, want: true},
		{name: "legacy bundle", mediaType: "application/vnd.dev.sigstore.bundle+json", want: true},
		{name: "unrelated JSON", mediaType: "application/json", want: false},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := isSigstoreBundleMediaType(test.mediaType); got != test.want {
				t.Fatalf("isSigstoreBundleMediaType(%q) = %t, want %t", test.mediaType, got, test.want)
			}
		})
	}
}
