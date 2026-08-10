package image

import (
	"errors"
	"fmt"
	"testing"
)

type typedNilPullError struct{}

func (*typedNilPullError) Error() string { return "pull failed" }

type opaqueNilPullError struct{}

func (opaqueNilPullError) Error() string { return "<nil>" }

func TestNilLikeError(t *testing.T) {
	var typedNil *typedNilPullError
	if !nilLikeError(typedNil) {
		t.Fatal("typed nil error was not recognized")
	}
	if !nilLikeError(fmt.Errorf("wrapped: %w", typedNil)) {
		t.Fatal("wrapped typed nil error was not recognized")
	}
	if !nilLikeError(opaqueNilPullError{}) {
		t.Fatal("opaque nil-rendering error was not recognized")
	}
	if nilLikeError(errors.New("real failure")) {
		t.Fatal("real error was recognized as nil")
	}
}

func TestNormalizedDockerPlatform(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
		valid bool
	}{
		{name: "explicit amd64", input: "linux/x86_64", want: "linux/amd64", valid: true},
		{name: "arm variant", input: "linux/aarch64/v8", want: "linux/arm64/v8", valid: true},
		{name: "architecture only", input: "amd64", want: "linux/amd64", valid: true},
		{name: "empty", input: "", valid: false},
		{name: "too many segments", input: "linux/amd64/v8/extra", valid: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := NormalizedDockerPlatform(test.input, "")
			if ok != test.valid {
				t.Fatalf("valid = %t, want %t", ok, test.valid)
			}
			if ok && got.OS+"/"+got.Architecture+platformVariant(got.Variant) != test.want {
				t.Fatalf("platform = %s/%s%s, want %s", got.OS, got.Architecture, platformVariant(got.Variant), test.want)
			}
		})
	}
}

func TestDockerPullProgressSummary(t *testing.T) {
	layers := map[string]DockerPullProgress{
		"completed": {Current: 1024, Total: 1024, Completed: true},
		"partial":   {Current: 512, Total: 1024},
		"unknown":   {Completed: true},
	}
	if got, want := DockerPullProgressSummary(layers), "Docker source pull: 2/3 layers complete; 1.5 KiB/2.0 KiB (75%); 1 layer(s) size pending"; got != want {
		t.Fatalf("summary = %q, want %q", got, want)
	}
}

func TestDockerImageReferenceAndAuthRejectsInvalidReference(t *testing.T) {
	if _, _, err := DockerImageReferenceAndAuth("not a valid image reference"); err == nil {
		t.Fatal("invalid image reference was accepted")
	}
}

func platformVariant(variant string) string {
	if variant == "" {
		return ""
	}
	return "/" + variant
}
