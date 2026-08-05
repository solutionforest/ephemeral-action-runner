package main

import (
	"io"
	"os"
	"runtime"
	"strings"
	"testing"
)

func TestVersionStringDefaults(t *testing.T) {
	got := versionString()
	for _, want := range []string{
		"ephemeral-action-runner dev",
		"commit: unknown",
		"buildDate: unknown",
		"sourceRevision: unknown",
		"sourceDigest: unknown",
		"buildDigest: unknown",
		"controllerSlot: unmanaged",
		"go: " + runtime.Version(),
		"platform: " + runtime.GOOS + "/" + runtime.GOARCH,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("versionString() missing %q in:\n%s", want, got)
		}
	}
}

func TestVersionStringInjectedMetadata(t *testing.T) {
	oldVersion, oldCommit, oldBuildDate, oldSourceRevision, oldSourceDigest, oldBuildDigest := version, commit, buildDate, sourceRevision, sourceDigest, buildDigest
	t.Cleanup(func() {
		version, commit, buildDate, sourceRevision, sourceDigest, buildDigest = oldVersion, oldCommit, oldBuildDate, oldSourceRevision, oldSourceDigest, oldBuildDigest
	})

	version = "v1.2.3-beta.1"
	commit = "abc1234"
	buildDate = "2026-07-07T00:00:00Z"
	sourceRevision = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	sourceDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	buildDigest = "sha256:2222222222222222222222222222222222222222222222222222222222222222"

	got := versionString()
	for _, want := range []string{
		"ephemeral-action-runner v1.2.3-beta.1",
		"commit: abc1234",
		"buildDate: 2026-07-07T00:00:00Z",
		"sourceRevision: sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		"sourceDigest: sha256:1111111111111111111111111111111111111111111111111111111111111111",
		"buildDigest: sha256:2222222222222222222222222222222222222222222222222222222222222222",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("versionString() missing %q in:\n%s", want, got)
		}
	}
}

func TestVersionStringControllerSlot(t *testing.T) {
	t.Setenv("EPAR_CONTROLLER_SLOT", "windows-amd64-old")
	if got := versionString(); !strings.Contains(got, "controllerSlot: windows-amd64-old") {
		t.Fatalf("versionString() = %q", got)
	}
}

func TestRunVersionCommand(t *testing.T) {
	got, err := captureStdout(t, func() error {
		return run([]string{"version"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "ephemeral-action-runner dev") {
		t.Fatalf("version output = %q", got)
	}
}

func TestRunCommandRoutingErrors(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{args: []string{"image"}, want: "image requires subcommand"},
		{args: []string{"pool"}, want: "pool requires subcommand"},
		{args: []string{"bogus"}, want: `unknown command "bogus"`},
	} {
		err := run(tc.args)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("run(%v) error = %v, want containing %q", tc.args, err, tc.want)
		}
	}
}

func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()

	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	defer func() {
		os.Stdout = oldStdout
	}()

	fnErr := fn()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(out), fnErr
}
